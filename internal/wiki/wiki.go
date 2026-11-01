// Package wiki resolves the ordinary Markdown links between pages and rewrites
// them when pages move.
//
// It knows nothing about storage: callers hand it an Index of page paths per
// space.
package wiki

import (
	"net/url"
	"path"
	"strings"
)

// Page is one entry of a space, as the index sees it.
type Page struct{ Path, Title string }

// Index is a read-only snapshot of one space's pages.
type Index struct {
	titles map[string]string // path -> title
}

// NewIndex builds an index from a space's pages.
func NewIndex(pages []Page) *Index {
	ix := &Index{titles: make(map[string]string, len(pages))}
	for _, p := range pages {
		ix.titles[p.Path] = p.Title
	}
	return ix
}

// Has reports whether a page exists at path.
func (ix *Index) Has(p string) bool { _, ok := ix.titles[p]; return ok }

// Title returns the title of the page at path.
func (ix *Index) Title(p string) string { return ix.titles[p] }

// Ref names a page: the space it is in and its path there.
type Ref struct{ Space, Path string }

// Env is everything needed to resolve the links written in one space.
type Env struct {
	Space    string                   // the space the links are written in
	Base     string                   // URL prefix of the site, "" or "/wiki"
	HasSpace func(slug string) bool   // does a space with this slug exist?
	Index    func(slug string) *Index // nil if the space is unknown or may not be read
}

func (e *Env) index(space string) *Index {
	if e.Index == nil {
		return nil
	}
	return e.Index(space)
}

// cleanPage normalises a path taken from a Markdown link.
func cleanPage(p string) string {
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".md")
	if p = path.Clean(p); p == "." {
		return ""
	}
	return p
}

// Link resolves the destination of an ordinary Markdown link written on the page
// at src (a path in the environment's space). Destinations are resolved as a
// browser would resolve them from the page's URL, so "sibling" means the page
// next to src. internal is false for anything that is not a link to a page:
// external URLs, anchors, files and paths that leave the space.
func (e *Env) Link(src, dest string) (ref Ref, internal, exists bool) {
	if i := strings.IndexAny(dest, "#?"); i >= 0 {
		dest = dest[:i]
	}
	if dest == "" || externalRe(dest) {
		return Ref{}, false, false
	}
	u, err := url.PathUnescape(dest)
	if err != nil {
		return Ref{}, false, false
	}
	if strings.HasPrefix(u, "/") {
		if e.Base != "" {
			if u != e.Base && !strings.HasPrefix(u, e.Base+"/") {
				return Ref{}, false, false
			}
			u = strings.TrimPrefix(u, e.Base)
		}
		first, rest, _ := strings.Cut(strings.Trim(u, "/"), "/")
		if first == "" || strings.HasPrefix(first, "_") || e.HasSpace == nil || !e.HasSpace(first) {
			return Ref{}, false, false
		}
		ref = Ref{first, cleanPage(rest)}
	} else {
		dir := path.Dir(src)
		if dir == "." {
			dir = ""
		}
		joined := path.Join(dir, u)
		if joined == ".." || strings.HasPrefix(joined, "../") {
			return Ref{}, false, false // leaves the space
		}
		ref = Ref{e.Space, cleanPage(joined)}
	}
	if ref.Path == "" || strings.HasPrefix(ref.Path, "_") || strings.Contains(ref.Path, "/_") || strings.HasSuffix(strings.ToLower(ref.Path), ".pdf") {
		return Ref{}, false, false
	}
	ix := e.index(ref.Space)
	return ref, true, ix != nil && ix.Has(ref.Path)
}

func externalRe(d string) bool {
	if strings.HasPrefix(d, "//") {
		return true
	}
	for i, r := range d {
		switch {
		case r == ':':
			return i > 0
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
		case i > 0 && (r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return false
}
