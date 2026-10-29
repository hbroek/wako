package archive

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hbroek/wako/internal/img"
	"github.com/hbroek/wako/internal/render"
	"github.com/hbroek/wako/internal/store"
)

// ---- fixtures ----

type env struct {
	t  *testing.T
	st *store.Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.RevisionWindow = 0
	return &env{t, st}
}

func (e *env) space(slug string) *store.Space {
	e.t.Helper()
	sp, err := e.st.CreateSpace(slug, strings.Title(slug), true)
	if err != nil {
		e.t.Fatal(err)
	}
	return sp
}

func put(t *testing.T, sp *store.Space, path, body string) {
	t.Helper()
	refs := store.Refs{Images: render.ImageRefs(body), Links: render.Links(body)}
	if _, _, err := sp.PutPage(path, titleOf(body, path), body, refs, store.Cond{By: "alice"}); err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
}

func titleOf(body, path string) string {
	if t := render.Title(body); t != "" {
		return t
	}
	return path[strings.LastIndex(path, "/")+1:]
}

func swatch(shade uint8) *image.Gray {
	im := image.NewGray(image.Rect(0, 0, 8, 6))
	for i := range im.Pix {
		im.Pix[i] = shade + uint8(i%7)
	}
	return im
}

func pngOf(t *testing.T, shade uint8) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, swatch(shade)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegOf(t *testing.T, shade uint8, segs ...[]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, swatch(shade), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	out := append([]byte(nil), b.Bytes()[:2]...)
	for _, s := range segs {
		out = append(out, s...)
	}
	return append(out, b.Bytes()[2:]...)
}

func gifOf(t *testing.T) []byte {
	t.Helper()
	g := &gif.GIF{}
	for i := 0; i < 2; i++ {
		f := image.NewPaletted(image.Rect(0, 0, 5, 5), color.Palette{color.Black, color.White})
		f.SetColorIndex(i, i, 1)
		g.Image = append(g.Image, f)
		g.Delay = append(g.Delay, 4)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, g); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

const svgText = `<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="10" height="10"/></svg>`

// addImage stores bytes in a space's library the way an upload does.
func addImage(t *testing.T, sp *store.Space, name string, data []byte) *store.Asset {
	t.Helper()
	mime, ok := img.Sniff(data, name)
	if !ok {
		t.Fatalf("%s is not an image", name)
	}
	res, err := img.Process(data, mime, img.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := sp.PutAsset(img.CleanName(name, res.Mime), res.Mime, res.Data, res.Width, res.Height)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// makeZip builds an archive in memory. Names ending in "/" are directories.
func makeZip(t *testing.T, files map[string][]byte) *zip.Reader {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(files[n])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return zr
}

func exportZip(t *testing.T, sp *store.Space) (*zip.Reader, []byte) {
	t.Helper()
	var b bytes.Buffer
	if err := Export(&b, sp, time.Unix(1_800_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatalf("export is not a valid zip: %v", err)
	}
	return zr, b.Bytes()
}

func entries(zr *zip.Reader) map[string]*zip.File {
	m := map[string]*zip.File{}
	for _, f := range zr.File {
		m[f.Name] = f
	}
	return m
}

func read(t *testing.T, f *zip.File) []byte {
	t.Helper()
	rc, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func names(zr *zip.Reader) string {
	var n []string
	for _, f := range zr.File {
		n = append(n, f.Name)
	}
	sort.Strings(n)
	return strings.Join(n, " ")
}

// ---- export ----

func TestExportLayout(t *testing.T) {
	e := newEnv(t)
	sp := e.space("main")
	shot := addImage(t, sp, "shot.png", pngOf(t, 10))
	addImage(t, sp, "only-top.png", pngOf(t, 40))
	spare := addImage(t, sp, "spare.png", pngOf(t, 80))
	crlf := "# Docs\r\n\r\nwindows line endings\r\nno final newline"
	put(t, sp, "docs", crlf)
	put(t, sp, "docs/ops", "# Ops\n\n![the shot](shot.png)\n")
	put(t, sp, "docs/dev", "# Dev\n\n![same](shot.png) and [link](ops)\n")
	put(t, sp, "top", "# Top\n\n![a](shot.png) ![b](only-top.png)\n")

	zr, _ := exportZip(t, sp)
	want := "_library/spare.png docs.md docs/dev.md docs/ops.md docs/shot.png only-top.png shot.png top.md wako.json"
	if got := names(zr); got != want {
		t.Fatalf("files:\n got %s\nwant %s", got, want)
	}
	z := entries(zr)

	// Pages are written exactly as stored: bytes, line endings, bare image names.
	for path, body := range map[string]string{
		"docs.md": crlf, "docs/ops.md": "# Ops\n\n![the shot](shot.png)\n", "top.md": "# Top\n\n![a](shot.png) ![b](only-top.png)\n",
	} {
		if got := string(read(t, z[path])); got != body {
			t.Errorf("%s changed:\n got %q\nwant %q", path, got, body)
		}
	}
	// Each image is next to the pages that use it, and unchanged.
	for _, f := range []string{"docs/shot.png", "shot.png"} {
		if got := read(t, z[f]); !bytes.Equal(got, shot.Data) {
			t.Errorf("%s differs from the library copy", f)
		}
		if z[f].Method != zip.Store {
			t.Errorf("%s is compressed again", f)
		}
	}
	if !bytes.Equal(read(t, z["_library/spare.png"]), spare.Data) {
		t.Error("unused image differs")
	}
	if z["docs/ops.md"].Method != zip.Deflate {
		t.Error("pages should be compressed")
	}
	if got, want := z["docs/ops.md"].Modified.UTC().Unix(), int64(0); got <= want {
		t.Errorf("modified time not set: %v", z["docs/ops.md"].Modified)
	}

	// The manifest describes the space and the order of the pages.
	var m Manifest
	if err := json.Unmarshal(read(t, z[ManifestName]), &m); err != nil {
		t.Fatal(err)
	}
	if m.Format != Format || m.Version != Version || m.Space.Slug != "main" || m.Space.Title != "Main" || !m.Space.PublicRead {
		t.Errorf("manifest header: %+v", m)
	}
	var paths []string
	for _, p := range m.Pages {
		paths = append(paths, p.Path+"="+p.Title)
	}
	if got := strings.Join(paths, " "); got != "docs=Docs docs/ops=Ops docs/dev=Dev top=Top" {
		t.Errorf("page order: %s", got)
	}
	var imgs []string
	for _, im := range m.Images {
		if im.SHA256 == "" || im.Size == 0 {
			t.Errorf("image without checksum: %+v", im)
		}
		imgs = append(imgs, fmt.Sprintf("%s:%v", im.File, im.Used))
	}
	if got := strings.Join(imgs, " "); got != "_library/spare.png:false docs/shot.png:true only-top.png:true shot.png:true" {
		t.Errorf("image list: %s", got)
	}
	if m.Images[0].SHA256 != spare.ID {
		t.Error("checksum is not the content hash")
	}
	// No frontmatter or other additions to the pages.
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".md") && bytes.HasPrefix(read(t, f), []byte("---")) {
			t.Errorf("%s starts with frontmatter", f.Name)
		}
	}
}

func TestExportKeepsSiblingPicturesApart(t *testing.T) {
	e := newEnv(t)
	sp := e.space("main")
	a := addImage(t, sp, "s.png", pngOf(t, 10))
	put(t, sp, "dir", "# Dir")
	put(t, sp, "dir/b", "![x](s.png)")
	// Image A is renamed but page "dir/b" cannot be rewritten, so it keeps "s.png" as an alias.
	if _, err := sp.RenameAsset(a.ID, "a2.png", func(body, from, to string) (string, bool) { return body, false }, ""); err != nil {
		t.Fatal(err)
	}
	other := addImage(t, sp, "s.png", pngOf(t, 99)) // a different picture takes the free name
	put(t, sp, "dir/c", "![y](s.png)")              // sorts after dir/b, binds the new picture

	zr, _ := exportZip(t, sp)
	z := entries(zr)
	if !bytes.Equal(read(t, z["dir/s.png"]), a.Data) || !bytes.Equal(read(t, z["dir/s-2.png"]), other.Data) {
		t.Fatalf("pictures mixed up: %s", names(zr))
	}
	if got := string(read(t, z["dir/b.md"])); got != "![x](s.png)" {
		t.Errorf("dir/b: %q", got)
	}
	if got := string(read(t, z["dir/c.md"])); got != "![y](s-2.png)" {
		t.Errorf("dir/c must name its own picture: %q", got)
	}
	var m Manifest
	json.Unmarshal(read(t, z[ManifestName]), &m)
	if len(m.Notes) != 1 || !strings.Contains(m.Notes[0], "dir/c") {
		t.Errorf("notes: %v", m.Notes)
	}
}

// ---- round trip ----

func snapshot(t *testing.T, sp *store.Space) string {
	t.Helper()
	var sb strings.Builder
	tree, _ := sp.Tree()
	var walk func(ns []*store.Node, depth int)
	walk = func(ns []*store.Node, depth int) {
		for _, n := range ns {
			p, _ := sp.GetPage(n.Path)
			fmt.Fprintf(&sb, "%s%s [%s] %q\n", strings.Repeat("  ", depth), n.Path, n.Title, p.Body)
			walk(n.Children, depth+1)
		}
	}
	walk(tree, 0)
	lib, _ := sp.AllAssets()
	for _, a := range lib {
		fmt.Fprintf(&sb, "image %s %s %s %dx%d used=%d\n", a.Name, a.ID, a.Mime, a.Width, a.Height, a.UsedBy)
	}
	uses, _ := sp.PageImages()
	for _, u := range uses {
		fmt.Fprintf(&sb, "binding %s %s %s\n", u.PagePath, u.Name, u.AssetID)
	}
	rows, _ := sp.LinkRows()
	for _, r := range rows {
		fmt.Fprintf(&sb, "link %s -> %s\n", r.SrcPath, r.Target)
	}
	return sb.String()
}

func TestRoundTripIsIdentical(t *testing.T) {
	e := newEnv(t)
	a, b := e.space("source"), e.space("copy")
	addImage(t, a, "shot.png", pngOf(t, 10))
	addImage(t, a, "photo.jpg", jpegOf(t, 60))
	addImage(t, a, "anim.gif", gifOf(t))
	addImage(t, a, "logo.svg", []byte(svgText))
	addImage(t, a, "spare.png", pngOf(t, 120))
	put(t, a, "zeta", "# Zeta first\n\nlast alphabetically, first in order\n")
	put(t, a, "docs", "# Docs\n\nSee [ops](docs/ops) and ![logo](logo.svg).\n")
	put(t, a, "docs/ops", "# Ops\r\n\r\n![the shot](shot.png) ![anim](anim.gif)\r\n")
	put(t, a, "docs/dev", "# Dev\n\n![photo](photo.jpg)\n\n```\n![not an image](nothing.png)\n```\n")
	put(t, a, "alpha", "no heading here, so the title is the name\n")
	if err := a.Reorder("", []string{"zeta", "docs", "alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Reorder("docs", []string{"ops", "dev"}); err != nil {
		t.Fatal(err)
	}

	zr, _ := exportZip(t, a)
	rep, err := Import(zr, b, Options{By: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.PagesCreated != 5 || rep.ImagesAdded != 5 || rep.PagesSkipped != 0 || rep.Placeholders != 0 || len(rep.Warnings) != 0 {
		t.Errorf("report: %+v", rep)
	}
	if got, want := snapshot(t, b), snapshot(t, a); got != want {
		t.Fatalf("the copy differs from the original:\n--- copy\n%s--- original\n%s", got, want)
	}
	// Importing the export of the copy gives the same files again.
	zr2, _ := exportZip(t, b)
	if names(zr2) != names(zr) {
		t.Errorf("second export has other files:\n%s\n%s", names(zr2), names(zr))
	}
	e1, e2 := entries(zr), entries(zr2)
	for n, f := range e1 {
		if n == ManifestName {
			continue
		}
		if !bytes.Equal(read(t, f), read(t, e2[n])) {
			t.Errorf("%s differs after a round trip", n)
		}
	}
	// Search and links work in the copy, and the import is credited and noted.
	if hits, _ := b.Search("alphabetically", 5); len(hits) != 1 || hits[0].Path != "zeta" {
		t.Errorf("search: %+v", hits)
	}
	if rows, _ := b.LinkRows(); len(rows) != 1 || rows[0].Target != "docs/ops" {
		t.Errorf("links: %+v", rows)
	}
	p, _ := b.GetPage("docs")
	revs, _ := b.Revisions(p.ID)
	if len(revs) != 1 || revs[0].Author != "bob" || revs[0].Note != "imported" {
		t.Errorf("history of an imported page: %+v", revs)
	}
}

func TestImportingAgainSkipsOrOverwrites(t *testing.T) {
	e := newEnv(t)
	a, b := e.space("a"), e.space("b")
	put(t, a, "docs", "# Docs\n\nfrom the export\n")
	put(t, a, "docs/new", "# New\n")
	put(t, b, "docs", "# Docs\n\nalready here\n")
	zr, _ := exportZip(t, a)

	rep, err := Import(zr, b, Options{})
	if err != nil || rep.PagesCreated != 1 || rep.PagesSkipped != 1 || rep.PagesUpdated != 0 {
		t.Fatalf("skip: %+v %v", rep, err)
	}
	if p, _ := b.GetPage("docs"); !strings.Contains(p.Body, "already here") {
		t.Error("an existing page was overwritten in skip mode")
	}
	rep, err = Import(zr, b, Options{Conflict: "overwrite", By: "carol"})
	if err != nil || rep.PagesUpdated != 2 || rep.PagesCreated != 0 {
		t.Fatalf("overwrite: %+v %v", rep, err)
	}
	p, _ := b.GetPage("docs")
	if !strings.Contains(p.Body, "from the export") {
		t.Error("page not replaced")
	}
	revs, _ := b.Revisions(p.ID)
	if len(revs) != 2 || revs[0].Author != "carol" || revs[1].Body != "" {
		t.Errorf("the old text must stay in the history: %+v", revs)
	}
	if old, _ := b.Revision(p.ID, revs[1].Version); !strings.Contains(old.Body, "already here") {
		t.Errorf("old revision: %+v", old)
	}
	if _, err := Import(zr, b, Options{Conflict: "merge"}); !errors.Is(err, ErrBadArchive) {
		t.Errorf("unknown conflict mode: %v", err)
	}
}

func TestImportUnder(t *testing.T) {
	e := newEnv(t)
	sp := e.space("main")
	put(t, sp, "inbox", "# Inbox\n")
	zr := makeZip(t, map[string][]byte{"one.md": []byte("# One\n"), "sub/two.md": []byte("# Two\n"), "pic.png": pngOf(t, 5)})
	rep, err := Import(zr, sp, Options{Under: "inbox"})
	if err != nil || rep.PagesCreated != 2 || rep.Placeholders != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	for _, p := range []string{"inbox/one", "inbox/sub", "inbox/sub/two"} {
		if _, err := sp.GetPage(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	if _, err := sp.GetPage("one"); err == nil {
		t.Error("page imported at the top as well")
	}
	if _, err := Import(zr, sp, Options{Under: "nowhere"}); !errors.Is(err, ErrBadArchive) {
		t.Errorf("missing parent: %v", err)
	}
}

// ---- plain folders ----

func tree(t *testing.T, sp *store.Space) string {
	t.Helper()
	var out []string
	nodes, _ := sp.Tree()
	var walk func(ns []*store.Node)
	walk = func(ns []*store.Node) {
		for _, n := range ns {
			out = append(out, n.Path+"="+n.Title)
			walk(n.Children)
		}
	}
	walk(nodes)
	return strings.Join(out, " | ")
}

func TestImportPlainMarkdownFolder(t *testing.T) {
	e := newEnv(t)
	sp := e.space("main")
	pic := pngOf(t, 33)
	zr := makeZip(t, map[string][]byte{
		// The whole folder is wrapped in one directory, as "Compress" makes it.
		"notes/Intro.md":                 []byte("# Intro\n\nsee ![a](images/pic%20one.png) and ![b](<images/pic one.png>) and ![c](./images/Pic%20One.PNG)\n"),
		"notes/My Notes.md":              []byte("no heading in this one\r\nsecond line\r\n"),
		"notes/Deep Dive/index.md":       []byte("# Deep Dive overview\n"),
		"notes/Deep Dive/Part 1.md":      []byte("# Part 1\n\n![](../images/pic%20one.png)\n"),
		"notes/Ideas.md":                 []byte("# Ideas\n"),
		"notes/Ideas/README.md":          []byte("# readme beside Ideas.md\n"),
		"notes/a/b/c.md":                 []byte("# Deeply nested\n"),
		"notes/résumé.md":                []byte("# CV\n"),
		"notes/CAPS and Spaces.md":       append([]byte{0xEF, 0xBB, 0xBF}, []byte("# Mixed Case\n")...),
		"notes/broken.md":                {0xff, 0xfe, 0x00, 'x'},
		"notes/images/pic one.png":       pic,
		"notes/.obsidian/workspace.json": []byte("{}"),
		"notes/__MACOSX/._Intro.md":      []byte("junk"),
		"notes/.DS_Store":                []byte("junk"),
		"notes/data.csv":                 []byte("a,b"),
		"notes/data2.csv":                []byte("a,b"),
		"notes/Makefile":                 []byte("all:"),
		"notes/fake.png":                 []byte("this is not a png"),
	})
	rep, err := Import(zr, sp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Depth first; siblings alphabetically, ignoring case.
	want := "a=a | a/b=b | a/b/c=Deeply nested | CAPS-and-Spaces=Mixed Case | Deep-Dive=Deep Dive overview | Deep-Dive/Part-1=Part 1 | " +
		"Ideas=Ideas | Ideas/README=readme beside Ideas.md | Intro=Intro | My-Notes=My Notes | r-sum=CV"
	if got := tree(t, sp); got != want {
		t.Errorf("tree:\n got %s\nwant %s", got, want)
	}
	if rep.PagesCreated != 9 || rep.Placeholders != 2 || rep.ImagesAdded != 1 || rep.PagesSkipped != 0 {
		t.Errorf("report: %+v", rep)
	}
	// Nothing was invented for the text; the BOM goes, line endings stay.
	if p, _ := sp.GetPage("My-Notes"); p.Body != "no heading in this one\r\nsecond line\r\n" || p.Title != "My Notes" {
		t.Errorf("My-Notes: %+v", p)
	}
	if p, _ := sp.GetPage("CAPS-and-Spaces"); p.Body != "# Mixed Case\n" {
		t.Errorf("BOM not removed: %q", p.Body)
	}
	// A folder without a page of its own got an empty one, titled with its name.
	if p, _ := sp.GetPage("a"); p.Body != "# a\n" {
		t.Errorf("placeholder: %q", p.Body)
	}
	// Images named by path now name the library picture, however they were written.
	intro, _ := sp.GetPage("Intro")
	if intro.Body != "# Intro\n\nsee ![a](pic-one.png) and ![b](<pic-one.png>) and ![c](pic-one.png)\n" {
		t.Errorf("Intro: %q", intro.Body)
	}
	if p, _ := sp.GetPage("Deep-Dive/Part-1"); p.Body != "# Part 1\n\n![](pic-one.png)\n" {
		t.Errorf("Part 1: %q", p.Body)
	}
	uses, _ := sp.PageImages()
	if len(uses) != 2 || uses[0].PagePath != "Deep-Dive/Part-1" || uses[0].Name != "pic-one.png" || uses[1].PagePath != "Intro" {
		t.Errorf("bindings: %+v", uses)
	}
	lib, _ := sp.AllAssets()
	if len(lib) != 1 || lib[0].Name != "pic-one.png" {
		t.Errorf("library: %+v", lib)
	}
	// What was left out is said so.
	joined := strings.Join(rep.Warnings, "\n")
	for _, w := range []string{"broken.md", "valid UTF-8", "fake.png", "not a PNG", "2 file(s) of type .csv", "type (no extension)"} {
		if !strings.Contains(joined, w) {
			t.Errorf("warnings lack %q:\n%s", w, joined)
		}
	}
	for _, quiet := range []string{".obsidian", "__MACOSX", ".DS_Store"} {
		if strings.Contains(joined, quiet) {
			t.Errorf("hidden files should be skipped without comment: %s", joined)
		}
	}
}

func TestImportOrdersBySomethingSensible(t *testing.T) {
	e := newEnv(t)
	sp := e.space("main")
	zr := makeZip(t, map[string][]byte{"b.md": []byte("# b\n"), "A.md": []byte("# A\n"), "c.md": []byte("# c\n"), "B/x.md": []byte("# x\n")})
	if _, err := Import(zr, sp, Options{}); err != nil {
		t.Fatal(err)
	}
	// Alphabetical regardless of case. "B/x.md" needs a parent "B", which is not
	// the page "b": slugs keep their case.
	if got := tree(t, sp); got != "A=A | B=B | B/x=x | b=b | c=c" {
		t.Errorf("tree: %s", got)
	}
}

func TestImportedImageNames(t *testing.T) {
	e := newEnv(t)
	sp := e.space("main")
	same := pngOf(t, 50)
	zr := makeZip(t, map[string][]byte{
		"a/p.md":       []byte("![x](logo.png)"),
		"b/p.md":       []byte("![x](./logo.png)"),
		"c/p.md":       []byte("![x](logo.png) ![y](same.png)"),
		"d/p.md":       []byte("![x](photo.jpeg) ![y](same.png)"),
		"a/logo.png":   pngOf(t, 10),
		"b/logo.png":   pngOf(t, 20), // another picture with the same name
		"c/logo.png":   pngOf(t, 10), // the same picture as a/logo.png
		"c/same.png":   same,
		"d/same.png":   same,
		"d/photo.jpeg": jpegOf(t, 70),
	})
	rep, err := Import(zr, sp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.ImagesAdded != 4 || rep.ImagesExisting != 2 { // logo, logo-2, same, photo; and two repeats
		t.Errorf("report: %+v", rep)
	}
	body := func(p string) string { pg, _ := sp.GetPage(p); return pg.Body }
	for path, want := range map[string]string{
		"a/p": "![x](logo.png)",
		"b/p": "![x](logo-2.png)",               // the other picture, so the text names it
		"c/p": "![x](logo.png) ![y](same.png)",  // identical to a's picture: one asset
		"d/p": "![x](photo.jpg) ![y](same.png)", // the extension follows the real type
	} {
		if got := body(path); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
	// Each page is bound to the picture from its own folder.
	uses, _ := sp.PageImages()
	got := map[string]string{}
	for _, u := range uses {
		a, _ := sp.AssetInfo(u.AssetID)
		got[u.PagePath+" "+u.Name] = a.Name
	}
	want := map[string]string{"a/p logo.png": "logo.png", "b/p logo-2.png": "logo-2.png", "c/p logo.png": "logo.png", "c/p same.png": "same.png", "d/p photo.jpg": "photo.jpg", "d/p same.png": "same.png"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("binding %q -> %q, want %q", k, got[k], v)
		}
	}
	// The pictures themselves came out distinct.
	logoA, _ := sp.FindAssetByName("logo.png")
	logoB, _ := sp.FindAssetByName("logo-2.png")
	if logoA.ID == logoB.ID {
		t.Error("two different pictures were merged")
	}
}

func TestImportCleansImages(t *testing.T) {
	e := newEnv(t)
	sp := e.space("main")
	gps := []byte{0xFF, 0xE1, 0x00, 0x1A, 'E', 'x', 'i', 'f', 0, 0, 'G', 'P', 'S', 'L', 'a', 't', '=', '5', '2', '.', '3', '7', 0, 0, 0, 0, 0, 0, 0, 0}
	big := image.NewGray(image.Rect(0, 0, 2000, 40))
	var bigPNG bytes.Buffer
	png.Encode(&bigPNG, big)
	zr := makeZip(t, map[string][]byte{
		"p.md": []byte("![a](holiday.jpg) ![b](wide.png)"), "holiday.jpg": jpegOf(t, 60, gps), "wide.png": bigPNG.Bytes(),
	})
	lim := img.DefaultLimits
	lim.MaxSide = 500
	if _, err := Import(zr, sp, Options{Limits: lim}); err != nil {
		t.Fatal(err)
	}
	a, _ := sp.FindAssetByName("holiday.jpg")
	full, _ := sp.GetAsset(a.ID)
	if bytes.Contains(full.Data, []byte("GPSLat")) {
		t.Error("location data survived an import")
	}
	w, _ := sp.FindAssetByName("wide.png")
	if w.Width != 500 {
		t.Errorf("imported pictures follow the size limits: %dx%d", w.Width, w.Height)
	}
}

// ---- hostile and damaged archives ----

func TestImportRefusesUnsafeEntries(t *testing.T) {
	e := newEnv(t)
	sp := e.space("main")
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	add := func(name string, mode os.FileMode, body string) {
		fh := &zip.FileHeader{Name: name, Method: zip.Deflate}
		fh.SetMode(mode)
		w, _ := zw.CreateHeader(fh)
		w.Write([]byte(body))
	}
	add("ok.md", 0o644, "# ok\n")
	add("../evil.md", 0o644, "# evil\n")
	add("/etc/abs.md", 0o644, "# abs\n")
	add("a/../../up.md", 0o644, "# up\n")
	add("C:/win.md", 0o644, "# win\n")
	add("dir\\..\\..\\back.md", 0o644, "# back\n")
	add("link.md", os.ModeSymlink|0o777, "/etc/passwd")
	add("dup.md", 0o644, "# first\n")
	add("dup.md", 0o644, "# second\n")
	add("nul\x00.md", 0o644, "# nul\n")
	zw.Close()
	zr, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Import(zr, sp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := tree(t, sp); got != "dup=first | ok=ok" {
		t.Errorf("tree: %s", got)
	}
	if rep.PagesCreated != 2 {
		t.Errorf("report: %+v", rep)
	}
	joined := strings.Join(rep.Warnings, "\n")
	for _, w := range []string{"unsafe path", "not a regular file", "twice"} {
		if !strings.Contains(joined, w) {
			t.Errorf("warnings lack %q:\n%s", w, joined)
		}
	}
	// Nothing outside the space was written.
	for _, bad := range []string{"../evil.md", "/etc/abs.md", "up.md", "win.md", "back.md"} {
		if _, err := os.Stat(bad); err == nil {
			t.Errorf("%s was written", bad)
		}
	}
}

func TestImportLimits(t *testing.T) {
	e := newEnv(t)
	sp := e.space("main")
	many := map[string][]byte{}
	for i := 0; i < 30; i++ {
		many[fmt.Sprintf("p%02d.md", i)] = []byte("# x\n")
	}
	if _, err := Import(makeZip(t, many), sp, Options{MaxFiles: 10}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("too many entries: %v", err)
	}
	if _, err := Import(makeZip(t, map[string][]byte{"a.md": bytes.Repeat([]byte("x"), 5000), "b.md": bytes.Repeat([]byte("y"), 5000)}), sp, Options{MaxTotalBytes: 6000}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("too large when unpacked: %v", err)
	}
	if tree(t, sp) != "" {
		t.Error("a refused archive must change nothing")
	}
	rep, err := Import(makeZip(t, map[string][]byte{
		"big.md": bytes.Repeat([]byte("x"), 3000), "small.md": []byte("# small\n"),
		"huge.png": pngOf(t, 1), "ok.png": pngOf(t, 2),
	}), sp, Options{MaxPageBytes: 1000, MaxImageBytes: 40})
	if err != nil {
		t.Fatal(err)
	}
	if got := tree(t, sp); got != "small=small" || rep.ImagesAdded != 0 && rep.ImagesAdded != 1 {
		t.Errorf("tree %s, report %+v", got, rep)
	}
	if j := strings.Join(rep.Warnings, "\n"); !strings.Contains(j, `"big.md": larger than`) {
		t.Errorf("warnings: %s", j)
	}
	// Pictures over the pixel limit are skipped, not imported.
	lim := img.DefaultLimits
	lim.MaxPixels = 10
	rep, _ = Import(makeZip(t, map[string][]byte{"p.md": []byte("![x](t.png)"), "t.png": pngOf(t, 9)}), sp, Options{Limits: lim})
	if rep.ImagesAdded != 0 || !strings.Contains(strings.Join(rep.Warnings, "\n"), "too many pixels") {
		t.Errorf("pixel limit: %+v", rep)
	}
	// A full media quota stops the import with a clear error.
	e.st.Quota = 10
	if _, err := Import(makeZip(t, map[string][]byte{"q.png": pngOf(t, 3)}), e.space("small"), Options{}); !errors.Is(err, store.ErrQuota) {
		t.Errorf("quota: %v", err)
	}
}

func TestManifestIsChecked(t *testing.T) {
	e := newEnv(t)
	sp := e.space("main")
	good := func() (map[string][]byte, *Manifest) {
		m := &Manifest{Format: Format, Version: Version, Pages: []ManifestPage{{"b", "b"}, {"a", "a"}}, Images: []ManifestImage{{File: "x.png", SHA256: "wrong", Size: 1}}}
		return map[string][]byte{"a.md": []byte("# a\n"), "b.md": []byte("# b\n"), "x.png": pngOf(t, 4)}, m
	}
	withManifest := func(files map[string][]byte, m any) *zip.Reader {
		data, _ := json.Marshal(m)
		files[ManifestName] = data
		return makeZip(t, files)
	}
	files, m := good()
	if _, err := Import(withManifest(files, m), sp, Options{}); !errors.Is(err, ErrBadArchive) || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("damaged image: %v", err)
	}
	if tree(t, sp) != "" {
		t.Error("a damaged archive must change nothing")
	}
	files, m = good()
	m.Format = "something-else"
	if _, err := Import(withManifest(files, m), sp, Options{}); !errors.Is(err, ErrBadArchive) {
		t.Errorf("other format: %v", err)
	}
	files, m = good()
	m.Version = 99
	if _, err := Import(withManifest(files, m), sp, Options{}); !errors.Is(err, ErrBadArchive) || !strings.Contains(err.Error(), "newer") {
		t.Errorf("newer format: %v", err)
	}
	if _, err := Import(makeZip(t, map[string][]byte{ManifestName: []byte("{not json"), "a.md": []byte("# a")}), sp, Options{}); !errors.Is(err, ErrBadArchive) {
		t.Errorf("invalid JSON: %v", err)
	}
	// With an honest manifest the pages come in the order it gives.
	files, m = good()
	m.Images[0].SHA256 = ""
	if _, err := Import(withManifest(files, m), sp, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, sp); got != "b=b | a=a" {
		t.Errorf("manifest order: %s", got)
	}
	zr, _ := exportZip(t, sp)
	if mm, err := ReadManifest(zr); err != nil || mm == nil || mm.Space.Slug != "main" {
		t.Errorf("ReadManifest: %+v %v", mm, err)
	}
	if mm, err := ReadManifest(makeZip(t, map[string][]byte{"a.md": []byte("x")})); mm != nil || err != nil {
		t.Errorf("no manifest: %+v %v", mm, err)
	}
}

// An archive of any bytes must never crash the importer or put a page anywhere
// it should not be.
func FuzzImport(f *testing.F) {
	seed := func(files map[string]string) {
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		for n, c := range files {
			w, _ := zw.Create(n)
			w.Write([]byte(c))
		}
		zw.Close()
		f.Add(b.Bytes())
	}
	seed(map[string]string{"a.md": "# A\n\n![x](img/p.png) [l](b)\n", "b.md": "# B\n", "img/p.png": "not a png", "wako.json": `{"format":"wako-export","version":1,"pages":[{"path":"b"},{"path":"a"}]}`})
	seed(map[string]string{"../evil.md": "x", "/abs.md": "y", "a/../../z.md": "z", "d\\e\\f.md": "w", "My Notes/index.md": "# i\n", "x.png": "q"})
	seed(map[string]string{"wako.json": `{"format":"wako-export","version":1,"images":[{"file":"a.png","sha256":"00"}]}`, "a.png": "q"})
	f.Add([]byte("PK\x03\x04 not really"))
	f.Add([]byte{})
	st, err := store.Open(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { st.Close() })
	n := 0
	f.Fuzz(func(t *testing.T, data []byte) {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		n++
		sp, err := st.CreateSpace(fmt.Sprintf("s%d", n), "S", true)
		if err != nil {
			t.Fatal(err)
		}
		defer st.DeleteSpace(sp.Slug)
		rep, _ := Import(zr, sp, Options{MaxTotalBytes: 8 << 20, MaxFiles: 500, MaxPageBytes: 1 << 20, MaxImageBytes: 1 << 20})
		if rep == nil {
			return
		}
		ix, err := sp.PageIndex()
		if err != nil {
			t.Fatal(err)
		}
		tr, _ := sp.Tree()
		var walk func(ns []*store.Node)
		walk = func(ns []*store.Node) {
			for _, nd := range ns {
				if _, err := store.SplitPath(nd.Path); err != nil || !ix.Has(nd.Path) {
					t.Fatalf("imported page with an invalid path %q (%v)", nd.Path, err)
				}
				walk(nd.Children)
			}
		}
		walk(tr)
	})
}
