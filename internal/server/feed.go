package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	htmlpkg "html"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hbroek/wako/internal/diff"
	"github.com/hbroek/wako/internal/render"
	"github.com/hbroek/wako/internal/store"
)

// spaceChange is one change, with the space it happened in.
type spaceChange struct {
	sp *store.Space
	c  *store.Change
}

// recentChanges returns the newest changes in the spaces the caller may read
// (or in just one of them), newest first.
func (s *Server) recentChanges(r *http.Request, only string, limit int) ([]spaceChange, error) {
	spaces, err := s.readableSpaces(r)
	if err != nil {
		return nil, err
	}
	var out []spaceChange
	for _, sp := range spaces {
		if only != "" && sp.Slug != only {
			continue
		}
		list, err := sp.RecentChanges(limit)
		if err != nil {
			return nil, err
		}
		for _, c := range list {
			out = append(out, spaceChange{sp, c})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].c.At.Equal(out[j].c.At) {
			return out[i].c.At.After(out[j].c.At)
		}
		return out[i].c.ID > out[j].c.ID
	})
	if limit <= 0 {
		limit = 50
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func feedLimit(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	switch {
	case n <= 0:
		return 50
	case n > 200:
		return 200
	}
	return n
}

// origin is the scheme and host the site is reached at, for the absolute URLs
// a feed needs. --public-url overrides it (set it behind a reverse proxy).
func (s *Server) origin(r *http.Request) string {
	if s.PublicURL != "" {
		return strings.TrimRight(s.PublicURL, "/")
	}
	scheme := "http"
	if s.isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + s.requestHost(r)
}

// changeURL is where an entry of the feed leads.
func (s *Server) changeURL(sc spaceChange) string {
	switch sc.c.Action {
	case "deleted":
		return s.u(sc.sp.Slug) + "/"
	}
	return s.u(sc.sp.Slug, sc.c.Path)
}

// changeSummary says in a few words what happened. When nobody recorded who
// did it (changes from before history existed, or a server with sign-in off),
// it says only what happened.
func changeSummary(c *store.Change) string {
	subject := func(known, unknown string) string {
		if c.Author != "" {
			return c.Author + " " + known
		}
		return unknown
	}
	switch c.Action {
	case "created":
		return subject("created the page", "Page created")
	case "restored":
		return subject("restored an earlier revision", "Earlier revision restored")
	case "moved":
		return subject("moved the page", "Page moved") + " from /" + c.OldPath + " to /" + c.Path
	case "deleted":
		return subject("deleted the page", "Page deleted") + " /" + c.Path
	}
	return subject("edited the page", "Page edited")
}

// ---- HTML list ----

type changeRow struct {
	When, Action, Title, Path, URL, Author, Note, Space, SpaceTitle, Summary string
}

func (s *Server) changeRows(list []spaceChange) []changeRow {
	rows := make([]changeRow, 0, len(list))
	for _, sc := range list {
		c := sc.c
		rows = append(rows, changeRow{When: fmtTime(c.At), Action: c.Action, Title: c.Title, Path: c.Path, URL: s.changeURL(sc),
			Author: c.Author, Note: c.Note, Space: sc.sp.Slug, SpaceTitle: sc.sp.Title, Summary: changeSummary(c)})
	}
	return rows
}

type changeJSON struct {
	Space   string `json:"space"`
	Action  string `json:"action"`
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Title   string `json:"title"`
	Author  string `json:"author,omitempty"`
	At      string `json:"at"`
	Note    string `json:"note,omitempty"`
	URL     string `json:"url"`
}

// changesAPI answers GET /_api/changes and /_api/spaces/{space}/changes.
func (s *Server) changesAPI(w http.ResponseWriter, r *http.Request, only string) {
	list, err := s.recentChanges(r, only, feedLimit(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]changeJSON, 0, len(list))
	for _, sc := range list {
		c := sc.c
		out = append(out, changeJSON{sc.sp.Slug, c.Action, c.Path, c.OldPath, c.Title, c.Author, c.At.UTC().Format(time.RFC3339), c.Note, s.changeURL(sc)})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- RSS ----

type rssDoc struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Atom    string     `xml:"xmlns:atom,attr"`
	DC      string     `xml:"xmlns:dc,attr"`
	Source  string     `xml:"xmlns:source,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title         string     `xml:"title"`
	Link          string     `xml:"link"`
	Description   string     `xml:"description"`
	Generator     string     `xml:"generator"`
	LastBuildDate string     `xml:"lastBuildDate,omitempty"`
	TTL           int        `xml:"ttl"`
	Self          rssAtom    `xml:"atom:link"`
	Items         []rssEntry `xml:"item"`
}

type rssAtom struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type rssEntry struct {
	Title       string  `xml:"title"`
	Link        string  `xml:"link"`
	GUID        rssGUID `xml:"guid"`
	PubDate     string  `xml:"pubDate"`
	Creator     string  `xml:"dc:creator,omitempty"`
	Category    string  `xml:"category"`
	Description string  `xml:"description"`
	Markdown    *rssMD  `xml:"source:markdown"`
}

// rssMD is <source:markdown>. The text is escaped by hand (only & < >, so
// quotes and line breaks stay readable) and written as it is.
type rssMD struct {
	Text string `xml:",innerxml"`
}

type rssGUID struct {
	Value       string `xml:",chardata"`
	IsPermaLink bool   `xml:"isPermaLink,attr"`
}

const (
	previewLines = 40
	excerptChars = 300
)

// entry is the body of a feed entry, as HTML and as Markdown: what happened,
// and for a new page its opening, for an edit the diff.
func (s *Server) entry(lc *linkCtx, sc spaceChange, link, origin string, global bool) (html, md string) {
	c := sc.c
	var m strings.Builder
	var b strings.Builder
	// "bob edited X" when the author is known, "Edited X" when it is not.
	verb := map[string]string{"created": "created", "restored": "restored an earlier revision of", "moved": "moved", "deleted": "deleted"}[c.Action]
	if verb == "" {
		verb = "edited"
	}
	if c.Author != "" {
		b.WriteString("<p><strong>" + htmlpkg.EscapeString(c.Author) + "</strong> " + verb)
		m.WriteString("**" + mdEscape(c.Author) + "** " + verb)
	} else {
		b.WriteString("<p>" + strings.ToUpper(verb[:1]) + verb[1:])
		m.WriteString(strings.ToUpper(verb[:1]) + verb[1:])
	}
	if c.Action == "deleted" {
		b.WriteString(" <code>/" + htmlpkg.EscapeString(c.Path) + "</code>")
		m.WriteString(" `/" + c.Path + "`")
	} else {
		b.WriteString(` <a href="` + htmlpkg.EscapeString(link) + `">` + htmlpkg.EscapeString(c.Title) + `</a>`)
		m.WriteString(" [" + mdEscape(c.Title) + "](" + link + ")")
	}
	if global {
		b.WriteString(" in " + htmlpkg.EscapeString(sc.sp.Title))
		m.WriteString(" in " + mdEscape(sc.sp.Title))
	}
	if c.Action == "moved" {
		b.WriteString(" (from <code>/" + htmlpkg.EscapeString(c.OldPath) + "</code>)")
		m.WriteString(" (from `/" + c.OldPath + "`)")
	}
	b.WriteString(".</p>")
	m.WriteString(".")
	if c.Note != "" {
		b.WriteString("<p><em>" + htmlpkg.EscapeString(c.Note) + "</em></p>")
		m.WriteString("\n\n*" + mdEscape(c.Note) + "*")
	}
	if c.PageID == 0 || (c.Action != "created" && c.Action != "edited" && c.Action != "restored") {
		return b.String(), m.String()
	}
	rev, err := sc.sp.Revision(c.PageID, c.Version)
	if err != nil {
		return b.String(), m.String() // pruned since
	}
	if c.Action == "created" {
		rev.Body = render.StripFrontMatter(rev.Body) // properties are not part of the text
		text := plainExcerpt(rev.Body)
		if text != "" {
			b.WriteString("<blockquote>" + htmlpkg.EscapeString(text) + "</blockquote>")
		}
		if open, cut := markdownExcerpt(rev.Body, excerptMD); open != "" {
			m.WriteString("\n\n" + s.feedMarkdown(lc, sc.sp, c.Path, c.PageID, open, origin))
			if cut {
				m.WriteString("\n\n[Read the rest](" + link + ")")
			}
		}
		return b.String(), m.String()
	}
	prev, err := sc.sp.RevisionBefore(c.PageID, rev.Version)
	if err != nil {
		return b.String(), m.String()
	}
	patch := diff.Unified(prev.Body, rev.Body, "before", "after")
	lines := strings.Split(strings.TrimRight(patch, "\n"), "\n")
	if len(lines) > 2 {
		lines = lines[2:] // drop the file name lines
		if len(lines) > previewLines {
			lines = append(lines[:previewLines], "…")
		}
		b.WriteString("<pre>" + htmlpkg.EscapeString(strings.Join(lines, "\n")) + "</pre>")
		m.WriteString("\n\n" + fenced("diff", strings.Join(lines, "\n")))
	}
	return b.String(), m.String()
}

// feed writes the change feed as RSS 2.0. only is a space slug, or "" for
// everything the caller may read.
func (s *Server) feed(w http.ResponseWriter, r *http.Request, only *store.Space) {
	slug := ""
	if only != nil {
		slug = only.Slug
	}
	list, err := s.recentChanges(r, slug, feedLimit(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	origin := s.origin(r)
	global := only == nil
	ch := rssChannel{Generator: "Wako", TTL: 30}
	if global {
		ch.Title, ch.Link = s.SiteName+" — recent changes", origin+s.u()+"/"
		ch.Description = "Pages created, edited, moved and deleted in every space you can read."
		ch.Self = rssAtom{origin + s.u("_feed"), "self", "application/rss+xml"}
	} else {
		ch.Title, ch.Link = only.Title+" — recent changes", origin+s.u(only.Slug)+"/"
		ch.Description = "Pages created, edited, moved and deleted in " + only.Title + "."
		ch.Self = rssAtom{origin + s.u(only.Slug, "_feed"), "self", "application/rss+xml"}
	}
	lc := s.newLinkCtx(r)
	for i, sc := range list {
		c := sc.c
		if i == 0 {
			ch.LastBuildDate = c.At.UTC().Format(time.RFC1123Z)
		}
		link := origin + s.changeURL(sc)
		title := c.Title
		if global {
			title = "[" + sc.sp.Title + "] " + title
		}
		title += " — " + map[string]string{"created": "new page", "edited": "edited", "restored": "restored", "moved": "moved", "deleted": "deleted"}[c.Action]
		if c.Author != "" {
			title += " by " + c.Author
		}
		desc, md := s.entry(lc, sc, link, origin, global)
		ch.Items = append(ch.Items, rssEntry{
			Title: title, Link: link, GUID: rssGUID{Value: "wako:" + sc.sp.Slug + ":" + strconv.FormatInt(c.ID, 10)},
			PubDate: c.At.UTC().Format(time.RFC1123Z), Creator: c.Author, Category: c.Action,
			Description: desc, Markdown: &rssMD{xmlText(md)},
		})
	}
	body, err := xml.MarshalIndent(rssDoc{Version: "2.0", Atom: "http://www.w3.org/2005/Atom", DC: "http://purl.org/dc/elements/1.1/", Source: sourceNS, Channel: ch}, "", "  ")
	if err != nil {
		s.fail(w, err)
		return
	}
	out := append([]byte(xml.Header), body...)
	out = append(out, '\n')
	sum := sha256.Sum256(out)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	h := w.Header()
	h.Set("Content-Type", "application/rss+xml; charset=utf-8")
	h.Set("ETag", etag)
	h.Set("Vary", "Accept, Authorization, Cookie")
	if s.principal(r) != nil || (only != nil && !only.PublicRead) {
		h.Set("Cache-Control", "private, max-age=300")
	} else {
		h.Set("Cache-Control", "public, max-age=300")
	}
	if len(list) > 0 {
		h.Set("Last-Modified", list[0].c.At.UTC().Format(http.TimeFormat))
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatch(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(out)
}

// spaceFeed serves /{space}/_feed.
func (s *Server) spaceFeed(w http.ResponseWriter, r *http.Request, sp *store.Space) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.feed(w, r, sp)
}

// challengeFeed asks a feed reader to sign in. Feed readers can send HTTP Basic
// credentials, so the API token goes in the password; the answer is the same
// whether or not the space exists, so it reveals nothing.
func (s *Server) challengeFeed(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="wako feed", charset="UTF-8"`)
	http.Error(w, "this feed needs credentials: any user name and an API token as the password", http.StatusUnauthorized)
}

func isFeedPath(p string) bool {
	return strings.HasSuffix(strings.TrimRight(p, "/"), "/_feed") || strings.TrimRight(p, "/") == "_feed"
}

// plainExcerpt is the start of a page's text on one line, without the heading
// and quote marks of its Markdown.
func plainExcerpt(body string) string {
	body = mdRef.ReplaceAllString(body, "$2") // a picture or link reads as its text
	var words []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimLeft(strings.TrimSpace(line), "#> \t")
		words = append(words, strings.Fields(line)...)
	}
	text := strings.Join(words, " ")
	if r := []rune(text); len(r) > excerptChars {
		text = string(r[:excerptChars]) + "…"
	}
	return text
}
