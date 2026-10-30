package server

import (
	"encoding/json"
	"errors"
	"fmt"
	htmlpkg "html"
	"html/template"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hbroek/wako/internal/diff"
	"github.com/hbroek/wako/internal/store"
)

func fmtTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") + " UTC" }

type revJSON struct {
	Version int64  `json:"version"`
	Title   string `json:"title"`
	Author  string `json:"author,omitempty"`
	At      string `json:"at"`
	Action  string `json:"action"`
	Note    string `json:"note,omitempty"`
	Size    int    `json:"size"`
	Current bool   `json:"current"`
}

func revisionList(revs []*store.Revision) []revJSON {
	out := make([]revJSON, 0, len(revs))
	for i, r := range revs {
		out = append(out, revJSON{r.Version, r.Title, r.Author, r.At.UTC().Format(time.RFC3339), r.Action, r.Note, r.Size, i == 0})
	}
	return out
}

// revRow is a line of the history page.
type revRow struct {
	Version    int64
	Title      string
	Author, At string
	What       string
	Size       int
	Current    bool
	Prev       bool // has an earlier revision, so "changes" can be shown
	ViewURL    string
	ChangesURL string
}

// revBanner describes the old revision shown instead of the current page.
type revBanner struct {
	Version    int64
	Author, At string
	Action     string
	Note       string
	Current    bool
	PageURL    string
	ChangesURL string
}

func actionWords(r *store.Revision) string {
	switch r.Action {
	case "created":
		return "created the page"
	case "restored":
		return "restored an earlier revision"
	}
	return "edited"
}

func (s *Server) revisionOf(w http.ResponseWriter, sp *store.Space, page *store.Page, ver string) (*store.Revision, bool) {
	n, err := strconv.ParseInt(ver, 10, 64)
	if err != nil || n < 1 {
		http.Error(w, "revision must be a number", http.StatusBadRequest)
		return nil, false
	}
	rev, err := sp.Revision(page.ID, n)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "there is no such revision (saves in quick succession are merged, and old revisions are pruned)", http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return rev, true
}

// historyPage lists a page's revisions: GET /{space}/{path}?history.
func (s *Server) historyPage(w http.ResponseWriter, r *http.Request, sp *store.Space, acc Access, page *store.Page, rep repr) {
	revs, err := sp.Revisions(page.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if rep == reprJSON {
		writeJSON(w, http.StatusOK, revisionList(revs))
		return
	}
	if rep != reprHTML {
		http.Error(w, "not acceptable", http.StatusNotAcceptable)
		return
	}
	d := s.pageChrome(r, sp, acc, page, "History of "+page.Title)
	base := s.u(sp.Slug, page.Path)
	for i, rv := range revs {
		row := revRow{Version: rv.Version, Title: rv.Title, Author: rv.Author, At: fmtTime(rv.At), Size: rv.Size, Current: i == 0,
			What: actionWords(rv), Prev: i < len(revs)-1,
			ViewURL: base + "?rev=" + strconv.FormatInt(rv.Version, 10), ChangesURL: base + "?diff=" + strconv.FormatInt(rv.Version, 10)}
		if rv.Note != "" {
			row.What += ": " + rv.Note
		}
		d.Revs = append(d.Revs, row)
	}
	s.renderHTML(w, "history", d)
}

// pageChrome fills the fields every page view shares.
func (s *Server) pageChrome(r *http.Request, sp *store.Space, acc Access, page *store.Page, title string) pageData {
	d := s.newData(r, sp, acc, page.Path, title)
	d.Crumbs = s.crumbs(sp, page.Path)
	d.PageURL = s.u(sp.Slug, page.Path)
	d.Version = page.Version
	return d
}

// revisionPage shows an old revision: GET /{space}/{path}?rev=N.
func (s *Server) revisionPage(w http.ResponseWriter, r *http.Request, sp *store.Space, acc Access, page *store.Page, rep repr, ver string) {
	rev, ok := s.revisionOf(w, sp, page, ver)
	if !ok {
		return
	}
	// A revision never changes, so it can be validated by its version alone.
	etag := etagFor(rev.Version, rep)
	if rep != reprHTML {
		w.Header().Set("ETag", etag)
		if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatch(inm, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	switch rep {
	case reprMD:
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8; variant=GFM")
		io.WriteString(w, rev.Body)
	case reprJSON:
		out := revJSON{rev.Version, rev.Title, rev.Author, rev.At.UTC().Format(time.RFC3339), rev.Action, rev.Note, rev.Size, false}
		if cur, err := sp.Revisions(page.ID); err == nil && len(cur) > 0 {
			out.Current = cur[0].Version == rev.Version
		}
		writeJSON(w, http.StatusOK, struct {
			revJSON
			Path string `json:"path"`
			Body string `json:"body"`
		}{out, page.Path, rev.Body})
	case reprHTML:
		bound, err := sp.Bindings(page.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		html, err := s.renderPage(r, sp, page.Path, rev.Body, bound, true)
		if err != nil {
			s.fail(w, err)
			return
		}
		d := s.pageChrome(r, sp, acc, page, rev.Title)
		d.Body = tmplHTML(html)
		d.MDURL = d.PageURL + ".md"
		d.Children = nil
		cur, _ := sp.Revisions(page.ID)
		d.Rev = &revBanner{Version: rev.Version, Author: rev.Author, At: fmtTime(rev.At), Action: actionWords(rev), Note: rev.Note,
			Current: len(cur) > 0 && cur[0].Version == rev.Version, PageURL: d.PageURL,
			ChangesURL: d.PageURL + "?diff=" + strconv.FormatInt(rev.Version, 10)}
		s.renderHTML(w, "page", d)
	default:
		http.Error(w, "not acceptable", http.StatusNotAcceptable)
	}
}

// diffOf returns the revisions a diff compares: `to`, and `from` (the revision
// before `to` unless given). With no earlier revision, from is nil and the
// diff shows the whole text as added.
func (s *Server) diffOf(w http.ResponseWriter, sp *store.Space, page *store.Page, toVer, fromVer string) (from, to *store.Revision, ok bool) {
	if toVer == "" { // default: the current revision
		revs, err := sp.Revisions(page.ID)
		if err != nil || len(revs) == 0 {
			http.Error(w, "this page has no history", http.StatusNotFound)
			return nil, nil, false
		}
		toVer = strconv.FormatInt(revs[0].Version, 10)
	}
	if to, ok = s.revisionOf(w, sp, page, toVer); !ok {
		return nil, nil, false
	}
	if fromVer != "" {
		if from, ok = s.revisionOf(w, sp, page, fromVer); !ok {
			return nil, nil, false
		}
		return from, to, true
	}
	prev, err := sp.RevisionBefore(page.ID, to.Version)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return nil, nil, false
	}
	return prev, to, true
}

func revLabel(prefix string, r *store.Revision) string {
	if r == nil {
		return "/dev/null"
	}
	return fmt.Sprintf("%s (revision %d)", prefix, r.Version)
}

func bodyOf(r *store.Revision) string {
	if r == nil {
		return ""
	}
	return r.Body
}

// diffPage shows what a revision changed: GET /{space}/{path}?diff=N[&from=M].
func (s *Server) diffPage(w http.ResponseWriter, r *http.Request, sp *store.Space, acc Access, page *store.Page, rep repr) {
	q := r.URL.Query()
	from, to, ok := s.diffOf(w, sp, page, q.Get("diff"), q.Get("from"))
	if !ok {
		return
	}
	unified := diff.Unified(bodyOf(from), to.Body, revLabel(page.Path, from), revLabel(page.Path, to))
	switch rep {
	case reprDiff:
		w.Header().Set("Content-Type", "text/x-diff; charset=utf-8")
		io.WriteString(w, unified)
	case reprHTML:
		d := s.pageChrome(r, sp, acc, page, "Changes in "+page.Title)
		d.DiffHTML = diffHTML(diff.Hunks(bodyOf(from), to.Body, 3))
		d.Rev = &revBanner{Version: to.Version, Author: to.Author, At: fmtTime(to.At), Action: actionWords(to), Note: to.Note,
			PageURL: d.PageURL}
		if from != nil {
			d.DiffAgainst = from.Version
		}
		d.HistoryURL = d.PageURL + "?history"
		s.renderHTML(w, "diff", d)
	default:
		http.Error(w, "not acceptable", http.StatusNotAcceptable)
	}
}

// diffHTML renders hunks as an HTML table. All text is escaped.
func diffHTML(hunks []diff.Hunk) template.HTML {
	if len(hunks) == 0 {
		return template.HTML(`<p class="hint">No differences.</p>`)
	}
	var b strings.Builder
	b.WriteString(`<table class="diff">`)
	num := func(n int) string {
		if n == 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	for _, h := range hunks {
		fmt.Fprintf(&b, `<tr class="hunk"><td colspan="3">@@ -%d,%d +%d,%d @@</td></tr>`, h.OldStart, h.OldCount, h.NewStart, h.NewCount)
		for _, l := range h.Lines {
			class, mark := "ctx", " "
			switch l.Kind {
			case '+':
				class, mark = "add", "+"
			case '-':
				class, mark = "del", "−"
			}
			text := htmlpkg.EscapeString(l.Text)
			if l.NoNewline {
				text += `<span class="nonl"> ⏎̸ no newline at end of file</span>`
			}
			fmt.Fprintf(&b, `<tr class="%s"><td class="n">%s</td><td class="n">%s</td><td class="t">%s %s</td></tr>`, class, num(l.Old), num(l.New), mark, text)
		}
	}
	b.WriteString(`</table>`)
	return template.HTML(b.String())
}

// ---- API ----

// revisionsAPI answers GET /_api/spaces/{space}/pages/{path}/revisions.
func (s *Server) revisionsAPI(w http.ResponseWriter, r *http.Request, sp *store.Space, page *store.Page) {
	revs, err := sp.Revisions(page.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, revisionList(revs))
}

// diffAPI answers GET /_api/spaces/{space}/pages/{path}/diff?to=N&from=M with a unified diff.
func (s *Server) diffAPI(w http.ResponseWriter, r *http.Request, sp *store.Space, page *store.Page) {
	q := r.URL.Query()
	from, to, ok := s.diffOf(w, sp, page, q.Get("to"), q.Get("from"))
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/x-diff; charset=utf-8")
	io.WriteString(w, diff.Unified(bodyOf(from), to.Body, revLabel(page.Path, from), revLabel(page.Path, to)))
}

// diffTextsAPI answers POST /_api/spaces/{space}/diff {"from": text, "to": text}.
// The editor uses it to show what another person changed. The HTML is escaped.
func (s *Server) diffTextsAPI(w http.ResponseWriter, r *http.Request) {
	var in struct{ From, To string }
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*maxBody+4096)).Decode(&in); err != nil {
		http.Error(w, "bad or too large JSON", http.StatusBadRequest)
		return
	}
	hunks := diff.Hunks(in.From, in.To, 3)
	writeJSON(w, http.StatusOK, map[string]any{"changed": len(hunks) > 0, "html": string(diffHTML(hunks))})
}

// ---- restore and patch ----

// restorePage handles POST /{space}/{path}?restore=N: the page gets the text of
// revision N as a new revision. Nothing is lost; the history keeps growing.
func (s *Server) restorePage(w http.ResponseWriter, r *http.Request, sp *store.Space, path string) {
	if path == "" {
		http.Error(w, "choose a page to restore", http.StatusBadRequest)
		return
	}
	page, err := sp.GetPage(path)
	if err != nil {
		s.fail(w, err)
		return
	}
	rev, ok := s.revisionOf(w, sp, page, r.URL.Query().Get("restore"))
	if !ok {
		return
	}
	c := store.Cond{By: s.actor(r), Action: "restored", Note: fmt.Sprintf("restored revision %d", rev.Version)}
	if im := r.Header.Get("If-Match"); im != "" {
		v, ok := parseVersion(im)
		if !ok {
			s.fail(w, store.ErrVersion)
			return
		}
		c.IfMatch = &v
	}
	if rev.Body == page.Body && rev.Title == page.Title {
		w.Header().Set("ETag", etagFor(page.Version, reprMD)) // already the current text
		w.Header().Set("Location", s.u(sp.Slug, page.Path))
		w.WriteHeader(http.StatusOK)
		return
	}
	page, _, err = sp.PutPage(path, rev.Title, rev.Body, refsOf(rev.Body), c)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("ETag", etagFor(page.Version, reprMD))
	w.Header().Set("Location", s.u(sp.Slug, page.Path))
	w.WriteHeader(http.StatusOK)
}

// patchTypes are the media types PATCH accepts.
func patchType(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && (mt == "text/x-diff" || mt == "text/x-patch")
}

// patchPage handles PATCH /{space}/{path}: a unified diff applied to the version
// named in If-Match. A stale version is 412, a diff that does not fit is 409.
func (s *Server) patchPage(w http.ResponseWriter, r *http.Request, sp *store.Space, path string) {
	w.Header().Set("Accept-Patch", "text/x-diff")
	if path == "" {
		w.Header().Set("Allow", "GET, HEAD, POST, OPTIONS")
		http.Error(w, "the space root has no text to patch", http.StatusMethodNotAllowed)
		return
	}
	if !patchType(r) {
		http.Error(w, "Content-Type must be text/x-diff (a unified diff)", http.StatusUnsupportedMediaType)
		return
	}
	im := r.Header.Get("If-Match")
	if im == "" || strings.TrimSpace(im) == "*" {
		http.Error(w, `PATCH needs If-Match with the version the diff was made against, e.g. If-Match: "v3"`, http.StatusPreconditionRequired)
		return
	}
	v, ok := parseVersion(im)
	if !ok {
		s.fail(w, store.ErrVersion)
		return
	}
	page, err := sp.GetPage(path)
	if err != nil {
		s.fail(w, err)
		return
	}
	if page.Version != v {
		s.fail(w, store.ErrVersion)
		return
	}
	patch, ok := readBody(w, r)
	if !ok {
		return
	}
	body, err := diff.Apply(page.Body, patch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if body == page.Body {
		w.Header().Set("ETag", etagFor(page.Version, reprMD))
		w.WriteHeader(http.StatusOK) // the diff changes nothing
		return
	}
	page, _, err = sp.PutPage(path, titleFor(body, path), body, refsOf(body), store.Cond{IfMatch: &v, By: s.actor(r)})
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("ETag", etagFor(page.Version, reprMD))
	w.Header().Set("Location", s.u(sp.Slug, page.Path))
	w.WriteHeader(http.StatusOK)
}
