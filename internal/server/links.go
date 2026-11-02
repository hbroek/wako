package server

import (
	"net/http"
	"sort"
	"strings"

	"github.com/hbroek/wako/internal/render"
	"github.com/hbroek/wako/internal/store"
	"github.com/hbroek/wako/internal/wiki"
)

// refsOf extracts what a page's Markdown refers to.
func refsOf(body string) store.Refs {
	return store.Refs{Images: render.ImageRefs(body), Links: render.Links(body)}
}

// expandShortLinks turns [[Page name]] shortcuts into ordinary Markdown links
// as a page is saved, so what is stored never needs wako to be read. It also
// returns the pages the shortcuts point at that do not exist yet.
func expandShortLinks(sp *store.Space, path, body string) (string, []wiki.Stub) {
	if !strings.Contains(body, "[[") {
		return body, nil
	}
	ix, err := sp.PageIndex()
	if err != nil {
		return body, nil
	}
	return wiki.ExpandShortLinks(path, body, ix, store.ValidSlug)
}

// createStubs makes the pages named by shortcuts (and any missing parents) once
// the page holding them is saved. Each starts as just its title, so the link
// works at once. Failures are ignored: the link then offers to create the page.
func createStubs(sp *store.Space, stubs []wiki.Stub, by string) {
	for _, st := range stubs {
		parts := strings.Split(st.Path, "/")
		for i := range parts {
			p := strings.Join(parts[:i+1], "/")
			if _, err := sp.GetPage(p); err == nil {
				continue
			}
			title := parts[i]
			if i == len(parts)-1 {
				title = st.Title
			}
			body := "# " + title + "\n"
			if _, _, err := sp.PutPage(p, titleFor(body, p), body, refsOf(body), store.Cond{MustNotExist: true, By: by}); err != nil {
				break
			}
		}
	}
}

// linkCtx resolves links on behalf of one request. It caches the per-space
// indexes it loads, and only reveals spaces the caller may read.
type linkCtx struct {
	s   *Server
	r   *http.Request
	idx map[string]*wiki.Index
	has map[string]bool
	// override supplies a different index for one space (the state before a move).
	override map[string]*wiki.Index
	// alias makes extra slugs count as existing spaces (a space being renamed).
	alias map[string]bool
}

func (s *Server) newLinkCtx(r *http.Request) *linkCtx {
	return &linkCtx{s: s, r: r, idx: map[string]*wiki.Index{}, has: map[string]bool{}}
}

func (c *linkCtx) hasSpace(slug string) bool {
	if c.alias[slug] {
		return true
	}
	if v, ok := c.has[slug]; ok {
		return v
	}
	_, err := c.s.st.GetSpace(slug)
	c.has[slug] = err == nil
	return err == nil
}

func (c *linkCtx) index(slug string) *wiki.Index {
	if ix, ok := c.override[slug]; ok {
		return ix
	}
	if ix, ok := c.idx[slug]; ok {
		return ix
	}
	var ix *wiki.Index
	if sp, err := c.s.st.GetSpace(slug); err == nil && c.s.access(c.r, sp).Read {
		ix, _ = sp.PageIndex()
	}
	c.idx[slug] = ix
	return ix
}

// env is the environment for links written in the given space.
func (c *linkCtx) env(space string) *wiki.Env {
	return &wiki.Env{Space: space, Base: c.s.base, HasSpace: c.hasSpace, Index: c.index}
}

// linkCheck tells the renderer which links on the page at src lead nowhere.
func (c *linkCtx) linkCheck(env *wiki.Env, src string) render.LinkCheck {
	return func(dest string) (internal, exists bool) {
		_, internal, exists = env.Link(src, dest)
		return internal, exists
	}
}

// renderPage renders the Markdown of the page at src (empty if it is not saved
// yet) with images resolved and links to missing pages marked.
func (s *Server) renderPage(r *http.Request, sp *store.Space, src, body string, bound map[string]*store.AssetInfo, fallback bool) (string, error) {
	opts := render.Options{Images: s.resolver(sp, bound, fallback), External: s.externalResizer(sp)}
	if src != "" {
		c := s.newLinkCtx(r)
		opts.Links = c.linkCheck(c.env(sp.Slug), src)
	}
	return render.Render(body, opts)
}

// ---- backlinks and broken links ----

type linkHit struct {
	Space      string `json:"space"`
	SpaceTitle string `json:"space_title"`
	Path       string `json:"path"`
	Title      string `json:"title"`
	URL        string `json:"url"`
}

// backlinks lists the pages, in spaces the caller may read, that link to the
// page at path in target.
func (s *Server) backlinks(r *http.Request, target *store.Space, path string) ([]linkHit, error) {
	spaces, err := s.readableSpaces(r)
	if err != nil {
		return nil, err
	}
	c := s.newLinkCtx(r)
	out := []linkHit{}
	for _, sp := range spaces {
		rows, err := sp.LinkRows()
		if err != nil {
			return nil, err
		}
		env := c.env(sp.Slug)
		seen := map[string]bool{}
		for _, row := range rows {
			if seen[row.SrcPath] || (sp.Slug == target.Slug && row.SrcPath == path) {
				continue
			}
			ref, _, exists := env.Link(row.SrcPath, row.Target)
			if exists && ref.Space == target.Slug && ref.Path == path {
				seen[row.SrcPath] = true
				out = append(out, linkHit{sp.Slug, sp.Title, row.SrcPath, row.SrcTitle, s.u(sp.Slug, row.SrcPath)})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Space != out[j].Space {
			return out[i].Space < out[j].Space
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

type brokenLink struct {
	Path      string `json:"path"` // the page that has the link
	Title     string `json:"title"`
	URL       string `json:"url"`
	Target    string `json:"target"` // as written
	CreateURL string `json:"create_url,omitempty"`
}

// brokenLinks lists links on the space's pages that lead nowhere.
func (s *Server) brokenLinks(r *http.Request, sp *store.Space) ([]brokenLink, error) {
	rows, err := sp.LinkRows()
	if err != nil {
		return nil, err
	}
	env := s.newLinkCtx(r).env(sp.Slug)
	out := []brokenLink{}
	for _, row := range rows {
		ref, internal, exists := env.Link(row.SrcPath, row.Target)
		if !internal || exists {
			continue
		}
		b := brokenLink{Path: row.SrcPath, Title: row.SrcTitle, URL: s.u(sp.Slug, row.SrcPath), Target: row.Target}
		if env.Index(ref.Space) != nil {
			b.CreateURL = s.u(ref.Space, ref.Path) + "?new"
		}
		out = append(out, b)
	}
	return out, nil
}

// ---- keeping links working when pages move or spaces are renamed ----

// moveRewriter updates links for a page that moves inside sp. Pages that do not
// link into the moved subtree and are not part of it are left alone.
func (s *Server) moveRewriter(r *http.Request, sp *store.Space) store.LinkRewriter {
	return func(old *wiki.Index, p store.LinkPage, mapPath func(string) string) (string, bool) {
		c := s.newLinkCtx(r)
		c.override = map[string]*wiki.Index{sp.Slug: old}
		env := c.env(sp.Slug)
		relevant := p.Path != p.NewPath
		for _, l := range p.Links {
			if relevant {
				break
			}
			if ref, _, exists := env.Link(p.Path, l); exists && ref.Space == sp.Slug && mapPath(ref.Path) != ref.Path {
				relevant = true
			}
		}
		if !relevant {
			return p.Body, false
		}
		return env.Rewrite(p.Path, p.NewPath, p.Body, func(space, pth string) (string, string) {
			if space == sp.Slug {
				return space, mapPath(pth)
			}
			return space, pth
		})
	}
}

// renameRewriter updates links inside a space whose slug changes from
// oldSlug to newSlug: absolute links and "space:" prefixes naming the space.
func (s *Server) renameRewriter(r *http.Request, oldSlug, newSlug string) store.LinkRewriter {
	return func(old *wiki.Index, p store.LinkPage, _ func(string) string) (string, bool) {
		c := s.newLinkCtx(r)
		c.override = map[string]*wiki.Index{oldSlug: old}
		c.alias = map[string]bool{oldSlug: true}
		env := c.env(oldSlug)
		return env.Rewrite(p.Path, p.NewPath, p.Body, func(space, pth string) (string, string) {
			if space == oldSlug {
				return newSlug, pth
			}
			return space, pth
		})
	}
}

// ---- search ----

type hitJSON struct {
	Space       string  `json:"space"`
	SpaceTitle  string  `json:"space_title"`
	Path        string  `json:"path"`
	Title       string  `json:"title"`
	TitleHTML   string  `json:"title_html"`
	SnippetHTML string  `json:"snippet_html"`
	URL         string  `json:"url"`
	Score       float64 `json:"score"`
}

// search runs a query over the spaces the caller may read (or just one), best
// matches first.
func (s *Server) search(r *http.Request, q string, limit int, only string) ([]hitJSON, error) {
	spaces, err := s.readableSpaces(r)
	if err != nil {
		return nil, err
	}
	out := []hitJSON{}
	for _, sp := range spaces {
		if only != "" && sp.Slug != only {
			continue
		}
		hits, err := sp.Search(q, limit)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			out = append(out, hitJSON{sp.Slug, sp.Title, h.Path, h.Title, h.TitleHTML, h.SnippetHTML,
				s.u(sp.Slug, h.Path), h.Score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score < out[j].Score })
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
