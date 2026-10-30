package server

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/hbroek/wako/internal/store"
)

//go:embed ui/*.html ui/app.css ui/app.js ui/theme.js ui/icon.svg ui/apple-touch-icon.png
var uiFS embed.FS

type treeCtx struct {
	Nodes    []*store.Node
	Cur      string
	Base     string
	Slug     string
	CanWrite bool
}

type pageData struct {
	Title       string
	Base        string
	SiteName    string
	HasLogo     bool
	AssetV      string
	Space       *store.Space
	SpaceList   []*store.Space
	Spaces      []*store.Space
	Path        string
	PageURL     string
	ParentURL   string
	MDURL       string
	Version     int64
	Mode        string // "edit", "create" or "new"
	Source      string
	Body        template.HTML
	Print       bool // ?print: the page and everything below it as one document
	Crumbs      []link
	Children    []link
	Tree        treeCtx
	CanWrite    bool
	CanEdit     bool   // may edit, or at least suggest an edit
	IsAdmin     bool   // server administrator
	CanManage   bool   // administrator of this space
	AuthOn      bool   // sign-in is enabled
	User        string // signed-in user's name
	Next        string // where to go after signing in
	HasKids     bool
	Assets      []assetView
	Query       string
	Usage       string
	Hits        []hitView
	Rev         *revBanner
	Revs        []revRow
	DiffHTML    template.HTML
	DiffAgainst int64
	HistoryURL  string
	Changes     []changeRow
	FeedURL     string
	FeedTitle   string
	SearchSpace string
	Broken      []brokenLink
	Proposals   []proposalRow
	Proposal    *proposalRow
	Pending     int // suggestions waiting for review (shown to reviewers)
	CanSuggest  bool
	CanReview   bool
	CanWithdraw bool
	Stale       bool // the page changed since the suggestion was written
	Decision    string
	DecidedBy   string
	DecidedAt   string
	AIOn        bool // a model is configured (and the caller may use it)
	AIAvailable bool // ... and settings may offer the switch
}

// hitView is a search result for the HTML page. The HTML fields were escaped
// by the store, which wraps matches in <mark>.
type hitView struct {
	Space, SpaceTitle, Path, URL string
	Title, Snippet               template.HTML
}

func tmplHTML(s string) template.HTML { return template.HTML(s) } // already sanitised by render.HTML

var tmpl = template.Must(template.New("wako").Funcs(template.FuncMap{
	"size": func(n int64) string {
		switch {
		case n >= 1<<20:
			return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
		case n >= 1<<10:
			return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
		}
		return fmt.Sprintf("%d B", n)
	},
	"list":   func(a ...string) []string { return a },
	"size64": func(n int) string { return humanSize(int64(n)) },
	"isOpen": func(node, cur string) bool { return cur == node || strings.HasPrefix(cur, node+"/") },
	"sub": func(c treeCtx, nodes []*store.Node) treeCtx {
		c.Nodes = nodes
		return c
	},
}).ParseFS(uiFS, "ui/*.html"))

// assetVersion busts caches when the embedded CSS or JS changes.
var assetVersion = func() string {
	h := sha256.New()
	for _, f := range []string{"ui/app.css", "ui/app.js", "ui/theme.js", "ui/icon.svg"} {
		b, _ := uiFS.ReadFile(f)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}()

func (s *Server) renderHTML(w http.ResponseWriter, name string, d pageData) {
	s.renderHTMLStatus(w, http.StatusOK, name, d)
}

// renderHTMLStatus is renderHTML with another status. The headers must be set
// before the status is written, or they are lost and the page loses its CSP.
func (s *Server) renderHTMLStatus(w http.ResponseWriter, code int, name string, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", pageCSP)
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	if err := tmpl.ExecuteTemplate(w, name, d); err != nil {
		s.logger().Error("rendering a template", "template", name, "err", err)
	}
}

// ui serves the embedded assets and the pages under /_ui/.
func (s *Server) ui(w http.ResponseWriter, r *http.Request, rest string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch rest {
	case "app.css", "app.js", "theme.js":
		b, _ := uiFS.ReadFile("ui/" + rest)
		ct := "text/css; charset=utf-8"
		if strings.HasSuffix(rest, ".js") {
			ct = "text/javascript; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Write(b)
	case "icon.svg":
		b, _ := uiFS.ReadFile("ui/icon.svg")
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Write(b)
	case "apple-touch-icon.png":
		b, _ := uiFS.ReadFile("ui/apple-touch-icon.png")
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Write(b)
	case "logo":
		if s.LogoFile == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, s.LogoFile)
	case "search":
		d := s.newData(r, nil, Access{}, "", "Search")
		d.Query = r.URL.Query().Get("q")
		d.SearchSpace = r.URL.Query().Get("space")
		hits, err := s.search(r, d.Query, 50, d.SearchSpace)
		if err != nil {
			s.fail(w, err)
			return
		}
		for _, h := range hits {
			d.Hits = append(d.Hits, hitView{h.Space, h.SpaceTitle, h.Path, h.URL, template.HTML(h.TitleHTML), template.HTML(h.SnippetHTML)})
		}
		s.renderHTML(w, "search", d)
	case "login":
		if s.auth == nil || s.principal(r) != nil {
			http.Redirect(w, r, s.safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
			return
		}
		d := s.newData(r, nil, Access{}, "", "Sign in")
		d.Next = s.safeNext(r.URL.Query().Get("next"))
		s.renderHTML(w, "login", d)
	case "account", "users":
		if s.auth == nil {
			http.NotFound(w, r)
			return
		}
		if s.principal(r) == nil {
			http.Redirect(w, r, s.u("_ui", "login")+"?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if rest == "users" && !s.isAdmin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		d := s.newData(r, nil, Access{}, "", map[string]string{"account": "Account", "users": "Users"}[rest])
		s.renderHTML(w, rest, d)
	case "ask":
		if s.AI != nil && s.auth != nil && s.principal(r) == nil {
			http.Redirect(w, r, s.u("_ui", "login")+"?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		s.aiPage(w, r)
	case "new-space":
		if !s.isAdmin(r) {
			http.NotFound(w, r)
			return
		}
		d := s.newData(r, nil, Access{}, "", "New space")
		s.renderHTML(w, "newspace", d)
	default:
		http.NotFound(w, r)
	}
}
