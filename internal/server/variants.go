package server

import (
	"container/list"
	"context"
	"errors"
	"sync"

	"github.com/hbroek/wako/internal/img"
	"github.com/hbroek/wako/internal/store"
)

// Smaller copies of large pictures are made on request with ?w=. Only these two
// widths exist, so nobody can make the server scale to arbitrary sizes.
const (
	widthContent = 800 // pictures in pages
	widthThumb   = 240 // library thumbnails
)

// variantMinSize is the file size below which a copy is not worth making.
func variantMinSize(w int) (int64, bool) {
	switch w {
	case widthContent:
		return 100 << 10, true
	case widthThumb:
		return 20 << 10, true
	}
	return 0, false
}

// scalable reports whether a copy of a at width w is made: JPEG or PNG only
// (a GIF may be animated, WebP cannot be re-encoded, SVG is already vector),
// longer than w on its longest side, and big enough to be worth shrinking.
// Whether the copy is then really smaller is decided when it is built.
func scalable(a *store.AssetInfo, w int) bool {
	min, ok := variantMinSize(w)
	if !ok || (a.Mime != "image/jpeg" && a.Mime != "image/png") || a.Width <= 0 || a.Height <= 0 {
		return false
	}
	long := a.Width
	if a.Height > long {
		long = a.Height
	}
	return long > w && a.Size > min
}

type vkey struct {
	id string
	w  int
}

// variant is a built copy. original means "the copy was not smaller; serve the
// original", which is cached too so the work is not repeated.
type variant struct {
	data     []byte
	mime     string
	w, h     int
	original bool
}

func (v *variant) cost() int64 { return int64(len(v.data)) + 64 }

type flight struct {
	done chan struct{}
	v    *variant
	err  error
}

// variantCache is an in-memory LRU of built copies with a byte budget. Equal
// requests share one build (single-flight), and a semaphore limits how many
// builds run at once so a burst cannot starve the server.
type variantCache struct {
	mu      sync.Mutex
	max     int64
	used    int64
	ll      *list.List // front = most recently used
	items   map[vkey]*list.Element
	flights map[vkey]*flight
	work    chan struct{}
}

type entry struct {
	key vkey
	v   *variant
}

func newVariantCache(maxBytes int64, work chan struct{}) *variantCache {
	return &variantCache{max: maxBytes, ll: list.New(), items: map[vkey]*list.Element{},
		flights: map[vkey]*flight{}, work: work}
}

// acquire takes one of the shared work slots.
func acquire(ctx context.Context, work chan struct{}) error {
	if err := ctx.Err(); err != nil { // never start work for a request that is gone
		return err
	}
	select {
	case work <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *variantCache) get(ctx context.Context, k vkey, build func() (*variant, error)) (*variant, error) {
	for {
		c.mu.Lock()
		if el, ok := c.items[k]; ok {
			c.ll.MoveToFront(el)
			v := el.Value.(*entry).v
			c.mu.Unlock()
			return v, nil
		}
		if f, ok := c.flights[k]; ok {
			c.mu.Unlock()
			select {
			case <-f.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			// The builder's own request may have been cancelled; try again then.
			if errors.Is(f.err, context.Canceled) && ctx.Err() == nil {
				continue
			}
			return f.v, f.err
		}
		f := &flight{done: make(chan struct{})}
		c.flights[k] = f
		c.mu.Unlock()

		if f.err = acquire(ctx, c.work); f.err == nil {
			f.v, f.err = build()
			<-c.work
		}
		c.mu.Lock()
		delete(c.flights, k)
		if f.err == nil {
			c.add(k, f.v)
		}
		c.mu.Unlock()
		close(f.done)
		return f.v, f.err
	}
}

// add stores v and evicts the least recently used copies over budget.
func (c *variantCache) add(k vkey, v *variant) {
	if v.cost() > c.max {
		return
	}
	c.items[k] = c.ll.PushFront(&entry{k, v})
	c.used += v.cost()
	for c.used > c.max {
		el := c.ll.Back()
		e := el.Value.(*entry)
		c.ll.Remove(el)
		delete(c.items, e.key)
		c.used -= e.v.cost()
	}
}

// buildVariant scales an asset to width w, keeping the original when the copy
// would not be smaller.
func buildVariant(sp *store.Space, a *store.AssetInfo, w int) (*variant, error) {
	full, err := sp.GetAsset(a.ID)
	if err != nil {
		return nil, err
	}
	out, vw, vh, err := img.Scale(full.Data, full.Mime, w)
	if err != nil || len(out) >= len(full.Data) {
		return &variant{original: true}, nil
	}
	return &variant{data: out, mime: full.Mime, w: vw, h: vh}, nil
}
