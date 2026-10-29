package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// User is an account. PasswordHash never leaves the server.
type User struct {
	ID           int64
	Name         string
	PasswordHash string
	IsAdmin      bool // server administrator: all spaces, all users
	CreatedAt    time.Time
}

// Role is what a member may do in one space.
type Role string

const (
	RoleViewer    Role = "viewer"    // read
	RoleSuggester Role = "suggester" // read, and propose changes that an editor reviews
	RoleEditor    Role = "editor"    // read, write pages and images
	RoleAdmin     Role = "admin"     // editor, plus members and settings
)

// ValidRole reports whether r is one of the roles.
func ValidRole(r Role) bool {
	return r == RoleViewer || r == RoleSuggester || r == RoleEditor || r == RoleAdmin
}

const userCols = `id, name, password_hash, is_admin, created_at`

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	var u User
	var ts int64
	if err := sc.Scan(&u.ID, &u.Name, &u.PasswordHash, &u.IsAdmin, &ts); err != nil {
		return nil, err
	}
	u.CreatedAt = time.Unix(ts, 0)
	return &u, nil
}

func one(u *User, err error) (*User, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) CreateUser(name, passwordHash string, admin bool) (*User, error) {
	now := time.Now()
	res, err := s.catalog.Exec(`INSERT INTO users(name, password_hash, is_admin, created_at) VALUES(?,?,?,?)`,
		name, passwordHash, admin, now.Unix())
	if err != nil {
		if isUnique(err) {
			return nil, ErrExists
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Name: name, PasswordHash: passwordHash, IsAdmin: admin, CreatedAt: now}, nil
}

func (s *Store) GetUser(id int64) (*User, error) {
	return one(scanUser(s.catalog.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id)))
}

// GetUserByName finds a user by name, ignoring case.
func (s *Store) GetUserByName(name string) (*User, error) {
	return one(scanUser(s.catalog.QueryRow(`SELECT `+userCols+` FROM users WHERE name = ?`, name)))
}

func (s *Store) ListUsers() ([]*User, error) {
	rows, err := s.catalog.Query(`SELECT ` + userCols + ` FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.catalog.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin = 1`).Scan(&n)
	return n, err
}

// UpdateUser changes a user's password hash and/or administrator flag. Taking
// the flag away from the last administrator fails with ErrLastAdmin. A new
// password ends all of the user's sessions.
func (s *Store) UpdateUser(id int64, passwordHash *string, admin *bool) (*User, error) {
	tx, err := s.catalog.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	u, err := one(scanUser(tx.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id)))
	if err != nil {
		return nil, err
	}
	if admin != nil && !*admin && u.IsAdmin {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin = 1`).Scan(&n); err != nil {
			return nil, err
		}
		if n <= 1 {
			return nil, ErrLastAdmin
		}
	}
	if passwordHash != nil {
		u.PasswordHash = *passwordHash
		if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
			return nil, err
		}
	}
	if admin != nil {
		u.IsAdmin = *admin
	}
	if _, err := tx.Exec(`UPDATE users SET password_hash = ?, is_admin = ? WHERE id = ?`, u.PasswordHash, u.IsAdmin, id); err != nil {
		return nil, err
	}
	return u, tx.Commit()
}

// DeleteUser removes a user with their memberships, sessions and API tokens.
// The last administrator cannot be deleted.
func (s *Store) DeleteUser(id int64) error {
	tx, err := s.catalog.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	u, err := one(scanUser(tx.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id)))
	if err != nil {
		return err
	}
	if u.IsAdmin {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin = 1`).Scan(&n); err != nil {
			return err
		}
		if n <= 1 {
			return ErrLastAdmin
		}
	}
	if _, err := tx.Exec(`DELETE FROM users WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- space roles ----

// Member is a user's role in a space.
type Member struct {
	User *User
	Role Role
}

// SetMember gives user a role in the space, replacing any earlier one.
func (s *Store) SetMember(spaceID, userID int64, role Role) error {
	if !ValidRole(role) {
		return fmt.Errorf("%w: unknown role %q", ErrInvalid, role)
	}
	_, err := s.catalog.Exec(`INSERT INTO space_members(space_id, user_id, role) VALUES(?,?,?)
		ON CONFLICT(space_id, user_id) DO UPDATE SET role = excluded.role`, spaceID, userID, string(role))
	return err
}

func (s *Store) RemoveMember(spaceID, userID int64) error {
	res, err := s.catalog.Exec(`DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, spaceID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListMembers(spaceID int64) ([]Member, error) {
	rows, err := s.catalog.Query(`SELECT u.id, u.name, u.password_hash, u.is_admin, u.created_at, m.role
		FROM space_members m JOIN users u ON u.id = m.user_id WHERE m.space_id = ? ORDER BY u.name`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var u User
		var ts int64
		var role string
		if err := rows.Scan(&u.ID, &u.Name, &u.PasswordHash, &u.IsAdmin, &ts, &role); err != nil {
			return nil, err
		}
		u.CreatedAt = time.Unix(ts, 0)
		out = append(out, Member{&u, Role(role)})
	}
	return out, rows.Err()
}

// UserRoles returns the user's role in every space they are a member of.
func (s *Store) UserRoles(userID int64) (map[int64]Role, error) {
	rows, err := s.catalog.Query(`SELECT space_id, role FROM space_members WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Role{}
	for rows.Next() {
		var id int64
		var role string
		if err := rows.Scan(&id, &role); err != nil {
			return nil, err
		}
		out[id] = Role(role)
	}
	return out, rows.Err()
}

// ---- sessions ----

// CreateSession starts a session whose refresh token has the given hash.
func (s *Store) CreateSession(userID int64, refreshHash string, now, expires time.Time) (int64, error) {
	s.catalog.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, now.Unix()) // housekeeping
	res, err := s.catalog.Exec(`INSERT INTO sessions(user_id, refresh_hash, created_at, expires_at) VALUES(?,?,?,?)`,
		userID, refreshHash, now.Unix(), expires.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SessionUser returns the owner of a live session.
func (s *Store) SessionUser(sid int64, now time.Time) (*User, error) {
	return one(scanUser(s.catalog.QueryRow(`SELECT u.id, u.name, u.password_hash, u.is_admin, u.created_at
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id = ? AND s.expires_at > ?`, sid, now.Unix())))
}

// RefreshSession exchanges a refresh token. The token in use is replaced by
// newHash and the old one stays valid for grace, so requests that were already
// in flight with it (a page and its images, say) still succeed; those get
// rotated == false and must not expect a new refresh token.
func (s *Store) RefreshSession(oldHash, newHash string, now, expires time.Time, grace time.Duration) (sid int64, u *User, rotated bool, err error) {
	tx, err := s.catalog.Begin()
	if err != nil {
		return 0, nil, false, err
	}
	defer tx.Rollback()
	err = tx.QueryRow(`SELECT id, user_id FROM sessions WHERE refresh_hash = ? AND expires_at > ?`, oldHash, now.Unix()).Scan(&sid, new(int64))
	if err == nil {
		if _, err := tx.Exec(`UPDATE sessions SET prev_hash = refresh_hash, prev_until = ?, refresh_hash = ?, expires_at = ? WHERE id = ?`,
			now.Add(grace).Unix(), newHash, expires.Unix(), sid); err != nil {
			return 0, nil, false, err
		}
		rotated = true
	} else if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRow(`SELECT id FROM sessions WHERE prev_hash = ? AND prev_until > ? AND expires_at > ?`,
			oldHash, now.Unix(), now.Unix()).Scan(&sid)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil, false, ErrNotFound
		}
	}
	if err != nil {
		return 0, nil, false, err
	}
	u, err = one(scanUser(tx.QueryRow(`SELECT u.id, u.name, u.password_hash, u.is_admin, u.created_at
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id = ?`, sid)))
	if err != nil {
		return 0, nil, false, err
	}
	return sid, u, rotated, tx.Commit()
}

func (s *Store) DeleteSession(sid int64) error {
	_, err := s.catalog.Exec(`DELETE FROM sessions WHERE id = ?`, sid)
	return err
}

func (s *Store) DeleteUserSessions(userID int64) error {
	_, err := s.catalog.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// ---- API tokens ----

// APIToken describes a token; the secret itself is never stored.
type APIToken struct {
	ID          int64
	Name        string
	CreatedAt   time.Time
	SuggestOnly bool // the token can read and suggest, but never write directly, whatever its owner may do
}

// CreateToken makes a token that has all of its owner's rights.
func (s *Store) CreateToken(userID int64, name, tokenHash string) (*APIToken, error) {
	return s.CreateTokenWith(userID, name, tokenHash, false)
}

// CreateTokenWith makes a token; with suggestOnly it can only propose changes.
func (s *Store) CreateTokenWith(userID int64, name, tokenHash string, suggestOnly bool) (*APIToken, error) {
	now := time.Now()
	res, err := s.catalog.Exec(`INSERT INTO api_tokens(user_id, name, token_hash, created_at, suggest_only) VALUES(?,?,?,?,?)`,
		userID, name, tokenHash, now.Unix(), suggestOnly)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &APIToken{ID: id, Name: name, CreatedAt: now, SuggestOnly: suggestOnly}, nil
}

// TokenUser returns the owner of the token with the given hash.
func (s *Store) TokenUser(tokenHash string) (*User, error) {
	u, _, err := s.LookupToken(tokenHash)
	return u, err
}

// LookupToken returns the token with the given hash and its owner.
func (s *Store) LookupToken(tokenHash string) (*User, *APIToken, error) {
	var u User
	var t APIToken
	var uts, tts int64
	err := s.catalog.QueryRow(`SELECT u.id, u.name, u.password_hash, u.is_admin, u.created_at, t.id, t.name, t.created_at, t.suggest_only
		FROM api_tokens t JOIN users u ON u.id = t.user_id WHERE t.token_hash = ?`, tokenHash).
		Scan(&u.ID, &u.Name, &u.PasswordHash, &u.IsAdmin, &uts, &t.ID, &t.Name, &tts, &t.SuggestOnly)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	u.CreatedAt, t.CreatedAt = time.Unix(uts, 0), time.Unix(tts, 0)
	return &u, &t, nil
}

func (s *Store) ListTokens(userID int64) ([]*APIToken, error) {
	rows, err := s.catalog.Query(`SELECT id, name, created_at, suggest_only FROM api_tokens WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIToken
	for rows.Next() {
		var t APIToken
		var ts int64
		if err := rows.Scan(&t.ID, &t.Name, &ts, &t.SuggestOnly); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(ts, 0)
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteToken(userID, id int64) error {
	res, err := s.catalog.Exec(`DELETE FROM api_tokens WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- settings ----

// Setting returns a stored setting, or "" if it is not set.
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.catalog.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSettingIfAbsent stores value unless the key is already set, and returns
// the value that is stored afterwards.
func (s *Store) SetSettingIfAbsent(key, value string) (string, error) {
	if _, err := s.catalog.Exec(`INSERT OR IGNORE INTO settings(key, value) VALUES(?,?)`, key, value); err != nil {
		return "", err
	}
	return s.Setting(key)
}
