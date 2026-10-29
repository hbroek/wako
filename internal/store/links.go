package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"strings"
	"unicode"

	"github.com/hbroek/wako/internal/render"
	"github.com/hbroek/wako/internal/wiki"
)

// syncRefs records what a page refers to: image bindings and links.
func syncRefs(q queryer, pageID int64, refs Refs) error {
	if err := syncImageBindings(q, pageID, refs.Images); err != nil {
		return err
	}
	return syncLinks(q, pageID, refs.Links)
}

func syncLinks(q queryer, pageID int64, links []string) error {
	if _, err := q.Exec(`DELETE FROM links WHERE src_page_id = ?`, pageID); err != nil {
		return err
	}
	for _, l := range links {
		if _, err := q.Exec(`INSERT OR IGNORE INTO links(src_page_id, kind, target) VALUES(?,'link',?)`,
			pageID, l); err != nil {
			return err
		}
	}
	return nil
}

// PageIndex returns a snapshot of the space's page paths and titles.
func (sp *Space) PageIndex() (*wiki.Index, error) { return pageIndex(sp.db) }

func pageIndex(q queryer) (*wiki.Index, error) {
	rows, err := q.Query(`SELECT path, title FROM pages`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pages []wiki.Page
	for rows.Next() {
		var p wiki.Page
		if err := rows.Scan(&p.Path, &p.Title); err != nil {
			return nil, err
		}
		pages = append(pages, p)
	}
	return wiki.NewIndex(pages), rows.Err()
}

// LinkRow is one link as written on one page.
type LinkRow struct {
	SrcPath, SrcTitle string
	Target            string
}

// LinkRows returns every link made by the pages of the space.
func (sp *Space) LinkRows() ([]LinkRow, error) {
	rows, err := sp.db.Query(`SELECT p.path, p.title, l.target FROM links l
		JOIN pages p ON p.id = l.src_page_id ORDER BY p.path, l.target`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LinkRow
	for rows.Next() {
		var r LinkRow
		if err := rows.Scan(&r.SrcPath, &r.SrcTitle, &r.Target); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- rewriting links when pages move ----

// LinkPage is a page whose links may need rewriting.
type LinkPage struct {
	Path    string // where the page was before the change
	NewPath string // where it is after
	Body    string
	Links   []string
}

// LinkRewriter returns the page's Markdown with its links updated, and whether
// it changed. old indexes the space as it was before the change; mapPath says
// where each of its pages ended up.
type LinkRewriter func(old *wiki.Index, p LinkPage, mapPath func(path string) string) (string, bool)

// rewriteLinks applies rw to every page that has links, inside tx. mapPath
// maps old paths to new ones; the pages table already holds the new state. Each
// page it changes gets a revision by `by` with the given note.
func (sp *Space) rewriteLinks(tx *sql.Tx, old *wiki.Index, rw LinkRewriter, oldPath func(newPath string) string, mapPath func(string) string, by, note string) (int, error) {
	type pg struct {
		id                int64
		path, title, body string
		links             []string
	}
	rows, err := tx.Query(`SELECT p.id, p.path, p.title, p.body, l.target FROM links l
		JOIN pages p ON p.id = l.src_page_id ORDER BY p.id`)
	if err != nil {
		return 0, err
	}
	var pages []*pg
	byID := map[int64]*pg{}
	for rows.Next() {
		var id int64
		var path, title, body string
		var l string
		if err := rows.Scan(&id, &path, &title, &body, &l); err != nil {
			rows.Close()
			return 0, err
		}
		p := byID[id]
		if p == nil {
			p = &pg{id: id, path: path, title: title, body: body}
			byID[id], pages = p, append(pages, p)
		}
		p.links = append(p.links, l)
	}
	rows.Close()

	changed := 0
	now := sp.now().Unix()
	for _, p := range pages {
		body, ok := rw(old, LinkPage{Path: oldPath(p.path), NewPath: p.path, Body: p.body, Links: p.links}, mapPath)
		if !ok || body == p.body {
			continue
		}
		if _, err := sp.writeBody(tx, p.id, p.path, p.title, body, by, "edited", note, false, now); err != nil {
			return changed, err
		}
		if err := syncLinks(tx, p.id, render.Links(body)); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}

// RewriteLinks applies rw to every page of the space that has links (used when
// the space itself is renamed). It returns how many pages changed.
func (sp *Space) RewriteLinks(rw LinkRewriter, by string) (int, error) {
	tx, err := sp.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	old, err := pageIndex(tx)
	if err != nil {
		return 0, err
	}
	n, err := sp.rewriteLinks(tx, old, rw, func(p string) string { return p }, func(p string) string { return p }, by, "links updated after the space was renamed")
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// MovePageRewriting is MovePage plus link maintenance: in one transaction the
// page (and its subtree) moves, and every page whose links pointed into the
// moved subtree, or that moved itself, is rewritten by rw. It returns the moved
// page and how many other pages were rewritten.
func (sp *Space) MovePageRewriting(from, to string, rw LinkRewriter, by string) (*Page, int, error) {
	fromParts, err := SplitPath(from)
	if err != nil {
		return nil, 0, err
	}
	toParts, err := SplitPath(to)
	if err != nil {
		return nil, 0, err
	}
	if len(fromParts) == 0 || len(toParts) == 0 {
		return nil, 0, fmt.Errorf("%w: empty path", ErrInvalid)
	}
	if len(toParts) > len(fromParts) && strings.Join(toParts[:len(fromParts)], "/") == strings.Join(fromParts, "/") {
		return nil, 0, fmt.Errorf("%w: cannot move a page below itself", ErrInvalid)
	}
	from, to = strings.Join(fromParts, "/"), strings.Join(toParts, "/")
	tx, err := sp.db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()

	src, err := find(tx, fromParts)
	if err != nil {
		return nil, 0, err
	}
	if _, err := find(tx, toParts); err == nil {
		return nil, 0, ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return nil, 0, err
	}
	parent, err := find(tx, toParts[:len(toParts)-1])
	if errors.Is(err, ErrNotFound) {
		return nil, 0, ErrParentMissing
	}
	if err != nil {
		return nil, 0, err
	}
	var parentID any
	if parent != nil {
		parentID = parent.ID
	}
	old, err := pageIndex(tx) // the space as it is before the move
	if err != nil {
		return nil, 0, err
	}
	now := sp.now().Unix()
	if _, err := tx.Exec(`UPDATE pages SET parent_id = ?, slug = ?, path = ?, version = version + 1, updated_at = ?
		WHERE id = ?`, parentID, toParts[len(toParts)-1], to, now, src.ID); err != nil {
		return nil, 0, err
	}
	if err := sp.recordChange(tx, "moved", &src.ID, to, from, src.Title, by, src.Version+1, "", now, false); err != nil {
		return nil, 0, err
	}
	// Descendants keep their place below the page; only the path prefix changes.
	if _, err := tx.Exec(`UPDATE pages SET path = ? || substr(path, ?) WHERE substr(path, 1, ?) = ?`,
		to, len(from)+1, len(from)+1, from+"/"); err != nil {
		return nil, 0, err
	}
	// The statement above rewrote "from/x" to "to" + "/x": substr(path, len(from)+1)
	// starts at the slash, so the prefix is replaced exactly.

	inSubtree := func(p, root string) bool { return p == root || strings.HasPrefix(p, root+"/") }
	mapPath := func(p string) string {
		if inSubtree(p, from) {
			return to + p[len(from):]
		}
		return p
	}
	oldPath := func(p string) string {
		if inSubtree(p, to) {
			return from + p[len(to):]
		}
		return p
	}
	n := 0
	if rw != nil {
		if n, err = sp.rewriteLinks(tx, old, rw, oldPath, mapPath, by, fmt.Sprintf("links updated after moving /%s to /%s", from, to)); err != nil {
			return nil, 0, err
		}
	}
	page, err := loadByID(tx, src.ID, to)
	if err != nil {
		return nil, 0, err
	}
	return page, n, tx.Commit()
}

// ---- search ----

// SearchHit is one page matching a search. TitleHTML and SnippetHTML are safe
// to insert into a page: text is escaped and matches are wrapped in <mark>.
type SearchHit struct {
	Path        string
	Title       string
	TitleHTML   string
	SnippetHTML string
	Score       float64 // lower is better (bm25)
}

// FTSQuery turns what a person typed into an FTS5 expression: every word must
// match, and the last one also matches as a prefix, so results appear while
// typing. Punctuation and FTS operators are dropped. It returns "" if there is
// nothing to search for.
func FTSQuery(q string) string {
	words := strings.FieldsFunc(q, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(words) > 12 {
		words = words[:12]
	}
	for i, w := range words {
		words[i] = `"` + w + `"`
	}
	if len(words) > 0 {
		words[len(words)-1] += "*"
	}
	return strings.Join(words, " ")
}

// Match markers are random for each process, so page text cannot contain them
// and forge a highlight.
var markOpen, markClose = newMarkers()

func newMarkers() (string, string) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	n := hex.EncodeToString(b[:])
	return "\x01" + n + "\x02", "\x03" + n + "\x04"
}

// safeMarks escapes text that carries the match markers and turns the markers
// into <mark> tags.
func safeMarks(s string) string {
	s = html.EscapeString(s)
	s = strings.ReplaceAll(s, markOpen, "<mark>")
	return strings.ReplaceAll(s, markClose, "</mark>")
}

// Search finds pages by title and body, best match first. A title match counts
// ten times a body match.
func (sp *Space) Search(q string, limit int) ([]SearchHit, error) {
	return sp.searchMatch(FTSQuery(q), limit)
}

// FTSQueryAny builds a query that matches pages containing any of the words
// (best match first), for finding material related to a question rather than
// pages that contain every word. Only letters and digits are kept.
func FTSQueryAny(words []string) string {
	var out []string
	for _, w := range words {
		w = strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return r
			}
			return -1
		}, w)
		if w != "" {
			out = append(out, `"`+w+`"`)
		}
		if len(out) == 24 {
			break
		}
	}
	return strings.Join(out, " OR ")
}

// SearchAny finds pages that contain any of the words, best match first.
func (sp *Space) SearchAny(words []string, limit int) ([]SearchHit, error) {
	return sp.searchMatch(FTSQueryAny(words), limit)
}

func (sp *Space) searchMatch(match string, limit int) ([]SearchHit, error) {
	if match == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := sp.db.Query(`SELECT p.path, p.title,
			highlight(pages_fts, 0, ?1, ?2),
			snippet(pages_fts, 1, ?1, ?2, '…', 16),
			bm25(pages_fts, 10.0, 1.0)
		FROM pages_fts JOIN pages p ON p.id = pages_fts.rowid
		WHERE pages_fts MATCH ?3 ORDER BY bm25(pages_fts, 10.0, 1.0), p.path LIMIT ?4`, markOpen, markClose, match, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		var title, snippet string
		if err := rows.Scan(&h.Path, &h.Title, &title, &snippet, &h.Score); err != nil {
			return nil, err
		}
		h.TitleHTML, h.SnippetHTML = safeMarks(title), safeMarks(snippet)
		out = append(out, h)
	}
	return out, rows.Err()
}
