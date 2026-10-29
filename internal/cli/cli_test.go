package cli_test

import (
	"archive/zip"
	"bytes"
	"image"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hbroek/wako/internal/auth"
	"github.com/hbroek/wako/internal/cli"
	"github.com/hbroek/wako/internal/server"
	"github.com/hbroek/wako/internal/store"
)

func init() { auth.Params = auth.PasswordParams{Time: 1, MemoryKiB: 8, Threads: 1} }

type rig struct {
	t      *testing.T
	srv    *server.Server
	st     *store.Store
	ts     *httptest.Server
	token  string
	viewer string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := server.New(st, "")
	if err := srv.EnableAuth(server.AuthOptions{AllowInsecure: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.Bootstrap(st, "root", "root-password"); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	r := &rig{t: t, srv: srv, st: st, ts: ts}
	root, _ := st.GetUserByName("root")
	r.token = "wako_" + auth.RandomToken(16)
	st.CreateToken(root.ID, "cli", auth.HashToken(r.token))
	hash, _ := auth.HashPassword("viewer-password")
	v, _ := st.CreateUser("viewer", hash, false)
	r.viewer = "wako_" + auth.RandomToken(16)
	st.CreateToken(v.ID, "cli", auth.HashToken(r.viewer))
	for _, s := range []struct {
		slug   string
		public bool
	}{{"main", true}, {"secret", false}} {
		if _, err := st.CreateSpace(s.slug, strings.Title(s.slug), s.public); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// run executes a wako command as the administrator and returns its output.
func (r *rig) run(stdin string, args ...string) (code int, out, errOut string) {
	return r.runAs(r.token, stdin, args...)
}

func (r *rig) runAs(token, stdin string, args ...string) (int, string, string) {
	var o, e bytes.Buffer
	env := map[string]string{"WAKO_SERVER": r.ts.URL, "WAKO_TOKEN": token, "WAKO_SPACE": "main"}
	code := cli.Run(args, strings.NewReader(stdin), &o, &e, func(k string) string { return env[k] })
	return code, o.String(), e.String()
}

func (r *rig) ok(stdin string, args ...string) string {
	r.t.Helper()
	code, out, errOut := r.run(stdin, args...)
	if code != 0 {
		r.t.Fatalf("wako %v: exit %d\nstdout: %s\nstderr: %s", args, code, out, errOut)
	}
	return out
}

func (r *rig) fails(code int, want string, stdin string, args ...string) {
	r.t.Helper()
	c, out, errOut := r.run(stdin, args...)
	if c != code || !strings.Contains(errOut, want) {
		r.t.Fatalf("wako %v: exit %d (want %d)\nstdout: %s\nstderr: %s\nwanted stderr to contain %q", args, c, code, out, errOut, want)
	}
}

func pngFile(t *testing.T, dir, name string, shade uint8) string {
	t.Helper()
	im := image.NewGray(image.Rect(0, 0, 6, 6))
	for i := range im.Pix {
		im.Pix[i] = shade + uint8(i)
	}
	var b bytes.Buffer
	png.Encode(&b, im)
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPagesFromTheCommandLine(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()

	if got := r.ok("# Docs\n\nfrom stdin\n", "put", "docs"); got != "created /docs (version 1)\n" {
		t.Errorf("put: %q", got)
	}
	file := filepath.Join(dir, "ops.md")
	os.WriteFile(file, []byte("# Ops\n\nfrom a file\n"), 0o644)
	if got := r.ok("", "put", "docs/ops", file); got != "created /docs/ops (version 1)\n" {
		t.Errorf("put file: %q", got)
	}
	if got := r.ok("# Docs\n\nsecond\n", "put", "/docs.md"); got != "updated /docs (version 2)\n" {
		t.Errorf("update (a leading slash and .md are fine): %q", got)
	}
	if got := r.ok("", "get", "docs"); got != "# Docs\n\nsecond\n" {
		t.Errorf("get: %q", got)
	}
	if got := r.ok("", "get", "docs/ops", "--rev", "1"); got != "# Ops\n\nfrom a file\n" {
		t.Errorf("get --rev: %q", got)
	}
	if got := r.ok("", "get", "docs", "--rev", "1"); got != "# Docs\n\nfrom stdin\n" {
		t.Errorf("flags may follow the operand: %q", got)
	}

	// The tree.
	r.ok("# Top\n", "put", "top")
	if got := r.ok("", "ls"); got != "docs  Docs\n  docs/ops  Ops\ntop  Top\n" {
		t.Errorf("ls: %q", got)
	}
	if got := r.ok("", "ls", "docs"); got != "docs  Docs\n  docs/ops  Ops\n" {
		t.Errorf("ls docs: %q", got)
	}
	r.fails(1, `no page "nope"`, "", "ls", "nope")
	if c, _, errOut := r.run("", "get", "nope"); c != 1 || errOut != "wako: not found\n" {
		t.Errorf("a missing page: %d %q", c, errOut)
	}

	// Preconditions.
	r.fails(1, "already exists", "x", "put", "docs", "--create")
	r.fails(1, "not in the state you expected", "x", "put", "docs", "--if-match", "1")
	if got := r.ok("# Docs\n\nthird\n", "put", "docs", "--if-match", "2"); !strings.Contains(got, "(version 3)") {
		t.Errorf("if-match: %q", got)
	}
	if got := r.ok("# Docs\n\nfourth\n", "put", "docs", "--if-match", `"v3"`); !strings.Contains(got, "(version 4)") {
		t.Errorf("if-match with the ETag form: %q", got)
	}

	// Move, search, delete.
	if got := r.ok("", "mv", "docs/ops", "operations"); got != "moved /docs/ops to /operations\n" {
		t.Errorf("mv: %q", got)
	}
	r.fails(1, "cannot move", "", "mv", "operations", "top")
	hits := r.ok("", "search", "from", "a", "file")
	if !strings.Contains(hits, "/operations  Ops") || !strings.Contains(hits, "from a file") || strings.Contains(hits, "<mark>") {
		t.Errorf("search: %q", hits)
	}
	if code, out, errOut := r.run("", "search", "zzzznothing"); code != 0 || out != "" || !strings.Contains(errOut, "no pages match") {
		t.Errorf("empty search: %d %q %q", code, out, errOut)
	}
}

func TestRemovingPages(t *testing.T) {
	r := newRig(t)
	r.ok("# A\n", "put", "a")
	r.ok("# B\n", "put", "a/b")
	r.fails(1, "has pages below it: use -r", "", "rm", "a")
	if got := r.ok("", "rm", "-r", "a"); got != "deleted /a\n" {
		t.Errorf("rm -r: %q", got)
	}
	r.fails(1, "not found", "", "get", "a")
	r.fails(1, "not found", "", "rm", "a")
}

func TestImagesAndSpaces(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	one := pngFile(t, dir, "My Shot.png", 10)
	got := r.ok("", "upload", one)
	if !strings.Contains(got, "My-Shot.png") || !strings.Contains(got, "(added)") || !strings.Contains(got, "![My-Shot](My-Shot.png)") {
		t.Errorf("upload: %q", got)
	}
	if got := r.ok("", "upload", one); !strings.Contains(got, "already in the library") {
		t.Errorf("repeat upload: %q", got)
	}
	bad := filepath.Join(dir, "x.png")
	os.WriteFile(bad, []byte("not an image"), 0o644)
	c, _, errOut := r.run("", "upload", one, bad)
	if c != 1 || !strings.Contains(errOut, "unsupported file type") || !strings.Contains(errOut, "1 of 2 files") {
		t.Errorf("upload with a bad file: %d %q", c, errOut)
	}
	r.fails(1, "no such file", "", "upload", filepath.Join(dir, "missing.png"))

	list := r.ok("", "spaces")
	if !strings.Contains(list, "main") || !strings.Contains(list, "public") || !strings.Contains(list, "secret") || !strings.Contains(list, "private") {
		t.Errorf("spaces: %q", list)
	}
	// A space the token cannot see is not listed.
	if c, out, _ := r.runAs(r.viewer, "", "spaces"); c != 0 || strings.Contains(out, "secret") || !strings.Contains(out, "main") {
		t.Errorf("viewer's spaces: %d %q", c, out)
	}
}

func TestMistakesAreExplained(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct {
		code int
		want string
		args []string
	}{
		{2, "unknown command", []string{"frobnicate"}},
		{2, "usage: wako get", []string{"get"}},
		{2, "usage: wako put", []string{"put"}},
		{2, "usage: wako mv", []string{"mv", "a"}},
		{2, "usage: wako search", []string{"search"}},
		{2, "usage: wako rm", []string{"rm"}},
		{2, "flag provided but not defined", []string{"get", "docs", "--bogus"}},
		{2, "Usage:", nil},
	} {
		r.fails(c.code, c.want, "", c.args...)
	}
	if c, out, _ := r.run("", "help"); c != 0 || !strings.Contains(out, "Commands:") {
		t.Errorf("help: %d", c)
	}
	// No space chosen.
	var o, e bytes.Buffer
	code := cli.Run([]string{"ls"}, strings.NewReader(""), &o, &e, func(k string) string {
		return map[string]string{"WAKO_SERVER": r.ts.URL, "WAKO_TOKEN": r.token}[k]
	})
	if code != 2 || !strings.Contains(e.String(), "which space?") {
		t.Errorf("no space: %d %q", code, e.String())
	}
	// Rights.
	if c, _, errOut := r.runAs("", "x", "put", "p"); c != 1 || !strings.Contains(errOut, "not signed in") || !strings.Contains(errOut, "API token") {
		t.Errorf("anonymous write: %d %q", c, errOut)
	}
	if c, _, errOut := r.runAs("wako_wrong", "", "get", "p"); c != 1 || !strings.Contains(errOut, "did not accept the token") {
		t.Errorf("wrong token: %d %q", c, errOut)
	}
	if c, _, errOut := r.runAs(r.viewer, "x", "put", "p"); c != 1 || !strings.Contains(errOut, "not allowed") {
		t.Errorf("viewer write: %d %q", c, errOut)
	}
	// Anonymous reading of a public space needs no token.
	r.ok("# Public\n", "put", "pub")
	if c, out, _ := r.runAs("", "", "get", "pub"); c != 0 || out != "# Public\n" {
		t.Errorf("anonymous read: %d %q", c, out)
	}
	// A server that is not there.
	var o2, e2 bytes.Buffer
	code = cli.Run([]string{"--server", "http://127.0.0.1:1", "spaces"}, strings.NewReader(""), &o2, &e2, func(string) string { return "" })
	if code != 1 || !strings.Contains(e2.String(), "cannot reach http://127.0.0.1:1") {
		t.Errorf("unreachable: %d %q", code, e2.String())
	}
	// Settings can come from flags, which win over the environment.
	var o3, e3 bytes.Buffer
	code = cli.Run([]string{"--server", r.ts.URL, "--token", r.token, "--space", "main", "ls"}, strings.NewReader(""), &o3, &e3,
		func(k string) string { return map[string]string{"WAKO_SPACE": "secret", "WAKO_TOKEN": "wako_wrong"}[k] })
	if code != 0 {
		t.Errorf("flags over environment: %d %q", code, e3.String())
	}
}

func TestExportAndImportFromTheCommandLine(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	img := pngFile(t, dir, "shot.png", 20)
	r.ok("", "upload", img)
	r.ok("# Docs\n\n![s](shot.png)\n", "put", "docs")
	r.ok("# Ops\n", "put", "docs/ops")

	// To a file, named by the server.
	wd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(wd)
	c, _, errOut := r.run("", "export")
	if c != 0 || !strings.Contains(errOut, "wrote main-") {
		t.Fatalf("export: %d %q", c, errOut)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "main-*.zip"))
	if len(matches) != 1 {
		t.Fatalf("export file: %v", matches)
	}
	// To standard output, or an explicit name.
	_, stdout, _ := r.run("", "export", "-o", "-")
	zr, err := zip.NewReader(bytes.NewReader([]byte(stdout)), int64(len(stdout)))
	if err != nil {
		t.Fatalf("stdout export is not a zip: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, " "); got != "wako.json docs.md docs/ops.md shot.png" {
		t.Errorf("files: %s", got)
	}
	r.ok("", "export", "-o", filepath.Join(dir, "copy.zip"))

	// Import it into a space that does not exist yet.
	r.fails(1, "not found", "", "--space", "mirror", "import", filepath.Join(dir, "copy.zip"))
	out := r.ok("", "--space", "mirror", "import", "--create", filepath.Join(dir, "copy.zip"))
	if !strings.Contains(out, "2 pages created, 0 updated, 0 skipped") || !strings.Contains(out, "1 images added") {
		t.Errorf("import: %q", out)
	}
	if got := r.ok("", "--space", "mirror", "get", "docs"); got != "# Docs\n\n![s](shot.png)\n" {
		t.Errorf("imported page: %q", got)
	}
	if got := r.ok("", "--space", "mirror", "ls"); got != "docs  Docs\n  docs/ops  Ops\n" {
		t.Errorf("imported tree: %q", got)
	}
	if list := r.ok("", "spaces"); !strings.Contains(list, "mirror") || !strings.Contains(list, "Mirror") || strings.Contains(list, "Main  ") && strings.Count(list, "Main") > 1 {
		t.Errorf("the new space is named from its slug, not the source's title: %q", list)
	}
	r.ok("", "--space", "titled", "import", "--create", "--title", "Our Wiki", filepath.Join(dir, "copy.zip"))
	if list := r.ok("", "spaces"); !strings.Contains(list, "Our Wiki") {
		t.Errorf("--title: %q", list)
	}
	// Again: everything is skipped; with overwrite, updated.
	if out := r.ok("", "--space", "mirror", "import", filepath.Join(dir, "copy.zip")); !strings.Contains(out, "0 pages created, 0 updated, 2 skipped") {
		t.Errorf("second import: %q", out)
	}
	if out := r.ok("", "--space", "mirror", "import", "--conflict", "overwrite", filepath.Join(dir, "copy.zip")); !strings.Contains(out, "2 updated") {
		t.Errorf("overwrite: %q", out)
	}
	r.fails(1, "conflict must be", "", "--space", "mirror", "import", "--conflict", "merge", filepath.Join(dir, "copy.zip"))
}

func TestImportingAFolder(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	notes := filepath.Join(dir, "my notes")
	os.MkdirAll(filepath.Join(notes, "Projects", "Alpha"), 0o755)
	os.MkdirAll(filepath.Join(notes, ".git"), 0o755)
	os.MkdirAll(filepath.Join(notes, "images"), 0o755)
	os.WriteFile(filepath.Join(notes, "Start Here.md"), []byte("# Start\n\n![pic](images/diagram%20one.png)\n"), 0o644)
	os.WriteFile(filepath.Join(notes, "Projects", "Alpha", "Plan.md"), []byte("# Plan\n"), 0o644)
	os.WriteFile(filepath.Join(notes, ".git", "config"), []byte("secret"), 0o644)
	os.WriteFile(filepath.Join(notes, ".hidden.md"), []byte("# hidden\n"), 0o644)
	os.WriteFile(filepath.Join(notes, "todo.txt"), []byte("not markdown"), 0o644)
	pngFile(t, notes, "images/diagram one.png", 50)

	c, out, errOut := r.run("", "import", notes)
	if c != 0 {
		t.Fatalf("import: %d %s", c, errOut)
	}
	if !strings.Contains(out, "2 pages created") || !strings.Contains(out, "2 empty parent pages added") || !strings.Contains(out, "1 images added") {
		t.Errorf("summary: %q", out)
	}
	if !strings.Contains(errOut, "todo.txt") && !strings.Contains(errOut, ".txt") {
		t.Errorf("skipped files not mentioned: %q", errOut)
	}
	if strings.Contains(errOut, ".git") || strings.Contains(errOut, "hidden") {
		t.Errorf("hidden files should be left out quietly: %q", errOut)
	}
	if got := r.ok("", "ls"); got != "Projects  Projects\n  Projects/Alpha  Alpha\n    Projects/Alpha/Plan  Plan\nStart-Here  Start\n" {
		t.Errorf("tree: %q", got)
	}
	if got := r.ok("", "get", "Start-Here"); got != "# Start\n\n![pic](diagram-one.png)\n" {
		t.Errorf("image path became a library name: %q", got)
	}
	// Below a page.
	r.ok("# Inbox\n", "put", "inbox")
	if out := r.ok("", "import", "--under", "inbox", notes); !strings.Contains(out, "2 pages created") {
		t.Errorf("under: %q", out)
	}
	if got := r.ok("", "ls", "inbox"); !strings.Contains(got, "inbox/Start-Here") {
		t.Errorf("tree under inbox: %q", got)
	}
}
