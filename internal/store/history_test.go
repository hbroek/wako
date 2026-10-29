package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hbroek/wako/internal/wiki"
)

type hist struct {
	t   *testing.T
	st  *Store
	sp  *Space
	now time.Time
}

func newHist(t *testing.T) *hist {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &hist{t: t, st: st, now: time.Unix(1_800_000_000, 0)}
	st.Now = func() time.Time { return h.now }
	h.sp, _ = st.CreateSpace("main", "Main", true)
	return h
}

func (h *hist) put(path, body, by string, mods ...func(*Cond)) *Page {
	h.t.Helper()
	c := Cond{By: by, Coalesce: true}
	for _, m := range mods {
		m(&c)
	}
	p, _, err := h.sp.PutPage(path, path, body, Refs{}, c)
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *hist) wait(d time.Duration) { h.now = h.now.Add(d) }

func (h *hist) revs(path string) []*Revision {
	h.t.Helper()
	p, err := h.sp.GetPage(path)
	if err != nil {
		h.t.Fatal(err)
	}
	r, err := h.sp.Revisions(p.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func versions(rs []*Revision) string {
	var s []string
	for _, r := range rs {
		s = append(s, fmt.Sprintf("%d:%s:%s", r.Version, r.Author, r.Action))
	}
	return strings.Join(s, " ")
}

func TestRevisionsCoalesceBurstsOfSaves(t *testing.T) {
	h := newHist(t)
	h.put("p", "one", "alice") // created, version 1
	h.wait(time.Minute)
	h.put("p", "two", "alice") // same author, soon: replaces the revision
	h.wait(time.Minute)
	h.put("p", "three", "alice")
	if got := versions(h.revs("p")); got != "3:alice:created" {
		t.Fatalf("a burst of saves is one revision: %s", got)
	}
	if r := h.revs("p")[0]; r.Size != len("three") || r.At != h.now {
		t.Errorf("revision: %+v", r)
	}
	p, _ := h.sp.GetPage("p")
	if rev, err := h.sp.Revision(p.ID, 3); err != nil || rev.Body != "three" || rev.Title != "p" {
		t.Fatalf("revision text: %+v %v", rev, err)
	}
	if _, err := h.sp.Revision(p.ID, 2); err != ErrNotFound {
		t.Errorf("a version that was merged away must be gone: %v", err)
	}

	h.wait(time.Minute)
	h.put("p", "four", "bob") // another author: a new revision
	h.wait(time.Minute)
	h.put("p", "five", "alice") // alice again, but the latest revision is bob's
	h.wait(11 * time.Minute)
	h.put("p", "six", "alice") // outside the window
	h.wait(time.Minute)
	h.put("p", "seven", "alice", func(c *Cond) { c.Action, c.Note = "restored", "restored revision 3" })
	h.wait(time.Minute)
	h.put("p", "eight", "alice") // not merged into a restore
	if got := versions(h.revs("p")); got != "8:alice:edited 7:alice:restored 6:alice:edited 5:alice:edited 4:bob:edited 3:alice:created" {
		t.Fatalf("history: %s", got)
	}
	// Lookups.
	p, _ = h.sp.GetPage("p")
	prev, err := h.sp.RevisionBefore(p.ID, 6)
	if err != nil || prev.Version != 5 || prev.Body != "five" {
		t.Fatalf("revision before 6: %+v %v", prev, err)
	}
	if _, err := h.sp.RevisionBefore(p.ID, 3); err != ErrNotFound {
		t.Errorf("before the first: %v", err)
	}
	if r, _ := h.sp.Revision(p.ID, 7); r.Note != "restored revision 3" || r.Body != "seven" {
		t.Errorf("restore revision: %+v", r)
	}
}

func TestRevisionWindowCanBeSwitchedOff(t *testing.T) {
	h := newHist(t)
	h.st.RevisionWindow = 0
	h.put("p", "1", "alice")
	h.put("p", "2", "alice")
	h.put("p", "3", "alice")
	if got := versions(h.revs("p")); got != "3:alice:edited 2:alice:edited 1:alice:created" {
		t.Fatalf("every save separate: %s", got)
	}
}

func TestRevisionRetention(t *testing.T) {
	h := newHist(t)
	h.st.RevisionWindow, h.st.RevisionKeep = 0, 3
	for i := 1; i <= 7; i++ {
		h.put("p", fmt.Sprint("v", i), "alice")
		h.put("other", fmt.Sprint("o", i), "alice")
	}
	if got := versions(h.revs("p")); got != "7:alice:edited 6:alice:edited 5:alice:edited" {
		t.Fatalf("keep 3: %s", got)
	}
	if n := len(h.revs("other")); n != 3 { // each page has its own allowance
		t.Errorf("other page: %d", n)
	}
	p, _ := h.sp.GetPage("p")
	if p.Body != "v7" {
		t.Error("the page itself must not be affected")
	}
	h.st.RevisionKeep = 0 // no cap
	for i := 0; i < 10; i++ {
		h.put("p", fmt.Sprint("more", i), "alice")
	}
	if n := len(h.revs("p")); n != 13 {
		t.Errorf("unlimited: %d", n)
	}
}

func TestChangeLog(t *testing.T) {
	h := newHist(t)
	h.put("docs", "# Docs", "alice")
	h.wait(time.Minute)
	h.put("docs", "# Docs v2", "alice") // merged into the "created" entry
	h.wait(time.Minute)
	h.put("docs/ops", "# Ops", "bob")
	h.put("docs/dev", "# Dev", "bob")
	h.wait(time.Minute)
	h.put("docs", "# Docs v3", "carol")
	h.wait(time.Minute)
	if _, _, err := h.sp.MovePageRewriting("docs/ops", "ops", nil, "alice"); err != nil {
		t.Fatal(err)
	}
	h.wait(time.Minute)
	if err := h.sp.DeletePage("docs", true, nil, "bob"); err != nil {
		t.Fatal(err)
	}
	var got []string
	changes, err := h.sp.RecentChanges(50)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		got = append(got, fmt.Sprintf("%s %s by %s v%d", c.Action, c.Path, c.Author, c.Version))
	}
	want := []string{
		"deleted docs by bob v3", "moved ops by alice v2", "edited docs by carol v3",
		"created docs/dev by bob v1", "created docs/ops by bob v1", "created docs by alice v2",
	}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Fatalf("change log:\n got %s\nwant %s", strings.Join(got, "; "), strings.Join(want, "; "))
	}
	del, mov := changes[0], changes[1]
	if del.PageID != 0 || !strings.Contains(del.Note, "1 page below") || del.Title != "docs" {
		t.Errorf("deleted entry: %+v", del)
	}
	if mov.OldPath != "docs/ops" || mov.Path != "ops" || mov.PageID == 0 {
		t.Errorf("moved entry: %+v", mov)
	}
	if limited, _ := h.sp.RecentChanges(2); len(limited) != 2 || limited[0].ID != del.ID {
		t.Errorf("limit: %+v", limited)
	}
	// Nothing in the log points at a page id any more, so a new page cannot inherit it.
	h.put("fresh", "new page", "dave")
	again, _ := h.sp.RecentChanges(50)
	for _, c := range again {
		if c.Action == "deleted" && c.PageID != 0 {
			t.Errorf("deleted entry keeps an id: %+v", c)
		}
		if c.Path == "docs/dev" && c.PageID != 0 {
			t.Errorf("entry of a page that was deleted with its parent keeps an id: %+v", c)
		}
	}
	// The history of a deleted page goes with it; the log does not.
	if _, err := h.sp.GetPage("docs"); err != ErrNotFound {
		t.Fatal("page still there")
	}
}

func TestChangeLogIsBounded(t *testing.T) {
	h := newHist(t)
	h.st.RevisionWindow, h.st.RevisionKeep = 0, 1
	for i := 0; i < changesKept+40; i++ {
		h.put("p", fmt.Sprint(i), "alice")
	}
	all, _ := h.sp.RecentChanges(changesKept + 500)
	if len(all) != changesKept {
		t.Fatalf("kept %d entries, want %d", len(all), changesKept)
	}
	if all[0].Version != int64(changesKept+40) {
		t.Errorf("newest entry: %+v", all[0])
	}
}

func TestRewritesAreRecordedInTheHistory(t *testing.T) {
	h := newHist(t)
	h.put("target", "# Target", "alice")
	if _, _, err := h.sp.PutPage("linker", "linker", "see [t](target)", Refs{Links: []string{"target"}}, Cond{By: "alice"}); err != nil {
		t.Fatal(err)
	}
	h.wait(time.Hour)
	// Moving "target" rewrites the link in "linker"; Bob made the move.
	if _, n, err := h.sp.MovePageRewriting("target", "moved", func(_ *wiki.Index, p LinkPage, m func(string) string) (string, bool) {
		if p.Path == "linker" {
			return strings.Replace(p.Body, "(target)", "(moved)", 1), true
		}
		return p.Body, false
	}, "bob"); err != nil || n != 1 {
		t.Fatalf("move: %d %v", n, err)
	}
	revs := h.revs("linker")
	if len(revs) != 2 || revs[0].Author != "bob" || !strings.Contains(revs[0].Note, "moving /target to /moved") || revs[0].Body != "" {
		t.Fatalf("linker history: %s / %+v", versions(revs), revs[0])
	}
	p, _ := h.sp.GetPage("linker")
	if r, _ := h.sp.Revision(p.ID, revs[0].Version); r.Body != "see [t](moved)" || r.Title != "linker" {
		t.Errorf("rewritten revision: %+v", r)
	}
	// An edit by alice soon after is a new revision, not merged into the system edit.
	h.put("linker", "see [t](moved) again", "alice")
	if got := versions(h.revs("linker")); !strings.HasPrefix(got, "3:alice:edited 2:bob:edited") {
		t.Errorf("history: %s", got)
	}
}

func TestUpgradeBuildsHistoryForExistingPages(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// A space whose database stops before the history tables, as one from an older build.
	res, err := st.catalog.Exec(`INSERT INTO spaces(slug, title, public_read, created_at) VALUES('old', 'Old', 1, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	db, err := openDB(st.spacePath(id), spaceMigrations[:6]) // everything before the history migration
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		v := 1
		if i%2 == 0 {
			v = 3
		}
		if _, err := db.Exec(`INSERT INTO pages(slug, path, title, body, position, version, created_at, updated_at)
			VALUES(?, ?, ?, ?, 0, ?, ?, ?)`, fmt.Sprint("p", i), fmt.Sprint("p", i), fmt.Sprint("Page ", i), fmt.Sprint("text ", i), v, 1000+i, 2000+i); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	sp, err := st.GetSpace("old") // opens the database and migrates it
	if err != nil {
		t.Fatal(err)
	}
	p, _ := sp.GetPage("p10")
	revs, err := sp.Revisions(p.ID)
	if err != nil || len(revs) != 1 || revs[0].Version != 3 || revs[0].Action != "edited" || revs[0].Size != len("text 10") {
		t.Fatalf("backfilled history: %+v %v", revs, err)
	}
	if r, _ := sp.Revision(p.ID, 3); r.Body != "text 10" {
		t.Errorf("text: %+v", r)
	}
	p1, _ := sp.GetPage("p11")
	if r, _ := sp.Revisions(p1.ID); r[0].Action != "created" {
		t.Errorf("version 1 is a creation: %+v", r[0])
	}
	changes, _ := sp.RecentChanges(100)
	if len(changes) != 50 || changes[0].Path != "p59" || changes[0].Action != "created" || changes[49].Path != "p10" {
		t.Errorf("the feed starts with the 50 most recent pages: %d, first %s last %s", len(changes), changes[0].Path, changes[len(changes)-1].Path)
	}
	// Editing after the upgrade just works.
	if _, _, err := sp.PutPage("p10", "Page 10", "new text", Refs{}, Cond{By: "alice"}); err != nil {
		t.Fatal(err)
	}
	if revs, _ := sp.Revisions(p.ID); len(revs) != 2 || revs[0].Version != 4 {
		t.Errorf("after an edit: %s", versions(revs))
	}
}

func TestCoalescingIsOptIn(t *testing.T) {
	h := newHist(t)
	for i := 0; i < 3; i++ {
		if _, _, err := h.sp.PutPage("p", "p", fmt.Sprint("v", i), Refs{}, Cond{By: "alice"}); err != nil { // no Coalesce
			t.Fatal(err)
		}
		h.wait(time.Second)
	}
	if got := versions(h.revs("p")); got != "3:alice:edited 2:alice:edited 1:alice:created" {
		t.Fatalf("saves that do not ask to be merged are kept: %s", got)
	}
}
