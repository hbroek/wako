package archive

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hbroek/wako/internal/img"
	"github.com/hbroek/wako/internal/render"
	"github.com/hbroek/wako/internal/store"
)

var (
	// ErrBadArchive means the zip cannot be imported as given.
	ErrBadArchive = errors.New("bad archive")
	// ErrTooLarge means the zip, or something in it, is over a limit.
	ErrTooLarge = errors.New("archive too large")
)

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadArchive, fmt.Sprintf(format, a...))
}

// Options control an import.
type Options struct {
	By       string // recorded as the author of the new pages
	Conflict string // for pages that already exist: "skip" (default) or "overwrite"
	Under    string // import below this existing page; "" imports at the top

	Limits        img.Limits
	MaxImageBytes int64 // largest image file; default 25 MiB
	MaxPageBytes  int64 // largest page text; default 4 MiB
	MaxTotalBytes int64 // largest total unpacked size; default 1 GiB
	MaxFiles      int   // most entries; default 50000
}

// Report says what an import did.
type Report struct {
	PagesCreated   int      `json:"pages_created"`
	PagesUpdated   int      `json:"pages_updated"`
	PagesSkipped   int      `json:"pages_skipped"`
	Placeholders   int      `json:"placeholders"` // empty pages made so that children have a parent
	ImagesAdded    int      `json:"images_added"`
	ImagesExisting int      `json:"images_existing"`
	Warnings       []string `json:"warnings,omitempty"`
}

func (r *Report) warn(format string, a ...any) {
	if len(r.Warnings) < 200 {
		r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...))
	}
}

// entryName cleans the name of a zip entry. ok is false for entries that must
// not be imported; reason says why when it is worth telling.
func entryName(raw string) (name string, ok bool, reason string) {
	raw = strings.ReplaceAll(raw, "\\", "/")
	if raw == "" || strings.HasSuffix(raw, "/") {
		return "", false, "" // a directory
	}
	if strings.ContainsRune(raw, 0) || strings.HasPrefix(raw, "/") || (len(raw) > 1 && raw[1] == ':') {
		return "", false, "unsafe path"
	}
	var parts []string
	for _, p := range strings.Split(raw, "/") {
		switch {
		case p == "" || p == ".":
		case p == "..":
			return "", false, "unsafe path"
		case strings.HasPrefix(p, ".") || p == "__MACOSX":
			return "", false, "" // hidden files, version-control folders, macOS resource forks
		default:
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "", false, ""
	}
	return strings.Join(parts, "/"), true, ""
}

func readEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, bad("cannot read %s: %v", f.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, bad("cannot read %s: %v", f.Name, err)
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}

func parseManifest(f *zip.File) (*Manifest, error) {
	data, err := readEntry(f, 8<<20)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, bad("wako.json is not valid JSON: %v", err)
	}
	if m.Format != Format {
		return nil, bad("wako.json is not a Wako export")
	}
	if m.Version > Version {
		return nil, bad("this export was made by a newer version of Wako (format %d)", m.Version)
	}
	return &m, nil
}

// ReadManifest returns the wako.json of an export, or nil if the zip has none.
func ReadManifest(zr *zip.Reader) (*Manifest, error) {
	for _, f := range zr.File {
		if name, ok, _ := entryName(f.Name); ok && name == ManifestName {
			return parseManifest(f)
		}
	}
	return nil, nil
}

// sizeSuffix is how a page asks for a picture at a width or height.
var sizeSuffix = regexp.MustCompile(`^\?[wh]=[0-9]{2,4}$`)

var segBad = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// slugFor turns a file or folder name into a page slug.
func slugFor(name string) string {
	s := strings.Trim(segBad.ReplaceAllString(name, "-"), "-._")
	if s == "" {
		s = "page"
	}
	if len(s) > 100 {
		s = strings.Trim(s[:100], "-._")
	}
	if !store.ValidSlug(s) { // ends in ".md" or in an image extension
		s += "-page"
	}
	return s
}

type page struct {
	file    string // name in the zip
	fileDir string // directory of the file, where its relative images are
	path    string // page path in the space, before "under"
	title   string // used when the text has no heading
	order   int
	body    string

	placeholder bool // made so that children have a parent
}

type imageFile struct {
	name string // path in the zip
	f    *zip.File
	sha  string // from the manifest, if any
}

func isMarkdown(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".md") || strings.HasSuffix(n, ".markdown")
}

func trimMarkdown(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".markdown"):
		return name[:len(name)-9]
	case strings.HasSuffix(n, ".md"):
		return name[:len(name)-3]
	}
	return name
}

func hashEntry(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Import reads an export, or any folder of Markdown files zipped up, into sp.
//
// Pages come from .md files; a folder's index.md or README.md is the page of
// the folder itself. File and folder names that are not valid page names are
// cleaned up (and a folder without a page of its own gets an empty one). Images
// are added to the media library, and the Markdown that refers to them by
// relative path ("images/shot.png") is changed to name them in the library.
// Hidden files, other file types, and anything with an unsafe path are skipped.
func Import(zr *zip.Reader, sp *store.Space, o Options) (*Report, error) {
	if o.MaxImageBytes <= 0 {
		o.MaxImageBytes = 25 << 20
	}
	if o.MaxPageBytes <= 0 {
		o.MaxPageBytes = 4 << 20
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = 1 << 30
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = 50000
	}
	if o.Limits.MaxPixels == 0 {
		o.Limits = img.DefaultLimits
	}
	if o.Conflict == "" {
		o.Conflict = "skip"
	}
	if o.Conflict != "skip" && o.Conflict != "overwrite" {
		return nil, bad("conflict must be skip or overwrite")
	}
	under := strings.Trim(o.Under, "/")
	if under != "" {
		if _, err := sp.GetPage(under); err != nil {
			return nil, bad("the page to import under (%s) does not exist", under)
		}
	}
	rep := &Report{}
	var err error

	// 1. Which entries can be imported at all.
	if len(zr.File) > o.MaxFiles {
		return nil, fmt.Errorf("%w: more than %d entries", ErrTooLarge, o.MaxFiles)
	}
	type entry struct {
		name string
		f    *zip.File
	}
	var entries []entry
	var total uint64
	seen := map[string]bool{}
	for _, f := range zr.File {
		name, ok, reason := entryName(f.Name)
		if !ok {
			if reason != "" {
				rep.warn("skipped %q: %s", f.Name, reason)
			}
			continue
		}
		if !f.Mode().IsRegular() {
			rep.warn("skipped %q: not a regular file", f.Name)
			continue
		}
		if seen[name] {
			rep.warn("skipped %q: the archive has it twice", f.Name)
			continue
		}
		seen[name] = true
		total += f.UncompressedSize64
		if total > uint64(o.MaxTotalBytes) {
			return nil, fmt.Errorf("%w: more than %d MiB when unpacked", ErrTooLarge, o.MaxTotalBytes>>20)
		}
		entries = append(entries, entry{name, f})
	}
	// A zip of a single folder ("notes/...") is that folder's contents.
	if len(entries) > 0 && !seen[ManifestName] {
		first, _, _ := strings.Cut(entries[0].name, "/")
		wrapped := true
		for _, e := range entries {
			if f, _, found := strings.Cut(e.name, "/"); !found || f != first {
				wrapped = false
				break
			}
		}
		if wrapped {
			for i := range entries {
				entries[i].name = strings.TrimPrefix(entries[i].name, first+"/")
			}
		}
	}

	// 2. The manifest gives the order of pages and checks the images.
	var manifest *Manifest
	for _, e := range entries {
		if e.name == ManifestName {
			if manifest, err = parseManifest(e.f); err != nil {
				return nil, err
			}
		}
	}
	order := map[string]int{}
	shas := map[string]string{}
	if manifest != nil {
		for i, p := range manifest.Pages {
			order[p.Path] = i
		}
		for _, im := range manifest.Images {
			shas[im.File] = im.SHA256
		}
	}

	// 3. Sort the entries into pages and images.
	var mdFiles []entry
	images := map[string]*imageFile{}
	var imageNames []string
	other := map[string]int{}
	for _, e := range entries {
		ext := strings.ToLower(path.Ext(e.name))
		switch {
		case e.name == ManifestName:
		case isMarkdown(e.name):
			if strings.HasPrefix(e.name, LibraryDir+"/") {
				rep.warn("skipped %q: pages do not belong in %s", e.name, LibraryDir)
				continue
			}
			if e.f.UncompressedSize64 > uint64(o.MaxPageBytes) {
				rep.warn("skipped %q: larger than %d MiB", e.name, o.MaxPageBytes>>20)
				continue
			}
			mdFiles = append(mdFiles, e)
		case store.IsAssetName(e.name):
			if e.f.UncompressedSize64 > uint64(o.MaxImageBytes) {
				rep.warn("skipped %q: larger than %d MiB", e.name, o.MaxImageBytes>>20)
				continue
			}
			images[e.name] = &imageFile{name: e.name, f: e.f, sha: shas[e.name]}
			imageNames = append(imageNames, e.name)
		default:
			other[ext]++
		}
	}
	var exts []string
	for ext := range other {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	for _, ext := range exts {
		if ext == "" {
			ext = "(no extension)"
		}
		rep.warn("skipped %d file(s) of type %s: only Markdown and images are imported", other[ext], ext)
	}
	// An export carries checksums; check them before changing anything.
	for _, name := range imageNames {
		im := images[name]
		if im.sha == "" {
			continue
		}
		got, err := hashEntry(im.f)
		if err != nil || got != im.sha {
			return nil, bad("%s does not match its checksum in %s: the archive is damaged", name, ManifestName)
		}
	}

	// 4. Page paths.
	pageFiles := map[string]bool{}
	for _, e := range mdFiles {
		pageFiles[strings.ToLower(trimMarkdown(e.name))] = true
	}
	var pages []*page
	taken := map[string]bool{}
	orig := map[string]string{} // page path -> the name it had in the archive
	sort.Slice(mdFiles, func(i, j int) bool { return strings.ToLower(mdFiles[i].name) < strings.ToLower(mdFiles[j].name) })
	for _, e := range mdFiles {
		body, err := readEntry(e.f, o.MaxPageBytes)
		if err != nil {
			return rep, err
		}
		if !utf8.Valid(body) {
			rep.warn("skipped %q: not valid UTF-8 text", e.name)
			continue
		}
		dir, base := path.Split(e.name)
		dir = strings.TrimSuffix(dir, "/")
		stem := trimMarkdown(base)
		pg := &page{file: e.name, fileDir: dir, body: strings.TrimPrefix(string(body), string(rune(0xFEFF))), title: stem}

		// docs/index.md and docs/README.md are the page of "docs", unless there is a docs.md.
		asFolder := dir != "" && (strings.EqualFold(stem, "index") || strings.EqualFold(stem, "readme")) && !pageFiles[strings.ToLower(dir)]
		var segs, names []string
		if dir != "" {
			names = strings.Split(dir, "/")
		}
		if !asFolder {
			names = append(names, stem)
		} else {
			pg.title = names[len(names)-1]
		}
		for _, n := range names {
			segs = append(segs, slugFor(n))
		}
		p := strings.Join(segs, "/")
		if taken[p] { // two names became the same slug
			for n := 2; ; n++ {
				if cand := fmt.Sprintf("%s-%d", p, n); !taken[cand] {
					p = cand
					break
				}
			}
		}
		taken[p] = true
		pg.path = p
		if i, ok := order[p]; ok && manifest != nil {
			pg.order = i
		} else {
			pg.order = 1 << 30 // not in a manifest: by name
		}
		for i := range segs {
			orig[strings.Join(segs[:i+1], "/")] = names[i]
		}
		pages = append(pages, pg)
	}

	// 5. Images go into the media library first, so pages can be bound to them.
	assetName := map[string]string{} // path in the zip -> library name
	sort.Strings(imageNames)
	lower := map[string]string{}
	for _, name := range imageNames {
		im := images[name]
		data, err := readEntry(im.f, o.MaxImageBytes)
		if err != nil {
			return rep, err
		}
		mime, ok := img.Sniff(data, name)
		if !ok {
			rep.warn("skipped %q: not a PNG, JPEG, GIF, WebP or SVG image, or a PDF", name)
			continue
		}
		res, err := img.Process(data, mime, o.Limits)
		if err != nil {
			rep.warn("skipped %q: %v", name, err)
			continue
		}
		a, created, err := sp.PutAsset(img.CleanName(path.Base(name), res.Mime), res.Mime, res.Data, res.Width, res.Height)
		if err != nil {
			return rep, err
		}
		if created {
			rep.ImagesAdded++
		} else {
			rep.ImagesExisting++
		}
		assetName[name] = a.Name
		lower[strings.ToLower(name)] = name
	}

	// 6. Pages whose parents are not in the archive get an empty parent.
	have := map[string]bool{}
	for _, pg := range pages {
		have[pg.path] = true
	}
	full := func(p string) string {
		if under == "" {
			return p
		}
		return under + "/" + p
	}
	var parents []string
	for _, pg := range pages {
		for p := path.Dir(pg.path); p != "." && p != ""; p = path.Dir(p) {
			if have[p] {
				continue
			}
			have[p] = true
			if _, err := sp.GetPage(full(p)); err == nil {
				continue // already in the space
			}
			parents = append(parents, p)
		}
	}
	for _, p := range parents {
		pages = append(pages, &page{path: p, title: orig[p], body: "# " + orig[p] + "\n", order: 1 << 30, placeholder: true})
	}
	// Parents first; siblings in the manifest's order, else by name.
	depth := func(p string) int { return strings.Count(p, "/") }
	sort.SliceStable(pages, func(i, j int) bool {
		a, b := pages[i], pages[j]
		if depth(a.path) != depth(b.path) {
			return depth(a.path) < depth(b.path)
		}
		if a.order != b.order {
			return a.order < b.order
		}
		if la, lb := strings.ToLower(a.path), strings.ToLower(b.path); la != lb {
			return la < lb
		}
		return a.path < b.path
	})

	// 7. The pages.
	for _, pg := range pages {
		target := full(pg.path)
		body := pg.body
		// Images named by relative path become names in the library.
		for _, d := range render.ImageDests(body) {
			ref := d
			if i := strings.IndexAny(ref, "?#"); i >= 0 {
				ref = ref[:i]
			}
			if dec, err := url.PathUnescape(ref); err == nil {
				ref = dec
			}
			file := path.Clean(path.Join(pg.fileDir, ref))
			if file == ".." || strings.HasPrefix(file, "../") {
				continue
			}
			if _, ok := assetName[file]; !ok {
				if alt, ok := lower[strings.ToLower(file)]; ok {
					file = alt
				}
			}
			if name, ok := assetName[file]; ok {
				// A size asked for with ?w=N or ?h=N stays on the library name.
				if i := strings.IndexByte(d, '?'); i >= 0 && sizeSuffix.MatchString(d[i:]) {
					name += d[i:]
				}
				if name == d {
					continue
				}
				if next, ok := render.RewriteImageDest(body, d, name); ok {
					body = next
				} else {
					rep.warn("%s: could not point %q at the library image %q", pg.file, d, name)
				}
			}
		}
		title := render.Title(body)
		if title == "" {
			title = pg.title
			if title == "" {
				title = path.Base(pg.path)
			}
		}
		_, err := sp.GetPage(target)
		exists := err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return rep, err
		}
		if exists && o.Conflict == "skip" {
			rep.PagesSkipped++
			continue
		}
		cond := store.Cond{By: o.By, Note: "imported", MustNotExist: !exists}
		refs := store.Refs{Images: render.ImageRefs(body), Links: render.Links(body)}
		_, created, err := sp.PutPage(target, title, body, refs, cond)
		if err != nil {
			return rep, fmt.Errorf("importing %s: %w", target, err)
		}
		switch {
		case pg.placeholder:
			rep.Placeholders++
		case created:
			rep.PagesCreated++
		default:
			rep.PagesUpdated++
		}
	}
	return rep, nil
}

// filesOf and entries2files give ReadManifest the cleaned names.
func filesOf(zr *zip.Reader, fs []*zip.File) []*zip.File { return fs }

func entries2files[T any](es []T, get func(T) (string, *zip.File)) []*zip.File {
	out := make([]*zip.File, 0, len(es))
	for _, e := range es {
		name, f := get(e)
		clone := *f
		clone.Name = name
		out = append(out, &clone)
	}
	return out
}
