package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hbroek/wako/internal/store"
)

type env struct {
	t   *testing.T
	dir string
	st  *store.Store
	h   http.Handler
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, slug := range []string{"main", "other"} {
		if _, err := st.CreateSpace(slug, slug, true); err != nil {
			t.Fatal(err)
		}
	}
	return &env{t: t, dir: dir, st: st, h: New(st, "")}
}

func (e *env) do(method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := localReq(method, target, strings.NewReader(body))
	req.Host = "localhost:8888"
	req.RemoteAddr = "127.0.0.1:1234" // a local peer, which may send X-Forwarded-* headers
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func expect(t *testing.T, rec *httptest.ResponseRecorder, status int) string {
	t.Helper()
	b, _ := io.ReadAll(rec.Body)
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, status, b)
	}
	return string(b)
}

const md = "text/markdown"

func TestPutGetNegotiation(t *testing.T) {
	e := newEnv(t)
	rec := e.do("PUT", "/main/docs", "# Docs\n\nhello **world**", "Content-Type", md)
	expect(t, rec, 201)
	if rec.Header().Get("ETag") != `"v1"` || rec.Header().Get("Location") != "/main/docs" {
		t.Fatalf("headers: %v", rec.Header())
	}

	// Markdown via Accept and via .md suffix return identical bodies.
	a := expect(t, e.do("GET", "/main/docs", "", "Accept", md), 200)
	b := expect(t, e.do("GET", "/main/docs.md", "", "Accept", "text/html"), 200)
	if a != "# Docs\n\nhello **world**" || a != b {
		t.Fatalf("markdown mismatch: %q vs %q", a, b)
	}
	// Browser gets rendered HTML.
	rec = e.do("GET", "/main/docs", "", "Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	html := expect(t, rec, 200)
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(html, "<strong>world</strong>") {
		t.Fatalf("html: %s", html)
	}
	// q-values pick the preferred type.
	rec = e.do("GET", "/main/docs", "", "Accept", "text/html;q=0.5, text/markdown")
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("content-type %q", ct)
	}
	expect(t, e.do("GET", "/main/docs", "", "Accept", "image/png"), 406)
	// JSON metadata.
	if j := expect(t, e.do("GET", "/main/docs", "", "Accept", "application/json"), 200); !strings.Contains(j, `"version":1`) {
		t.Fatalf("json: %s", j)
	}
}

func TestHTMLSanitised(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/x", "<script>alert(1)</script>\n\n[a](javascript:alert(1))", "Content-Type", md), 201)
	html := expect(t, e.do("GET", "/main/x", ""), 200)
	if strings.Contains(html, "alert(1)") {
		t.Fatalf("unsanitised: %s", html)
	}
}

func TestETagAndConditions(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/p", "v1", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/p", "dup", "Content-Type", md, "If-None-Match", "*"), 412)
	expect(t, e.do("PUT", "/main/p", "v2", "Content-Type", md, "If-Match", `"v1"`), 200)
	expect(t, e.do("PUT", "/main/p", "stale", "Content-Type", md, "If-Match", `"v1"`), 412)
	expect(t, e.do("PUT", "/main/nope", "x", "Content-Type", md, "If-Match", `"v1"`), 412)
	expect(t, e.do("PUT", "/main/p", "x", "Content-Type", "application/pdf"), 415)
	expect(t, e.do("GET", "/main/p", "", "Accept", md, "If-None-Match", `"v2"`), 304)
	expect(t, e.do("GET", "/main/p", "", "Accept", md, "If-None-Match", `"v1"`), 200)
	// HTML shows the navigation tree, so it is never validated by the page's ETag.
	if got := e.do("GET", "/main/p", "", "Accept", "text/html").Header().Get("ETag"); got != "" {
		t.Fatalf("html etag %q", got)
	}
	if got := e.do("GET", "/main/p", "", "Accept", "application/json").Header().Get("ETag"); got != `"v2-json"` {
		t.Fatalf("json etag %q", got)
	}
}

func TestTreeCreateMoveDelete(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/a/b", "x", "Content-Type", md), 409) // parent missing
	expect(t, e.do("PUT", "/main/a", "# A", "Content-Type", md), 201)
	rec := e.do("POST", "/main/a", "# Child", "Content-Type", md, "Slug", "b")
	expect(t, rec, 201)
	if rec.Header().Get("Location") != "/main/a/b" {
		t.Fatalf("location %q", rec.Header().Get("Location"))
	}
	expect(t, e.do("POST", "/main/a", `{"slug":"c","title":"See","body":"c"}`, "Content-Type", "application/json"), 201)
	expect(t, e.do("POST", "/main/a", "dup", "Content-Type", md, "Slug", "b"), 409)
	expect(t, e.do("POST", "/main/missing", "x", "Content-Type", md, "Slug", "z"), 404)
	expect(t, e.do("POST", "/main/a", "x", "Content-Type", md, "Slug", "bad.md"), 400)

	tree := expect(t, e.do("GET", "/_api/spaces/main/tree", ""), 200)
	if !strings.Contains(tree, `"path":"a/b"`) || !strings.Contains(tree, `"title":"See"`) {
		t.Fatalf("tree: %s", tree)
	}

	expect(t, e.do("DELETE", "/main/a", ""), 409)
	expect(t, e.do("POST", "/main/a/b?move=a/c", ""), 409) // target exists
	expect(t, e.do("POST", "/main/a?move=a/b/inside", ""), 400)
	expect(t, e.do("POST", "/main/a/b?move=moved", ""), 200)
	expect(t, e.do("GET", "/main/a/b", ""), 404)
	expect(t, e.do("GET", "/main/moved.md", ""), 200)
	expect(t, e.do("DELETE", "/main/a?recursive=true", ""), 204)
	expect(t, e.do("GET", "/main/a/c", ""), 404)
	expect(t, e.do("DELETE", "/main/moved", "", "If-Match", `"v9"`), 412)
	expect(t, e.do("DELETE", "/main/moved", ""), 204)
}

func TestSpacesAreIsolatedDatabases(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/same", "in main", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/other/same", "in other", "Content-Type", md), 201)
	if got := expect(t, e.do("GET", "/other/same.md", ""), 200); got != "in other" {
		t.Fatalf("got %q", got)
	}
	expect(t, e.do("GET", "/nospace/x", ""), 404)

	files, _ := filepath.Glob(filepath.Join(e.dir, "spaces", "*.db"))
	if len(files) != 2 {
		t.Fatalf("want 2 space databases, got %v", files)
	}

	expect(t, e.do("DELETE", "/_api/spaces/other", ""), 204)
	expect(t, e.do("GET", "/other/same", ""), 404)
	expect(t, e.do("GET", "/main/same.md", ""), 200)
	files, _ = filepath.Glob(filepath.Join(e.dir, "spaces", "*.db*"))
	if len(files) == 0 || len(files) > 3 { // main.db plus its -wal/-shm
		t.Fatalf("files after delete: %v", files)
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCreateSpaceAPI(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("POST", "/_api/spaces", `{"slug":"team","title":"Team"}`, "Content-Type", "application/json"), 201)
	expect(t, e.do("POST", "/_api/spaces", `{"slug":"team"}`, "Content-Type", "application/json"), 409)
	expect(t, e.do("POST", "/_api/spaces", `{"slug":"_api"}`, "Content-Type", "application/json"), 400)
	expect(t, e.do("POST", "/_api/spaces", `{"slug":"healthz"}`, "Content-Type", "application/json"), 400)
	if list := expect(t, e.do("GET", "/_api/spaces", ""), 200); !strings.Contains(list, `"team"`) {
		t.Fatalf("list: %s", list)
	}
	expect(t, e.do("PUT", "/team/p", "ok", "Content-Type", md), 201)
}

func TestEditabilityHeaders(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/doc", "# Doc", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/doc/kid", "kid", "Content-Type", md), 201)

	h := e.do("HEAD", "/main/doc", "", "Accept", md).Header()
	if ct := h.Get("Content-Type"); ct != "text/markdown; charset=utf-8; variant=CommonMark" {
		t.Fatalf("content-type %q", ct)
	}
	if a := h.Get("Allow"); a != "GET, HEAD, PUT, PATCH, POST, DELETE, OPTIONS" {
		t.Fatalf("allow %q", a)
	}
	links := strings.Join(h.Values("Link"), "\n")
	for _, want := range []string{`</main/doc.md>; rel="edit"`, `rel="edit-form"`, `rel="create-form"`, `ldp#BasicContainer>; rel="type"`} {
		if !strings.Contains(links, want) {
			t.Errorf("Link missing %q in %s", want, links)
		}
	}
	if h.Get("Accept-Post") == "" || h.Get("WAC-Allow") != `user="read write append", public="read"` {
		t.Errorf("headers: %v", h)
	}

	opt := e.do("OPTIONS", "/main/doc", "")
	expect(t, opt, 204)
	if opt.Header().Get("Allow") == "" {
		t.Error("OPTIONS without Allow")
	}
	// A leaf page is not a container.
	leaf := e.do("HEAD", "/main/doc/kid", "").Header()
	if strings.Contains(strings.Join(leaf.Values("Link"), ""), "BasicContainer") {
		t.Error("leaf advertised as container")
	}
	// 405 carries Allow.
	rec := e.do("LINK", "/main/doc", "x")
	expect(t, rec, 405)
	if rec.Header().Get("Allow") == "" {
		t.Error("405 without Allow")
	}
	// Space root is a collection.
	root := e.do("OPTIONS", "/main/", "").Header()
	if root.Get("Allow") != "GET, HEAD, POST, OPTIONS" {
		t.Errorf("root allow %q", root.Get("Allow"))
	}
	expect(t, e.do("PUT", "/main/", "x", "Content-Type", md), 405)
}

func TestEditorForms(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/doc", "# Doc", "Content-Type", md), 201)
	if f := expect(t, e.do("GET", "/main/doc?edit", ""), 200); !strings.Contains(f, "<textarea") || !strings.Contains(f, `data-version="1"`) {
		t.Fatalf("edit form: %s", f)
	}
	if f := expect(t, e.do("GET", "/main/doc?create", ""), 200); !strings.Contains(f, `name="slug"`) {
		t.Fatalf("create form: %s", f)
	}
}

func TestBasePath(t *testing.T) {
	e := newEnv(t)
	h := New(e.st, "/wiki")
	req := localReq("PUT", "/wiki/main/p", strings.NewReader("x"))
	req.Header.Set("Content-Type", md)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 201 || rec.Header().Get("Location") != "/wiki/main/p" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("GET", "/main/p", nil))
	if rec.Code != 404 {
		t.Fatalf("outside base path: %d", rec.Code)
	}
}

func TestReservedAndInvalidPaths(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/_assets", "x", "Content-Type", md), 405) // the upload endpoint takes POST only
	expect(t, e.do("PUT", "/main/_other", "x", "Content-Type", md), 404)
	expect(t, e.do("PUT", "/main/a%20b", "x", "Content-Type", md), 400)
	expect(t, e.do("GET", "/main/..", ""), 404)
	expect(t, e.do("GET", "/healthz", ""), 200)
	expect(t, e.do("GET", "/main", ""), 301)
	if home := expect(t, e.do("GET", "/", "", "Accept", md), 200); !strings.Contains(home, "[main](/main/)") {
		t.Fatalf("home: %s", home)
	}
}

func TestMissingPageForBrowsers(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/main/home", "", "Accept", "text/html")
	body := expect(t, rec, 404)
	if !strings.Contains(body, "Create this page") || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("missing page: %s", body)
	}
	if f := expect(t, e.do("GET", "/main/home?new", "", "Accept", "text/html"), 200); !strings.Contains(f, `data-mode="new"`) {
		t.Fatalf("new form: %s", f)
	}
	expect(t, e.do("GET", "/main/home", "", "Accept", md), 404) // API clients still get a plain 404
}

func TestReorder(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/a", "a", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/b", "b", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/c", "c", "Content-Type", md), 201)
	order := func() string {
		tree := expect(t, e.do("GET", "/_api/spaces/main/tree", ""), 200)
		var out []string
		for _, s := range []string{"a", "b", "c"} {
			out = append(out, fmt.Sprintf("%04d%s", strings.Index(tree, `"slug":"`+s+`"`), s))
		}
		sort.Strings(out)
		var b strings.Builder
		for _, o := range out {
			b.WriteString(o[4:])
		}
		return b.String()
	}
	if got := order(); got != "abc" {
		t.Fatalf("initial order %s", got)
	}
	expect(t, e.do("POST", "/main/?reorder", `{"order":["c","a","b"]}`, "Content-Type", "application/json"), 204)
	if got := order(); got != "cab" {
		t.Fatalf("order after reorder %s", got)
	}
	expect(t, e.do("POST", "/main/?reorder", `{"order":["c","a"]}`, "Content-Type", "application/json"), 400)
	expect(t, e.do("POST", "/main/?reorder", `{"order":["c","a","a"]}`, "Content-Type", "application/json"), 400)
	expect(t, e.do("POST", "/main/nope?reorder", `{"order":[]}`, "Content-Type", "application/json"), 404)
	// Reordering children of a page.
	expect(t, e.do("POST", "/main/a", "x", "Content-Type", md, "Slug", "x"), 201)
	expect(t, e.do("POST", "/main/a", "y", "Content-Type", md, "Slug", "y"), 201)
	expect(t, e.do("POST", "/main/a?reorder", `{"order":["y","x"]}`, "Content-Type", "application/json"), 204)
	kids := expect(t, e.do("GET", "/main/a", "", "Accept", "application/json"), 200)
	if strings.Index(kids, `"a/y"`) > strings.Index(kids, `"a/x"`) {
		t.Fatalf("children order: %s", kids)
	}
}

func TestUIShell(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/docs", "# Docs", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/docs/ops", "# Ops", "Content-Type", md), 201)
	page := expect(t, e.do("GET", "/main/docs/ops", "", "Accept", "text/html"), 200)
	for _, want := range []string{
		`id="nav"`, `data-path="docs/ops"`, `data-path="docs"`, `draggable="true"`,
		`class="spaces"`, `/other/`, `id="edit-link"`, `data-dialog="move"`, `/_ui/app.js?v=`, "Wako",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// The current page's ancestors are expanded.
	if !strings.Contains(page, `<li data-path="docs" class="open has-kids"`) {
		t.Errorf("ancestor not open: %s", page)
	}
	css := e.do("GET", "/_ui/app.css", "")
	expect(t, css, 200)
	if !strings.Contains(css.Header().Get("Cache-Control"), "immutable") || !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") {
		t.Errorf("css headers: %v", css.Header())
	}
	js := e.do("GET", "/_ui/app.js", "")
	expect(t, js, 200)
	if !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("js type: %v", js.Header())
	}
	expect(t, e.do("GET", "/_ui/logo", ""), 404) // no logo configured
	expect(t, e.do("GET", "/_ui/new-space", ""), 200)
	expect(t, e.do("GET", "/_ui/nothing", ""), 404)
	if s := expect(t, e.do("GET", "/main/?settings", "", "Accept", "text/html"), 200); !strings.Contains(s, `id="space-settings"`) {
		t.Errorf("settings page: %s", s)
	}
}

func TestBranding(t *testing.T) {
	e := newEnv(t)
	logo := filepath.Join(t.TempDir(), "logo.svg")
	os.WriteFile(logo, []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), 0o644)
	srv := New(e.st, "")
	srv.SiteName, srv.LogoFile = "Acme Docs", logo
	do := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, localReq("GET", target, nil))
		return rec
	}
	home := do("/").Body.String()
	if !strings.Contains(home, "Acme Docs") || !strings.Contains(home, `/_ui/logo`) {
		t.Errorf("branding missing: %s", home)
	}
	if rec := do("/_ui/logo"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "<svg") {
		t.Errorf("logo: %d", rec.Code)
	}
}

func TestRenderEndpointAndSpaceUpdate(t *testing.T) {
	e := newEnv(t)
	html := expect(t, e.do("POST", "/_api/spaces/main/render", "# Hi\n\n<script>x</script>\n\n*em*", "Content-Type", md), 200)
	if !strings.Contains(html, "<h1") || !strings.Contains(html, "<em>em</em>") || strings.Contains(html, "<script") {
		t.Fatalf("render: %s", html)
	}
	expect(t, e.do("POST", "/_api/spaces/nope/render", "x"), 404)

	rec := e.do("PUT", "/_api/spaces/other", `{"title":"Renamed","slug":"renamed","public_read":false}`, "Content-Type", "application/json")
	expect(t, rec, 200)
	if rec.Header().Get("Location") != "/renamed/" {
		t.Fatalf("location %q", rec.Header().Get("Location"))
	}
	expect(t, e.do("GET", "/other/", ""), 404)
	expect(t, e.do("PUT", "/renamed/p", "kept", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/_api/spaces/renamed", `{"slug":"main"}`), 409)
	expect(t, e.do("PUT", "/_api/spaces/renamed", `{"slug":"_ui"}`), 400)
	expect(t, e.do("PUT", "/_api/spaces/renamed", `{"title":" "}`), 400)
	expect(t, e.do("PUT", "/_api/spaces/ghost", `{"title":"x"}`), 404)
}

// ---- Phase 3: assets ----

func pngBytes(t *testing.T, shade uint8) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 4, 4))
	for i := range img.Pix {
		img.Pix[i] = shade
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (e *env) upload(space, name string, data []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do("POST", "/"+space+"/_assets?name="+name, string(data), "Content-Type", "image/png")
}

func decodeAsset(t *testing.T, body string) (id, url string) {
	t.Helper()
	var a assetJSON
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("bad JSON %q: %v", body, err)
	}
	return a.ID, a.URL
}

func TestAssetUploadServeAndDedupe(t *testing.T) {
	e := newEnv(t)
	data := pngBytes(t, 7)
	rec := e.upload("main", "shot.png", data)
	id, url := decodeAsset(t, expect(t, rec, 201))
	if url != "/main/_assets/"+id+"/shot.png" || rec.Header().Get("Location") != url {
		t.Fatalf("url %q location %q", url, rec.Header().Get("Location"))
	}

	get := e.do("GET", url, "")
	if got := expect(t, get, 200); got != string(data) {
		t.Fatal("served bytes differ")
	}
	h := get.Header()
	if h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("ETag") != `"`+id+`"` || !strings.HasPrefix(h.Get("Cache-Control"), "public") ||
		!strings.Contains(h.Get("Cache-Control"), "immutable") {
		t.Fatalf("headers: %v", h)
	}
	expect(t, e.do("GET", url, "", "If-None-Match", `"`+id+`"`), 304)
	expect(t, e.do("HEAD", url, ""), 200)
	expect(t, e.do("GET", "/main/_assets/"+id, ""), 200)
	expect(t, e.do("GET", "/main/_assets/"+id+"/anything.png", ""), 200)

	// Same bytes are stored once and keep their library name.
	var again assetJSON
	json.Unmarshal([]byte(expect(t, e.upload("main", "again.png", data), 200)), &again)
	if again.ID != id || again.Name != "shot.png" {
		t.Fatalf("dedupe: %+v", again)
	}
	// Different bytes under a taken name get a numeric suffix; names stay unique.
	var second assetJSON
	json.Unmarshal([]byte(expect(t, e.upload("main", "shot.png", pngBytes(t, 8)), 201)), &second)
	if second.Name != "shot-2.png" || second.ID == id {
		t.Fatalf("name clash: %+v", second)
	}

	// Lookup by name redirects to the immutable URL.
	rec = e.do("GET", "/main/_assets/shot-2.png", "")
	if rec.Code != 302 || rec.Header().Get("Location") != second.URL {
		t.Fatalf("name lookup: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	expect(t, e.do("GET", "/main/_assets/nope.png", ""), 404)

	// Libraries are per space: nothing leaks, and names are free again.
	expect(t, e.do("GET", "/other/_assets/"+id+"/shot.png", ""), 404)
	expect(t, e.do("GET", "/other/_assets/shot.png", ""), 404)
	var other assetJSON
	json.Unmarshal([]byte(expect(t, e.upload("other", "shot.png", data), 201)), &other)
	if other.ID != id || other.Name != "shot.png" {
		t.Fatalf("other space: %+v", other)
	}
	expect(t, e.do("DELETE", "/other/_assets/"+id, ""), 204)
	expect(t, e.do("GET", "/other/_assets/"+id, ""), 404)
	expect(t, e.do("GET", url, ""), 200) // main's copy is untouched
}

func TestAssetMultipartAndNames(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "../My Shot!.PNG")
	fw.Write(pngBytes(t, 9))
	mw.Close()
	rec := e.do("POST", "/main/_assets", buf.String(), "Content-Type", mw.FormDataContentType())
	_, url := decodeAsset(t, expect(t, rec, 201))
	if !strings.HasSuffix(url, "/My-Shot.png") {
		t.Fatalf("name not cleaned: %s", url)
	}
	// A claimed extension never decides the type or the stored name.
	_, url = decodeAsset(t, expect(t, e.upload("main", "evil.html", pngBytes(t, 1)), 201))
	if !strings.HasSuffix(url, "/evil.png") {
		t.Fatalf("extension not normalised: %s", url)
	}
	expect(t, e.do("POST", "/main/_assets", "", "Content-Type", "multipart/form-data; boundary=x"), 413)
}

func TestAssetRejections(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("POST", "/main/_assets?name=a.png", "just text", "Content-Type", "image/png"), 415)
	expect(t, e.do("POST", "/main/_assets?name=a.png", "<html><script>x</script>", "Content-Type", "image/png"), 415)
	expect(t, e.do("POST", "/main/_assets?name=a.svg", "<html><body>not svg</body></html>", "Content-Type", "image/svg+xml"), 415)
	expect(t, e.do("POST", "/main/_assets?name=a.png", "<svg xmlns='http://www.w3.org/2000/svg'/>", "Content-Type", "image/png"), 415) // svg needs a .svg name
	expect(t, e.do("POST", "/main/_assets", "", "Content-Type", "image/png"), 400)
	expect(t, e.do("PUT", "/main/_assets", "x"), 405)

	svg := `<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"><script>alert(1)</script></svg>`
	rec := e.do("POST", "/main/_assets?name=logo.svg", svg, "Content-Type", "image/svg+xml")
	_, url := decodeAsset(t, expect(t, rec, 201))
	got := e.do("GET", url, "")
	if got.Header().Get("Content-Type") != "image/svg+xml" ||
		!strings.Contains(got.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("svg headers: %v", got.Header())
	}

	// Size limit.
	small := New(e.st, "")
	small.MaxUpload = 100
	big := bytes.Repeat([]byte{0}, 10_000)
	copy(big, pngBytes(t, 3))
	req := localReq("POST", "/main/_assets?name=big.png", bytes.NewReader(big))
	req.Header.Set("Content-Type", "image/png")
	rec2 := httptest.NewRecorder()
	small.ServeHTTP(rec2, req)
	if rec2.Code != 413 {
		t.Fatalf("size limit: %d", rec2.Code)
	}
}

// Pages refer to library images by bare name; the page binds the name to an asset.
func TestPagesBindImagesByName(t *testing.T) {
	e := newEnv(t)
	id, url := decodeAsset(t, expect(t, e.upload("main", "shot.png", pngBytes(t, 5)), 201))
	src := "# Shots\n\n![shot](shot.png)\n\n![gone](missing.png)\n\n![web](https://example.com/a.png)\n"
	expect(t, e.do("PUT", "/main/docs", "# Docs", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/docs/shots", src, "Content-Type", md), 201)

	// The Markdown view keeps the bare names; HTML resolves the bound one only.
	if got := expect(t, e.do("GET", "/main/docs/shots.md", ""), 200); got != src {
		t.Fatalf("markdown changed: %q", got)
	}
	html := expect(t, e.do("GET", "/main/docs/shots", "", "Accept", "text/html"), 200)
	for _, want := range []string{`<img src="` + url + `"`, `src="missing.png"`, `src="https://example.com/a.png"`} {
		if !strings.Contains(html, want) {
			t.Errorf("html missing %s:\n%s", want, html)
		}
	}

	// Moving or renaming the page keeps its images, because the page owns the binding.
	expect(t, e.do("POST", "/main/docs/shots?move=moved", ""), 200)
	if html := expect(t, e.do("GET", "/main/moved", "", "Accept", "text/html"), 200); !strings.Contains(html, `<img src="`+url+`"`) {
		t.Fatalf("image lost after move: %s", html)
	}

	// Reuse: a second page binds the same library image.
	expect(t, e.do("PUT", "/main/other", "![again](shot.png)", "Content-Type", md), 201)
	if html := expect(t, e.do("GET", "/main/other", "", "Accept", "text/html"), 200); !strings.Contains(html, `<img src="`+url+`"`) {
		t.Fatalf("reuse failed: %s", html)
	}
	list := expect(t, e.do("GET", "/main/_assets", "", "Accept", "application/json"), 200)
	if !strings.Contains(list, `"used_by":2`) {
		t.Fatalf("library list: %s", list)
	}

	// In use: refuse unless forced. Releasing the references frees it.
	expect(t, e.do("DELETE", "/main/_assets/"+id, ""), 409)
	expect(t, e.do("PUT", "/main/other", "no image now", "Content-Type", md), 200)
	expect(t, e.do("DELETE", "/main/_assets/"+id, ""), 409) // moved still uses it
	expect(t, e.do("DELETE", "/main/moved", ""), 204)
	expect(t, e.do("DELETE", "/main/_assets/"+id, ""), 204)
	expect(t, e.do("GET", url, ""), 404)
}

// A bound name keeps its image; a name uploaded later is not picked up by
// pages that were saved without it until they are saved again.
func TestBindingsAreStable(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/p", "![x](late.png)", "Content-Type", md), 201)
	if html := expect(t, e.do("GET", "/main/p", "", "Accept", "text/html"), 200); !strings.Contains(html, `src="late.png"`) {
		t.Fatalf("unresolved name should stay as written: %s", html)
	}
	_, url := decodeAsset(t, expect(t, e.upload("main", "late.png", pngBytes(t, 20)), 201))
	if html := expect(t, e.do("GET", "/main/p", "", "Accept", "text/html"), 200); strings.Contains(html, url) {
		t.Fatal("saved pages must not bind images implicitly at render time")
	}
	expect(t, e.do("PUT", "/main/p", "![x](late.png) saved again", "Content-Type", md), 200)
	if html := expect(t, e.do("GET", "/main/p", "", "Accept", "text/html"), 200); !strings.Contains(html, url) {
		t.Fatalf("saving should bind the name: %s", html)
	}
	// Code samples do not bind.
	expect(t, e.do("PUT", "/main/code", "```\n![x](late.png)\n```", "Content-Type", md), 201)
	list := expect(t, e.do("GET", "/main/_assets", "", "Accept", "application/json"), 200)
	if !strings.Contains(list, `"used_by":1`) {
		t.Fatalf("code block counted as use: %s", list)
	}
}

func TestPreviewUsesPageBindingsThenLibrary(t *testing.T) {
	e := newEnv(t)
	_, bound := decodeAsset(t, expect(t, e.upload("main", "bound.png", pngBytes(t, 30)), 201))
	expect(t, e.do("PUT", "/main/p", "![x](bound.png)", "Content-Type", md), 201)
	_, fresh := decodeAsset(t, expect(t, e.upload("main", "fresh.png", pngBytes(t, 31)), 201))

	html := expect(t, e.do("POST", "/_api/spaces/main/render?page=p", "![a](bound.png) ![b](fresh.png) ![c](nope.png)", "Content-Type", md), 200)
	for _, want := range []string{bound, fresh, `src="nope.png"`} {
		if !strings.Contains(html, want) {
			t.Errorf("preview missing %q: %s", want, html)
		}
	}
}

func TestMediaLibraryPage(t *testing.T) {
	e := newEnv(t)
	expect(t, e.upload("main", "alpha.png", pngBytes(t, 40)), 201)
	expect(t, e.upload("main", "beta.png", pngBytes(t, 41)), 201)
	page := expect(t, e.do("GET", "/main/_assets/", "", "Accept", "text/html"), 200)
	for _, want := range []string{"Media library", "alpha.png", "beta.png", "data-del", `id="lib-upload"`} {
		if !strings.Contains(page, want) {
			t.Errorf("library page missing %q", want)
		}
	}
	q := expect(t, e.do("GET", "/main/_assets?q=alp", "", "Accept", "application/json"), 200)
	if !strings.Contains(q, "alpha.png") || strings.Contains(q, "beta.png") {
		t.Fatalf("search: %s", q)
	}
	expect(t, e.do("GET", "/main/_assets?q=%25", "", "Accept", "application/json"), 200) // % is literal, not a wildcard
	expect(t, e.do("GET", "/main/_assets", "", "Accept", "image/png"), 406)
}

func TestAssetGC(t *testing.T) {
	e := newEnv(t)
	expect(t, e.upload("main", "used.png", pngBytes(t, 10)), 201)
	orphan, _ := decodeAsset(t, expect(t, e.upload("main", "orphan.png", pngBytes(t, 11)), 201))
	expect(t, e.do("PUT", "/main/p", "![x](used.png)", "Content-Type", md), 201)

	if n, _, err := GCAssets(e.st, time.Hour); err != nil || n != 0 {
		t.Fatalf("fresh orphans must survive: n=%d err=%v", n, err)
	}
	n, size, err := GCAssets(e.st, 0)
	if err != nil || n != 1 || size == 0 {
		t.Fatalf("gc: n=%d bytes=%d err=%v", n, size, err)
	}
	expect(t, e.do("GET", "/main/_assets/"+orphan, ""), 404)
	if list := expect(t, e.do("GET", "/main/_assets", "", "Accept", "application/json"), 200); !strings.Contains(list, "used.png") {
		t.Fatalf("referenced asset was collected: %s", list)
	}
}

func TestAssetCapsAndSpaceRename(t *testing.T) {
	e := newEnv(t)
	id, url := decodeAsset(t, expect(t, e.upload("main", "a.png", pngBytes(t, 12)), 201))
	h := e.do("OPTIONS", url, "").Header()
	if h.Get("Allow") != "GET, HEAD, POST, DELETE, OPTIONS" || h.Get("WAC-Allow") != `user="read write append", public="read"` {
		t.Fatalf("asset caps: %v", h)
	}
	if h.Get("Accept-Patch") != "" {
		t.Fatal("assets must not advertise Accept-Patch")
	}
	up := e.do("OPTIONS", "/main/_assets", "").Header()
	if up.Get("Allow") != "GET, HEAD, POST, OPTIONS" || up.Get("Accept-Post") == "" {
		t.Fatalf("library caps: %v", up)
	}

	// Pages hold bare names, so renaming the space needs no rewriting.
	expect(t, e.do("PUT", "/main/p", "![a](a.png)", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/_api/spaces/main", `{"slug":"wiki"}`, "Content-Type", "application/json"), 200)
	if body := expect(t, e.do("GET", "/wiki/p.md", ""), 200); body != "![a](a.png)" {
		t.Fatalf("markdown after rename: %q", body)
	}
	want := `<img src="/wiki/_assets/` + id + `/a.png"`
	if html := expect(t, e.do("GET", "/wiki/p", "", "Accept", "text/html"), 200); !strings.Contains(html, want) {
		t.Fatalf("image after rename: %s", html)
	}
	expect(t, e.do("GET", "/main/_assets/"+id+"/a.png", ""), 404)
}

func TestEditorOffersMediaLibrary(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/d", "x", "Content-Type", md), 201)
	f := expect(t, e.do("GET", "/main/d?edit", ""), 200)
	for _, want := range []string{`id="media-btn"`, `id="media"`, `id="pick"`, `data-page="d"`, `data-library="/main/_assets"`} {
		if !strings.Contains(f, want) {
			t.Errorf("editor missing %s", want)
		}
	}
	if f := expect(t, e.do("GET", "/main/d?create", ""), 200); strings.Contains(f, `data-page="d"`) {
		t.Error("create mode must not bind to the parent page")
	}
}

// OPTIONS, HEAD and GET describe a resource the same way.
func TestOptionsMatchesHead(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/parent", "# P", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/parent/child", "# C", "Content-Type", md), 201)
	for _, path := range []string{"/main/parent", "/main/parent/child", "/main/", "/main/ghost"} {
		opt, head := e.do("OPTIONS", path, "").Header(), e.do("HEAD", path, "").Header()
		for _, k := range []string{"Allow", "Accept-Post", "Accept-Patch", "Wac-Allow"} {
			if opt.Get(k) != head.Get(k) {
				t.Errorf("%s: %s differs: OPTIONS %q, HEAD %q", path, k, opt.Get(k), head.Get(k))
			}
		}
		if strings.Contains(strings.Join(opt.Values("Link"), " "), "BasicContainer") != strings.Contains(strings.Join(head.Values("Link"), " "), "BasicContainer") {
			t.Errorf("%s: container type differs between OPTIONS and HEAD", path)
		}
	}
	has := func(path string) bool {
		return strings.Contains(strings.Join(e.do("OPTIONS", path, "").Header().Values("Link"), " "), "BasicContainer")
	}
	if !has("/main/parent") || has("/main/parent/child") || !has("/main/") {
		t.Error("only a page with children (and the space root) is a container")
	}
	if got := e.do("OPTIONS", "/main/ghost", "").Header().Get("Allow"); got != "GET, HEAD, PUT, OPTIONS" {
		t.Errorf("a missing page can only be created: %q", got)
	}
}

// localReq is a request as a local program or browser would send it: to a local
// name, from a local peer.
func localReq(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = "localhost:8888"
	req.RemoteAddr = "127.0.0.1:1234"
	return req
}
