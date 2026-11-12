package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hbroek/wako/internal/archive"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func zipFiles(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(b)
	}
	return out
}

func importReport(t *testing.T, rec *httptest.ResponseRecorder, want int) archive.Report {
	t.Helper()
	var rep archive.Report
	body := status(t, rec, want)
	var wrapped struct {
		Report *archive.Report `json:"report"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("report: %v: %s", err, body)
	}
	if json.Unmarshal([]byte(body), &wrapped) == nil && wrapped.Report != nil {
		return *wrapped.Report
	}
	return rep
}

func TestExportDownload(t *testing.T) {
	e := newEnv(t)
	up := e.upload("main", "shot.png", pngBytes(t, 5))
	_, _ = up, up
	expect(t, e.do("PUT", "/main/docs", "# Docs\n", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/docs/ops", "# Ops\n\n![s](shot.png)\n", "Content-Type", md), 201)

	rec := e.do("GET", "/_api/spaces/main/export", "")
	body := expect(t, rec, 200)
	h := rec.Header()
	if h.Get("Content-Type") != "application/zip" || !strings.HasPrefix(h.Get("Content-Disposition"), `attachment; filename="main-`) ||
		!strings.HasSuffix(h.Get("Content-Disposition"), `.zip"`) || h.Get("Cache-Control") != "private, no-store" {
		t.Errorf("headers: %v", h)
	}
	files := zipFiles(t, []byte(body))
	if files["docs.md"] != "# Docs\n" || files["docs/ops.md"] != "# Ops\n\n![s](shot.png)\n" || files["docs/shot.png"] == "" || files["wako.json"] == "" {
		t.Errorf("files: %v", keys(files))
	}
	if rec := e.do("HEAD", "/_api/spaces/main/export", ""); rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "application/zip" {
		t.Errorf("HEAD: %d %d bytes", rec.Code, rec.Body.Len())
	}
	expect(t, e.do("POST", "/_api/spaces/main/export", ""), 405)
	expect(t, e.do("GET", "/_api/spaces/nospace/export", ""), 404)
}

func keys(m map[string]string) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	return k
}

func TestImportUpload(t *testing.T) {
	e := newEnv(t)
	z := zipOf(t, map[string]string{"one.md": "# One\n", "dir/two.md": "# Two\n"})

	// A raw body.
	rep := importReport(t, e.do("POST", "/_api/spaces/main/import", string(z), "Content-Type", "application/zip"), 200)
	if rep.PagesCreated != 2 || rep.Placeholders != 1 {
		t.Errorf("report: %+v", rep)
	}
	expect(t, e.do("GET", "/main/dir/two", "", "Accept", md), 200)
	// A form upload, as a browser sends it, with options.
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	mw.WriteField("note", "ignored")
	fw, _ := mw.CreateFormFile("file", "export.zip")
	fw.Write(zipOf(t, map[string]string{"one.md": "# One again\n", "three.md": "# Three\n"}))
	mw.Close()
	rep = importReport(t, e.do("POST", "/_api/spaces/main/import?conflict=overwrite&under=dir", form.String(), "Content-Type", mw.FormDataContentType()), 200)
	if rep.PagesCreated != 2 || rep.PagesUpdated != 0 {
		t.Errorf("form report: %+v", rep)
	}
	expect(t, e.do("GET", "/main/dir/three", "", "Accept", md), 200)
	if got := md_(t, e, "main", "one"); got != "# One\n" {
		t.Errorf("under= must not touch the top level: %q", got)
	}
	rep = importReport(t, e.do("POST", "/_api/spaces/main/import", string(z), "Content-Type", "application/zip"), 200)
	if rep.PagesSkipped != 2 || rep.PagesCreated != 0 {
		t.Errorf("skip: %+v", rep)
	}
	// The temporary copy of each upload is deleted.
	left, _ := filepath.Glob(filepath.Join(os.TempDir(), "wako-import-*.zip"))
	if len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestImportRejections(t *testing.T) {
	e := newEnv(t)
	post := func(body, ct string) *httptest.ResponseRecorder {
		return e.do("POST", "/_api/spaces/main/import", body, "Content-Type", ct)
	}
	for name, c := range map[string]struct {
		body, ct string
		want     int
		msg      string
	}{
		"not a zip":      {"this is plain text", "application/zip", 400, "not a zip"},
		"empty":          {"", "application/zip", 400, "not a zip"},
		"form, no file":  {"--x\r\nContent-Disposition: form-data; name=\"other\"\r\n\r\nv\r\n--x--\r\n", "multipart/form-data; boundary=x", 400, "part"},
		"newer format":   {string(zipOf(t, map[string]string{"wako.json": `{"format":"wako-export","version":99}`, "a.md": "x"})), "application/zip", 400, "newer"},
		"other manifest": {string(zipOf(t, map[string]string{"wako.json": `{"format":"nope","version":1}`})), "application/zip", 400, "not a Wako export"},
		"bad conflict":   {string(zipOf(t, map[string]string{"a.md": "x"})), "application/zip", 400, ""},
	} {
		url := "/_api/spaces/main/import"
		if name == "bad conflict" {
			url += "?conflict=merge"
		}
		rec := e.do("POST", url, c.body, "Content-Type", c.ct)
		if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.msg) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if rec := e.do("POST", "/_api/spaces/main/import?under=nowhere", string(zipOf(t, map[string]string{"a.md": "x"})), "Content-Type", "application/zip"); rec.Code != 400 {
		t.Errorf("missing parent: %d", rec.Code)
	}
	if rec := e.do("GET", "/_api/spaces/main/import", ""); rec.Code != 405 {
		t.Errorf("GET: %d", rec.Code)
	}
	// Too big: refused before it is read in full.
	e.h.(*Server).MaxImport = 2000
	big := zipOf(t, map[string]string{"a.md": strings.Repeat("random text that will not compress well ", 200) + string(pngBytes(t, 3))})
	rec := post(string(big)+strings.Repeat("x", 4000), "application/zip")
	if rec.Code != 413 {
		t.Errorf("too large: %d %s", rec.Code, rec.Body.String())
	}
	e.h.(*Server).MaxImport = 0
	// A zip that is small but unpacks to far more: a few KB of upload, 6 MB of text.
	bomb := zipOf(t, map[string]string{"a.md": strings.Repeat("A", 3<<20), "b.md": strings.Repeat("B", 3<<20)})
	if len(bomb) > 20000 {
		t.Fatalf("the test archive is not small enough to make its point: %d bytes", len(bomb))
	}
	e.h.(*Server).MaxImport = 8192 // so unpacked data may be at most 4x that
	rec = post(string(bomb), "application/zip")
	if rec.Code != 413 || !strings.Contains(rec.Body.String(), "unpacked") {
		t.Errorf("zip bomb: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := e.st.GetSpace("main"); err == nil {
		if sp, _ := e.st.GetSpace("main"); sp != nil {
			if _, err := sp.GetPage("a"); err == nil {
				t.Error("a refused archive left pages behind")
			}
		}
	}
	e.h.(*Server).MaxImport = 0
	left, _ := filepath.Glob(filepath.Join(os.TempDir(), "wako-import-*.zip"))
	if len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestImportQuotaStopsWithAReport(t *testing.T) {
	e := newEnv(t)
	e.st.Quota = 10 // less than any picture
	z := zipOf(t, map[string]string{"a.md": "# A\n", "pic.png": string(pngBytes(t, 7))})
	rec := e.do("POST", "/_api/spaces/main/import", string(z), "Content-Type", "application/zip")
	rep := importReport(t, rec, 507)
	if rec.Code != 507 || rep.PagesCreated != 0 {
		t.Errorf("quota: %d %+v", rec.Code, rep)
	}
}

// Over the wire, an export of one space imports into another unchanged.
func TestRoundTripOverHTTP(t *testing.T) {
	a := newAuthEnv(t)
	a.user("root", true)
	a.user("alice", false)
	src, dst := a.space("src", true), a.space("dst", false)
	_ = dst
	root := bearer(a.token("root"))
	mdT := header("Content-Type", "text/markdown")
	a.do("POST", "/src/_assets?name=shot.png", string(pngBytes(t, 8)), header("Content-Type", "image/png"), root)
	a.do("PUT", "/src/docs", "# Docs\n\n[ops](docs/ops)\n", mdT, root)
	a.do("PUT", "/src/docs/ops", "# Ops\n\n![s](shot.png)\n", mdT, root)
	a.do("PUT", "/src/zed", "# Zed first\n", mdT, root)
	a.do("POST", "/src?reorder", `{"order":["zed","docs"]}`, header("Content-Type", "application/json"), root)
	a.st.SetMember(src.ID, a.users["alice"].ID, "admin")
	a.st.SetMember(2, a.users["alice"].ID, "admin") // dst

	alice := bearer(a.token("alice"))
	rec := a.do("GET", "/_api/spaces/src/export", "", alice)
	z := status(t, rec, 200)
	rep := importReport(t, a.do("POST", "/_api/spaces/dst/import", z, header("Content-Type", "application/zip"), alice), 200)
	if rep.PagesCreated != 3 || rep.ImagesAdded != 1 {
		t.Errorf("report: %+v", rep)
	}
	for _, p := range []string{"zed", "docs", "docs/ops"} {
		got := status(t, a.do("GET", "/dst/"+p+".md", "", alice), 200)
		want := status(t, a.do("GET", "/src/"+p+".md", "", alice), 200)
		if got != want {
			t.Errorf("%s: %q vs %q", p, got, want)
		}
	}
	tree := func(space string) string {
		return status(t, a.do("GET", "/_api/spaces/"+space+"/tree", "", alice), 200)
	}
	if tree("src") != tree("dst") {
		t.Errorf("trees differ:\n%s\n%s", tree("src"), tree("dst"))
	}
	// The importer is credited.
	var revs []revJSON
	json.Unmarshal([]byte(status(t, a.do("GET", "/dst/docs?history", "", alice, header("Accept", "application/json")), 200)), &revs)
	if len(revs) != 1 || revs[0].Author != "alice" || revs[0].Note != "imported" {
		t.Errorf("history: %+v", revs)
	}
	// And the imported image renders from the new space.
	html := status(t, a.do("GET", "/dst/docs/ops", "", alice, header("Accept", "text/html")), 200)
	if !strings.Contains(html, "/dst/_assets/") {
		t.Errorf("image not served from the new space: %s", html)
	}
	// With a browser session the import needs the CSRF token.
	j := a.login("alice")
	if rec := a.do("POST", "/_api/spaces/dst/import", z, header("Content-Type", "application/zip"), withJar(j)); rec.Code != 403 {
		t.Errorf("import without CSRF: %d", rec.Code)
	}
	if rec := a.do("POST", "/_api/spaces/dst/import", z, header("Content-Type", "application/zip"), withJar(j), withCSRF(j)); rec.Code != 200 {
		t.Errorf("import with CSRF: %d", rec.Code)
	}
}

func TestSettingsPageOffersExportAndImport(t *testing.T) {
	e := newEnv(t)
	page := expect(t, e.do("GET", "/main/?settings", "", "Accept", "text/html"), 200)
	for _, want := range []string{`href="/_api/spaces/main/export"`, `id="import-form"`, `data-url="/_api/spaces/main/import"`, `name="conflict"`} {
		if !strings.Contains(page, want) {
			t.Errorf("settings page lacks %q", want)
		}
	}
	if js := expect(t, e.do("GET", "/_ui/app.js", ""), 200); !strings.Contains(js, "import-form") {
		t.Error("script lacks the import form")
	}
}

func TestPicturePlacementButton(t *testing.T) {
	e := newEnv(t)
	page := expect(t, e.do("GET", "/main/?settings", "", "Accept", "text/html"), 200)
	if !strings.Contains(page, `id="imgplace"`) {
		t.Error("top bar lacks the picture placement button")
	}
	if js := expect(t, e.do("GET", "/_ui/theme.js", ""), 200); !strings.Contains(js, "img-place") {
		t.Error("theme.js does not restore the remembered placement")
	}
	if css := expect(t, e.do("GET", "/_ui/app.css", ""), 200); !strings.Contains(css, "data-img=left") || !strings.Contains(css, "data-img=center") {
		t.Error("stylesheet lacks the placement rules")
	}
}

func TestPagesMayOpenPickedPicturesFromMemory(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/main/?settings", "", "Accept", "text/html")
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' data: blob: https:") {
		t.Errorf("the page policy must let the editor open a picked photo (blob:) to shrink it, got %q", csp)
	}
}
