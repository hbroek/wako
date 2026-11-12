package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hbroek/wako/internal/img"
)

func noiseImg(w, h int) *image.RGBA {
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < len(im.Pix); i += 4 {
		im.Pix[i], im.Pix[i+1], im.Pix[i+2], im.Pix[i+3] = uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255
	}
	return im
}

func jpegOf(t *testing.T, im image.Image, segs ...[]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, im, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	out := append([]byte(nil), b.Bytes()[:2]...)
	for _, s := range segs {
		out = append(out, s...)
	}
	return append(out, b.Bytes()[2:]...)
}

func pngOf(t *testing.T, im image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func app1(payload []byte) []byte {
	n := len(payload) + 2
	return append([]byte{0xFF, 0xE1, byte(n >> 8), byte(n)}, payload...)
}

func exifWithOrientation(o int, extra string) []byte {
	tiff := []byte("II*\x00\x08\x00\x00\x00")
	tiff = binary.LittleEndian.AppendUint16(tiff, 1)
	tiff = binary.LittleEndian.AppendUint16(tiff, 0x0112)
	tiff = binary.LittleEndian.AppendUint16(tiff, 3)
	tiff = binary.LittleEndian.AppendUint32(tiff, 1)
	tiff = binary.LittleEndian.AppendUint16(tiff, uint16(o))
	tiff = append(tiff, 0, 0, 0, 0, 0, 0)
	tiff = append(tiff, extra...)
	return app1(append([]byte("Exif\x00\x00"), tiff...))
}

func (e *env) post(space, name, contentType string, data []byte, query ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	q := "name=" + name
	for _, x := range query {
		q += "&" + x
	}
	return e.do("POST", "/"+space+"/_assets?"+q, string(data), "Content-Type", contentType)
}

func asset(t *testing.T, rec *httptest.ResponseRecorder, status int) assetJSON {
	t.Helper()
	var a assetJSON
	body := expect(t, rec, status)
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("bad JSON %q: %v", body, err)
	}
	return a
}

func decodeSize(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not an image: %v", err)
	}
	return cfg.Width, cfg.Height
}

func TestUploadStripsMetadataAndRecordsSize(t *testing.T) {
	e := newEnv(t)
	dirty := jpegOf(t, noiseImg(64, 48), exifWithOrientation(1, "GPSLatitude=52.37"), app1([]byte("http://ns.adobe.com/xap/1.0/\x00secret-xmp")))
	dirty = append(dirty, "TRAILING-SECRET"...)
	a := asset(t, e.post("main", "holiday.jpg", "image/jpeg", dirty), 201)
	if a.Width != 64 || a.Height != 48 {
		t.Fatalf("size not recorded: %+v", a)
	}
	body := expect(t, e.do("GET", a.URL, ""), 200)
	for _, secret := range []string{"GPSLatitude", "secret-xmp", "TRAILING", "Exif"} {
		if strings.Contains(body, secret) {
			t.Errorf("%q survived upload", secret)
		}
	}
	if len(body) >= len(dirty) {
		t.Error("stored file not smaller than the upload")
	}
	// The same photo with different metadata is the same stored picture.
	again := asset(t, e.post("main", "other.jpg", "image/jpeg", jpegOf(t, noiseImg(64, 48), exifWithOrientation(1, "different"))), 200)
	if again.ID != a.ID {
		t.Fatal("cleaned duplicates should dedupe")
	}
}

func TestUploadAppliesOrientation(t *testing.T) {
	e := newEnv(t)
	a := asset(t, e.post("main", "phone.jpg", "image/jpeg", jpegOf(t, noiseImg(40, 20), exifWithOrientation(6, ""))), 201)
	if a.Width != 20 || a.Height != 40 {
		t.Fatalf("rotated size %dx%d", a.Width, a.Height)
	}
	if w, h := decodeSize(t, []byte(expect(t, e.do("GET", a.URL, ""), 200))); w != 20 || h != 40 {
		t.Fatalf("served %dx%d", w, h)
	}
}

func TestUploadCompactsAndModes(t *testing.T) {
	e := newEnv(t)
	big := jpegOf(t, noiseImg(2400, 1200))
	a := asset(t, e.post("main", "big.jpg", "image/jpeg", big), 201)
	if a.Width != 1600 || a.Height != 800 {
		t.Fatalf("compact: %dx%d", a.Width, a.Height)
	}
	if w, h := decodeSize(t, []byte(expect(t, e.do("GET", a.URL, ""), 200))); w != 1600 || h != 800 {
		t.Fatalf("served %dx%d", w, h)
	}
	orig := asset(t, e.post("main", "big.jpg", "image/jpeg", big, "mode=original"), 201)
	if orig.Width != 2400 || orig.Name != "big-2.jpg" {
		t.Fatalf("original mode: %+v", orig)
	}
	expect(t, e.post("main", "big.jpg", "image/jpeg", big, "mode=sharpen"), 400)

	// The mode can also be set for the whole server.
	srv := New(e.st, "")
	srv.ImageLimits.Mode = img.Original
	req := localReq("POST", "/main/_assets?name=again.jpg", bytes.NewReader(jpegOf(t, noiseImg(1800, 100))))
	req.Header.Set("Content-Type", "image/jpeg")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if a := asset(t, rec, 201); a.Width != 1800 {
		t.Fatalf("server-wide original mode: %+v", a)
	}
}

func TestUploadLimits(t *testing.T) {
	e := newEnv(t)
	srv := New(e.st, "")
	srv.ImageLimits.MaxPixels = 1_000_000
	srv.ImageLimits.MaxStored = 50_000
	srv.ImageLimits.MaxFrames = 2
	up := func(name, ct string, data []byte) int {
		req := localReq("POST", "/main/_assets?name="+name, bytes.NewReader(data))
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	// A small file that unpacks into a huge bitmap.
	if got := up("flat.png", "image/png", pngOf(t, image.NewGray(image.Rect(0, 0, 3000, 3000)))); got != 413 {
		t.Errorf("pixel limit: %d", got)
	}
	// Too big once processed.
	if got := up("noisy.jpg", "image/jpeg", jpegOf(t, noiseImg(400, 400))); got != 413 {
		t.Errorf("stored limit: %d", got)
	}
	// Too many GIF frames.
	g := &gif.GIF{}
	for i := 0; i < 3; i++ {
		g.Image = append(g.Image, image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.Black, color.White}))
		g.Delay = append(g.Delay, 1)
	}
	var gb bytes.Buffer
	gif.EncodeAll(&gb, g)
	if got := up("anim.gif", "image/gif", gb.Bytes()); got != 413 {
		t.Errorf("frame limit: %d", got)
	}
	// Looks like a JPEG but is cut off.
	jpg := jpegOf(t, noiseImg(100, 100))
	if got := up("cut.jpg", "image/jpeg", jpg[:len(jpg)/2]); got != 400 {
		t.Errorf("truncated: %d", got)
	}
	// And a good small one still works.
	if got := up("ok.png", "image/png", pngBytes(t, 9)); got != 201 {
		t.Errorf("valid upload: %d", got)
	}
}

func TestVariantsServedAndCached(t *testing.T) {
	e := newEnv(t)
	big := asset(t, e.post("main", "photo.jpg", "image/jpeg", jpegOf(t, noiseImg(1600, 1200))), 201)
	if big.Size < 100<<10 {
		t.Fatalf("test image too small for a variant: %d", big.Size)
	}
	rec := e.do("GET", big.URL+"?w=800", "")
	body := expect(t, rec, 200)
	if w, h := decodeSize(t, []byte(body)); w != 800 || h != 600 {
		t.Fatalf("variant %dx%d", w, h)
	}
	if len(body) >= int(big.Size) {
		t.Error("variant not smaller than the original")
	}
	h := rec.Header()
	if h.Get("ETag") != `"`+big.ID+`-w800"` || h.Get("Content-Type") != "image/jpeg" ||
		!strings.Contains(h.Get("Cache-Control"), "immutable") || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("variant headers: %v", h)
	}
	// A revalidation is answered without building anything.
	srv := e.h.(*Server)
	before := srv.variants.used
	expect(t, e.do("GET", big.URL+"?w=800", "", "If-None-Match", `"`+big.ID+`-w800"`), 304)
	if srv.variants.used != before {
		t.Error("revalidation changed the cache")
	}
	if srv.variants.used == 0 {
		t.Error("variant was not cached")
	}
	// Thumbnails exist too, and the original is unaffected.
	if w, _ := decodeSize(t, []byte(expect(t, e.do("GET", big.URL+"?w=240", ""), 200))); w != 240 {
		t.Errorf("thumbnail width %d", w)
	}
	if w, _ := decodeSize(t, []byte(expect(t, e.do("GET", big.URL, ""), 200))); w != 1600 {
		t.Errorf("original width %d", w)
	}
	// Only the two whitelisted widths.
	for _, bad := range []string{"500", "0", "-1", "abc", "9999", "801"} {
		expect(t, e.do("GET", big.URL+"?w="+bad, ""), 400)
	}
	// Variants obey the space's read rules: another space has no such image.
	expect(t, e.do("GET", "/other/_assets/"+big.ID+"/photo.jpg?w=800", ""), 404)
}

func TestVariantRules(t *testing.T) {
	e := newEnv(t)
	// Small pictures and files under the size threshold are served as they are.
	small := asset(t, e.post("main", "small.jpg", "image/jpeg", jpegOf(t, noiseImg(500, 400))), 201)
	r := e.do("GET", small.URL+"?w=800", "")
	expect(t, r, 200)
	if r.Header().Get("ETag") != `"`+small.ID+`"` {
		t.Errorf("small image got variant etag %q", r.Header().Get("ETag"))
	}
	// Wide but tiny in bytes (flat colour): not worth a copy.
	flat := asset(t, e.post("main", "flat.png", "image/png", pngOf(t, image.NewGray(image.Rect(0, 0, 2000, 1000)))), 201)
	if flat.Size > 100<<10 {
		t.Fatalf("flat png unexpectedly large: %d", flat.Size)
	}
	if r := e.do("GET", flat.URL+"?w=800", ""); r.Header().Get("ETag") != `"`+flat.ID+`"` {
		t.Errorf("flat png got a variant")
	}
	// Large PNGs scale and stay PNGs.
	bigPNG := asset(t, e.post("main", "shot.png", "image/png", pngOf(t, noiseImg(1500, 900))), 201)
	r = e.do("GET", bigPNG.URL+"?w=800", "")
	body := expect(t, r, 200)
	if w, _ := decodeSize(t, []byte(body)); w != 800 || r.Header().Get("Content-Type") != "image/png" {
		t.Errorf("png variant: width %d type %s", w, r.Header().Get("Content-Type"))
	}
	// A GIF may be animated: never rescaled, even when ?w= is valid.
	g := &gif.GIF{}
	pal := color.Palette{color.Black, color.White}
	for i := 0; i < 2; i++ {
		f := image.NewPaletted(image.Rect(0, 0, 1000, 1000), pal)
		for k := range f.Pix {
			f.Pix[k] = uint8((k / 7 * (i + 1)) % 2)
		}
		g.Image = append(g.Image, f)
		g.Delay = append(g.Delay, 3)
	}
	var gb bytes.Buffer
	gif.EncodeAll(&gb, g)
	ga := asset(t, e.post("main", "anim.gif", "image/gif", gb.Bytes()), 201)
	if r := e.do("GET", ga.URL+"?w=240", ""); r.Header().Get("ETag") != `"`+ga.ID+`"` || r.Header().Get("Content-Type") != "image/gif" {
		t.Errorf("gif was scaled: %v", r.Header())
	}
}

func TestSmallPicturesAreEnlargedToTheSizeAsked(t *testing.T) {
	e := newEnv(t)
	small := asset(t, e.post("main", "small.png", "image/png", pngBytes(t, 4)), 201)
	expect(t, e.do("PUT", "/main/enlarged", "![a](small.png#w=240)\n\n![b](small.png#h=400)\n", "Content-Type", md), 201)
	html := expect(t, e.do("GET", "/main/enlarged", "", "Accept", "text/html"), 200)
	for _, want := range []string{`src="` + small.URL + `"`, `width="240"`, `height="240"`, `width="400"`, `height="400"`} {
		if !strings.Contains(html, want) {
			t.Errorf("html missing %s\n%s", want, html)
		}
	}
}

func TestPagesUseSizedLazyImages(t *testing.T) {
	e := newEnv(t)
	big := asset(t, e.post("main", "big.jpg", "image/jpeg", jpegOf(t, noiseImg(1600, 1200))), 201)
	small := asset(t, e.post("main", "small.png", "image/png", pngBytes(t, 4)), 201)
	src := "![first](big.jpg)\n\n![second](small.png)\n\n![third](big.jpg)\n"
	expect(t, e.do("PUT", "/main/gallery", src, "Content-Type", md), 201)

	html := expect(t, e.do("GET", "/main/gallery", "", "Accept", "text/html"), 200)
	for _, want := range []string{
		`<a href="` + big.URL + `"`, `src="` + big.URL + `?s=800"`, `width="800"`, `height="600"`, `style="--r:1.3333"`,
		`src="` + small.URL + `"`, `width="4"`, `height="4"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("html missing %s\n%s", want, html)
		}
	}
	if strings.Count(html, `decoding="async"`) != 3 || strings.Count(html, `loading="lazy"`) != 2 {
		t.Errorf("async/lazy attributes: %s", html)
	}
	first := html[strings.Index(html, "<img"):]
	if first = first[:strings.Index(first, ">")]; strings.Contains(first, "loading") {
		t.Errorf("first image should load eagerly: %s", first)
	}
	// Small images are not wrapped in a link to themselves.
	if strings.Contains(html, `<a href="`+small.URL) {
		t.Error("small image wrapped in a link")
	}
	// The Markdown is untouched.
	if got := expect(t, e.do("GET", "/main/gallery.md", ""), 200); got != src {
		t.Fatalf("markdown changed: %q", got)
	}
	// The live preview renders the same way.
	prev := expect(t, e.do("POST", "/_api/spaces/main/render?page=gallery", "![x](big.jpg)", "Content-Type", md), 200)
	if !strings.Contains(prev, `?s=800`) || !strings.Contains(prev, `width="800"`) {
		t.Errorf("preview: %s", prev)
	}
}

func TestMediaQuota(t *testing.T) {
	e := newEnv(t)
	a := asset(t, e.post("main", "a.png", "image/png", pngBytes(t, 1)), 201)
	b := pngBytes(t, 2)
	e.st.Quota = a.Size + int64(len(b)) // room for exactly these two
	asset(t, e.post("main", "b.png", "image/png", b), 201)
	// Full: a third distinct image is refused, but known content still dedupes.
	rec := e.post("main", "c.png", "image/png", pngBytes(t, 3))
	if body := expect(t, rec, 507); !strings.Contains(body, "quota") {
		t.Errorf("message: %s", body)
	}
	asset(t, e.post("main", "again.png", "image/png", b), 200)
	// Quotas are per space.
	e.st.Quota = a.Size + 10
	asset(t, e.post("other", "c.png", "image/png", pngBytes(t, 3)), 201)
	// Deleting frees room.
	e.st.Quota = a.Size + int64(len(b)) + 5
	expect(t, e.do("DELETE", "/main/_assets/"+a.ID, ""), 204)
	asset(t, e.post("main", "c.png", "image/png", pngBytes(t, 3)), 201)
	// Negative means unlimited.
	e.st.Quota = -1
	asset(t, e.post("main", "d.png", "image/png", pngBytes(t, 5)), 201)
	e.st.Quota = 10 << 20
	page := expect(t, e.do("GET", "/main/_assets/", "", "Accept", "text/html"), 200)
	if !strings.Contains(page, "of 10.0 MB") {
		t.Errorf("usage not shown: %s", page)
	}
}

func TestLibraryShowsThumbnails(t *testing.T) {
	e := newEnv(t)
	big := asset(t, e.post("main", "big.jpg", "image/jpeg", jpegOf(t, noiseImg(1600, 1200))), 201)
	asset(t, e.post("main", "tiny.png", "image/png", pngBytes(t, 4)), 201)
	page := expect(t, e.do("GET", "/main/_assets/", "", "Accept", "text/html"), 200)
	for _, want := range []string{`src="` + big.URL + `?s=240"`, `width="240"`, `height="180"`, "1600×1200", "of 200.0 MB"} {
		if !strings.Contains(page, want) {
			t.Errorf("library page missing %q", want)
		}
	}
	list := expect(t, e.do("GET", "/main/_assets", "", "Accept", "application/json"), 200)
	if !strings.Contains(list, `"width":1600`) {
		t.Errorf("json dims: %s", list)
	}
}

// ---- the cache itself ----

func TestVariantCacheSingleFlight(t *testing.T) {
	work := make(chan struct{}, 2)
	c := newVariantCache(1<<20, work)
	var builds int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.get(context.Background(), vkey{"id", 800}, func() (*variant, error) {
				atomic.AddInt32(&builds, 1)
				time.Sleep(30 * time.Millisecond)
				return &variant{data: []byte("x")}, nil
			})
			if err != nil || string(v.data) != "x" {
				t.Errorf("get: %v %v", v, err)
			}
		}()
	}
	wg.Wait()
	if builds != 1 {
		t.Fatalf("built %d times, want once", builds)
	}
}

func TestVariantCacheLimitsConcurrencyAndEvicts(t *testing.T) {
	work := make(chan struct{}, 2)
	c := newVariantCache(1000, work)
	var running, peak int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.get(context.Background(), vkey{string(rune('a' + i)), 800}, func() (*variant, error) {
				n := atomic.AddInt32(&running, 1)
				for {
					p := atomic.LoadInt32(&peak)
					if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				atomic.AddInt32(&running, -1)
				return &variant{data: make([]byte, 300)}, nil
			})
		}(i)
	}
	wg.Wait()
	if peak > 2 {
		t.Fatalf("%d builds ran at once, limit is 2", peak)
	}
	if c.used > 1000 || len(c.items) > 3 {
		t.Fatalf("budget exceeded: used %d, %d items", c.used, len(c.items))
	}
	// Least recently used goes first.
	c = newVariantCache(800, work) // room for two 364-byte entries
	build := func() (*variant, error) { return &variant{data: make([]byte, 300)}, nil }
	ctx := context.Background()
	c.get(ctx, vkey{"a", 1}, build)
	c.get(ctx, vkey{"b", 1}, build)
	c.get(ctx, vkey{"a", 1}, build) // touch a
	c.get(ctx, vkey{"c", 1}, build) // evicts b
	if _, ok := c.items[vkey{"b", 1}]; ok {
		t.Error("b should have been evicted")
	}
	if _, ok := c.items[vkey{"a", 1}]; !ok {
		t.Error("a was evicted although recently used")
	}
	// A cancelled request does not poison the cache.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := newVariantCache(1<<20, work).get(cctx, vkey{"z", 1}, build); err == nil {
		t.Error("cancelled request should fail")
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// ---- images next to their pages ----

func TestImagesResolveNextToThePage(t *testing.T) {
	e := newEnv(t)
	data := pngBytes(t, 11)
	shot := asset(t, e.upload("main", "shot.png", data), 201)
	asset(t, e.upload("main", "unused.png", pngBytes(t, 12)), 201)
	putPage(t, e, "main", "docs", "# Docs")
	putPage(t, e, "main", "docs/ops", "# Ops\n\n![the shot](shot.png)\n")

	// A client with only the page's Markdown URL resolves the bare name the way
	// any Markdown viewer does, and gets the picture.
	base, _ := url.Parse("http://wako.test/main/docs/ops.md")
	ref, _ := url.Parse("shot.png")
	resolved := base.ResolveReference(ref)
	if resolved.Path != "/main/docs/shot.png" {
		t.Fatalf("resolved to %s", resolved.Path)
	}
	rec := e.do("GET", resolved.Path, "")
	if got := expect(t, rec, 200); got != string(data) {
		t.Fatal("wrong bytes")
	}
	h := rec.Header()
	if h.Get("Content-Type") != "image/png" || h.Get("ETag") != `"`+shot.ID+`"` || h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(h.Get("Cache-Control"), "no-cache") || strings.Contains(h.Get("Cache-Control"), "immutable") {
		t.Errorf("headers: %v", h)
	}
	expect(t, e.do("HEAD", resolved.Path, ""), 200)
	expect(t, e.do("GET", resolved.Path, "", "If-None-Match", `"`+shot.ID+`"`), 304)
	// Even with a browser's Accept header for images.
	expect(t, e.do("GET", resolved.Path, "", "Accept", "image/avif,image/webp,*/*;q=0.8"), 200)

	// Only next to pages that use it.
	for _, p := range []string{"/main/shot.png", "/main/docs/ops/shot.png", "/main/docs/other/shot.png", "/main/docs/unused.png", "/main/docs/nope.png"} {
		if rec := e.do("GET", p, ""); rec.Code != 404 {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	// The same image appears next to every page that uses it, still stored once.
	putPage(t, e, "main", "docs/ops/child", "![c](shot.png)")
	putPage(t, e, "main", "top", "![t](shot.png)")
	for _, p := range []string{"/main/docs/ops/shot.png", "/main/shot.png"} {
		if got := expect(t, e.do("GET", p, ""), 200); got != string(data) {
			t.Errorf("GET %s: wrong bytes", p)
		}
	}
	if list := expect(t, e.do("GET", "/main/_assets", "", "Accept", "application/json"), 200); strings.Count(list, `"name":"shot.png"`) != 1 {
		t.Errorf("image stored more than once: %s", list)
	}

	// The picture follows its page when it moves.
	expect(t, e.do("DELETE", "/main/docs/ops/child", ""), 204)
	expect(t, e.do("DELETE", "/main/top", ""), 204)
	expect(t, e.do("POST", "/main/docs/ops?move=ops", ""), 200)
	expect(t, e.do("GET", "/main/docs/shot.png", ""), 404)
	if got := expect(t, e.do("GET", "/main/shot.png", ""), 200); got != string(data) {
		t.Error("image did not follow the moved page")
	}
	// And goes away with it.
	expect(t, e.do("DELETE", "/main/ops", ""), 204)
	expect(t, e.do("GET", "/main/shot.png", ""), 404)
	// The library copy is untouched.
	expect(t, e.do("GET", shot.URL, ""), 200)
}

func TestPageRelativeImagesKeepVariants(t *testing.T) {
	e := newEnv(t)
	big := asset(t, e.post("main", "photo.jpg", "image/jpeg", jpegOf(t, noiseImg(1600, 1200))), 201)
	putPage(t, e, "main", "docs", "# Docs")
	putPage(t, e, "main", "docs/trip", "![p](photo.jpg)")

	rec := e.do("GET", "/main/docs/photo.jpg?w=800", "")
	body := expect(t, rec, 200)
	if w, h := decodeSize(t, []byte(body)); w != 800 || h != 600 {
		t.Fatalf("variant %dx%d", w, h)
	}
	if rec.Header().Get("ETag") != `"`+big.ID+`-w800"` || !strings.Contains(rec.Header().Get("Cache-Control"), "no-cache") {
		t.Errorf("headers: %v", rec.Header())
	}
	expect(t, e.do("GET", "/main/docs/photo.jpg?w=800", "", "If-None-Match", `"`+big.ID+`-w800"`), 304)
	expect(t, e.do("GET", "/main/docs/photo.jpg?w=555", ""), 400)
	if w, _ := decodeSize(t, []byte(expect(t, e.do("GET", "/main/docs/photo.jpg", ""), 200))); w != 1600 {
		t.Errorf("original width %d", w)
	}
}

func TestPagesCannotShadowImages(t *testing.T) {
	e := newEnv(t)
	putPage(t, e, "main", "docs", "# Docs")
	for _, slug := range []string{"x.png", "y.JPG", "z.jpeg", "w.gif", "v.webp", "u.svg", "pic.PNG"} {
		if rec := e.do("PUT", "/main/docs/"+slug, "x", "Content-Type", md); rec.Code != 400 {
			t.Errorf("PUT %s = %d, want 400", slug, rec.Code)
		}
		if rec := e.do("POST", "/main/docs", "x", "Content-Type", md, "Slug", slug); rec.Code != 400 {
			t.Errorf("POST slug %s = %d, want 400", slug, rec.Code)
		}
	}
	// Names that merely contain an extension, or are not images, are fine.
	for _, slug := range []string{"png", "x.png.txt", "notes.pngs", "a-png", "photo.jpg.md-ish"} {
		if rec := e.do("PUT", "/main/docs/"+slug, "x", "Content-Type", md); rec.Code != 201 {
			t.Errorf("PUT %s = %d, want 201", slug, rec.Code)
		}
	}
	expect(t, e.do("POST", "/main/docs/png?move=x.png", ""), 400)
}

func TestPagesChooseTheImageSize(t *testing.T) {
	e := newEnv(t)
	land := asset(t, e.post("main", "land.jpg", "image/jpeg", jpegOf(t, noiseImg(1600, 1200))), 201)
	port := asset(t, e.post("main", "port.jpg", "image/jpeg", jpegOf(t, noiseImg(1200, 1600))), 201)

	// The server makes the copies: ?w= is a width, ?h= a height, whichever way the picture is turned.
	for _, c := range []struct {
		url        string
		wantW, wHt int
	}{
		{land.URL + "?w=400", 400, 300},
		{land.URL + "?h=600", 800, 600},
		{port.URL + "?w=400", 400, 533},
		{port.URL + "?h=400", 300, 400},
		{port.URL + "?s=800", 600, 800},
		{land.URL + "?w=60", 60, 45},
		{port.URL + "?h=60", 45, 60},
	} {
		if w, h := decodeSize(t, []byte(expect(t, e.do("GET", c.url, ""), 200))); w != c.wantW || h != c.wHt {
			t.Errorf("%s: %dx%d, want %dx%d", c.url, w, h, c.wantW, c.wHt)
		}
	}
	// Only the listed sizes, and only one of w, h and s at a time.
	for _, bad := range []string{"?w=500", "?h=10000", "?w=abc", "?w=400&h=400", "?w=400&s=800"} {
		expect(t, e.do("GET", land.URL+bad, ""), 400)
	}
	// Equal requests share a copy; the validators say so.
	expect(t, e.do("GET", land.URL+"?w=400", "", "If-None-Match", `"`+land.ID+`-w400"`), 304)

	// A page asks with a suffix on the name, and gets the right picture and size.
	src := "![a](land.jpg?w=400)\n\n![b](port.jpg?h=400)\n\n![c](land.jpg?w=500)\n\n![d](land.jpg)\n"
	expect(t, e.do("PUT", "/main/sizes", src, "Content-Type", md), 201)
	html := expect(t, e.do("GET", "/main/sizes", "", "Accept", "text/html"), 200)
	for _, want := range []string{
		`src="` + land.URL + `?w=400"`, `width="400"`, `height="300"`,
		`src="` + port.URL + `?h=400"`, `width="300"`, `height="400"`,
		`src="` + land.URL + `?s=800"`, // an unlisted size, or none, gets the default
	} {
		if !strings.Contains(html, want) {
			t.Errorf("html missing %s\n%s", want, html)
		}
	}
	if strings.Contains(html, "?w=500") {
		t.Error("an unlisted size reached the page")
	}
	// The suffix does not hide a use of the image: renaming it still rewrites the page.
	expect(t, e.do("POST", "/main/_assets/"+land.ID+"?rename=landscape.jpg", ""), 200)
	if got := e.do("GET", "/main/sizes.md", "").Body.String(); !strings.Contains(got, "![a](landscape.jpg?w=400)") || strings.Contains(got, "land.jpg") {
		t.Errorf("rename left the sized reference alone: %q", got)
	}
}

var testPDF = []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n")

func TestPDFAttachments(t *testing.T) {
	e := newEnv(t)
	a := asset(t, e.post("main", "Manual.pdf", "application/pdf", testPDF), 201)
	if a.Mime != "application/pdf" || a.Name != "Manual.pdf" || a.Size != int64(len(testPDF)) {
		t.Fatalf("stored: %+v", a)
	}
	// The type comes from the content, never from the name or the header.
	expect(t, e.post("main", "fake.pdf", "application/pdf", []byte("<html><script>alert(1)</script>")), 415)
	// A PDF under an image name is still just a PDF, named as one.
	if o := asset(t, e.post("main", "photo.png", "image/png", append(append([]byte{}, testPDF...), '\n')), 201); o.Mime != "application/pdf" || o.Name != "photo.pdf" {
		t.Errorf("a PDF sent as a png: %+v", o)
	}
	// Same upload limit as the images.
	small := New(e.st, "")
	small.MaxUpload = 20
	req := localReq("POST", "/main/_assets?name=big.pdf", bytes.NewReader(testPDF))
	req.Header.Set("Content-Type", "application/pdf")
	rec := httptest.NewRecorder()
	small.ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Errorf("size limit: %d", rec.Code)
	}

	// Served as a PDF, inline, with no sandbox (browsers will not show a sandboxed PDF).
	r := e.do("GET", a.URL, "")
	if r.Code != 200 || r.Header().Get("Content-Type") != "application/pdf" || r.Body.String() != string(testPDF) {
		t.Fatalf("serving: %d %v", r.Code, r.Header())
	}
	if got := r.Header().Get("Content-Disposition"); got != `inline; filename="Manual.pdf"` {
		t.Errorf("disposition: %q", got)
	}
	if csp := r.Header().Get("Content-Security-Policy"); strings.Contains(csp, "sandbox") || r.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers: %v", r.Header())
	}

	// A page links to it by name: a real link to the file, not a missing page.
	expect(t, e.do("PUT", "/main/docs", "# Docs\n", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/docs/guide", "# Guide\n\nRead the [Manual](Manual.pdf).\n", "Content-Type", md), 201)
	html := expect(t, e.do("GET", "/main/docs/guide", "", "Accept", "text/html"), 200)
	if !strings.Contains(html, `<a href="`+a.URL+`">Manual</a>`) || strings.Contains(html, `class="missing"`) {
		t.Errorf("the link: %s", html)
	}
	if got := brokenOf(t, e, "main"); len(got) != 0 {
		t.Errorf("an attachment shows as a broken link: %+v", got)
	}
	// It is served next to the page too, where a relative link points.
	if r := e.do("GET", "/main/docs/Manual.pdf", ""); r.Code != 200 || r.Header().Get("Content-Type") != "application/pdf" {
		t.Errorf("next to the page: %d %v", r.Code, r.Header())
	}
	// No page can take a name that belongs to an attachment.
	expect(t, e.do("PUT", "/main/docs/x.pdf", "# no\n", "Content-Type", md), 400)

	// The library lists it, and renaming rewrites the page.
	lib := expect(t, e.do("GET", "/main/_assets/", "", "Accept", "text/html"), 200)
	if !strings.Contains(lib, `class="filetile"`) || !strings.Contains(lib, "Manual.pdf") {
		t.Errorf("the library page: %s", lib)
	}
	expect(t, e.do("POST", "/main/_assets/"+a.ID+"?rename=Handbook.pdf", ""), 200)
	if got := e.do("GET", "/main/docs/guide.md", "").Body.String(); !strings.Contains(got, "[Manual](Handbook.pdf)") {
		t.Errorf("rename left the page alone: %q", got)
	}
}

// A picture in a space anyone may read can be shown by another site (a feed reader, say);
// a private space keeps the same-origin policy.
func TestPublicPicturesMayBeShownByOtherSites(t *testing.T) {
	e := newEnv(t)
	if _, err := e.st.CreateSpace("secret", "secret", false); err != nil {
		t.Fatal(err)
	}
	for space, want := range map[string]string{"main": "cross-origin", "secret": "same-origin"} {
		a := asset(t, e.post(space, "p.png", "image/png", pngOf(t, noiseImg(40, 30))), 201)
		for _, target := range []string{
			"/" + space + "/_assets/" + a.ID + "/p.png",
			"/" + space + "/_assets/" + a.ID + "/p.png?w=240",
		} {
			rec := e.do("GET", target, "")
			if rec.Code != 200 {
				t.Fatalf("%s: %d", target, rec.Code)
			}
			if got := rec.Header().Get("Cross-Origin-Resource-Policy"); got != want {
				t.Errorf("%s: Cross-Origin-Resource-Policy %q, want %q", target, got, want)
			}
		}
	}
	// Pages and the API stay same-origin, even in a public space.
	if got := e.do("GET", "/main/", "").Header().Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
		t.Errorf("a page: %q", got)
	}
}
