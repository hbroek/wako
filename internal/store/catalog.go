package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Store is the catalog of spaces. Later phases add users and memberships here.
type Store struct {
	dir     string
	catalog *sql.DB

	// Quota is the most media (stored bytes) one space may hold; negative means
	// no limit. Set it before serving.
	Quota int64

	// Page history. A save by the same author within RevisionWindow of their last
	// one replaces that revision instead of adding another, so a burst of Ctrl+S
	// is one entry. RevisionKeep caps the revisions kept per page (0: no cap).
	RevisionWindow time.Duration
	RevisionKeep   int

	// Now is the clock; tests replace it.
	Now func() time.Time

	mu  sync.Mutex
	dbs map[int64]*sql.DB // open space databases by space id
}

// Space is one wiki. Page operations act on the space's own database.
type Space struct {
	ID         int64
	Slug       string
	Title      string
	PublicRead bool
	AIEnabled  bool // the AI features (ask, draft) may use this space's pages
	CreatedAt  time.Time

	db    *sql.DB
	quota int64
	st    *Store
}

func (sp *Space) now() time.Time { return sp.st.Now() }

// Now is the store's clock.
func (sp *Space) Now() time.Time { return sp.st.Now() }

// DefaultQuota is the media quota of a space unless configured otherwise.
const DefaultQuota = 200 << 20

var catalogMigrations = []string{
	`CREATE TABLE spaces(
		id INTEGER PRIMARY KEY,
		slug TEXT NOT NULL UNIQUE,
		title TEXT NOT NULL,
		public_read INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL
	);`,
	// Users, space roles, sessions and API tokens.
	`CREATE TABLE users(
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE COLLATE NOCASE,
		password_hash TEXT NOT NULL,
		is_admin INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE space_members(
		space_id INTEGER NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		role TEXT NOT NULL CHECK (role IN ('viewer', 'editor', 'admin')),
		PRIMARY KEY (space_id, user_id)
	);
	CREATE INDEX space_members_user ON space_members(user_id);
	CREATE TABLE sessions(
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		refresh_hash TEXT NOT NULL UNIQUE,
		prev_hash TEXT,            -- the previous refresh token, honoured briefly after rotation
		prev_until INTEGER,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	);
	CREATE INDEX sessions_prev ON sessions(prev_hash);
	CREATE TABLE api_tokens(
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name TEXT NOT NULL,
		token_hash TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);`,
	// Suggesters (read, and propose changes that an editor reviews), tokens that
	// can only suggest, and a per-space switch for the AI features.
	`CREATE TABLE space_members_new(
		space_id INTEGER NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		role TEXT NOT NULL CHECK (role IN ('viewer', 'suggester', 'editor', 'admin')),
		PRIMARY KEY (space_id, user_id)
	);
	INSERT INTO space_members_new SELECT space_id, user_id, role FROM space_members;
	DROP TABLE space_members;
	ALTER TABLE space_members_new RENAME TO space_members;
	CREATE INDEX space_members_user ON space_members(user_id);
	ALTER TABLE api_tokens ADD COLUMN suggest_only INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE spaces ADD COLUMN ai_enabled INTEGER NOT NULL DEFAULT 0;`,
}

// Open opens the catalog at dir/wako.db; spaces live under dir/spaces/.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "spaces"), 0o700); err != nil {
		return nil, err
	}
	cat, err := openDB(filepath.Join(dir, "wako.db"), catalogMigrations)
	if err != nil {
		return nil, err
	}
	return &Store{dir: dir, catalog: cat, dbs: map[int64]*sql.DB{}, Quota: DefaultQuota, RevisionWindow: 10 * time.Minute, RevisionKeep: 100, Now: time.Now}, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, db := range s.dbs {
		db.Close()
	}
	return s.catalog.Close()
}

func (s *Store) spacePath(id int64) string {
	return filepath.Join(s.dir, "spaces", fmt.Sprintf("%d.db", id))
}

// attach returns the (cached) database of the space with the given id.
func (s *Store) attach(id int64) (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db, ok := s.dbs[id]; ok {
		return db, nil
	}
	db, err := openDB(s.spacePath(id), spaceMigrations)
	if err != nil {
		return nil, err
	}
	if err := backfillDimensions(db); err != nil {
		db.Close()
		return nil, err
	}
	s.dbs[id] = db
	return db, nil
}

func (s *Store) CreateSpace(slug, title string, publicRead bool) (*Space, error) {
	if !ValidSlug(slug) || reservedSpaces[strings.ToLower(slug)] {
		return nil, fmt.Errorf("%w: bad space slug %q", ErrInvalid, slug)
	}
	if title == "" {
		title = slug
	}
	now := time.Now()
	res, err := s.catalog.Exec(`INSERT INTO spaces(slug, title, public_read, created_at) VALUES(?,?,?,?)`,
		slug, title, publicRead, unix(now))
	if err != nil {
		if isUnique(err) {
			return nil, ErrExists
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	db, err := s.attach(id)
	if err != nil {
		s.catalog.Exec(`DELETE FROM spaces WHERE id = ?`, id)
		return nil, err
	}
	return &Space{ID: id, Slug: slug, Title: title, PublicRead: publicRead, CreatedAt: now, db: db, quota: s.Quota, st: s}, nil
}

func (s *Store) scanSpace(sc interface{ Scan(...any) error }) (*Space, error) {
	var sp Space
	var ts int64
	if err := sc.Scan(&sp.ID, &sp.Slug, &sp.Title, &sp.PublicRead, &ts, &sp.AIEnabled); err != nil {
		return nil, err
	}
	sp.CreatedAt = time.Unix(ts, 0)
	db, err := s.attach(sp.ID)
	if err != nil {
		return nil, err
	}
	sp.db, sp.quota, sp.st = db, s.Quota, s
	return &sp, nil
}

func (s *Store) GetSpace(slug string) (*Space, error) {
	sp, err := s.scanSpace(s.catalog.QueryRow(
		`SELECT id, slug, title, public_read, created_at, ai_enabled FROM spaces WHERE slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sp, err
}

func (s *Store) ListSpaces() ([]*Space, error) {
	rows, err := s.catalog.Query(`SELECT id, slug, title, public_read, created_at, ai_enabled FROM spaces ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Space
	for rows.Next() {
		sp, err := s.scanSpace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// DeleteSpace removes the space and its database files.
func (s *Store) DeleteSpace(slug string) error {
	var id int64
	err := s.catalog.QueryRow(`SELECT id FROM spaces WHERE slug = ?`, slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := s.catalog.Exec(`DELETE FROM spaces WHERE id = ?`, id); err != nil {
		return err
	}
	s.mu.Lock()
	if db, ok := s.dbs[id]; ok {
		db.Close()
		delete(s.dbs, id)
	}
	s.mu.Unlock()
	p := s.spacePath(id)
	for _, f := range []string{p, p + "-wal", p + "-shm"} {
		os.Remove(f)
	}
	return nil
}

// SpaceUpdate lists the fields UpdateSpace changes; nil fields stay as they are.
type SpaceUpdate struct {
	Slug       *string
	Title      *string
	PublicRead *bool
	AIEnabled  *bool
}

// UpdateSpace changes a space's slug, title or visibility. The database file is
// named by id, so renaming the slug does not move any data.
func (s *Store) UpdateSpace(slug string, u SpaceUpdate) (*Space, error) {
	sp, err := s.GetSpace(slug)
	if err != nil {
		return nil, err
	}
	if u.Slug != nil {
		if !ValidSlug(*u.Slug) || reservedSpaces[strings.ToLower(*u.Slug)] {
			return nil, fmt.Errorf("%w: bad space slug %q", ErrInvalid, *u.Slug)
		}
		sp.Slug = *u.Slug
	}
	if u.Title != nil {
		if strings.TrimSpace(*u.Title) == "" {
			return nil, fmt.Errorf("%w: empty title", ErrInvalid)
		}
		sp.Title = *u.Title
	}
	if u.PublicRead != nil {
		sp.PublicRead = *u.PublicRead
	}
	if u.AIEnabled != nil {
		sp.AIEnabled = *u.AIEnabled
	}
	if _, err := s.catalog.Exec(`UPDATE spaces SET slug = ?, title = ?, public_read = ?, ai_enabled = ? WHERE id = ?`,
		sp.Slug, sp.Title, sp.PublicRead, sp.AIEnabled, sp.ID); err != nil {
		if isUnique(err) {
			return nil, ErrExists
		}
		return nil, err
	}
	return sp, nil
}
