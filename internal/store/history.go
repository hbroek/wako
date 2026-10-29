package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Revision is one saved state of a page.
type Revision struct {
	ID      int64
	PageID  int64
	Version int64
	Title   string
	Body    string // empty in listings
	Size    int    // length of the body in bytes
	Author  string
	Action  string // created | edited | restored
	Note    string
	At      time.Time
}

// Change is one entry of the change log (the feed): something happened to a page.
type Change struct {
	ID      int64
	At      time.Time
	Action  string // created | edited | restored | moved | deleted
	PageID  int64  // 0 for pages that no longer exist
	Path    string
	OldPath string // for moves
	Title   string
	Author  string
	Version int64
	Note    string
}

// changesKept is how many log entries a space keeps.
const changesKept = 1000

// writeBody replaces a page's title and text, bumps its version and records the
// change in its history. It returns the new version.
func (sp *Space) writeBody(q queryer, pageID int64, path, title, body, by, action, note string, coalesce bool, now int64) (int64, error) {
	if _, err := q.Exec(`UPDATE pages SET title = ?, body = ?, version = version + 1, updated_at = ? WHERE id = ?`,
		title, body, now, pageID); err != nil {
		return 0, err
	}
	var version int64
	if err := q.QueryRow(`SELECT version FROM pages WHERE id = ?`, pageID).Scan(&version); err != nil {
		return 0, err
	}
	return version, sp.recordWrite(q, pageID, path, version, title, body, by, action, note, coalesce, now)
}

// recordWrite adds the revision and the log entry for a content change. With
// coalesce, a change by the author of the page's latest revision within the
// revision window replaces that revision instead (and updates its log entry).
func (sp *Space) recordWrite(q queryer, pageID int64, path string, version int64, title, body, by, action, note string, coalesce bool, now int64) error {
	window := int64(sp.st.RevisionWindow / time.Second)
	if coalesce && window > 0 {
		var id, at int64
		var lastBy, lastAction string
		err := q.QueryRow(`SELECT id, author, action, created_at FROM revisions WHERE page_id = ? ORDER BY version DESC LIMIT 1`, pageID).
			Scan(&id, &lastBy, &lastAction, &at)
		if err == nil && lastBy == by && (lastAction == "created" || lastAction == "edited") && now-at < window {
			if _, err := q.Exec(`UPDATE revisions SET version = ?, title = ?, body = ?, created_at = ? WHERE id = ?`,
				version, title, body, now, id); err != nil {
				return err
			}
			return sp.recordChange(q, action, &pageID, path, "", title, by, version, note, now, true)
		}
	}
	if _, err := q.Exec(`INSERT INTO revisions(page_id, version, title, body, author, action, note, created_at)
		VALUES(?,?,?,?,?,?,?,?)`, pageID, version, title, body, by, action, note, now); err != nil {
		return err
	}
	if keep := sp.st.RevisionKeep; keep > 0 {
		if _, err := q.Exec(`DELETE FROM revisions WHERE page_id = ? AND id NOT IN
			(SELECT id FROM revisions WHERE page_id = ? ORDER BY version DESC LIMIT ?)`, pageID, pageID, keep); err != nil {
			return err
		}
	}
	return sp.recordChange(q, action, &pageID, path, "", title, by, version, note, now, false)
}

// recordChange appends to the change log. merge updates the page's latest entry
// instead when it is a recent create or edit by the same author.
func (sp *Space) recordChange(q queryer, action string, pageID *int64, path, oldPath, title, by string, version int64, note string, now int64, merge bool) error {
	if merge && pageID != nil {
		var id, at int64
		var lastBy, lastAction string
		err := q.QueryRow(`SELECT id, author, action, at FROM changes WHERE page_id = ? ORDER BY id DESC LIMIT 1`, *pageID).
			Scan(&id, &lastBy, &lastAction, &at)
		if err == nil && lastBy == by && (lastAction == "created" || lastAction == "edited") &&
			now-at < int64(sp.st.RevisionWindow/time.Second) {
			_, err := q.Exec(`UPDATE changes SET at = ?, path = ?, title = ?, version = ? WHERE id = ?`, now, path, title, version, id)
			return err
		}
	}
	var pid any
	if pageID != nil {
		pid = *pageID
	}
	if _, err := q.Exec(`INSERT INTO changes(at, action, page_id, path, old_path, title, author, version, note)
		VALUES(?,?,?,?,?,?,?,?,?)`, now, action, pid, path, oldPath, title, by, version, note); err != nil {
		return err
	}
	_, err := q.Exec(`DELETE FROM changes WHERE id <= (SELECT id FROM changes ORDER BY id DESC LIMIT 1 OFFSET ?)`, changesKept)
	return err
}

const revCols = `id, page_id, version, title, length(CAST(body AS BLOB)), author, action, note, created_at`

func scanRevision(sc interface{ Scan(...any) error }, withBody bool) (*Revision, error) {
	var r Revision
	var ts int64
	var err error
	if withBody {
		err = sc.Scan(&r.ID, &r.PageID, &r.Version, &r.Title, &r.Size, &r.Author, &r.Action, &r.Note, &ts, &r.Body)
	} else {
		err = sc.Scan(&r.ID, &r.PageID, &r.Version, &r.Title, &r.Size, &r.Author, &r.Action, &r.Note, &ts)
	}
	if err != nil {
		return nil, err
	}
	r.At = time.Unix(ts, 0)
	return &r, nil
}

// Revisions lists a page's history, newest first, without the texts.
func (sp *Space) Revisions(pageID int64) ([]*Revision, error) {
	rows, err := sp.db.Query(`SELECT `+revCols+` FROM revisions WHERE page_id = ? ORDER BY version DESC`, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Revision
	for rows.Next() {
		r, err := scanRevision(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Revision returns the revision with the given version, with its text.
func (sp *Space) Revision(pageID, version int64) (*Revision, error) {
	r, err := scanRevision(sp.db.QueryRow(`SELECT `+revCols+`, body FROM revisions WHERE page_id = ? AND version = ?`, pageID, version), true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// RevisionBefore returns the revision just before the given version, or
// ErrNotFound if it is the first one kept.
func (sp *Space) RevisionBefore(pageID, version int64) (*Revision, error) {
	r, err := scanRevision(sp.db.QueryRow(`SELECT `+revCols+`, body FROM revisions WHERE page_id = ? AND version < ?
		ORDER BY version DESC LIMIT 1`, pageID, version), true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// RecentChanges returns the newest entries of the change log, newest first.
func (sp *Space) RecentChanges(limit int) ([]*Change, error) {
	if limit <= 0 {
		limit = 50
	} else if limit > changesKept {
		limit = changesKept
	}
	rows, err := sp.db.Query(`SELECT id, at, action, COALESCE(page_id, 0), path, old_path, title, author, version, note
		FROM changes ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Change
	for rows.Next() {
		var c Change
		var ts int64
		if err := rows.Scan(&c.ID, &ts, &c.Action, &c.PageID, &c.Path, &c.OldPath, &c.Title, &c.Author, &c.Version, &c.Note); err != nil {
			return nil, err
		}
		c.At = time.Unix(ts, 0)
		out = append(out, &c)
	}
	return out, rows.Err()
}

func (r *Revision) String() string {
	return fmt.Sprintf("v%d by %q (%s)", r.Version, r.Author, r.Action)
}
