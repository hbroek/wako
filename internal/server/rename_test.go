package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func (e *env) rename(space, id, to string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do("POST", "/"+space+"/_assets/"+id+"?rename="+to, "")
}

func TestRenameImageRewritesEveryPage(t *testing.T) {
	e := newEnv(t)
	a := asset(t, e.upload("main", "shot.png", pngBytes(t, 5)), 201)
	b := asset(t, e.upload("main", "keep.png", pngBytes(t, 6)), 201)

	expect(t, e.do("PUT", "/main/docs", "# Docs", "Content-Type", md), 201)
	one := "# One\n\n![the shot](shot.png \"Title\")\n\n![other](keep.png)\n\n```\n![code](shot.png)\n```\n"
	two := "Second page: ![a](shot.png) and again ![b](shot.png)\n"
	expect(t, e.do("PUT", "/main/docs/one", one, "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/two", two, "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/unrelated", "![x](keep.png)", "Content-Type", md), 201)
	v1 := e.do("GET", "/main/docs/one", "", "Accept", md).Header().Get("ETag")

	rec := e.rename("main", a.ID, "diagram.png")
	var got assetJSON
	body := expect(t, rec, 200)
	if err := jsonUnmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "diagram.png" || got.PagesUpdated == nil || *got.PagesUpdated != 2 || *got.PagesAliased != 0 ||
		rec.Header().Get("Location") != "/main/_assets/"+a.ID+"/diagram.png" {
		t.Fatalf("rename result: %s (Location %q)", body, rec.Header().Get("Location"))
	}

	// Markdown changed in both pages, but not inside the code block or in other images.
	wantOne := strings.Replace(one, "![the shot](shot.png \"Title\")", "![the shot](diagram.png \"Title\")", 1)
	if got := expect(t, e.do("GET", "/main/docs/one.md", ""), 200); got != wantOne {
		t.Fatalf("page one:\n%q\nwant\n%q", got, wantOne)
	}
	if got := expect(t, e.do("GET", "/main/two.md", ""), 200); got != strings.ReplaceAll(two, "shot.png", "diagram.png") {
		t.Fatalf("page two: %q", got)
	}
	if got := expect(t, e.do("GET", "/main/unrelated.md", ""), 200); got != "![x](keep.png)" {
		t.Fatalf("unrelated page changed: %q", got)
	}

	// Pages still render the image, now under its new name.
	html := expect(t, e.do("GET", "/main/two", "", "Accept", "text/html"), 200)
	if want := `src="/main/_assets/` + a.ID + `/diagram.png"`; !strings.Contains(html, want) {
		t.Fatalf("page does not show the renamed image: %s", html)
	}
	// Library: new name resolves, old name is gone and free.
	if r := e.do("GET", "/main/_assets/diagram.png", ""); r.Code != 302 {
		t.Errorf("new name lookup: %d", r.Code)
	}
	expect(t, e.do("GET", "/main/_assets/shot.png", ""), 404)
	list := expect(t, e.do("GET", "/main/_assets", "", "Accept", "application/json"), 200)
	if !strings.Contains(list, `"name":"diagram.png"`) || strings.Contains(list, `"name":"shot.png"`) ||
		!strings.Contains(list, `"used_by":2`) {
		t.Fatalf("library: %s", list)
	}

	// Rewritten pages have a new version, so a stale editor gets a conflict.
	if v2 := e.do("GET", "/main/docs/one", "", "Accept", md).Header().Get("ETag"); v2 == v1 {
		t.Error("page version did not change")
	}
	expect(t, e.do("PUT", "/main/docs/one", "stale edit", "Content-Type", md, "If-Match", v1), 412)
	// Saving again keeps the binding (the name in the text is the new one).
	expect(t, e.do("PUT", "/main/two", "![a](diagram.png) edited", "Content-Type", md), 200)
	if html := expect(t, e.do("GET", "/main/two", "", "Accept", "text/html"), 200); !strings.Contains(html, "diagram.png") {
		t.Fatal("binding lost after saving")
	}

	// The old name can be taken by a different image without touching those pages.
	c := asset(t, e.upload("main", "shot.png", pngBytes(t, 7)), 201)
	if c.Name != "shot.png" || c.ID == a.ID {
		t.Fatalf("freed name not reusable: %+v", c)
	}
	if html := expect(t, e.do("GET", "/main/docs/one", "", "Accept", "text/html"), 200); !strings.Contains(html, a.ID) || strings.Contains(html, c.ID) {
		t.Error("page must keep showing its own image")
	}
	_ = b
}

func TestRenameValidationAndConflicts(t *testing.T) {
	e := newEnv(t)
	a := asset(t, e.upload("main", "a.png", pngBytes(t, 1)), 201)
	b := asset(t, e.upload("main", "b.png", pngBytes(t, 2)), 201)
	j := asset(t, e.post("main", "p.jpg", "image/jpeg", jpegOf(t, noiseImg(30, 30))), 201)

	// Taken by another image.
	if msg := expect(t, e.rename("main", a.ID, "b.png"), 409); !strings.Contains(msg, "already") {
		t.Errorf("conflict message: %s", msg)
	}
	// Same name is a harmless no-op.
	var same assetJSON
	jsonUnmarshal(expect(t, e.rename("main", a.ID, "a.png"), 200), &same)
	if same.Name != "a.png" || *same.PagesUpdated != 0 {
		t.Errorf("no-op: %+v", same)
	}
	// Invalid names.
	for _, bad := range []string{"", "has%20space.png", "dir%2Fx.png", "..%2Fx.png", ".hidden.png", "-x.png", "x.gif", "x.png.exe", "x.", strings.Repeat("a", 120) + ".png", "a%00b.png"} {
		if r := e.rename("main", a.ID, bad); r.Code != 400 {
			t.Errorf("rename to %q: %d", bad, r.Code)
		}
	}
	// A missing extension gets the usual one; JPEG accepts .jpeg.
	var n assetJSON
	jsonUnmarshal(expect(t, e.rename("main", b.ID, "plain"), 200), &n)
	if n.Name != "plain.png" {
		t.Errorf("extension not added: %+v", n)
	}
	jsonUnmarshal(expect(t, e.rename("main", j.ID, "photo.jpeg"), 200), &n)
	if n.Name != "photo.jpeg" {
		t.Errorf("jpeg ext: %+v", n)
	}
	expect(t, e.rename("main", j.ID, "photo.png"), 400)
	// Unknown id, wrong space, and a missing target.
	expect(t, e.rename("main", strings.Repeat("0", 64), "x.png"), 404)
	expect(t, e.rename("other", a.ID, "x.png"), 404)
	expect(t, e.do("POST", "/main/_assets/"+a.ID, ""), 400)
	// Names are per space: the same name is free in another space.
	o := asset(t, e.upload("other", "a.png", pngBytes(t, 1)), 201)
	expect(t, e.rename("other", o.ID, "renamed.png"), 200)
	expect(t, e.do("GET", "/main/_assets/a.png", ""), 302)
}

func TestRenameMergesIntoExistingReference(t *testing.T) {
	e := newEnv(t)
	a := asset(t, e.upload("main", "old.png", pngBytes(t, 1)), 201)
	// The page already mentions "new.png" (no such image yet, so it is unbound).
	expect(t, e.do("PUT", "/main/p", "![a](old.png) ![b](new.png)", "Content-Type", md), 201)
	var r assetJSON
	jsonUnmarshal(expect(t, e.rename("main", a.ID, "new.png"), 200), &r)
	if *r.PagesUpdated != 1 {
		t.Fatalf("%+v", r)
	}
	if got := expect(t, e.do("GET", "/main/p.md", ""), 200); got != "![a](new.png) ![b](new.png)" {
		t.Fatalf("markdown: %q", got)
	}
	// Both references now point at the library image after the next save.
	expect(t, e.do("PUT", "/main/p", "![a](new.png) ![b](new.png) again", "Content-Type", md), 200)
	html := expect(t, e.do("GET", "/main/p", "", "Accept", "text/html"), 200)
	if strings.Count(html, "/_assets/"+a.ID+"/new.png") != 2 {
		t.Fatalf("both references should show the image: %s", html)
	}
}

func TestRenameUIAndHeaders(t *testing.T) {
	e := newEnv(t)
	asset(t, e.upload("main", "a.png", pngBytes(t, 1)), 201)
	page := expect(t, e.do("GET", "/main/_assets/", "", "Accept", "text/html"), 200)
	for _, want := range []string{`data-rename`, `data-name="a.png"`, `id="rename"`, `id="rename-to"`} {
		if !strings.Contains(page, want) {
			t.Errorf("library page missing %q", want)
		}
	}
}
