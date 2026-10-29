package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Page struct {
	ID          int64
	ParentID    *int64
	Slug        string
	Title       string
	Body        string
	Position    int64
	Version     int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Path        string
	HasChildren bool
}

// Node is one entry of a space's page tree.
type Node struct {
	Slug     string  `json:"slug"`
	Title    string  `json:"title"`
	Path     string  `json:"path"`
	Children []*Node `json:"children,omitempty"`
}

// Refs is what a page's Markdown refers to, extracted by the caller.
type Refs struct {
	Images []string // bare image names, bound to library assets on save
	Links  []string // destinations of its links to other pages, as written
}

// Cond holds the preconditions of a write, and who made it.
type Cond struct {
	IfMatch      *int64 // required current version
	MustNotExist bool

	By     string // the author, recorded in the history and the change feed
	Action string // "edited" (the default) or "restored"
	Note   string // shown next to the revision, e.g. "restored revision 4"

	// Coalesce lets this save replace the author's latest revision when it
	// follows closely (see Store.RevisionWindow). Set it for the interactive
	// editor, where Ctrl+S is pressed over and over; programs get a revision per call.
	Coalesce bool
}

var spaceMigrations = []string{
	`CREATE TABLE pages(
		id INTEGER PRIMARY KEY,
		parent_id INTEGER REFERENCES pages(id) ON DELETE CASCADE,
		slug TEXT NOT NULL,
		title TEXT NOT NULL,
		body TEXT NOT NULL,
		position INTEGER NOT NULL DEFAULT 0,
		version INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE UNIQUE INDEX pages_unique_slug ON pages(COALESCE(parent_id, 0), slug);
	CREATE INDEX pages_parent ON pages(parent_id);`,
	`CREATE TABLE assets(
		id TEXT PRIMARY KEY,           -- sha256 of the content, so identical uploads dedupe
		name TEXT NOT NULL,
		mime TEXT NOT NULL,
		size INTEGER NOT NULL,
		data BLOB NOT NULL,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE page_assets(
		page_id INTEGER NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
		asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
		PRIMARY KEY(page_id, asset_id)
	);
	CREATE INDEX page_assets_asset ON page_assets(asset_id);`,
	// The media library: names are unique per space and pages bind images by name.
	`UPDATE assets SET name = substr(id, 1, 8) || '-' || name
		WHERE rowid NOT IN (SELECT MIN(rowid) FROM assets GROUP BY name);
	CREATE UNIQUE INDEX assets_name ON assets(name);
	DROP TABLE page_assets;
	CREATE TABLE page_assets(
		page_id INTEGER NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
		name TEXT NOT NULL,            -- the name the page's Markdown uses
		asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
		PRIMARY KEY(page_id, name)
	);
	CREATE INDEX page_assets_asset ON page_assets(asset_id);`,
	// Pixel size is stored at upload so layout and variants never read the blob.
	`ALTER TABLE assets ADD COLUMN width INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE assets ADD COLUMN height INTEGER NOT NULL DEFAULT 0;`,
	// Search and links. pages.path is the denormalised full path; links holds
	// the links each page makes, as written, and is resolved when it is read.
	`ALTER TABLE pages ADD COLUMN path TEXT NOT NULL DEFAULT '';
	WITH RECURSIVE t(id, path) AS (
		SELECT id, slug FROM pages WHERE parent_id IS NULL
		UNION ALL SELECT p.id, t.path || '/' || p.slug FROM pages p JOIN t ON p.parent_id = t.id
	) UPDATE pages SET path = (SELECT path FROM t WHERE t.id = pages.id);
	CREATE UNIQUE INDEX pages_path ON pages(path);
	CREATE TABLE links(
		src_page_id INTEGER NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
		kind TEXT NOT NULL,
		target TEXT NOT NULL,
		PRIMARY KEY(src_page_id, kind, target)
	);
	CREATE VIRTUAL TABLE pages_fts USING fts5(
		title, body, content='pages', content_rowid='id', tokenize='porter unicode61 remove_diacritics 2'
	);
	CREATE TRIGGER pages_fts_ai AFTER INSERT ON pages BEGIN
		INSERT INTO pages_fts(rowid, title, body) VALUES (new.id, new.title, new.body);
	END;
	CREATE TRIGGER pages_fts_ad AFTER DELETE ON pages BEGIN
		INSERT INTO pages_fts(pages_fts, rowid, title, body) VALUES ('delete', old.id, old.title, old.body);
	END;
	CREATE TRIGGER pages_fts_au AFTER UPDATE OF title, body ON pages BEGIN
		INSERT INTO pages_fts(pages_fts, rowid, title, body) VALUES ('delete', old.id, old.title, old.body);
		INSERT INTO pages_fts(rowid, title, body) VALUES (new.id, new.title, new.body);
	END;
	INSERT INTO pages_fts(pages_fts) VALUES ('rebuild');`,
	// Pages use plain Markdown links only; [[wiki links]] were dropped.
	`DELETE FROM links WHERE kind != 'link';`,
	// History: a snapshot per content change, and a log of changes for the feed.
	`CREATE TABLE revisions(
		id INTEGER PRIMARY KEY,
		page_id INTEGER NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
		version INTEGER NOT NULL,
		title TEXT NOT NULL,
		body TEXT NOT NULL,
		author TEXT NOT NULL DEFAULT '',
		action TEXT NOT NULL,           -- created | edited | restored
		note TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL
	);
	CREATE UNIQUE INDEX revisions_page_version ON revisions(page_id, version);
	CREATE TABLE changes(
		id INTEGER PRIMARY KEY,
		at INTEGER NOT NULL,
		action TEXT NOT NULL,           -- created | edited | restored | moved | deleted
		page_id INTEGER,                -- no foreign key: the log outlives deleted pages
		path TEXT NOT NULL,
		old_path TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL,
		author TEXT NOT NULL DEFAULT '',
		version INTEGER NOT NULL DEFAULT 0,
		note TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX changes_page ON changes(page_id, id);
	-- Pages that exist already start their history here.
	INSERT INTO revisions(page_id, version, title, body, action, created_at)
		SELECT id, version, title, body, CASE WHEN version = 1 THEN 'created' ELSE 'edited' END, updated_at FROM pages;
	INSERT INTO changes(at, action, page_id, path, title, version)
		SELECT updated_at, CASE WHEN version = 1 THEN 'created' ELSE 'edited' END, id, path, title, version
		FROM (SELECT * FROM pages ORDER BY updated_at DESC, id DESC LIMIT 50) ORDER BY updated_at, id;`,
	// Suggestions: changes proposed by people (or programs) who may not write
	// directly, waiting for an editor to accept or reject them.
	`CREATE TABLE proposals(
		id INTEGER PRIMARY KEY,
		path TEXT NOT NULL,
		title TEXT NOT NULL,
		body TEXT NOT NULL,                       -- the whole proposed text
		base_version INTEGER NOT NULL,            -- the page version it was written against; 0 for a new page
		base_body TEXT NOT NULL DEFAULT '',       -- that version's text, to show the change and to rebase it
		author TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL DEFAULT '',          -- api | ai:<model> | mcp
		note TEXT NOT NULL DEFAULT '',            -- the proposer's explanation
		status TEXT NOT NULL DEFAULT 'pending',   -- pending | accepted | rejected | withdrawn
		created_at INTEGER NOT NULL,
		decided_by TEXT NOT NULL DEFAULT '',
		decided_at INTEGER NOT NULL DEFAULT 0,
		decision_note TEXT NOT NULL DEFAULT '',
		applied_version INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX proposals_status ON proposals(status, id);`,
}

type queryer interface {
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
	Exec(string, ...any) (sql.Result, error)
}

const pageCols = `p.id, p.parent_id, p.slug, p.title, p.body, p.position, p.version,
	p.created_at, p.updated_at, EXISTS(SELECT 1 FROM pages c WHERE c.parent_id = p.id)`

func scanPage(sc interface{ Scan(...any) error }) (*Page, error) {
	var p Page
	var parent sql.NullInt64
	var c, u int64
	if err := sc.Scan(&p.ID, &parent, &p.Slug, &p.Title, &p.Body, &p.Position,
		&p.Version, &c, &u, &p.HasChildren); err != nil {
		return nil, err
	}
	if parent.Valid {
		p.ParentID = &parent.Int64
	}
	p.CreatedAt, p.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return &p, nil
}

// find walks parts from the space root. It returns nil, nil for the root itself.
func find(q queryer, parts []string) (*Page, error) {
	var cur *Page
	for i, slug := range parts {
		var parentID any // NULL at the root
		if cur != nil {
			parentID = cur.ID
		}
		row := q.QueryRow(`SELECT `+pageCols+` FROM pages p
			WHERE p.parent_id IS ? AND p.slug = ?`, parentID, slug)
		p, err := scanPage(row)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		p.Path = strings.Join(parts[:i+1], "/")
		cur = p
	}
	return cur, nil
}

func (sp *Space) GetPage(path string) (*Page, error) {
	parts, err := SplitPath(path)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, ErrNotFound
	}
	return find(sp.db, parts)
}

// Children lists the pages directly under path ("" is the space root).
func (sp *Space) Children(path string) ([]*Page, error) {
	parts, err := SplitPath(path)
	if err != nil {
		return nil, err
	}
	parent, err := find(sp.db, parts)
	if err != nil {
		return nil, err
	}
	var parentID any
	if parent != nil {
		parentID = parent.ID
	}
	rows, err := sp.db.Query(`SELECT `+pageCols+` FROM pages p
		WHERE p.parent_id IS ? ORDER BY p.position, p.slug`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Page
	for rows.Next() {
		p, err := scanPage(rows)
		if err != nil {
			return nil, err
		}
		p.Path = strings.Trim(path+"/"+p.Slug, "/")
		out = append(out, p)
	}
	return out, rows.Err()
}

type treeEntry struct {
	changes int64 // total_changes() when the tree was built
	nodes   []*Node
}

// treeCache holds the last tree built for each space database. Every write
// through the space's single connection raises total_changes(), so comparing it
// is a cheap and exact test for "nothing changed since".
var treeCache sync.Map // *sql.DB -> *treeEntry

// Tree returns every page of the space as a nested tree. The result is shared
// between callers: read it, never modify it.
func (sp *Space) Tree() ([]*Node, error) {
	var changes int64
	if err := sp.db.QueryRow(`SELECT total_changes()`).Scan(&changes); err != nil {
		return nil, err
	}
	if e, ok := treeCache.Load(sp.db); ok && e.(*treeEntry).changes == changes {
		return e.(*treeEntry).nodes, nil
	}
	nodes, err := sp.buildTree()
	if err != nil {
		return nil, err
	}
	treeCache.Store(sp.db, &treeEntry{changes, nodes})
	return nodes, nil
}

func (sp *Space) buildTree() ([]*Node, error) {
	rows, err := sp.db.Query(`SELECT id, parent_id, slug, title FROM pages ORDER BY position, slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type rec struct {
		parent sql.NullInt64
		node   *Node
	}
	byID := map[int64]*rec{}
	var order []int64
	for rows.Next() {
		var id int64
		r := rec{node: &Node{}}
		if err := rows.Scan(&id, &r.parent, &r.node.Slug, &r.node.Title); err != nil {
			return nil, err
		}
		byID[id] = &r
		order = append(order, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var fill func(id int64) string
	fill = func(id int64) string {
		r := byID[id]
		if r.node.Path != "" {
			return r.node.Path
		}
		if r.parent.Valid {
			r.node.Path = fill(r.parent.Int64) + "/" + r.node.Slug
		} else {
			r.node.Path = r.node.Slug
		}
		return r.node.Path
	}
	for _, id := range order {
		fill(id)
	}
	var roots []*Node
	for _, id := range order {
		r := byID[id]
		if r.parent.Valid {
			p := byID[r.parent.Int64].node
			p.Children = append(p.Children, r.node)
		} else {
			roots = append(roots, r.node)
		}
	}
	return roots, nil
}

// PutPage creates or replaces the page at path. The parent must exist. refs
// lists what the Markdown refers to: image names are bound to library assets,
// and links are recorded for backlinks and the broken-link report.
func (sp *Space) PutPage(path, title, body string, refs Refs, c Cond) (*Page, bool, error) {
	parts, err := SplitPath(path)
	if err != nil {
		return nil, false, err
	}
	if len(parts) == 0 {
		return nil, false, fmt.Errorf("%w: empty path", ErrInvalid)
	}
	tx, err := sp.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	parent, err := find(tx, parts[:len(parts)-1])
	if errors.Is(err, ErrNotFound) {
		return nil, false, ErrParentMissing
	}
	if err != nil {
		return nil, false, err
	}
	var parentID any
	if parent != nil {
		parentID = parent.ID
	}
	slug := parts[len(parts)-1]
	now := sp.now().Unix()

	existing, err := find(tx, parts)
	switch {
	case errors.Is(err, ErrNotFound):
		if c.IfMatch != nil {
			return nil, false, ErrVersion
		}
		var pos int64
		if err := tx.QueryRow(`SELECT COALESCE(MAX(position), -1) + 1 FROM pages
			WHERE parent_id IS ?`, parentID).Scan(&pos); err != nil {
			return nil, false, err
		}
		res, err := tx.Exec(`INSERT INTO pages(parent_id, slug, path, title, body, position, version, created_at, updated_at)
			VALUES(?,?,?,?,?,?,1,?,?)`, parentID, slug, strings.Join(parts, "/"), title, body, pos, now, now)
		if err != nil {
			return nil, false, err
		}
		id, _ := res.LastInsertId()
		if err := syncRefs(tx, id, refs); err != nil {
			return nil, false, err
		}
		if err := sp.recordWrite(tx, id, strings.Join(parts, "/"), 1, title, body, c.By, "created", c.Note, false, now); err != nil {
			return nil, false, err
		}
		p, err := loadByID(tx, id, strings.Join(parts, "/"))
		if err != nil {
			return nil, false, err
		}
		return p, true, tx.Commit()
	case err != nil:
		return nil, false, err
	}

	if c.MustNotExist {
		return nil, false, ErrExists
	}
	if c.IfMatch != nil && *c.IfMatch != existing.Version {
		return nil, false, ErrVersion
	}
	action := c.Action
	if action == "" {
		action = "edited"
	}
	// Saves of the same text by the same author in quick succession are one revision.
	if _, err := sp.writeBody(tx, existing.ID, existing.Path, title, body, c.By, action, c.Note, c.Coalesce && action == "edited" && c.Note == "", now); err != nil {
		return nil, false, err
	}
	if err := syncRefs(tx, existing.ID, refs); err != nil {
		return nil, false, err
	}
	p, err := loadByID(tx, existing.ID, existing.Path)
	if err != nil {
		return nil, false, err
	}
	return p, false, tx.Commit()
}

func loadByID(q queryer, id int64, path string) (*Page, error) {
	p, err := scanPage(q.QueryRow(`SELECT `+pageCols+` FROM pages p WHERE p.id = ?`, id))
	if err != nil {
		return nil, err
	}
	p.Path = path
	return p, nil
}

// DeletePage removes a page. Pages with children need recursive. by is
// recorded in the change feed.
func (sp *Space) DeletePage(path string, recursive bool, ifMatch *int64, by string) error {
	tx, err := sp.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	parts, err := SplitPath(path)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return ErrNotFound
	}
	p, err := find(tx, parts)
	if err != nil {
		return err
	}
	if ifMatch != nil && *ifMatch != p.Version {
		return ErrVersion
	}
	if p.HasChildren && !recursive {
		return ErrHasChildren
	}
	var below int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pages WHERE substr(path, 1, ?) = ?`, len(p.Path)+1, p.Path+"/").Scan(&below); err != nil {
		return err
	}
	note := ""
	if below > 0 {
		note = fmt.Sprintf("with %d page%s below it", below, map[bool]string{true: "", false: "s"}[below == 1])
	}
	// Log entries outlive the page, so they must not keep its id: SQLite may hand
	// the same id to a page created later, and the feed would show the wrong history.
	if _, err := tx.Exec(`UPDATE changes SET page_id = NULL WHERE page_id IN
		(SELECT id FROM pages WHERE path = ?1 OR substr(path, 1, ?2) = ?3)`, p.Path, len(p.Path)+1, p.Path+"/"); err != nil {
		return err
	}
	if err := sp.recordChange(tx, "deleted", nil, p.Path, "", p.Title, by, p.Version, note, sp.now().Unix(), false); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM pages WHERE id = ?`, p.ID); err != nil { // children, revisions and links cascade
		return err
	}
	return tx.Commit()
}

// Reorder sets the order of the pages under parentPath ("" is the space root).
// order must list every child slug exactly once.
func (sp *Space) Reorder(parentPath string, order []string) error {
	parts, err := SplitPath(parentPath)
	if err != nil {
		return err
	}
	tx, err := sp.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	parent, err := find(tx, parts)
	if err != nil {
		return err
	}
	var parentID any
	if parent != nil {
		parentID = parent.ID
	}
	rows, err := tx.Query(`SELECT slug FROM pages WHERE parent_id IS ?`, parentID)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			rows.Close()
			return err
		}
		have[slug] = true
	}
	rows.Close()
	if len(order) != len(have) {
		return fmt.Errorf("%w: order must list all %d children", ErrInvalid, len(have))
	}
	seen := map[string]bool{}
	for _, slug := range order {
		if !have[slug] || seen[slug] {
			return fmt.Errorf("%w: unexpected or repeated slug %q", ErrInvalid, slug)
		}
		seen[slug] = true
	}
	for i, slug := range order {
		if _, err := tx.Exec(`UPDATE pages SET position = ? WHERE parent_id IS ? AND slug = ?`, i, parentID, slug); err != nil {
			return err
		}
	}
	return tx.Commit()
}
