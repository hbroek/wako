// Package store persists wako data in SQLite.
//
// A catalog database (wako.db) lists the spaces; every space keeps its pages
// in a database of its own (spaces/<id>.db), so a space can be backed up,
// moved or deleted as a single file.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrExists        = errors.New("already exists")
	ErrVersion       = errors.New("version mismatch")
	ErrHasChildren   = errors.New("page has children")
	ErrParentMissing = errors.New("parent page does not exist")
	ErrInvalid       = errors.New("invalid")
	ErrInUse         = errors.New("asset is used by pages")
	ErrQuota         = errors.New("media quota exceeded")
	ErrLastAdmin     = errors.New("there must be at least one administrator")
)

var (
	slugRe         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	reservedSpaces = map[string]bool{"healthz": true}
)

// ImageExts are the file extensions of library images.
var ImageExts = []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg"}

// IsAssetName reports whether name looks like the file name of anything in the
// library: an image or a PDF attachment.
func IsAssetName(name string) bool {
	return IsImageName(name) || (len(name) > 4 && strings.EqualFold(name[len(name)-4:], ".pdf"))
}

// IsImageName reports whether name looks like the file name of a library image.
func IsImageName(name string) bool {
	n := strings.ToLower(name)
	for _, e := range ImageExts {
		if len(n) > len(e) && strings.HasSuffix(n, e) {
			return true
		}
	}
	return false
}

// ValidSlug reports whether s is usable as a page slug. Slugs may not start
// with "_" (reserved), end in ".md" (that suffix selects the Markdown view) or
// end in an image or ".pdf" extension (a page's images and attachments are
// served next to it, under names like shot.png, so those URLs belong to them).
func ValidSlug(s string) bool {
	return len(s) <= 100 && slugRe.MatchString(s) && !strings.HasSuffix(strings.ToLower(s), ".md") && !IsAssetName(s)
}

// SplitPath splits and validates a page path such as "docs/ops/runbook".
func SplitPath(p string) ([]string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil, nil
	}
	parts := strings.Split(p, "/")
	for _, s := range parts {
		if !ValidSlug(s) {
			return nil, fmt.Errorf("%w: bad slug %q", ErrInvalid, s)
		}
	}
	return parts, nil
}

// openDB opens a SQLite file and applies migrations. One connection
// serialises access, which keeps SQLite free of busy errors.
func openDB(path string, migrations []string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db, migrations); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB, migrations []string) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	var cur int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&cur); err != nil {
		return err
	}
	for i := cur; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version) VALUES(?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func unix(t time.Time) int64 { return t.Unix() }
