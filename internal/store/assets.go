package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hbroek/wako/internal/img"
)

// Asset is an uploaded file (an image) in a space's media library. Library
// names are unique within a space.
type Asset struct {
	ID        string // hex sha256 of Data
	Name      string
	Mime      string
	Size      int64
	Width     int // pixels; 0 if unknown
	Height    int
	Data      []byte
	CreatedAt time.Time
}

// AssetInfo describes a library entry without its bytes.
type AssetInfo struct {
	ID        string
	Name      string
	Mime      string
	Size      int64
	Width     int // pixels; 0 if unknown
	Height    int
	CreatedAt time.Time
	UsedBy    int // pages bound to it
}

// syncImageBindings makes the page's image bindings match the names its
// Markdown uses. A name that is already bound stays bound to the same asset,
// even if the library changed since. A new name binds to the library asset of
// that name; names with no such asset stay unbound.
func syncImageBindings(q queryer, pageID int64, names []string) error {
	rows, err := q.Query(`SELECT name FROM page_assets WHERE page_id = ?`, pageID)
	if err != nil {
		return err
	}
	bound := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		bound[n] = true
	}
	rows.Close()

	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
		if bound[n] {
			continue
		}
		if _, err := q.Exec(`INSERT OR IGNORE INTO page_assets(page_id, name, asset_id)
			SELECT ?, name, id FROM assets WHERE name = ?`, pageID, n); err != nil {
			return err
		}
	}
	for n := range bound {
		if !want[n] {
			if _, err := q.Exec(`DELETE FROM page_assets WHERE page_id = ? AND name = ?`, pageID, n); err != nil {
				return err
			}
		}
	}
	return nil
}

// Bindings returns the page's image names and the library assets they point to.
func (sp *Space) Bindings(pageID int64) (map[string]*AssetInfo, error) {
	rows, err := sp.db.Query(`SELECT b.name, a.id, a.name, a.mime, a.size, a.width, a.height, a.created_at
		FROM page_assets b JOIN assets a ON a.id = b.asset_id WHERE b.page_id = ?`, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*AssetInfo{}
	for rows.Next() {
		var as string
		var a AssetInfo
		var ts int64
		if err := rows.Scan(&as, &a.ID, &a.Name, &a.Mime, &a.Size, &a.Width, &a.Height, &ts); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(ts, 0)
		out[as] = &a
	}
	return out, rows.Err()
}

// FindAssetByName looks a library asset up by its name.
func (sp *Space) FindAssetByName(name string) (*AssetInfo, error) {
	var a AssetInfo
	var ts int64
	err := sp.db.QueryRow(`SELECT id, name, mime, size, width, height, created_at FROM assets WHERE name = ?`, name).
		Scan(&a.ID, &a.Name, &a.Mime, &a.Size, &a.Width, &a.Height, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = time.Unix(ts, 0)
	return &a, nil
}

// AssetInfo describes one library entry without reading its bytes.
func (sp *Space) AssetInfo(id string) (*AssetInfo, error) {
	var a AssetInfo
	var ts int64
	err := sp.db.QueryRow(`SELECT id, name, mime, size, width, height, created_at FROM assets WHERE id = ?`, id).
		Scan(&a.ID, &a.Name, &a.Mime, &a.Size, &a.Width, &a.Height, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = time.Unix(ts, 0)
	return &a, nil
}

// AssetUsage reports how much media the space stores, and its quota (negative
// means unlimited).
func (sp *Space) AssetUsage() (count int, bytes, quota int64, err error) {
	err = sp.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM assets`).Scan(&count, &bytes)
	return count, bytes, sp.quota, err
}

// ListAssets returns the media library, newest first. q filters by name.
func (sp *Space) ListAssets(q string, limit int) ([]*AssetInfo, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	rows, err := sp.db.Query(`SELECT a.id, a.name, a.mime, a.size, a.width, a.height, a.created_at,
			(SELECT COUNT(*) FROM page_assets b WHERE b.asset_id = a.id)
		FROM assets a WHERE a.name LIKE ? ESCAPE '\' ORDER BY a.created_at DESC, a.name LIMIT ?`, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AssetInfo
	for rows.Next() {
		var a AssetInfo
		var ts int64
		if err := rows.Scan(&a.ID, &a.Name, &a.Mime, &a.Size, &a.Width, &a.Height, &ts, &a.UsedBy); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(ts, 0)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// PutAsset adds an image to the library. Identical content is stored once, and
// the existing entry (with its existing name) is returned. A name that is
// already taken by different content gets a numeric suffix: shot.png becomes
// shot-2.png. The space's quota is checked inside the INSERT, so concurrent
// uploads cannot overshoot it.
func (sp *Space) PutAsset(name, mime string, data []byte, width, height int) (a *Asset, created bool, err error) {
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:])
	if existing, err := sp.GetAsset(id); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	stem, ext := name, ""
	if i := strings.LastIndex(name, "."); i > 0 {
		stem, ext = name[:i], name[i:]
	}
	now := time.Now()
	for n := 1; n < 1000; n++ {
		candidate := name
		if n > 1 {
			candidate = fmt.Sprintf("%s-%d%s", stem, n, ext)
		}
		res, err := sp.db.Exec(`INSERT INTO assets(id, name, mime, size, width, height, data, created_at)
			SELECT ?,?,?,?,?,?,?,?
			WHERE ? < 0 OR (SELECT COALESCE(SUM(size), 0) FROM assets) + ? <= ?`,
			id, candidate, mime, len(data), width, height, data, now.Unix(),
			sp.quota, len(data), sp.quota)
		if err == nil {
			if n, _ := res.RowsAffected(); n == 0 {
				return nil, false, ErrQuota
			}
			return &Asset{ID: id, Name: candidate, Mime: mime, Size: int64(len(data)),
				Width: width, Height: height, Data: data, CreatedAt: now}, true, nil
		}
		if !isUnique(err) {
			return nil, false, err
		}
	}
	return nil, false, fmt.Errorf("%w: no free name for %q", ErrExists, name)
}

// backfillDimensions fills in width and height for assets stored before they
// were recorded. SVGs without numeric sizes stay at 0 and are skipped.
func backfillDimensions(db *sql.DB) error {
	rows, err := db.Query(`SELECT id, mime, data FROM assets WHERE width = 0 AND mime != 'image/svg+xml'`)
	if err != nil {
		return err
	}
	type dim struct {
		id   string
		w, h int
	}
	var todo []dim
	for rows.Next() {
		var id, mime string
		var data []byte
		if err := rows.Scan(&id, &mime, &data); err != nil {
			rows.Close()
			return err
		}
		if w, h := img.Dimensions(mime, data); w > 0 {
			todo = append(todo, dim{id, w, h})
		}
	}
	rows.Close()
	for _, d := range todo {
		if _, err := db.Exec(`UPDATE assets SET width = ?, height = ? WHERE id = ?`, d.w, d.h, d.id); err != nil {
			return err
		}
	}
	return nil
}

func (sp *Space) GetAsset(id string) (*Asset, error) {
	var a Asset
	var ts int64
	err := sp.db.QueryRow(`SELECT id, name, mime, size, width, height, data, created_at FROM assets WHERE id = ?`, id).
		Scan(&a.ID, &a.Name, &a.Mime, &a.Size, &a.Width, &a.Height, &a.Data, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = time.Unix(ts, 0)
	return &a, nil
}

// DeleteAsset removes an asset. Assets that pages still use need force.
func (sp *Space) DeleteAsset(id string, force bool) error {
	var used int
	if err := sp.db.QueryRow(`SELECT COUNT(*) FROM page_assets WHERE asset_id = ?`, id).Scan(&used); err != nil {
		return err
	}
	if used > 0 && !force {
		return ErrInUse
	}
	res, err := sp.db.Exec(`DELETE FROM assets WHERE id = ?`, id) // page_assets cascades
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GCAssets deletes assets that no page uses and that are older than minAge
// (so uploads for a page still being edited survive).
func (sp *Space) GCAssets(minAge time.Duration) (count int, bytes int64, err error) {
	cutoff := time.Now().Add(-minAge).Unix()
	err = sp.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM assets
		WHERE created_at <= ? AND id NOT IN (SELECT asset_id FROM page_assets)`, cutoff).Scan(&count, &bytes)
	if err != nil || count == 0 {
		return 0, 0, err
	}
	_, err = sp.db.Exec(`DELETE FROM assets
		WHERE created_at <= ? AND id NOT IN (SELECT asset_id FROM page_assets)`, cutoff)
	return count, bytes, err
}

// RewriteFunc renames the image reference oldName to newName inside a page's
// Markdown. It returns false when it cannot do so safely.
type RewriteFunc func(body, oldName, newName string) (string, bool)

// RenameResult says what a rename did to the pages that use the image.
type RenameResult struct {
	Asset   *AssetInfo
	Updated int // pages whose Markdown now uses the new name
	Aliased int // pages that keep the old name, bound as an alias
}

// RenameAsset gives a library image a new name and renames it in every page
// that uses it, all in one transaction. A page whose Markdown rewrite fails
// keeps working: its binding stays under the old name, which now acts as an
// alias for the image. Rewritten pages get a new version, so editors that had
// the old text open see a conflict instead of overwriting the rename.
func (sp *Space) RenameAsset(id, name string, rewrite RewriteFunc, by string) (*RenameResult, error) {
	tx, err := sp.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var old string
	if err := tx.QueryRow(`SELECT name FROM assets WHERE id = ?`, id).Scan(&old); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	res := &RenameResult{}
	if old != name {
		var taken int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM assets WHERE name = ? AND id != ?`, name, id).Scan(&taken); err != nil {
			return nil, err
		}
		if taken > 0 {
			return nil, ErrExists
		}
		if _, err := tx.Exec(`UPDATE assets SET name = ? WHERE id = ?`, name, id); err != nil {
			return nil, err
		}

		type use struct {
			page    int64
			path    string
			title   string
			body    string
			binding string
		}
		rows, err := tx.Query(`SELECT p.id, p.path, p.title, p.body, b.name FROM page_assets b
			JOIN pages p ON p.id = b.page_id WHERE b.asset_id = ?`, id)
		if err != nil {
			return nil, err
		}
		var uses []use
		for rows.Next() {
			var u use
			if err := rows.Scan(&u.page, &u.path, &u.title, &u.body, &u.binding); err != nil {
				rows.Close()
				return nil, err
			}
			uses = append(uses, u)
		}
		rows.Close()

		now := sp.now().Unix()
		for _, u := range uses {
			newBody, ok := rewrite(u.body, u.binding, name)
			if !ok {
				res.Aliased++
				continue
			}
			// Rename the binding first: if the page already binds the new name to
			// something else, keep the alias and leave its Markdown alone.
			if _, err := tx.Exec(`UPDATE page_assets SET name = ? WHERE page_id = ? AND name = ?`,
				name, u.page, u.binding); err != nil {
				if !isUnique(err) {
					return nil, err
				}
				res.Aliased++
				continue
			}
			if newBody != u.body {
				if _, err := sp.writeBody(tx, u.page, u.path, u.title, newBody, by, "edited",
					fmt.Sprintf("image renamed from %s to %s", old, name), false, now); err != nil {
					return nil, err
				}
				res.Updated++
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if res.Asset, err = sp.AssetInfo(id); err != nil {
		return nil, err
	}
	return res, nil
}

// SiblingImage finds the image a page in directory dir ("" is the space root)
// uses under name: the image appears at /{space}/{dir}/{name}, next to those
// pages, although only one copy is stored. If sibling pages use the same name
// for different images, the page that sorts first wins.
func (sp *Space) SiblingImage(dir, name string) (*AssetInfo, error) {
	var (
		a   AssetInfo
		ts  int64
		row *sql.Row
	)
	const cols = `SELECT a.id, a.name, a.mime, a.size, a.width, a.height, a.created_at
		FROM page_assets b JOIN pages p ON p.id = b.page_id JOIN assets a ON a.id = b.asset_id `
	if dir == "" {
		row = sp.db.QueryRow(cols+`WHERE p.parent_id IS NULL AND b.name = ? ORDER BY p.path LIMIT 1`, name)
	} else {
		row = sp.db.QueryRow(cols+`JOIN pages d ON d.id = p.parent_id WHERE d.path = ? AND b.name = ? ORDER BY p.path LIMIT 1`, dir, name)
	}
	err := row.Scan(&a.ID, &a.Name, &a.Mime, &a.Size, &a.Width, &a.Height, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = time.Unix(ts, 0)
	return &a, nil
}

// PageImage records that the page at PagePath uses the library image AssetID
// under the name Name in its Markdown.
type PageImage struct {
	PagePath string
	Name     string
	AssetID  string
}

// PageImages lists every image binding in the space, ordered by page.
func (sp *Space) PageImages() ([]PageImage, error) {
	rows, err := sp.db.Query(`SELECT p.path, b.name, b.asset_id FROM page_assets b
		JOIN pages p ON p.id = b.page_id ORDER BY p.path, b.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PageImage
	for rows.Next() {
		var u PageImage
		if err := rows.Scan(&u.PagePath, &u.Name, &u.AssetID); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// AllAssets lists the whole media library, by name, without the image data.
func (sp *Space) AllAssets() ([]*AssetInfo, error) {
	rows, err := sp.db.Query(`SELECT id, name, mime, size, width, height, created_at,
			(SELECT COUNT(*) FROM page_assets b WHERE b.asset_id = assets.id)
		FROM assets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AssetInfo
	for rows.Next() {
		var a AssetInfo
		var ts int64
		if err := rows.Scan(&a.ID, &a.Name, &a.Mime, &a.Size, &a.Width, &a.Height, &ts, &a.UsedBy); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(ts, 0)
		out = append(out, &a)
	}
	return out, rows.Err()
}
