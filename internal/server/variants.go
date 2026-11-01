package server

import (
	"container/list"
	"context"
	"errors"
	"sync"

	"github.com/hbroek/wako/internal/img"
	"github.com/hbroek/wako/internal/store"
)

// Smaller copies of large pictures are made on request. A page asks for one with
// ?w=N (N pixels wide), ?h=N (N pixels high), and the server itself uses ?s=N
// (longest side N) for the default picture size and library thumbnails. N must be
// one of sizeLadder, so nobody can make the server scale to arbitrary sizes.
const (
	widthContent = 800 // longest side of a picture in a page, unless it asks for another size
	widthThumb   = 240 // longest side of a library thumbnail
)

// 60 is the thumbnail size.
var sizeLadder = []int{60, 240, 400, 600, 800, 1200, 1600}

func validSize(n int) bool {
	for _, v := range sizeLadder {
		if v == n {
			return true
		}
	}
	return false
}

// limitFor turns "n pixels wide" (dim 'w'), "n pixels high" ('h') or "longest
// side n" ('s') into a longest-side limit for this asset, or 0 when its size is unknown.
func limitFor(a *store.AssetInfo, dim byte, n int) int {
	return limitForSize(a.Width, a.Height, dim, n)
}

// limitForSize is limitFor for a picture of w by h pixels.
func limitForSize(w, h int, dim byte, n int) int {
	long := w
	if h > long {
		long = h
	}
	base := long
	switch dim {
	case 'w':
		base = w
	case 'h':
		base = h
	}
	if base <= 0 || long <= 0 {
		return 0
	}
	return (n*long + base/2) / base
}

// minFor is the file size below which a copy is not worth making: a bigger
// bar for the default page picture than for a size someone asked for.
func minFor(dim byte, n int) int64 {
	if dim == 's' && n >= widthContent {
		return 100 << 10
	}
	return 20 << 10
}

// scalable reports whether a copy with this longest-side limit is made: JPEG or
// PNG only (a GIF may be animated, WebP cannot be re-encoded, SVG is already
// vector), longer than the limit on its longest side, and bigger than min bytes.
// Whether the copy is then really smaller is decided when it is built.
func scalable(a *store.AssetInfo, limit int, min int64) bool {
	if limit <= 0 || (a.Mime != "image/jpeg" && a.Mime != "image/png") || a.Width <= 0 || a.Height <= 0 {
		return false
	}
	long := a.Width
	if a.Height > long {
		long = a.Height
	}
	return long > limit && a.Size > min
}

type vkey struct {
	id string
	w  int // longest side of the copy
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
