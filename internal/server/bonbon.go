package server

import (
	"encoding/json"
	stdhtml "html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Markdown browsers (BonBon, Mardo) search a site from their address bar when
// the site says where its search is. They read /.well-known/bonbon.json, then
// ask that address for ?q=words with Accept: text/markdown, and show the
// Markdown that comes back. wako answers at /_search, a name no space can have.
//
// The search is the same one the rest of wako uses (every word must match,
// case does not matter, the last word also matches as a prefix) and it covers
// exactly the spaces the request may read: a reader who sends a Bearer token
// also finds what that token may see, anyone else only public spaces.

const (
	bonbonPath     = ".well-known/bonbon.json"
	searchPath     = "_search"
	searchPageSize = 20
	searchMaxHits  = 100 // pages 1 to 5
	searchMaxQuery = 200 // characters of the words that are used
)

// wellKnownBonbon serves /.well-known/bonbon.json.
func (s *Server) wellKnownBonbon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "public, max-age=600")
	h.Set("Access-Control-Allow-Origin", "*") // public, the same for everyone
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		return
	}
	b, _ := json.Marshal(map[string]string{"urlSearch": s.u(searchPath)})
	w.Write(append(b, '\n'))
}

// siteSearch answers GET /_search?q=words[&page=N] with a Markdown page of
// matches. It never answers HTML.
func (s *Server) siteSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/markdown; charset=utf-8")
	h.Set("Cache-Control", "private, no-cache") // the answer depends on who asks
	h.Set("Vary", "Accept, Authorization, Cookie")
	h.Set("X-Content-Type-Options", "nosniff")

	q := strings.Join(strings.Fields(r.URL.Query().Get("q")), " ")
	if rs := []rune(q); len(rs) > searchMaxQuery {
		q = string(rs[:searchMaxQuery])
	}
	if q == "" {
		w.WriteHeader(http.StatusBadRequest)
		if r.Method != http.MethodHead {
			w.Write([]byte("Say what to look for: `?q=words`.\n"))
		}
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > searchMaxHits/searchPageSize {
		page = searchMaxHits / searchPageSize
	}
	hits, err := s.search(r, q, searchMaxHits, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	if r.Method == http.MethodHead {
		return
	}

	var b strings.Builder
	from := (page - 1) * searchPageSize
	if from >= len(hits) {
		if page == 1 {
			b.WriteString("No pages match " + mdQuote(q) + ".\n")
		} else {
			b.WriteString("No more pages match " + mdQuote(q) + ".\n")
		}
		w.Write([]byte(b.String()))
		return
	}
	to := min(from+searchPageSize, len(hits))
	for _, hit := range hits[from:to] {
		title := strings.TrimSpace(hit.Title)
		if title == "" {
			title = hit.Path
		}
		b.WriteString("### [" + mdText(oneLine(title)) + "](" + mdURL(hit.URL) + ")\n\n")
		line := mdText(oneLine(snippetText(hit.SnippetHTML)))
		if hit.SpaceTitle != "" {
			if line != "" {
				line = " · " + line
			}
			line = "*" + mdText(oneLine(hit.SpaceTitle)) + "*" + line
		}
		if line != "" {
			b.WriteString(line + "\n\n")
		}
	}
	if to < len(hits) {
		next := s.u(searchPath) + "?q=" + url.QueryEscape(q) + "&page=" + strconv.Itoa(page+1)
		b.WriteString("[More results](" + mdURL(next) + ")\n")
	}
	w.Write([]byte(b.String()))
}

var htmlTag = regexp.MustCompile(`<[^>]*>`)

// snippetText turns the highlighted HTML excerpt of a hit into plain text.
func snippetText(s string) string {
	return stdhtml.UnescapeString(htmlTag.ReplaceAllString(s, ""))
}

// oneLine joins the words of s with single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// mdText makes text show as itself in Markdown: no emphasis, links, HTML,
// headings, lists or tables can come from it.
func mdText(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch r {
		case '\\', '`', '*', '_', '{', '}', '[', ']', '<', '>', '(', ')', '|', '~', '&', '!', '"':
			b.WriteByte('\\')
		case '#', '+', '-', '=', '.', ':':
			if i == 0 { // would start a heading, a list or a rule
				b.WriteByte('\\')
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

// mdQuote shows words in quotes, as plain text.
func mdQuote(s string) string { return "\"" + mdText(s) + "\"" }

// mdURL makes an address safe to put inside ( ) of a Markdown link.
func mdURL(u string) string {
	return strings.NewReplacer(" ", "%20", "(", "%28", ")", "%29", "<", "%3C", ">", "%3E", "\\", "%5C", "\n", "", "\r", "").Replace(u)
}
