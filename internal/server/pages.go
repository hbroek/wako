package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/hbroek/wako/internal/img"
	"github.com/hbroek/wako/internal/render"
	"github.com/hbroek/wako/internal/store"
)

// cutMD strips a ".md" suffix, which forces the Markdown representation.
func cutMD(p string) (string, bool) {
	if strings.HasSuffix(p, ".md") {
		return strings.TrimSuffix(p, ".md"), true
	}
	return p, false
}

func (s *Server) space(w http.ResponseWriter, r *http.Request, slug, rest string) {
	feed := strings.Trim(rest, "/") == "_feed"
	anonymous := s.auth != nil && s.principal(r) == nil
	sp, err := s.st.GetSpace(slug)
	if err != nil {
		if feed && anonymous {
			s.challengeFeed(w) // the same answer whether or not the space exists
			return
		}
		s.fail(w, err)
		return
	}
	acc := s.access(r, sp)
	if !acc.Read {
		if feed && anonymous {
			s.challengeFeed(w)
			return
		}
		http.NotFound(w, r) // do not reveal private spaces
		return
	}
	if feed {
		s.spaceFeed(w, r, sp)
		return
	}
	if first, tail, _ := strings.Cut(strings.Trim(rest, "/"), "/"); first == "_assets" {
		s.assets(w, r, sp, acc, tail)
		return
	}
	if first, tail, _ := strings.Cut(strings.Trim(rest, "/"), "/"); first == "_proposals" {
		s.proposalsRoute(w, r, sp, acc, tail)
		return
	}
	path, forceMD := cutMD(strings.Trim(rest, "/"))
	if strings.HasPrefix(path, "_") || strings.Contains(path, "/_") {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.getPage(w, r, sp, acc, path, forceMD)
	case http.MethodOptions:
		// The same answer as HEAD would give: a page is a container only if it has children.
		container, missing := path == "", false
		if path != "" {
			if pg, err := sp.GetPage(path); err == nil {
				container = pg.HasChildren
			} else {
				missing = true
			}
		}
		s.setCaps(w, sp, acc, kindOf(path), path, container)
		if missing && acc.Write { // a page that is not there yet can be created, nothing more
			w.Header().Set("Allow", "GET, HEAD, PUT, OPTIONS")
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPut, http.MethodPatch, http.MethodPost, http.MethodDelete:
		if !acc.Write {
			if acc.Suggest && suggestable(r) {
				s.suggest(w, r, sp, path)
				return
			}
			s.setCaps(w, sp, acc, kindOf(path), path, true)
			s.deny(w, r)
			return
		}
		switch r.Method {
		case http.MethodPut:
			s.putPage(w, r, sp, path)
		case http.MethodPatch:
			s.patchPage(w, r, sp, path)
		case http.MethodPost:
			s.postPage(w, r, sp, path)
		default:
			s.deletePage(w, r, sp, path)
		}
	default:
		s.setCaps(w, sp, acc, kindOf(path), path, true)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func kindOf(path string) string {
	if path == "" {
		return "root"
	}
	return "page"
}

type pageJSON struct {
	Path      string `json:"path"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Version   int64  `json:"version"`
	UpdatedAt string `json:"updated_at"`
	Children  []link `json:"children"`
}

type link struct {
	Title string `json:"title"`
	Path  string `json:"path"`
	URL   string `json:"-"`
}

func (s *Server) getPage(w http.ResponseWriter, r *http.Request, sp *store.Space, acc Access, path string, forceMD bool) {
	rep := negotiate(r.Header.Get("Accept"))
	if forceMD {
		rep = reprMD
	}

	var page *store.Page
	if path != "" {
		var err error
		page, err = sp.GetPage(path)
		if errors.Is(err, store.ErrInvalid) {
			err = store.ErrNotFound
		}
		if err != nil {
			if errors.Is(err, store.ErrNotFound) && store.IsImageName(path) {
				// An image used by a page in this directory is served here, next to
				// the page, exactly where a relative ![alt](name) in its Markdown
				// points, although only one copy is stored.
				dir, name := parentPath(path), path[strings.LastIndex(path, "/")+1:]
				if a, ierr := sp.SiblingImage(dir, name); ierr == nil {
					s.serveAsset(w, r, sp, a.ID, false)
					return
				}
			}
			if errors.Is(err, store.ErrNotFound) {
				if acc.Write {
					s.setCaps(w, sp, acc, "page", path, false)
					w.Header().Set("Allow", "GET, HEAD, PUT, OPTIONS")
				}
				if rep == reprHTML {
					s.missingPage(w, r, sp, acc, path)
					return
				}
			}
			s.fail(w, err)
			return
		}
	}
	kids, err := sp.Children(path)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.setCaps(w, sp, acc, kindOf(path), path, len(kids) > 0)
	q := r.URL.Query()
	if rep == reprNone || (rep == reprDiff && !(q.Has("diff") && page != nil)) {
		http.Error(w, "not acceptable", http.StatusNotAcceptable)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	if page != nil {
		switch {
		case q.Has("history"):
			s.historyPage(w, r, sp, acc, page, rep)
			return
		case q.Has("rev"):
			s.revisionPage(w, r, sp, acc, page, rep, q.Get("rev"))
			return
		case q.Has("diff"):
			s.diffPage(w, r, sp, acc, page, rep)
			return
		}
	}

	var links []link
	for _, k := range kids {
		links = append(links, link{Title: k.Title, Path: k.Path, URL: s.u(sp.Slug, k.Path)})
	}

	// The space root is generated, so it has no ETag.
	// HTML embeds the navigation tree, which changes with other pages, so only
	// the page's own representations (Markdown, JSON) are validated by ETag.
	if page != nil && rep != reprHTML {
		etag := etagFor(page.Version, rep)
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", page.UpdatedAt.UTC().Format(http.TimeFormat))
		if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatch(inm, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	switch rep {
	case reprMD:
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8; variant=GFM")
		if page != nil {
			io.WriteString(w, page.Body)
			return
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# %s\n\n", sp.Title)
		for _, l := range links {
			fmt.Fprintf(&b, "- [%s](%s)\n", l.Title, l.URL)
		}
		io.WriteString(w, b.String())
	case reprJSON:
		out := pageJSON{Path: path, Title: sp.Title, Children: links}
		if out.Children == nil {
			out.Children = []link{}
		}
		if page != nil {
			out.Title, out.Body, out.Version = page.Title, page.Body, page.Version
			out.UpdatedAt = page.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		writeJSON(w, 200, out)
	case reprHTML:
		s.htmlPage(w, r, sp, acc, path, page, links)
	}
}

func (s *Server) htmlPage(w http.ResponseWriter, r *http.Request, sp *store.Space, acc Access, path string, page *store.Page, kids []link) {
	title := sp.Title
	if page != nil {
		title = page.Title
	}
	d := s.newData(r, sp, acc, path, title)
	d.Children = kids
	d.Crumbs = s.crumbs(sp, path)
	d.PageURL = s.u(sp.Slug, path)
	d.ParentURL = s.u(sp.Slug, parentPath(path)) + "/"
	if path == "" {
		d.PageURL += "/"
	}
	if page != nil {
		d.Version, d.HasKids = page.Version, page.HasChildren
		d.MDURL = d.PageURL + ".md"
	}

	q := r.URL.Query()
	switch {
	case (acc.Write || acc.Suggest) && q.Has("edit") && page != nil:
		d.Mode, d.Source = "edit", page.Body
		s.renderHTML(w, "editor", d)
	case (acc.Write || acc.Suggest) && q.Has("create"):
		d.Mode = "create"
		s.renderHTML(w, "editor", d)
	case path == "" && q.Has("changes"):
		list, err := s.recentChanges(r, sp.Slug, 100)
		if err != nil {
			s.fail(w, err)
			return
		}
		d.Changes = s.changeRows(list)
		d.Title = "Recent changes"
		s.renderHTML(w, "changes", d)
	case path == "" && q.Has("broken"):
		list, err := s.brokenLinks(r, sp)
		if err != nil {
			s.fail(w, err)
			return
		}
		d.Broken = list
		d.Title = "Broken links"
		s.renderHTML(w, "broken", d)
	case acc.Admin && path == "" && q.Has("settings"):
		s.renderHTML(w, "settings", d)
	default:
		if page != nil {
			bound, err := sp.Bindings(page.ID)
			if err != nil {
				s.fail(w, err)
				return
			}
			html, err := s.renderPage(r, sp, page.Path, page.Body, bound, false)
			if err != nil {
				s.fail(w, err)
				return
			}
			d.Body = tmplHTML(html)
		}
		s.renderHTML(w, "page", d)
	}
}

func parentPath(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

func (s *Server) crumbs(sp *store.Space, path string) []link {
	out := []link{{Title: sp.Title, URL: s.u(sp.Slug) + "/"}}
	acc := ""
	for _, seg := range strings.Split(path, "/") {
		if seg == "" {
			continue
		}
		acc += "/" + seg
		out = append(out, link{Title: seg, URL: s.u(sp.Slug, acc)})
	}
	return out
}

func (s *Server) putPage(w http.ResponseWriter, r *http.Request, sp *store.Space, path string) {
	if path == "" {
		w.Header().Set("Allow", "GET, HEAD, POST, OPTIONS")
		http.Error(w, "cannot PUT the space root", http.StatusMethodNotAllowed)
		return
	}
	if !markdownType(r) {
		http.Error(w, "Content-Type must be text/markdown", http.StatusUnsupportedMediaType)
		return
	}
	var c store.Cond
	if im := r.Header.Get("If-Match"); im != "" {
		if strings.TrimSpace(im) == "*" {
			if _, err := sp.GetPage(path); err != nil {
				s.fail(w, store.ErrVersion)
				return
			}
		} else {
			v, ok := parseVersion(im)
			if !ok {
				s.fail(w, store.ErrVersion)
				return
			}
			c.IfMatch = &v
		}
	}
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == "*" {
		c.MustNotExist = true
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	page, created, err := sp.PutPage(path, titleFor(body, path), body, refsOf(body), s.cond(r, c))
	if errors.Is(err, store.ErrExists) {
		err = store.ErrVersion // If-None-Match: * failed
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("ETag", etagFor(page.Version, reprMD))
	w.Header().Set("Location", s.u(sp.Slug, page.Path))
	if created {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusOK)
	}
}

func (s *Server) postPage(w http.ResponseWriter, r *http.Request, sp *store.Space, path string) {
	q := r.URL.Query()
	if q.Has("reorder") {
		var in struct {
			Order []string `json:"order"`
		}
		raw, ok := readBody(w, r)
		if !ok {
			return
		}
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			http.Error(w, "bad JSON: want {\"order\":[slugs]}", http.StatusBadRequest)
			return
		}
		if err := sp.Reorder(path, in.Order); err != nil {
			s.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if q.Has("restore") {
		s.restorePage(w, r, sp, path)
		return
	}
	if q.Has("move") {
		if path == "" {
			http.Error(w, "cannot move the space root", http.StatusBadRequest)
			return
		}
		to := strings.Trim(q.Get("move"), "/")
		to, _ = cutMD(to)
		page, _, err := sp.MovePageRewriting(path, to, s.moveRewriter(r, sp), s.actor(r))
		if err != nil {
			s.fail(w, err)
			return
		}
		w.Header().Set("Location", s.u(sp.Slug, page.Path))
		w.Header().Set("ETag", etagFor(page.Version, reprMD))
		w.WriteHeader(http.StatusOK)
		return
	}

	if path != "" {
		if _, err := sp.GetPage(path); err != nil {
			s.fail(w, err)
			return
		}
	}
	var slug, title, body string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var in struct{ Slug, Title, Body string }
		raw, ok := readBody(w, r)
		if !ok {
			return
		}
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			http.Error(w, "bad JSON", http.StatusBadRequest)
			return
		}
		slug, title, body = in.Slug, in.Title, in.Body
	} else {
		if !markdownType(r) {
			http.Error(w, "Content-Type must be text/markdown or application/json", http.StatusUnsupportedMediaType)
			return
		}
		var ok bool
		if body, ok = readBody(w, r); !ok {
			return
		}
		slug = r.Header.Get("Slug")
	}
	if !store.ValidSlug(slug) {
		http.Error(w, "a valid slug is required", http.StatusBadRequest)
		return
	}
	if title == "" {
		title = titleFor(body, slug)
	}
	page, _, err := sp.PutPage(strings.Trim(path+"/"+slug, "/"), title, body, refsOf(body), store.Cond{MustNotExist: true, By: s.actor(r)})
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("ETag", etagFor(page.Version, reprMD))
	w.Header().Set("Location", s.u(sp.Slug, page.Path))
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) deletePage(w http.ResponseWriter, r *http.Request, sp *store.Space, path string) {
	if path == "" {
		w.Header().Set("Allow", "GET, HEAD, POST, OPTIONS")
		http.Error(w, "cannot delete the space root", http.StatusMethodNotAllowed)
		return
	}
	var ifMatch *int64
	if im := r.Header.Get("If-Match"); im != "" && strings.TrimSpace(im) != "*" {
		v, ok := parseVersion(im)
		if !ok {
			s.fail(w, store.ErrVersion)
			return
		}
		ifMatch = &v
	}
	if err := sp.DeletePage(path, r.URL.Query().Get("recursive") == "true", ifMatch, s.actor(r)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// missingPage answers a browser asking for a page that does not exist yet.
func (s *Server) missingPage(w http.ResponseWriter, r *http.Request, sp *store.Space, acc Access, path string) {
	d := s.newData(r, sp, acc, path, "Page not found")
	d.PageURL = s.u(sp.Slug, path)
	d.Crumbs = s.crumbs(sp, path)
	if (acc.Write || acc.Suggest) && r.URL.Query().Has("new") {
		d.Mode = "new"
		s.renderHTML(w, "editor", d)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	s.renderHTML(w, "missing", d)
}

// resolver turns the bare image names in a page into library URLs. Saved pages
// use only their own bindings; with fallback (live preview of unsaved text) a
// name the page has not bound yet is looked up in the library.
func (s *Server) resolver(sp *store.Space, bound map[string]*store.AssetInfo, fallback bool) render.Resolver {
	return func(name string) (render.Image, bool) {
		a, ok := bound[name]
		if !ok && fallback {
			if found, err := sp.FindAssetByName(name); err == nil {
				a, ok = found, true
			}
		}
		if !ok {
			return render.Image{}, false
		}
		return s.pageImage(sp, a), true
	}
}

// pageImage chooses what a page shows for an asset: the original, or, for a
// large JPEG or PNG, the 800 px copy linked to the original. Width and height
// are those of what is shown, so the browser reserves the right space.
func (s *Server) pageImage(sp *store.Space, a *store.AssetInfo) render.Image {
	full := s.assetURL(sp, a.ID, a.Name)
	if scalable(a, widthContent) {
		w, h := img.Fitted(a.Width, a.Height, widthContent)
		return render.Image{Src: full + "?w=" + strconv.Itoa(widthContent), Link: full, Width: w, Height: h}
	}
	return render.Image{Src: full, Width: a.Width, Height: a.Height}
}
