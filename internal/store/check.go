package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileProblems compares the spaces in the catalog with the database files on
// disk. It is cheap, so it runs at every start.
type FileProblems struct {
	Missing []string // "slug (file)": a space whose file is gone; serving it would show an empty space
	Orphans []string // files in spaces/ that no space owns (kept, never deleted)
}

func (s *Store) CheckFiles() (*FileProblems, error) {
	rows, err := s.catalog.Query(`SELECT id, slug FROM spaces ORDER BY id`)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	var fp FileProblems
	for rows.Next() {
		var id int64
		var slug string
		if err := rows.Scan(&id, &slug); err != nil {
			rows.Close()
			return nil, err
		}
		file := s.spacePath(id)
		known[filepath.Base(file)] = true
		if _, err := os.Stat(file); err != nil {
			fp.Missing = append(fp.Missing, fmt.Sprintf("%s (%s)", slug, filepath.Base(file)))
		}
	}
	rows.Close()
	entries, err := os.ReadDir(filepath.Join(s.dir, "spaces"))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, "-wal") || strings.HasSuffix(n, "-shm") || known[n] {
			continue
		}
		fp.Orphans = append(fp.Orphans, n)
	}
	sort.Strings(fp.Orphans)
	return &fp, nil
}

// CheckReport is the result of Check.
type CheckReport struct {
	Spaces   int
	Pages    int
	Problems []string
	Fixed    []string
}

func (r *CheckReport) problem(format string, a ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, a...))
}

// Check examines every database: SQLite's own integrity check (quick, or
// complete when deep), foreign keys, the search index, and that every page's
// stored path matches its place in the tree. With repair it fixes what can be
// derived from other data: the search index and the stored paths.
func (s *Store) Check(deep, repair bool) (*CheckReport, error) {
	rep := &CheckReport{}
	fp, err := s.CheckFiles()
	if err != nil {
		return nil, err
	}
	for _, m := range fp.Missing {
		rep.problem("space %s: its database file is missing (restore it from a backup, or remove the space with: wakod forget-space)", m)
	}
	for _, o := range fp.Orphans {
		rep.problem("spaces/%s belongs to no space (kept; delete it by hand if it is not needed)", o)
	}
	pragma := "quick_check"
	if deep {
		pragma = "integrity_check"
	}
	if err := s.checkDB("the catalog", s.catalog, pragma, rep); err != nil {
		return nil, err
	}
	rows, err := s.catalog.Query(`SELECT id, slug FROM spaces ORDER BY id`)
	if err != nil {
		return nil, err
	}
	type sp struct {
		id   int64
		slug string
	}
	var spaces []sp
	for rows.Next() {
		var x sp
		if err := rows.Scan(&x.id, &x.slug); err != nil {
			rows.Close()
			return nil, err
		}
		spaces = append(spaces, x)
	}
	rows.Close()
	missing := map[string]bool{}
	for _, m := range fp.Missing {
		missing[strings.SplitN(m, " ", 2)[0]] = true
	}
	for _, x := range spaces {
		if missing[x.slug] {
			continue
		}
		db, err := s.attach(x.id)
		if err != nil {
			rep.problem("space %s: cannot open its database: %v", x.slug, err)
			continue
		}
		rep.Spaces++
		name := "space " + x.slug
		if err := s.checkDB(name, db, pragma, rep); err != nil {
			return nil, err
		}
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM pages`).Scan(&n)
		rep.Pages += n

		// The search index must agree with the pages.
		if _, err := db.Exec(`INSERT INTO pages_fts(pages_fts, rank) VALUES('integrity-check', 1)`); err != nil { // rank=1: also compare with the pages
			rep.problem("%s: the search index does not match the pages (%v)", name, err)
			if repair {
				if _, err := db.Exec(`INSERT INTO pages_fts(pages_fts) VALUES('rebuild')`); err != nil {
					return nil, err
				}
				rep.Fixed = append(rep.Fixed, name+": rebuilt the search index")
			}
		}
		// Stored paths must follow the tree.
		bad, err := countPathMismatches(db)
		if err != nil {
			return nil, err
		}
		if bad > 0 {
			rep.problem("%s: %d page(s) have a stored path that does not match their place in the tree", name, bad)
			if repair {
				if err := rewritePaths(db); err != nil {
					return nil, err
				}
				rep.Fixed = append(rep.Fixed, name+": recomputed the stored paths")
			}
		}
	}
	return rep, nil
}

func (s *Store) checkDB(name string, db *sql.DB, pragma string, rep *CheckReport) error {
	rows, err := db.Query(`PRAGMA ` + pragma)
	if err != nil {
		return err
	}
	var msgs []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			rows.Close()
			return err
		}
		if m != "ok" {
			msgs = append(msgs, m)
		}
	}
	rows.Close()
	if len(msgs) > 0 {
		if len(msgs) > 5 {
			msgs = append(msgs[:5], "…")
		}
		rep.problem("%s: SQLite reports damage: %s", name, strings.Join(msgs, "; "))
	}
	fk, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	n := 0
	for fk.Next() {
		n++
	}
	fk.Close()
	if n > 0 {
		rep.problem("%s: %d row(s) refer to something that no longer exists", name, n)
	}
	return nil
}

const pathCTE = `WITH RECURSIVE t(id, path) AS (
		SELECT id, slug FROM pages WHERE parent_id IS NULL
		UNION ALL SELECT p.id, t.path || '/' || p.slug FROM pages p JOIN t ON p.parent_id = t.id
	)`

func countPathMismatches(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(pathCTE + ` SELECT COUNT(*) FROM pages p
		WHERE NOT EXISTS (SELECT 1 FROM t WHERE t.id = p.id AND t.path = p.path)`).Scan(&n)
	return n, err
}

func rewritePaths(db *sql.DB) error {
	// The unique index on path forbids intermediate collisions, so go through temporary names.
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE pages SET path = '#' || id`); err != nil {
		return err
	}
	if _, err := tx.Exec(pathCTE + ` UPDATE pages SET path = (SELECT path FROM t WHERE t.id = pages.id)`); err != nil {
		return err
	}
	return tx.Commit()
}
