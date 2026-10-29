package store

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDimensionsAreBackfilledAndQuotaIsAtomic(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := st.CreateSpace("main", "Main", true)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	png.Encode(&buf, image.NewGray(image.Rect(0, 0, 30, 20)))
	// An asset stored by an older version has no recorded size.
	a, _, err := sp.PutAsset("old.png", "image/png", buf.Bytes(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	st, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sp, _ = st.GetSpace("main")
	info, err := sp.AssetInfo(a.ID)
	if err != nil || info.Width != 30 || info.Height != 20 {
		t.Fatalf("backfill: %+v %v", info, err)
	}

	// The quota check lives in the INSERT, so a full space never grows.
	sp.quota = info.Size
	if _, _, err := sp.PutAsset("more.png", "image/png", []byte("0123456789"), 1, 1); err != ErrQuota {
		t.Fatalf("want ErrQuota, got %v", err)
	}
	n, used, quota, _ := sp.AssetUsage()
	if n != 1 || used != info.Size || quota != info.Size {
		t.Fatalf("usage %d %d %d", n, used, quota)
	}
}

func TestRenameAssetAliasesPagesThatCannotBeRewritten(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sp, _ := st.CreateSpace("main", "Main", true)
	a, _, err := sp.PutAsset("old.png", "image/png", []byte("img-a"), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"fine", "stubborn"} {
		if _, _, err := sp.PutPage(p, p, "![x](old.png)", Refs{Images: []string{"old.png"}}, Cond{}); err != nil {
			t.Fatal(err)
		}
	}
	// A rewriter that refuses the second page it is asked about, as when the
	// Markdown is too unusual to edit safely.
	n := 0
	res, err := sp.RenameAsset(a.ID, "new.png", func(body, from, to string) (string, bool) {
		n++
		if n == 2 {
			return body, false
		}
		return strings.ReplaceAll(body, from, to), true
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated != 1 || res.Aliased != 1 || res.Asset.Name != "new.png" {
		t.Fatalf("result %+v", res)
	}
	names := map[string]int{}
	for _, p := range []string{"fine", "stubborn"} {
		pg, _ := sp.GetPage(p)
		b, _ := sp.Bindings(pg.ID)
		for name := range b {
			names[name]++
		}
	}
	// One page moved to the new name; the other keeps the old one as an alias,
	// and both still point at the same image.
	if names["new.png"] != 1 || names["old.png"] != 1 {
		t.Fatalf("bindings: %v", names)
	}
	// The old name is free for a different image; the aliasing page is unaffected.
	b, _, err := sp.PutAsset("old.png", "image/png", []byte("img-b"), 1, 1)
	if err != nil || b.Name != "old.png" || b.ID == a.ID {
		t.Fatalf("freed name: %+v %v", b, err)
	}
	if n, _, _, _ := sp.AssetUsage(); n != 2 {
		t.Fatalf("usage %d", n)
	}
	if info, _ := sp.FindAssetByName("old.png"); info.ID != b.ID {
		t.Fatal("lookup by the old name must find the new image")
	}
	pg, _ := sp.GetPage("stubborn")
	if bound, _ := sp.Bindings(pg.ID); bound["old.png"].ID != a.ID {
		t.Fatal("alias must keep pointing at the original image")
	}

	// Errors roll everything back.
	if _, err := sp.RenameAsset(a.ID, "old.png", func(string, string, string) (string, bool) { return "", true }, ""); err != ErrExists {
		t.Fatalf("want ErrExists, got %v", err)
	}
	if info, _ := sp.AssetInfo(a.ID); info.Name != "new.png" {
		t.Fatalf("failed rename changed the name to %q", info.Name)
	}
	if _, err := sp.RenameAsset("nope", "x.png", nil, ""); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSearchAndIndexStayInSync(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sp, _ := st.CreateSpace("main", "Main", true)
	put := func(path, title, body string) *Page {
		t.Helper()
		p, _, err := sp.PutPage(path, title, body, Refs{}, Cond{})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	paths := func(q string) []string {
		t.Helper()
		hits, err := sp.Search(q, 20)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		var out []string
		for _, h := range hits {
			out = append(out, h.Path)
		}
		return out
	}
	eq := func(got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	put("ops", "Operations", "How we run the production servers.")
	put("deploy", "Deploying", "The deployment script is running every night.")
	put("misc", "Miscellany", "Notes about operations and other things.")
	eq(paths("run"), "ops", "deploy") // prefix and stemming: "running" matches "run"
	eq(paths("running"), "ops", "deploy")
	eq(paths("oper"), "ops", "misc")       // typing a prefix; the title match ranks first
	eq(paths("operations"), "ops", "misc") // title counts ten times a body match
	eq(paths("production servers"), "ops") // all words must match
	eq(paths("production nonsense"))
	eq(paths(""))
	eq(paths("   "))
	eq(paths("AND OR NOT")) // operators are just words
	eq(paths(`"unbalanced`), nil...)
	eq(paths("ops*()-+:^"), "ops", "misc") // punctuation is dropped; the stem "op" also prefixes "oper"

	// Edits and deletes update the index; cascading deletes too.
	put("ops", "Operations", "Now about databases only.")
	eq(paths("production"))
	eq(paths("databases"), "ops")
	put("ops/child", "Child Page", "A unique zebra lives here.")
	eq(paths("zebra"), "ops/child")
	if err := sp.DeletePage("ops", true, nil, ""); err != nil {
		t.Fatal(err)
	}
	eq(paths("zebra"))
	eq(paths("databases"))
	// Moving a page keeps it searchable under its new path.
	put("a", "Alpha", "findable words")
	if _, _, err := sp.MovePageRewriting("a", "b", nil, ""); err != nil {
		t.Fatal(err)
	}
	eq(paths("findable"), "b")
}

func TestSearchSnippetsAreSafe(t *testing.T) {
	st, _ := Open(t.TempDir())
	defer st.Close()
	sp, _ := st.CreateSpace("main", "Main", true)
	sp.PutPage("x", "<b>Bold</b> title", "<script>alert(1)</script> needle in a <img src=x onerror=alert(2)> haystack & more", Refs{}, Cond{})
	hits, err := sp.Search("needle", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("%v %v", hits, err)
	}
	h := hits[0]
	if strings.Contains(h.SnippetHTML, "<script") || strings.Contains(h.SnippetHTML, "<img") || !strings.Contains(h.SnippetHTML, "<mark>needle</mark>") {
		t.Errorf("snippet: %s", h.SnippetHTML)
	}
	if !strings.Contains(h.SnippetHTML, "&amp;") && strings.Contains(h.SnippetHTML, "& more") {
		t.Errorf("ampersand not escaped: %s", h.SnippetHTML)
	}
	hits, _ = sp.Search("bold", 5)
	if len(hits) != 1 || strings.Contains(hits[0].TitleHTML, "<b>") || !strings.Contains(hits[0].TitleHTML, "<mark>Bold</mark>") {
		t.Errorf("title: %+v", hits)
	}
	// Markers in the text cannot forge a highlight.
	sp.PutPage("y", "Y", "plain \x01forged\x02 forgery word", Refs{}, Cond{})
	if hits, _ := sp.Search("forgery", 5); len(hits) != 1 || strings.Contains(hits[0].SnippetHTML, "<mark>forged") {
		t.Errorf("forged highlight: %+v", hits)
	}
}

func TestFTSQuery(t *testing.T) {
	for in, want := range map[string]string{
		"":                              "",
		"hello":                         `"hello"*`,
		"Hello World":                   `"Hello" "World"*`,
		`say "cheese" -x OR y*`:         `"say" "cheese" "x" "OR" "y"*`,
		"naïve café 123":                `"naïve" "café" "123"*`,
		"((( )))":                       "",
		"a b c d e f g h i j k l m n o": `"a" "b" "c" "d" "e" "f" "g" "h" "i" "j" "k" "l"*`,
	} {
		if got := FTSQuery(in); got != want {
			t.Errorf("FTSQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathsFollowMoves(t *testing.T) {
	st, _ := Open(t.TempDir())
	defer st.Close()
	sp, _ := st.CreateSpace("main", "Main", true)
	for _, p := range []string{"a", "a/b", "a/b/c", "a2", "a2/x", "z"} {
		if _, _, err := sp.PutPage(p, p, "x", Refs{}, Cond{}); err != nil {
			t.Fatal(err)
		}
	}
	// "a" moves under "z"; "a2" (shares a prefix) must not move with it.
	pg, _, err := sp.MovePageRewriting("a", "z/a", nil, "")
	if err != nil || pg.Path != "z/a" {
		t.Fatalf("%+v %v", pg, err)
	}
	ix, _ := sp.PageIndex()
	for _, want := range []string{"z/a", "z/a/b", "z/a/b/c", "a2", "a2/x", "z"} {
		if !ix.Has(want) {
			t.Errorf("index lacks %s", want)
		}
	}
	for _, gone := range []string{"a", "a/b", "a/b/c"} {
		if ix.Has(gone) {
			t.Errorf("index still has %s", gone)
		}
	}
	// And the page can be found by walking the tree at its new place.
	if p, err := sp.GetPage("z/a/b/c"); err != nil || p.Path != "z/a/b/c" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := sp.GetPage("a/b/c"); err != ErrNotFound {
		t.Fatalf("old path still resolves: %v", err)
	}
	if _, _, err := sp.MovePageRewriting("z", "z/a/inside", nil, ""); err == nil {
		t.Fatal("moving a page below itself must fail")
	}
}

func TestSiblingImageChoosesDeterministically(t *testing.T) {
	st, _ := Open(t.TempDir())
	defer st.Close()
	sp, _ := st.CreateSpace("main", "Main", true)
	a, _, _ := sp.PutAsset("s.png", "image/png", []byte("image-a"), 1, 1)
	put := func(path string, imgs ...string) {
		t.Helper()
		if _, _, err := sp.PutPage(path, path, "x", Refs{Images: imgs}, Cond{}); err != nil {
			t.Fatal(err)
		}
	}
	put("docs")
	put("docs/b", "s.png") // binds image A under "s.png"
	// Image A is renamed, but "b" cannot be rewritten, so it keeps "s.png" as an alias.
	if _, err := sp.RenameAsset(a.ID, "a2.png", func(body, from, to string) (string, bool) { return body, false }, ""); err != nil {
		t.Fatal(err)
	}
	// "s.png" is free again and now means a different image.
	b, _, _ := sp.PutAsset("s.png", "image/png", []byte("image-b"), 1, 1)
	put("docs/a", "s.png") // sorts before docs/b and binds image B

	got, err := sp.SiblingImage("docs", "s.png")
	if err != nil || got.ID != b.ID {
		t.Fatalf("want the image of docs/a (%s), got %+v %v", b.ID[:8], got, err)
	}
	if _, err := sp.SiblingImage("", "s.png"); err != ErrNotFound {
		t.Errorf("root: %v", err)
	}
	if _, err := sp.SiblingImage("docs/a", "s.png"); err != ErrNotFound {
		t.Errorf("below a leaf page: %v", err)
	}
	if _, err := sp.SiblingImage("nodir", "s.png"); err != ErrNotFound {
		t.Errorf("missing dir: %v", err)
	}
	put("top", "a2.png")
	if got, err := sp.SiblingImage("", "a2.png"); err != nil || got.ID != a.ID {
		t.Errorf("root-level page: %+v %v", got, err)
	}
	if !IsImageName("x.PNG") || IsImageName(".png") || IsImageName("png") || IsImageName("a.pngx") {
		t.Error("IsImageName")
	}
}

// ---- users, roles, sessions, tokens ----

func TestUsersAndRoles(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root, err := st.CreateUser("Root", "hash", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser("root", "hash", false); err != ErrExists { // names ignore case
		t.Fatalf("duplicate name: %v", err)
	}
	if u, err := st.GetUserByName("ROOT"); err != nil || u.ID != root.ID {
		t.Fatalf("lookup ignoring case: %v %v", u, err)
	}
	bob, _ := st.CreateUser("bob", "hash", false)

	// The last administrator is protected, in both ways.
	no := false
	if _, err := st.UpdateUser(root.ID, nil, &no); err != ErrLastAdmin {
		t.Fatalf("demoting the last admin: %v", err)
	}
	if err := st.DeleteUser(root.ID); err != ErrLastAdmin {
		t.Fatalf("deleting the last admin: %v", err)
	}
	yes := true
	if _, err := st.UpdateUser(bob.ID, nil, &yes); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateUser(root.ID, nil, &no); err != nil {
		t.Fatalf("demoting one of two admins: %v", err)
	}
	if n, _ := st.CountAdmins(); n != 1 {
		t.Fatalf("admins: %d", n)
	}

	// Roles, replaced in place, listed, and removed with the space or the user.
	main, _ := st.CreateSpace("main", "Main", true)
	other, _ := st.CreateSpace("other", "Other", false)
	if err := st.SetMember(main.ID, root.ID, "owner"); err == nil {
		t.Fatal("unknown role accepted")
	}
	st.SetMember(main.ID, root.ID, RoleViewer)
	st.SetMember(main.ID, root.ID, RoleEditor)
	st.SetMember(other.ID, root.ID, RoleAdmin)
	roles, _ := st.UserRoles(root.ID)
	if roles[main.ID] != RoleEditor || roles[other.ID] != RoleAdmin || len(roles) != 2 {
		t.Fatalf("roles: %v", roles)
	}
	if m, _ := st.ListMembers(main.ID); len(m) != 1 || m[0].User.Name != "Root" || m[0].Role != RoleEditor {
		t.Fatalf("members: %+v", m)
	}
	if err := st.RemoveMember(main.ID, bob.ID); err != ErrNotFound {
		t.Fatalf("removing a non-member: %v", err)
	}
	st.DeleteSpace("other")
	if roles, _ := st.UserRoles(root.ID); len(roles) != 1 {
		t.Fatalf("deleting a space must drop its memberships: %v", roles)
	}
	if err := st.DeleteUser(root.ID); err != nil {
		t.Fatal(err)
	}
	if m, _ := st.ListMembers(main.ID); len(m) != 0 {
		t.Fatalf("deleting a user must drop their memberships: %+v", m)
	}
}

func TestSessionsAndRefresh(t *testing.T) {
	st, _ := Open(t.TempDir())
	defer st.Close()
	u, _ := st.CreateUser("alice", "hash", false)
	t0 := time.Unix(1_700_000_000, 0)
	sid, err := st.CreateSession(u.ID, "r1", t0, t0.Add(7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.SessionUser(sid, t0.Add(time.Hour)); err != nil || got.ID != u.ID {
		t.Fatalf("live session: %v %v", got, err)
	}
	if _, err := st.SessionUser(sid, t0.Add(8*24*time.Hour)); err != ErrNotFound {
		t.Fatalf("expired session: %v", err)
	}

	// Rotation: the new token works, and the old one for a short grace period.
	later := t0.Add(time.Hour)
	if _, _, rotated, err := st.RefreshSession("r1", "r2", later, later.Add(7*24*time.Hour), 30*time.Second); err != nil || !rotated {
		t.Fatalf("rotate: %v %v", rotated, err)
	}
	if _, _, rotated, err := st.RefreshSession("r1", "r3", later.Add(10*time.Second), later.Add(7*24*time.Hour), 30*time.Second); err != nil || rotated {
		t.Fatalf("old token inside the grace period: rotated=%v err=%v", rotated, err)
	}
	if _, _, _, err := st.RefreshSession("r1", "r4", later.Add(31*time.Second), later.Add(7*24*time.Hour), 30*time.Second); err != ErrNotFound {
		t.Fatalf("old token after the grace period: %v", err)
	}
	if _, u2, rotated, err := st.RefreshSession("r2", "r5", later.Add(time.Minute), later.Add(8*24*time.Hour), time.Second); err != nil || !rotated || u2.ID != u.ID {
		t.Fatalf("current token: %v %v %v", u2, rotated, err)
	}
	if _, _, _, err := st.RefreshSession("nope", "x", later, later.Add(time.Hour), time.Second); err != ErrNotFound {
		t.Fatalf("unknown token: %v", err)
	}

	// A new password, or deleting the user, ends the sessions.
	hash := "new"
	st.UpdateUser(u.ID, &hash, nil)
	if _, err := st.SessionUser(sid, later); err != ErrNotFound {
		t.Fatalf("session survived a password change: %v", err)
	}
	sid2, _ := st.CreateSession(u.ID, "q1", later, later.Add(time.Hour))
	st.DeleteSession(sid2)
	if _, err := st.SessionUser(sid2, later); err != ErrNotFound {
		t.Fatal("deleted session still valid")
	}
}

func TestTokensAndSettings(t *testing.T) {
	st, _ := Open(t.TempDir())
	defer st.Close()
	a, _ := st.CreateUser("a", "h", false)
	b, _ := st.CreateUser("b", "h", false)
	tok, err := st.CreateToken(a.ID, "ci", "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if u, err := st.TokenUser("hash-1"); err != nil || u.ID != a.ID {
		t.Fatalf("token owner: %v %v", u, err)
	}
	if _, err := st.TokenUser("other"); err != ErrNotFound {
		t.Fatal("unknown token accepted")
	}
	if err := st.DeleteToken(b.ID, tok.ID); err != ErrNotFound { // not theirs
		t.Fatalf("deleting someone else's token: %v", err)
	}
	if list, _ := st.ListTokens(a.ID); len(list) != 1 || list[0].Name != "ci" {
		t.Fatalf("tokens: %+v", list)
	}
	st.DeleteUser(a.ID)
	if _, err := st.TokenUser("hash-1"); err != ErrNotFound {
		t.Fatal("token survived its user")
	}
	if v, _ := st.Setting("k"); v != "" {
		t.Fatalf("unset: %q", v)
	}
	if v, _ := st.SetSettingIfAbsent("k", "first"); v != "first" {
		t.Fatal("first")
	}
	if v, _ := st.SetSettingIfAbsent("k", "second"); v != "first" {
		t.Fatalf("overwritten: %q", v)
	}
}

func TestBackupIsAConsistentCopy(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	main, _ := st.CreateSpace("main", "Main", true)
	other, _ := st.CreateSpace("other", "Other", false)
	u, _ := st.CreateUser("alice", "hash", true)
	st.SetMember(other.ID, u.ID, RoleEditor)
	main.PutPage("docs", "Docs", "# Docs\n", Refs{}, Cond{By: "alice"})
	main.PutAsset("pic.png", "image/png", []byte("pngbytes"), 1, 1)
	other.PutPage("secret", "Secret", "# Secret\n", Refs{}, Cond{})
	st.SetSettingIfAbsent("signing_key", "k")

	to := t.TempDir() + "/backup"
	if err := st.Backup(to); err != nil {
		t.Fatal(err)
	}
	// It is a data directory: open it and find everything.
	cp, err := Open(to)
	if err != nil {
		t.Fatalf("the backup does not open: %v", err)
	}
	defer cp.Close()
	spaces, _ := cp.ListSpaces()
	if len(spaces) != 2 || spaces[0].Slug != "main" || spaces[1].PublicRead {
		t.Fatalf("spaces: %+v", spaces)
	}
	m, _ := cp.GetSpace("main")
	if p, err := m.GetPage("docs"); err != nil || p.Body != "# Docs\n" {
		t.Errorf("page: %+v %v", p, err)
	}
	if a, err := m.FindAssetByName("pic.png"); err != nil || a.Size != 8 {
		t.Errorf("image: %+v %v", a, err)
	}
	if hits, _ := m.Search("docs", 5); len(hits) != 1 {
		t.Errorf("search index missing: %+v", hits)
	}
	o, _ := cp.GetSpace("other")
	if _, err := o.GetPage("secret"); err != nil {
		t.Error("second space missing")
	}
	if cu, err := cp.GetUserByName("alice"); err != nil || !cu.IsAdmin {
		t.Errorf("user: %+v %v", cu, err)
	}
	if roles, _ := cp.UserRoles(u.ID); roles[o.ID] != RoleEditor {
		t.Errorf("roles: %v", roles)
	}
	if v, _ := cp.Setting("signing_key"); v != "k" {
		t.Error("settings missing")
	}
	// The original keeps working and the copy does not follow it.
	main.PutPage("later", "Later", "x", Refs{}, Cond{})
	if _, err := m.GetPage("later"); err == nil {
		t.Error("the copy changed with the original")
	}
	// A directory that holds something is never overwritten, and a failed
	// backup leaves nothing half-written behind.
	if err := st.Backup(to); err == nil {
		t.Error("backup into a non-empty directory")
	}
	if err := st.Backup(to + "/spaces/1.db/x"); err == nil {
		t.Error("impossible target accepted")
	}
}

func TestBackupWhileWriting(t *testing.T) {
	st, _ := Open(t.TempDir())
	defer st.Close()
	sp, _ := st.CreateSpace("main", "Main", true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			sp.PutPage(fmt.Sprint("p", i%20), "P", fmt.Sprint("text ", i), Refs{}, Cond{})
		}
	}()
	to := t.TempDir() + "/b"
	if err := st.Backup(to); err != nil {
		t.Fatal(err)
	}
	<-done
	cp, err := Open(to)
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	m, _ := cp.GetSpace("main")
	if _, err := m.Tree(); err != nil {
		t.Errorf("the copy is damaged: %v", err)
	}
}

func TestCheckFindsAndRepairsDamage(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	main, _ := st.CreateSpace("main", "Main", true)
	gone, _ := st.CreateSpace("gone", "Gone", true)
	main.PutPage("a", "A", "alpha text", Refs{}, Cond{})
	main.PutPage("a/b", "B", "beta text", Refs{}, Cond{})
	main.PutPage("c", "C", "gamma text", Refs{}, Cond{})

	if rep, err := st.Check(true, false); err != nil || len(rep.Problems) != 0 || rep.Spaces != 2 || rep.Pages != 3 {
		t.Fatalf("a healthy store: %+v %v", rep, err)
	}
	fp, _ := st.CheckFiles()
	if len(fp.Missing) != 0 || len(fp.Orphans) != 0 {
		t.Fatalf("files: %+v", fp)
	}

	// The search index drifts from the pages, and a stored path goes wrong.
	main.db.Exec(`INSERT INTO pages_fts(pages_fts, rowid, title, body) VALUES('delete', 1, 'A', 'alpha text')`)
	main.db.Exec(`UPDATE pages SET path = 'wrong' WHERE slug = 'b'`)
	rep, err := st.Check(false, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(rep.Problems, "\n")
	if !strings.Contains(joined, "search index") || !strings.Contains(joined, "1 page(s) have a stored path") || len(rep.Fixed) != 0 {
		t.Fatalf("problems:\n%s", joined)
	}
	rep, err = st.Check(false, true)
	if err != nil || len(rep.Fixed) != 2 {
		t.Fatalf("repair: %+v %v", rep, err)
	}
	if rep, _ := st.Check(true, false); len(rep.Problems) != 0 {
		t.Errorf("still damaged after repair: %v", rep.Problems)
	}
	if p, err := main.GetPage("a/b"); err != nil || p.Path != "a/b" {
		t.Errorf("repaired page: %+v %v", p, err)
	}
	if hits, _ := main.Search("alpha", 5); len(hits) != 1 {
		t.Errorf("search after repair: %+v", hits)
	}
	if ix, _ := main.PageIndex(); !ix.Has("a/b") || ix.Has("wrong") {
		t.Error("paths not repaired")
	}

	// A space whose file disappears must be noticed, not served as an empty space.
	os.WriteFile(filepath.Join(dir, "spaces", "99.db"), []byte("stray"), 0o600)
	st.Close()
	os.Remove(filepath.Join(dir, "spaces", fmt.Sprintf("%d.db", gone.ID)))
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	fp, err = st2.CheckFiles()
	if err != nil || len(fp.Missing) != 1 || !strings.HasPrefix(fp.Missing[0], "gone (") || len(fp.Orphans) != 1 || fp.Orphans[0] != "99.db" {
		t.Fatalf("file problems: %+v %v", fp, err)
	}
	rep, err = st2.Check(false, false)
	if err != nil || !strings.Contains(strings.Join(rep.Problems, "\n"), "wakod forget-space") || !strings.Contains(strings.Join(rep.Problems, "\n"), "99.db") {
		t.Errorf("check report: %+v %v", rep, err)
	}
	if rep.Spaces != 1 {
		t.Errorf("only the space that can be opened is checked: %d", rep.Spaces)
	}
	if err := st2.DeleteSpace("gone"); err != nil {
		t.Fatal(err)
	}
	if fp, _ := st2.CheckFiles(); len(fp.Missing) != 0 {
		t.Errorf("after forgetting: %+v", fp)
	}
}

func TestDataIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh")
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.CreateSpace("main", "Main", true)
	for _, p := range []string{dir, filepath.Join(dir, "spaces")} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s: %v %v", p, fi.Mode().Perm(), err)
		}
	}
	bk := filepath.Join(t.TempDir(), "bk")
	if err := st.Backup(bk); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(bk); fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("backup directory: %v", fi.Mode().Perm())
	}
}

func TestTreeCacheFollowsEveryChange(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sp, _ := st.CreateSpace("main", "Main", true)
	paths := func() string {
		tr, err := sp.Tree()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		var walk func(ns []*Node)
		walk = func(ns []*Node) {
			for _, n := range ns {
				out = append(out, n.Path+"="+n.Title)
				walk(n.Children)
			}
		}
		walk(tr)
		return strings.Join(out, ",")
	}
	sp.PutPage("a", "A", "x", Refs{}, Cond{})
	if got := paths(); got != "a=A" {
		t.Fatal(got)
	}
	sp.PutPage("a/b", "B", "x", Refs{}, Cond{})
	if got := paths(); got != "a=A,a/b=B" {
		t.Fatalf("a new page is missing from the cached tree: %s", got)
	}
	sp.PutPage("a", "A2", "x", Refs{}, Cond{})
	if got := paths(); got != "a=A2,a/b=B" {
		t.Fatalf("a new title is missing: %s", got)
	}
	if err := sp.DeletePage("a/b", false, nil, "t"); err != nil {
		t.Fatal(err)
	}
	if got := paths(); got != "a=A2" {
		t.Fatalf("a deleted page is still in the tree: %s", got)
	}
	// Another space has its own tree.
	other, _ := st.CreateSpace("two", "Two", true)
	if tr, _ := other.Tree(); len(tr) != 0 {
		t.Fatal("trees are mixed up between spaces")
	}
}

func TestTreeCacheSeesReorder(t *testing.T) {
	st, _ := Open(t.TempDir())
	defer st.Close()
	sp, _ := st.CreateSpace("main", "Main", true)
	sp.PutPage("a", "A", "x", Refs{}, Cond{})
	sp.PutPage("b", "B", "x", Refs{}, Cond{})
	first := func() string { tr, _ := sp.Tree(); return tr[0].Slug }
	if first() != "a" {
		t.Fatal("setup")
	}
	if err := sp.Reorder("", []string{"b", "a"}); err != nil {
		t.Fatal(err)
	}
	if first() != "b" {
		t.Fatal("the cached tree ignores a reorder")
	}
}
