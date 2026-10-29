package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
)

// Backup writes a consistent copy of every database to dir, which must not
// exist or be empty. It uses SQLite's VACUUM INTO, so it is safe while the
// server is running and the copies are compact. The result is a data directory:
// start a server on it with -data-dir. It contains password hashes and the
// signing key, so it should be kept private.
func (s *Store) Backup(dir string) (err error) {
	if entries, rerr := os.ReadDir(dir); rerr == nil && len(entries) > 0 {
		return fmt.Errorf("backup directory %s is not empty", dir)
	}
	if err := os.MkdirAll(filepath.Join(dir, "spaces"), 0o700); err != nil {
		return err
	}
	defer func() {
		if err != nil { // leave nothing half-done behind
			os.RemoveAll(filepath.Join(dir, "spaces"))
			os.Remove(filepath.Join(dir, "wako.db"))
		}
	}()
	vacuum := func(db *sql.DB, to string) error {
		if _, err := db.Exec(`VACUUM INTO ?`, to); err != nil {
			return fmt.Errorf("copying to %s: %w", to, err)
		}
		return nil
	}
	if err := vacuum(s.catalog, filepath.Join(dir, "wako.db")); err != nil {
		return err
	}
	spaces, err := s.ListSpaces()
	if err != nil {
		return err
	}
	for _, sp := range spaces {
		if err := vacuum(sp.db, s.spaceCopyPath(dir, sp.ID)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) spaceCopyPath(dir string, id int64) string {
	return filepath.Join(dir, "spaces", fmt.Sprintf("%d.db", id))
}
