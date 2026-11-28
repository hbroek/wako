// Package server implements wako's HTTP interface.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hbroek/wako/internal/ai"
	"github.com/hbroek/wako/internal/auth"
	"github.com/hbroek/wako/internal/img"
	"github.com/hbroek/wako/internal/render"
	"github.com/hbroek/wako/internal/store"
)

const maxBody = 4 << 20

type Server struct {
	st   *store.Store
	base string // URL prefix, "" or "/wiki"

	resizeState // the client for /_resize and its test switches

	SiteName string // shown in the header and page titles

	// ExternalResize is the server-wide switch for /_resize (pictures from other
	// sites); each space has its own switch as well.
	ExternalResize bool
	LogoFile       string // optional image file served as the logo and favicon

	MaxUpload   int64      // largest accepted upload in bytes; 0 means 25 MiB
	ImageLimits img.Limits // what uploaded pictures are checked and shrunk to

	work     chan struct{} // limits concurrent image decoding and scaling
	variants *variantCache
	auth     *authState // nil: sign-in is off and everyone is an administrator

	MaxImport int64 // largest import (a zip) in bytes; 0 means 256 MiB

	Logger         *slog.Logger   // nil: no logging
	AccessLog      bool           // log one line per request (needs Logger)
	TrustedProxies []netip.Prefix // whose X-Forwarded-* headers are believed; nil means loopback only
	AllowedHosts   []string       // names served when sign-in is off, besides local ones

	PublicURL string // scheme://host the site is reached at, for feed links; derived from the request if empty

	AI     ai.Provider   // the language model behind "ask" and "draft"; nil: off
	aiLim  *auth.Limiter // AI requests per user per hour
	aiWork chan struct{} // limits concurrent model calls
}

// New returns a server for st. Set the exported fields before serving.
func New(st *store.Store, basePath string) *Server {
	work := make(chan struct{}, 2)
	return &Server{st: st, base: basePath, SiteName: "Wako", ImageLimits: img.DefaultLimits, ExternalResize: true,
		work: work, variants: newVariantCache(64<<20, work)}
}

// Access is what the caller may do in a space.
type Access struct {
	Read, Write, Admin bool // Admin: manage the space's members and settings
	Suggest            bool // may propose changes for an editor to review (writers may too, but need not)
}

// newData fills the fields every HTML page shares: branding, the space
// switcher and the navigation tree.
func (s *Server) newData(r *http.Request, sp *store.Space, acc Access, cur, title string) pageData {
	d := pageData{
		Title: title, Base: s.base, SiteName: s.SiteName, HasLogo: s.LogoFile != "",
		AssetV: assetVersion, Space: sp, Path: cur, CanWrite: acc.Write, CanSuggest: acc.Suggest && !acc.Write, CanEdit: acc.Write || acc.Suggest, IsAdmin: s.isAdmin(r), CanManage: acc.Admin, AuthOn: s.auth != nil,
		AIOn: s.AI != nil && s.aiAllowed(r), AIAvailable: s.AI != nil,
		ResizeAvailable: s.ExternalResize, ResizeOn: s.ExternalResize && sp != nil && sp.ExternalResize,
		ImageMaxSide: s.ImageLimits.MaxSide, ImageOriginal: s.ImageLimits.Mode == img.Original,
	}
	if p := s.principal(r); p != nil {
		d.User = p.user.Name
	}
	d.Next = s.safeNext(r.URL.RequestURI())
	if sp == nil {
		d.FeedURL, d.FeedTitle = s.u("_feed"), s.SiteName+" — recent changes"
		return d
	}
	d.FeedURL, d.FeedTitle = s.u(sp.Slug, "_feed"), sp.Title+" — recent changes"
	d.SpaceList, _ = s.readableSpaces(r)
	nodes, err := sp.Tree()
	if err != nil {
		s.logger().Error("building the page tree", "err", err)
	}
	if acc.Write {
		d.Pending, _ = sp.CountPending()
	}
	d.Tree = treeCtx{Nodes: nodes, Cur: cur, Base: s.base, Slug: sp.Slug, CanWrite: acc.Write}
	return d
}

func (s *Server) u(parts ...string) string {
	return s.base + "/" + strings.Trim(strings.Join(parts, "/"), "/")
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if s.base != "" {
		if p != s.base && !strings.HasPrefix(p, s.base+"/") {
			http.NotFound(w, r)
			return
		}
		p = strings.TrimPrefix(p, s.base)
	}
	p = strings.TrimPrefix(p, "/")
	first, rest, _ := strings.Cut(p, "/")
	switch {
	case first == "":
		s.home(w, r)
	case first == "healthz":
		w.Write([]byte("ok\n"))
	case first == "_api":
		s.api(w, r, rest)
	case first == "_ui":
		s.ui(w, r, rest)
	case first == "favicon.ico" && rest == "":
		// Browsers that ignore the SVG icon (Safari) and tools that never read the page ask for this.
		if s.LogoFile != "" {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, s.LogoFile)
			return
		}
		s.ui(w, r, "apple-touch-icon.png")
	case first == "_resize" && rest == "":
		s.resize(w, r)
	case first == ".well-known" && rest == "bonbon.json":
		s.wellKnownBonbon(w, r)
	case first == searchPath && rest == "":
		s.siteSearch(w, r)
	case first == "_mcp" && rest == "":
		s.mcpEndpoint(w, r)
	case first == "_feed" && rest == "":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.feed(w, r, nil)
	case strings.HasPrefix(first, "_"):
		http.NotFound(w, r)
	default:
		if rest == "" && !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		s.space(w, r, first, rest)
	}
}

// ---- content negotiation ----

type repr int

const (
	reprNone repr = iota
	reprHTML
	reprMD
	reprJSON
	reprDiff
)

// negotiate picks a representation from an Accept header. Wildcards and an
// absent header select HTML.
func negotiate(accept string) repr {
	if strings.TrimSpace(accept) == "" {
		return reprHTML
	}
	type item struct {
		mt string
		q  float64
	}
	var items []item
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		q := 1.0
		if v, ok := params["q"]; ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				q = f
			}
		}
		if q > 0 {
			items = append(items, item{mt, q})
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].q > items[j].q })
	for _, it := range items {
		switch it.mt {
		case "text/markdown", "text/x-markdown":
			return reprMD
		case "application/json":
			return reprJSON
		case "text/x-diff", "text/x-patch":
			return reprDiff
		case "text/html", "application/xhtml+xml", "text/*", "*/*":
			return reprHTML
		}
	}
	return reprNone
}

func etagFor(version int64, r repr) string {
	switch r {
	case reprHTML:
		return fmt.Sprintf(`"v%d-html"`, version)
	case reprJSON:
		return fmt.Sprintf(`"v%d-json"`, version)
	}
	return fmt.Sprintf(`"v%d"`, version)
}

func etagMatch(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == "*" || t == etag {
			return true
		}
	}
	return false
}

// parseVersion reads an If-Match value of the form "v7" (Markdown ETag).
func parseVersion(h string) (int64, bool) {
	h = strings.Trim(strings.TrimPrefix(strings.TrimSpace(h), "W/"), `"`)
	if !strings.HasPrefix(h, "v") {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(h, "v"), 10, 64)
	return n, err == nil
}

// ---- helpers ----

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, store.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, store.ErrExists), errors.Is(err, store.ErrHasChildren), errors.Is(err, store.ErrParentMissing),
		errors.Is(err, store.ErrInUse):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, store.ErrVersion):
		http.Error(w, "precondition failed", http.StatusPreconditionFailed)
	case errors.Is(err, store.ErrTooManyProposals):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, store.ErrLastAdmin):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, store.ErrQuota):
		http.Error(w, "this space's media quota is full; delete unused images first", http.StatusInsufficientStorage)
	case errors.Is(err, img.ErrUnsupported):
		http.Error(w, err.Error(), http.StatusUnsupportedMediaType)
	case errors.Is(err, img.ErrCorrupt):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, img.ErrTooManyPixels), errors.Is(err, img.ErrTooManyFrames), errors.Is(err, img.ErrTooLarge):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	default:
		s.logger().Error("internal error", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func readBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return "", false
	}
	if !utf8.Valid(b) {
		http.Error(w, "body must be UTF-8", http.StatusBadRequest)
		return "", false
	}
	return string(b), true
}

// markdownType reports whether the request body may be treated as Markdown.
func markdownType(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return true
	}
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && (mt == "text/markdown" || mt == "text/x-markdown" || mt == "text/plain")
}

func titleFor(body, path string) string {
	if t := render.Title(body); t != "" {
		return t
	}
	return path[strings.LastIndex(path, "/")+1:]
}

// setCaps writes the headers that tell clients what they may do (see PLAN.md 2a).
// kind is "page", "root" or "readonly" resources.
func (s *Server) setCaps(w http.ResponseWriter, sp *store.Space, acc Access, kind, path string, container bool) {
	h := w.Header()
	h.Set("Vary", "Accept, Authorization, Cookie")
	allow := []string{"GET", "HEAD"}
	user := []string{"read"}
	if acc.Write && sp != nil {
		user = append(user, "write", "append")
		base := s.u(sp.Slug, path)
		if path == "" {
			base += "/"
		}
		if kind == "page" {
			allow = append(allow, "PUT", "PATCH", "POST", "DELETE")
			h.Set("Accept-Patch", "text/x-diff")
			h.Add("Link", fmt.Sprintf(`<%s.md>; rel="edit"`, base))
			h.Add("Link", fmt.Sprintf(`<%s?edit>; rel="edit-form"`, base))
		} else {
			allow = append(allow, "POST")
			user = []string{"read", "append"}
		}
		h.Add("Link", fmt.Sprintf(`<%s?create>; rel="create-form"`, base))
		h.Set("Accept-Post", "text/markdown, application/json")
		if container || kind == "root" {
			h.Add("Link", `<http://www.w3.org/ns/ldp#BasicContainer>; rel="type"`)
		}
	}
	if acc.Suggest && !acc.Write && sp != nil {
		// Writes from here become suggestions: 202 Accepted, with the review address.
		h.Set("Wako-Mode", "suggest")
		allow = append(allow, "PUT", "POST")
		if kind == "page" {
			allow = append(allow, "PATCH")
			h.Set("Accept-Patch", "text/x-diff")
		}
		h.Set("Accept-Post", "text/markdown, application/json")
	}
	allow = append(allow, "OPTIONS")
	h.Set("Allow", strings.Join(allow, ", "))
	pub := ""
	if sp != nil && sp.PublicRead {
		pub = "read"
	}
	h.Set("WAC-Allow", fmt.Sprintf(`user="%s", public="%s"`, strings.Join(user, " "), pub))
}

// ---- home and API ----

func (s *Server) readableSpaces(r *http.Request) ([]*store.Space, error) {
	all, err := s.st.ListSpaces()
	if err != nil {
		return nil, err
	}
	var out []*store.Space
	for _, sp := range all {
		if s.access(r, sp).Read {
			out = append(out, sp)
		}
	}
	return out, nil
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Allow", "GET, HEAD, OPTIONS")
	w.Header().Set("Vary", "Accept, Authorization, Cookie")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	spaces, err := s.readableSpaces(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	switch negotiate(r.Header.Get("Accept")) {
	case reprMD:
		var b strings.Builder
		b.WriteString("# Spaces\n\n")
		for _, sp := range spaces {
			fmt.Fprintf(&b, "- [%s](%s/)\n", sp.Title, s.u(sp.Slug))
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8; variant=GFM")
		io.WriteString(w, b.String())
	case reprJSON:
		writeJSON(w, 200, spacesJSON(spaces))
	case reprHTML:
		d := s.newData(r, nil, Access{}, "", s.SiteName)
		d.Spaces = spaces
		s.renderHTML(w, "home", d)
	default:
		http.Error(w, "not acceptable", http.StatusNotAcceptable)
	}
}

type spaceJSON struct {
	Slug           string `json:"slug"`
	Title          string `json:"title"`
	PublicRead     bool   `json:"public_read"`
	AIEnabled      bool   `json:"ai_enabled"`
	ExternalResize bool   `json:"external_resize"`
}

func spacesJSON(spaces []*store.Space) []spaceJSON {
	out := make([]spaceJSON, 0, len(spaces))
	for _, sp := range spaces {
		out = append(out, spaceJSON{sp.Slug, sp.Title, sp.PublicRead, sp.AIEnabled, sp.ExternalResize})
	}
	return out
}

func (s *Server) api(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if s.accountAPI(w, r, parts) {
		return
	}
	if parts[0] == "ai" {
		s.aiAPI(w, r, parts[1:])
		return
	}
	switch {
	case len(parts) == 1 && parts[0] == "spaces":
		switch r.Method {
		case http.MethodGet:
			spaces, err := s.readableSpaces(r)
			if err != nil {
				s.fail(w, err)
				return
			}
			writeJSON(w, 200, spacesJSON(spaces))
		case http.MethodPost:
			if !s.isAdmin(r) {
				s.deny(w, r)
				return
			}
			var in struct {
				Slug       string `json:"slug"`
				Title      string `json:"title"`
				PublicRead *bool  `json:"public_read"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
				http.Error(w, "bad JSON", http.StatusBadRequest)
				return
			}
			pub := in.PublicRead != nil && *in.PublicRead // a space is private until someone turns reading on
			sp, err := s.st.CreateSpace(in.Slug, in.Title, pub)
			if err != nil {
				s.fail(w, err)
				return
			}
			w.Header().Set("Location", s.u(sp.Slug)+"/")
			writeJSON(w, 201, spaceJSON{sp.Slug, sp.Title, sp.PublicRead, sp.AIEnabled, sp.ExternalResize})
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case len(parts) == 2 && parts[0] == "spaces" && r.Method == http.MethodPut:
		// Space settings belong to the space's administrators.
		cur, err := s.st.GetSpace(parts[1])
		if err != nil || !s.access(r, cur).Read {
			http.NotFound(w, r)
			return
		}
		if !s.access(r, cur).Admin {
			s.deny(w, r)
			return
		}
		var in struct {
			Slug           *string `json:"slug"`
			Title          *string `json:"title"`
			PublicRead     *bool   `json:"public_read"`
			AIEnabled      *bool   `json:"ai_enabled"`
			ExternalResize *bool   `json:"external_resize"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
			http.Error(w, "bad JSON", http.StatusBadRequest)
			return
		}
		// Switching the AI on sends the space's pages to the model's service: a decision for the server's administrator.
		if in.AIEnabled != nil && *in.AIEnabled != cur.AIEnabled && !s.isAdmin(r) {
			s.deny(w, r)
			return
		}
		sp, err := s.st.UpdateSpace(parts[1], store.SpaceUpdate{Slug: in.Slug, Title: in.Title, PublicRead: in.PublicRead, AIEnabled: in.AIEnabled, ExternalResize: in.ExternalResize})
		if err != nil {
			s.fail(w, err)
			return
		}
		if sp.Slug != parts[1] {
			// Absolute links and "space:" prefixes in the space's own pages name
			// the old slug. (Links from other spaces are not touched; they show
			// up in those spaces' broken-link reports.)
			if _, err := sp.RewriteLinks(s.renameRewriter(r, parts[1], sp.Slug), s.actor(r)); err != nil {
				s.logger().Error("rewriting links after renaming a space", "space", parts[1], "err", err)
			}
		}
		w.Header().Set("Location", s.u(sp.Slug)+"/")
		writeJSON(w, 200, spaceJSON{sp.Slug, sp.Title, sp.PublicRead, sp.AIEnabled, sp.ExternalResize})
	case len(parts) == 2 && parts[0] == "spaces" && r.Method == http.MethodDelete:
		if cur, err := s.st.GetSpace(parts[1]); err != nil || !s.access(r, cur).Read {
			http.NotFound(w, r)
			return
		}
		if !s.isAdmin(r) {
			s.deny(w, r)
			return
		}
		if err := s.st.DeleteSpace(parts[1]); err != nil {
			s.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 3 && parts[0] == "spaces" && parts[2] == "tree" && r.Method == http.MethodGet:
		sp, err := s.st.GetSpace(parts[1])
		if err != nil || !s.access(r, sp).Read {
			http.NotFound(w, r)
			return
		}
		tree, err := sp.Tree()
		if err != nil {
			s.fail(w, err)
			return
		}
		if tree == nil {
			tree = []*store.Node{}
		}
		writeJSON(w, 200, tree)
	case len(parts) == 1 && parts[0] == "search" && r.Method == http.MethodGet:
		s.searchAPI(w, r, "")
	case len(parts) == 3 && parts[0] == "spaces" && parts[2] == "search" && r.Method == http.MethodGet:
		s.searchAPI(w, r, parts[1])
	case len(parts) == 4 && parts[0] == "spaces" && parts[2] == "links" && parts[3] == "broken" && r.Method == http.MethodGet:
		sp, err := s.st.GetSpace(parts[1])
		if err != nil || !s.access(r, sp).Read {
			http.NotFound(w, r)
			return
		}
		list, err := s.brokenLinks(r, sp)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 200, list)
	case len(parts) == 3 && parts[0] == "spaces" && parts[2] == "export":
		s.exportSpace(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "spaces" && parts[2] == "import":
		s.importSpace(w, r, parts[1])
	case len(parts) == 1 && parts[0] == "changes" && r.Method == http.MethodGet:
		s.changesAPI(w, r, "")
	case len(parts) == 3 && parts[0] == "spaces" && parts[2] == "changes" && r.Method == http.MethodGet:
		if sp, err := s.st.GetSpace(parts[1]); err != nil || !s.access(r, sp).Read {
			http.NotFound(w, r)
			return
		}
		s.changesAPI(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "spaces" && parts[2] == "diff" && r.Method == http.MethodPost:
		if sp, err := s.st.GetSpace(parts[1]); err != nil || !s.access(r, sp).Read {
			http.NotFound(w, r)
			return
		}
		s.diffTextsAPI(w, r)
	case len(parts) >= 5 && parts[0] == "spaces" && parts[2] == "pages" && r.Method == http.MethodGet &&
		(parts[len(parts)-1] == "revisions" || parts[len(parts)-1] == "diff"):
		sp, err := s.st.GetSpace(parts[1])
		if err != nil || !s.access(r, sp).Read {
			http.NotFound(w, r)
			return
		}
		page, err := sp.GetPage(strings.Join(parts[3:len(parts)-1], "/"))
		if err != nil {
			s.fail(w, err)
			return
		}
		if parts[len(parts)-1] == "revisions" {
			s.revisionsAPI(w, r, sp, page)
		} else {
			s.diffAPI(w, r, sp, page)
		}
	case len(parts) >= 5 && parts[0] == "spaces" && parts[2] == "pages" && parts[len(parts)-1] == "backlinks" && r.Method == http.MethodGet:
		sp, err := s.st.GetSpace(parts[1])
		if err != nil || !s.access(r, sp).Read {
			http.NotFound(w, r)
			return
		}
		pagePath := strings.Join(parts[3:len(parts)-1], "/")
		if _, err := sp.GetPage(pagePath); err != nil {
			s.fail(w, err)
			return
		}
		list, err := s.backlinks(r, sp, pagePath)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 200, list)
	case len(parts) == 3 && parts[0] == "spaces" && parts[2] == "render" && r.Method == http.MethodPost:
		sp, err := s.st.GetSpace(parts[1])
		if err != nil || !s.access(r, sp).Read {
			http.NotFound(w, r)
			return
		}
		src, ok := readBody(w, r)
		if !ok {
			return
		}
		var bound map[string]*store.AssetInfo
		pagePath := ""
		if pg, err := sp.GetPage(r.URL.Query().Get("page")); err == nil {
			bound, _ = sp.Bindings(pg.ID)
			pagePath = pg.Path // relative links in the preview resolve from here
		}
		html, err := s.renderPage(r, sp, pagePath, src, bound, true)
		if err != nil {
			s.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, html)
	default:
		http.NotFound(w, r)
	}
}

// searchAPI answers GET /_api/search?q= and /_api/spaces/{space}/search?q=.
func (s *Server) searchAPI(w http.ResponseWriter, r *http.Request, space string) {
	if space == "" {
		space = r.URL.Query().Get("space")
	}
	if space != "" {
		if sp, err := s.st.GetSpace(space); err != nil || !s.access(r, sp).Read {
			http.NotFound(w, r)
			return
		}
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	hits, err := s.search(r, r.URL.Query().Get("q"), limit, space)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, hits)
}
