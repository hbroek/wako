// Package archive exports a space as a zip file and imports one back.
//
// The zip holds the pages as the .md files they are, laid out as the page tree
// (page "docs/ops" is docs/ops.md, and its children live in docs/ops/). Each
// image sits next to the pages that use it, so the Markdown works unchanged in
// any viewer. A small wako.json says the order of pages and describes the space.
package archive

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/hbroek/wako/internal/render"
	"github.com/hbroek/wako/internal/store"
)

const (
	ManifestName = "wako.json"
	LibraryDir   = "_library" // images no page uses
	Format       = "wako-export"
	Version      = 1
)

// Manifest is the wako.json of an export.
type Manifest struct {
	Format   string          `json:"format"`
	Version  int             `json:"version"`
	Exported string          `json:"exported"`
	Space    ManifestSpace   `json:"space"`
	Pages    []ManifestPage  `json:"pages"`  // in tree order: parents first, siblings in their order
	Images   []ManifestImage `json:"images"` // every image file in the zip
	Notes    []string        `json:"notes,omitempty"`
}

type ManifestSpace struct {
	Slug       string `json:"slug"`
	Title      string `json:"title"`
	PublicRead bool   `json:"public_read"`
}

type ManifestPage struct {
	Path  string `json:"path"`
	Title string `json:"title"`
}

type ManifestImage struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Used   bool   `json:"used"` // by at least one page
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// flatten lists the tree depth first, in sibling order.
func flatten(nodes []*store.Node, out *[]*store.Node) {
	for _, n := range nodes {
		*out = append(*out, n)
		flatten(n.Children, out)
	}
}

// Export writes the space to w as a zip. Page texts are written exactly as stored.
func Export(w io.Writer, sp *store.Space, now time.Time) error {
	tree, err := sp.Tree()
	if err != nil {
		return err
	}
	var pages []*store.Node
	flatten(tree, &pages)
	uses, err := sp.PageImages()
	if err != nil {
		return err
	}
	library, err := sp.AllAssets()
	if err != nil {
		return err
	}
	byID := map[string]*store.AssetInfo{}
	for _, a := range library {
		byID[a.ID] = a
	}

	// Decide where every image goes: next to each page that uses it.
	files := map[string]string{}              // file in the zip -> asset id
	renames := map[string]map[string]string{} // page -> name it used -> name written
	used := map[string]bool{}                 // asset id -> a page uses it
	var notes []string
	for _, u := range uses {
		if byID[u.AssetID] == nil {
			continue
		}
		used[u.AssetID] = true
		dir, name := dirOf(u.PagePath), u.Name
		if cur, ok := files[join(dir, name)]; ok && cur != u.AssetID {
			// Two pages in one directory use the same name for different images
			// (possible only through an alias). The second keeps its own picture
			// under a new name, and its text is adjusted.
			stem, ext := name, ""
			if i := strings.LastIndex(name, "."); i > 0 {
				stem, ext = name[:i], name[i:]
			}
			for n := 2; ; n++ {
				cand := fmt.Sprintf("%s-%d%s", stem, n, ext)
				if other, taken := files[join(dir, cand)]; !taken || other == u.AssetID {
					name = cand
					break
				}
			}
			if renames[u.PagePath] == nil {
				renames[u.PagePath] = map[string]string{}
			}
			renames[u.PagePath][u.Name] = name
			notes = append(notes, fmt.Sprintf("page %s: image %q was written as %q because a sibling page uses that name for another picture", u.PagePath, u.Name, name))
		}
		files[join(dir, name)] = u.AssetID
	}
	for _, a := range library {
		if !used[a.ID] {
			files[join(LibraryDir, a.Name)] = a.ID
		}
	}
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)

	m := Manifest{Format: Format, Version: Version, Exported: now.UTC().Format(time.RFC3339), Notes: notes,
		Space: ManifestSpace{Slug: sp.Slug, Title: sp.Title, PublicRead: sp.PublicRead}}
	for _, p := range pages {
		m.Pages = append(m.Pages, ManifestPage{p.Path, p.Title})
	}
	for _, f := range names {
		a := byID[files[f]]
		m.Images = append(m.Images, ManifestImage{File: f, SHA256: a.ID, Size: a.Size, Used: !strings.HasPrefix(f, LibraryDir+"/")})
	}

	zw := zip.NewWriter(w)
	add := func(name string, mod time.Time, method uint16, data []byte) error {
		fh := &zip.FileHeader{Name: name, Method: method, Modified: mod}
		fh.SetMode(0o644)
		out, err := zw.CreateHeader(fh)
		if err != nil {
			return err
		}
		_, err = out.Write(data)
		return err
	}
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := add(ManifestName, now, zip.Deflate, append(manifest, '\n')); err != nil {
		return err
	}
	for _, n := range pages {
		p, err := sp.GetPage(n.Path)
		if err != nil {
			return err
		}
		body := p.Body
		for old, renamed := range renames[n.Path] {
			if next, ok := render.RewriteImageRef(body, old, renamed); ok {
				body = next
			}
		}
		if err := add(n.Path+".md", p.UpdatedAt, zip.Deflate, []byte(body)); err != nil {
			return err
		}
	}
	for _, f := range names {
		a, err := sp.GetAsset(files[f])
		if err != nil {
			return err
		}
		// Images are compressed already.
		if err := add(f, a.CreatedAt, zip.Store, a.Data); err != nil {
			return err
		}
	}
	return zw.Close()
}
