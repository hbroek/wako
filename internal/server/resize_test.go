package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/gif"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hbroek/wako/internal/render"
	"github.com/hbroek/wako/internal/store"
)

// remotePictures serves a few pictures, and counts how often it is asked.
func remotePictures(t *testing.T) (base string, hits *atomic.Int64) {
	t.Helper()
	hits = new(atomic.Int64)
	var g bytes.Buffer
	gif.Encode(&g, image.NewGray(image.Rect(0, 0, 4, 4)), nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/land.jpg", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/plain") // the header is not believed, the bytes are
		w.Write(jpegOf(t, noiseImg(400, 300)))
	})
	mux.HandleFunc("/pic.png", func(w http.ResponseWriter, r *http.Request) { w.Write(pngBytes(t, 4)) })
	mux.HandleFunc("/anim.gif", func(w http.ResponseWriter, r *http.Request) { w.Write(g.Bytes()) })
	mux.HandleFunc("/page.html", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html><script>1</script>")) })
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/land.jpg", http.StatusFound) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, hits
}

// resizeLink signs the address of a copy the way the renderer does.
func (e *propEnv) resizeLink(img string, size map[string]int, who ...reqOpt) (int, string) {
	e.t.Helper()
	dim, n := byte('w'), size["w"]
	if size["h"] > 0 {
		dim, n = 'h', size["h"]
	}
	return 200, e.srv.resizeURL("main", dim, n, img)
}

// pageHTML renders a page body as a reader of the space sees it.
func (e *propEnv) pageHTML(body string) string {
	e.t.Helper()
	sp, err := e.st.GetSpace("main")
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := e.srv.renderPage(httptest.NewRequest("GET", "/main/", nil), sp, "", body, nil, false)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestResizeServesSignedCopiesOfOutsidePictures(t *testing.T) {
	e := newPropEnv(t)
	e.srv.allowPrivateFetch = true // the "other site" is on this machine
	remote, hits := remotePictures(t)

	code, link := e.resizeLink(remote+"/land.jpg", map[string]int{"w": 60}, e.ed)
	if code != 200 || !strings.HasPrefix(link, "/_resize?") || !strings.Contains(link, "sig=") {
		t.Fatalf("link: %d %q", code, link)
	}

	// Anyone can load the signed link; the copy is the size asked for, and re-encoded.
	rec := e.do("GET", link, "")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" || !strings.Contains(rec.Header().Get("Cache-Control"), "max-age") {
		t.Fatalf("serving: %d %v", rec.Code, rec.Header())
	}
	if w, h := decodeSize(t, rec.Body.Bytes()); w != 60 || h != 45 {
		t.Errorf("size %dx%d", w, h)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("headers: %v", rec.Header())
	}
	// A second request is answered from the cache, and a matching validator costs nothing.
	e.do("GET", link, "")
	etag := rec.Header().Get("ETag")
	if r := e.do("GET", link, "", header("If-None-Match", etag)); r.Code != 304 {
		t.Errorf("revalidation: %d", r.Code)
	}
	if hits.Load() != 1 {
		t.Errorf("the other site was asked %d times, want 1", hits.Load())
	}
	// By height, and through a redirect.
	_, byH := e.resizeLink(remote+"/land.jpg", map[string]int{"h": 60}, e.ed)
	if w, h := decodeSize(t, e.do("GET", byH, "").Body.Bytes()); w != 80 || h != 60 {
		t.Errorf("by height: %dx%d", w, h)
	}
	_, viaHop := e.resizeLink(remote+"/hop", map[string]int{"w": 240}, e.ed)
	if r := e.do("GET", viaHop, ""); r.Code != 200 {
		t.Errorf("redirect: %d", r.Code)
	}

	// A link cannot be altered: another picture, another size, or none at all.
	if r := e.do("GET", strings.Replace(link, "w=60", "w=240", 1), ""); r.Code != 403 {
		t.Errorf("changed size: %d", r.Code)
	}
	if r := e.do("GET", "/_resize?w=60&space=main&img="+remote+"/pic.png", ""); r.Code != 403 {
		t.Errorf("unsigned: %d", r.Code)
	}
	lu, _ := url.Parse(link)
	q := lu.Query()
	q.Del("sig")
	if r := e.do("GET", "/_resize?"+q.Encode(), ""); r.Code != 403 {
		t.Errorf("sig removed: %d", r.Code)
	}

	// Only JPEG and PNG are resized; anything else, or a failure, is refused plainly.
	for path, want := range map[string]int{"/anim.gif": 415, "/page.html": 415, "/missing.jpg": 502} {
		_, l := e.resizeLink(remote+path, map[string]int{"w": 60}, e.ed)
		if r := e.do("GET", l, ""); r.Code != want {
			t.Errorf("%s: %d, want %d", path, r.Code, want)
		}
	}
	if _, l := e.resizeLink(remote+"/pic.png", map[string]int{"w": 60}, e.ed); e.do("GET", l, "").Code != 200 {
		t.Error("a PNG should work")
	}
	// A picture over the byte limit is refused.
	e.srv.resizeMax = 1000
	_, big := e.resizeLink(remote+"/land.jpg?big=1", map[string]int{"w": 60}, e.ed)
	if r := e.do("GET", big, ""); r.Code != 413 {
		t.Errorf("too large: %d", r.Code)
	}
}

func TestResizeNeverReachesThisMachineOrItsNetwork(t *testing.T) {
	e := newPropEnv(t)
	remote, hits := remotePictures(t) // on 127.0.0.1
	sp, _ := e.st.GetSpace("main")
	for _, addr := range []string{remote + "/land.jpg", "http://127.0.0.1/x.jpg", "http://[::1]/x.jpg", "http://10.0.0.5/x.jpg",
		"http://192.168.1.1/x.jpg", "http://169.254.169.254/latest/meta-data/a.jpg", "http://100.64.0.1/x.jpg", "http://[fe80::1]/x.jpg", "http://0.0.0.0/x.jpg"} {
		if l, ok := e.srv.externalResizer(sp)(addr, render.Size{W: 60}); ok {
			t.Errorf("%s: link made (%s)", addr, l)
		}
	}
	// Even a correctly signed link to a name that resolves here is refused when it is fetched.
	e.srv.allowPrivateFetch = true
	_, l := e.resizeLink("http://localhost:"+strings.Split(remote, ":")[2]+"/land.jpg", map[string]int{"w": 60}, e.ed)
	e.srv.allowPrivateFetch = false
	if r := e.do("GET", l, ""); r.Code != 502 || hits.Load() != 0 {
		t.Errorf("a name for this machine: %d, %d requests reached it", r.Code, hits.Load())
	}
}

func TestPicturesOnOtherSitesAreResizedWhenAPageIsShown(t *testing.T) {
	e := newPropEnv(t)
	got := e.pageHTML("![a](https://example.com/photo.jpg#w=600) ![b](https://example.com/p.PNG#h=240) ![c](https://example.com/logo.svg#w=400) " +
		"![d](https://example.com/photo.jpg#w=601) ![e](https://example.com/photo#w=600) ![f](http://127.0.0.1/p.jpg#w=600)")
	if strings.Count(got, "/_resize?") != 2 || !strings.Contains(got, "w=600") || !strings.Contains(got, "h=240") {
		t.Errorf("copies: %s", got)
	}
	// Each resized picture links to the original; the others are shown at the size asked for.
	for _, want := range []string{`<a href="https://example.com/photo.jpg"`, `src="https://example.com/logo.svg"`, `width="400"`, `src="https://example.com/photo.jpg"`, `src="http://127.0.0.1/p.jpg"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "#w=") || strings.Contains(got, "#h=") {
		t.Errorf("a size fragment is left: %s", got)
	}
}

func TestBlockedAddresses(t *testing.T) {
	for _, c := range []struct {
		ip      string
		blocked bool
	}{
		{"8.8.8.8", false}, {"1.1.1.1", false}, {"2606:4700:4700::1111", false},
		{"127.0.0.1", true}, {"::1", true}, {"10.1.2.3", true}, {"172.16.0.1", true}, {"172.31.255.255", true}, {"192.168.0.1", true},
		{"169.254.169.254", true}, {"fe80::1", true}, {"fc00::1", true}, {"fd12::1", true}, {"100.64.0.1", true}, {"0.0.0.0", true},
		{"224.0.0.1", true}, {"::ffff:127.0.0.1", true}, {"::ffff:10.0.0.1", true}, {"198.18.0.1", true}, {"255.255.255.255", true},
	} {
		if got := blockedIP(netip.MustParseAddr(c.ip)); got != c.blocked {
			t.Errorf("%s: blocked=%v, want %v", c.ip, got, c.blocked)
		}
	}
}

func TestResizeSwitches(t *testing.T) {
	e := newPropEnv(t)
	e.srv.allowPrivateFetch = true
	remote, hits := remotePictures(t)
	e.user("root", true)
	rootT := bearer(e.token("root"))
	other := e.space("other", true)
	e.st.SetMember(other.ID, e.users["ed"].ID, store.RoleViewer) // may read "other", not edit it

	code, link := e.resizeLink(remote+"/land.jpg", map[string]int{"w": 60}, e.ed)
	if code != 200 || e.do("GET", link, "").Code != 200 {
		t.Fatalf("on by default: %d", code)
	}
	// A link made for one space is no good for another, and only editors of a space can make one.
	if r := e.do("GET", strings.Replace(link, "space=main", "space=other", 1), ""); r.Code != 403 {
		t.Errorf("moved to another space: %d", r.Code)
	}

	// The space's switch: space administrators set it (editors cannot), and the editor, the link maker and the address all follow.
	set := func(who reqOpt, on bool) int {
		b, _ := json.Marshal(map[string]any{"external_resize": on})
		return e.do("PUT", "/_api/spaces/main", string(b), who, header("Content-Type", "application/json")).Code
	}
	if code := set(e.ed, false); code != 403 && code != 404 {
		t.Errorf("an editor switched it off: %d", code)
	}
	page := e.do("GET", "/main/?create", "", e.ed, header("Accept", "text/html")).Body.String()
	if !strings.Contains(page, `id="web-url"`) {
		t.Error("the editor does not offer pictures from other sites while the switch is on")
	}
	if code := set(rootT, false); code != 200 {
		t.Fatalf("an administrator switching it off: %d", code)
	}
	if got := e.pageHTML("![a](" + remote + "/pic.png#w=60)"); strings.Contains(got, "_resize") {
		t.Errorf("a link was made with the space switch off: %s", got)
	}
	if r := e.do("GET", link, ""); r.Code != 403 || !strings.Contains(r.Body.String(), "this space") {
		t.Errorf("an existing link with the space switch off: %d %s", r.Code, r.Body.String())
	}
	if page := e.do("GET", "/main/?create", "", e.ed, header("Accept", "text/html")).Body.String(); strings.Contains(page, `id="web-url"`) {
		t.Error("the editor offers pictures from other sites with the switch off")
	}
	if list := e.do("GET", "/_api/spaces", "", e.ed).Body.String(); !strings.Contains(list, `"external_resize":false`) {
		t.Errorf("the space list does not say: %s", list)
	}
	if code := set(rootT, true); code != 200 || e.do("GET", link, "").Code != 200 {
		t.Errorf("switching it back on: %d", code)
	}

	// The server's switch beats the space's.
	hitsBefore := hits.Load()
	e.srv.ExternalResize = false
	if got := e.pageHTML("![a](" + remote + "/pic.png#w=60)"); strings.Contains(got, "_resize") {
		t.Errorf("a link was made with the server switch off: %s", got)
	}
	if r := e.do("GET", link, ""); r.Code != 403 || !strings.Contains(r.Body.String(), "this server") {
		t.Errorf("an existing link with the server switch off: %d %s", r.Code, r.Body.String())
	}
	if hits.Load() != hitsBefore {
		t.Error("the other site was contacted while resizing was off")
	}
	if page := e.do("GET", "/main/?settings", "", rootT, header("Accept", "text/html")).Body.String(); strings.Contains(page, `name="resize"`) || !strings.Contains(page, "switched off for the whole server") {
		t.Error("settings should say the server has it off")
	}
	e.srv.ExternalResize = true
	if page := e.do("GET", "/main/?settings", "", rootT, header("Accept", "text/html")).Body.String(); !strings.Contains(page, `name="resize"`) {
		t.Error("settings do not offer the switch")
	}
	// Nothing answers at the bare /resize path (a space may even be called that).
	if r := e.do("GET", "/resize?w=60&img="+remote+"/land.jpg", ""); r.Code == 200 && strings.HasPrefix(r.Header().Get("Content-Type"), "image/") {
		t.Error("/resize should not be an address")
	}
}
