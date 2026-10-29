package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/hbroek/wako/internal/store"
)

// The personas of the access matrix. Everyone but "anon" has a token.
var personas = []string{"anon", "outsider", "viewer", "editor", "spaceadmin", "serveradmin"}

type perms struct{ read, write, admin bool }

// expected is the rule from the plan, written out independently of the code:
//
//	server admin: everything. A member: their role. Everyone else: read if public.
func expected(persona string, public bool) perms {
	switch persona {
	case "serveradmin", "spaceadmin":
		return perms{true, true, true}
	case "editor":
		return perms{true, true, false}
	case "viewer":
		return perms{true, false, false}
	}
	return perms{read: public}
}

type matrix struct {
	*aenv
	tokens map[string]string
	assets map[string]string // space -> asset id
}

func newMatrix(t *testing.T) *matrix {
	a := newAuthEnv(t)
	a.st.RevisionWindow = 0 // every save is its own revision
	m := &matrix{aenv: a, tokens: map[string]string{}, assets: map[string]string{}}
	for _, p := range personas[1:] {
		a.user(p, p == "serveradmin")
		m.tokens[p] = a.token(p)
	}
	for slug, public := range map[string]bool{"pub": true, "priv": false} {
		sp := a.space(slug, public)
		for _, r := range []struct {
			user string
			role store.Role
		}{{"viewer", store.RoleViewer}, {"editor", store.RoleEditor}, {"spaceadmin", store.RoleAdmin}} {
			if err := a.st.SetMember(sp.ID, a.users[r.user].ID, r.role); err != nil {
				t.Fatal(err)
			}
		}
		admin := bearer(m.tokens["serveradmin"])
		for _, path := range []string{"start", "doomed1", "doomed2", "doomed3"} {
			if rec := a.do("PUT", "/"+slug+"/"+path, "# "+path, header("Content-Type", "text/markdown"), admin); rec.Code != 201 {
				t.Fatalf("seed %s/%s: %d %s", slug, path, rec.Code, rec.Body.String())
			}
		}
		for _, persona := range personas {
			for i, body := range []string{"a\n", "a\nb\n"} { // two revisions to restore from, and a text to patch
				for _, name := range []string{"restore-", "patch-"} {
					if name == "patch-" && i == 1 {
						continue
					}
					if rec := a.do("PUT", "/"+slug+"/"+name+persona, body, header("Content-Type", "text/markdown"), admin); rec.Code > 201 {
						t.Fatalf("seed %s: %d", name+persona, rec.Code)
					}
				}
			}
		}
		rec := a.do("POST", "/"+slug+"/_assets?name=seed.png", string(seedPNG(t, slug)), header("Content-Type", "image/png"), admin)
		if rec.Code != 201 {
			t.Fatalf("seed asset: %d %s", rec.Code, rec.Body.String())
		}
		var as assetJSON
		json.Unmarshal(rec.Body.Bytes(), &as)
		m.assets[slug] = as.ID
	}
	return m
}

func seedPNG(t *testing.T, slug string) []byte {
	shade := uint8(10)
	if slug == "priv" {
		shade = 200
	}
	return pngBytes(t, shade)
}

// opts returns the request options that authenticate as persona.
func (m *matrix) opts(persona string, more ...reqOpt) []reqOpt {
	o := more
	if persona != "anon" {
		o = append(o, bearer(m.tokens[persona]))
	}
	return o
}

// want is the status an operation must return for a persona with these rights.
// A space the caller cannot read does not exist for them (404), whatever they
// try; otherwise a missing right is 401 for anonymous callers and 403 for the rest.
func want(persona string, p perms, allowed bool, ok int) int {
	switch {
	case !p.read:
		return 404
	case allowed:
		return ok
	case persona == "anon":
		return 401
	}
	return 403
}

func TestAccessMatrix(t *testing.T) {
	m := newMatrix(t)
	json := header("Content-Type", "application/json")
	mdType := header("Content-Type", "text/markdown")
	n := 0
	for _, space := range []string{"pub", "priv"} {
		public := space == "pub"
		for _, persona := range personas {
			p := expected(persona, public)
			name := fmt.Sprintf("%s/%s", space, persona)
			check := func(op, method, path, body string, allowed bool, ok int, more ...reqOpt) {
				t.Helper()
				n++
				rec := m.do(method, path, body, m.opts(persona, more...)...)
				if w := want(persona, p, allowed, ok); rec.Code != w {
					t.Errorf("%s: %s = %d, want %d (%s)", name, op, rec.Code, w, strings.TrimSpace(rec.Body.String()))
				}
				if rec.Code == 401 && rec.Header().Get("WWW-Authenticate") == "" {
					t.Errorf("%s: %s: 401 without WWW-Authenticate", name, op)
				}
			}
			check("read page", "GET", "/"+space+"/start", "", p.read, 200)
			check("read page as markdown", "GET", "/"+space+"/start.md", "", p.read, 200)
			check("read tree", "GET", "/_api/spaces/"+space+"/tree", "", p.read, 200)
			check("read asset", "GET", "/"+space+"/_assets/"+m.assets[space], "", p.read, 200)
			check("list library", "GET", "/"+space+"/_assets", "", p.read, 200, header("Accept", "application/json"))
			check("search", "GET", "/_api/spaces/"+space+"/search?q=start", "", p.read, 200)
			check("backlinks", "GET", "/_api/spaces/"+space+"/pages/start/backlinks", "", p.read, 200)
			check("preview", "POST", "/_api/spaces/"+space+"/render", "x", p.read, 200, mdType)

			check("history", "GET", "/"+space+"/start?history", "", p.read, 200, header("Accept", "application/json"))
			check("old revision", "GET", "/"+space+"/start?rev=1", "", p.read, 200, header("Accept", "text/markdown"))
			check("diff of a revision", "GET", "/"+space+"/start?diff=1", "", p.read, 200, header("Accept", "text/x-diff"))
			check("revisions API", "GET", "/_api/spaces/"+space+"/pages/start/revisions", "", p.read, 200)
			check("diff API", "GET", "/_api/spaces/"+space+"/pages/start/diff", "", p.read, 200)
			check("changes API", "GET", "/_api/spaces/"+space+"/changes", "", p.read, 200)
			check("diff two texts", "POST", "/_api/spaces/"+space+"/diff", `{"from":"a","to":"b"}`, p.read, 200, json)
			check("changes page", "GET", "/"+space+"/?changes", "", p.read, 200, header("Accept", "text/html"))

			check("restore", "POST", "/"+space+"/restore-"+persona+"?restore=1", "", p.write, 200)
			check("patch", "PATCH", "/"+space+"/patch-"+persona, "@@ -1 +1,2 @@\n a\n+b\n", p.write, 200, header("Content-Type", "text/x-diff"), header("If-Match", `"v1"`))
			check("export", "GET", "/_api/spaces/"+space+"/export", "", p.admin, 200)
			check("import", "POST", "/_api/spaces/"+space+"/import", string(tinyZip(t)), p.admin, 200, header("Content-Type", "application/zip"))

			check("write page", "PUT", fmt.Sprintf("/%s/new-%s", space, persona), "# new", p.write, 201, mdType)
			check("create child", "POST", "/"+space+"/start", "# c", p.write, 201, mdType, header("Slug", "child-"+persona))
			check("move page", "POST", "/"+space+"/new-"+persona+"?move=moved-"+persona, "", p.write, 200)
			check("delete page", "DELETE", "/"+space+"/doomed"+fmt.Sprint(1+len(persona)%3), "", p.write, 204)
			check("upload image", "POST", "/"+space+"/_assets?name=up-"+persona+".png", string(pngBytes(t, uint8(30+len(persona)+len(space)))), p.write, 201, header("Content-Type", "image/png"))
			check("rename image", "POST", "/"+space+"/_assets/"+m.assets[space]+"?rename=renamed-"+persona+".png", "", p.write, 200)

			check("list members", "GET", "/_api/spaces/"+space+"/members", "", p.admin, 200)
			check("add member", "PUT", "/_api/spaces/"+space+"/members/outsider", `{"role":"viewer"}`, p.admin, 200, json)
			check("remove member", "DELETE", "/_api/spaces/"+space+"/members/outsider", "", p.admin, 204)
			check("space settings", "PUT", "/_api/spaces/"+space, `{"title":"Retitled"}`, p.admin, 200, json)
			check("settings page", "GET", "/"+space+"/?settings", "", p.read, 200, header("Accept", "text/html"))
			if p.read { // the settings form itself is only for administrators of the space
				page := m.do("GET", "/"+space+"/?settings", "", m.opts(persona, header("Accept", "text/html"))...).Body.String()
				if strings.Contains(page, `id="space-settings"`) != p.admin || strings.Contains(page, `id="members"`) != p.admin {
					t.Errorf("%s: settings form shown=%v, admin=%v", name, strings.Contains(page, `id="space-settings"`), p.admin)
				}
				if strings.Contains(page, `>Settings</a>`) != p.admin || strings.Contains(page, `id="edit-link"`) && !p.write {
					t.Errorf("%s: header/actions do not match the rights", name)
				}
			}
		}
	}
	if n < 300 {
		t.Fatalf("matrix too small: %d checks", n)
	}
}

// The checks that are not about one space.
func TestGlobalPermissions(t *testing.T) {
	m := newMatrix(t)
	json := header("Content-Type", "application/json")
	for _, persona := range personas {
		admin := persona == "serveradmin"
		status := func(allowed bool, ok int) int {
			switch {
			case allowed:
				return ok
			case persona == "anon":
				return 401
			}
			return 403
		}
		for _, c := range []struct {
			op, method, path, body string
			allowed                bool
			ok                     int
		}{
			{"create space", "POST", "/_api/spaces", fmt.Sprintf(`{"slug":"new-%s"}`, persona), admin, 201},
			{"list users", "GET", "/_api/users", "", admin, 200},
			{"add user", "POST", "/_api/users", fmt.Sprintf(`{"name":"made-by-%s","password":"long-enough-1"}`, persona), admin, 201},
			{"new-space page", "GET", "/_ui/new-space", "", admin, 200},
		} {
			rec := m.do(c.method, c.path, c.body, m.opts(persona, json)...)
			if c.op == "new-space page" { // an HTML page: hidden from those who may not use it
				if (rec.Code == 200) != c.allowed {
					t.Errorf("%s: %s = %d", persona, c.op, rec.Code)
				}
				continue
			}
			if w := status(c.allowed, c.ok); rec.Code != w {
				t.Errorf("%s: %s = %d, want %d (%s)", persona, c.op, rec.Code, w, strings.TrimSpace(rec.Body.String()))
			}
		}
		// Deleting a space: only a server administrator, and a hidden space is still hidden.
		for _, space := range []string{"pub", "priv"} {
			if persona == "serveradmin" {
				continue // done last, below
			}
			p := expected(persona, space == "pub")
			rec := m.do("DELETE", "/_api/spaces/"+space, "", m.opts(persona)...)
			if w := want(persona, p, false, 204); rec.Code != w {
				t.Errorf("%s: delete space %s = %d, want %d", persona, space, rec.Code, w)
			}
		}
	}
	if rec := m.do("DELETE", "/_api/spaces/priv", "", bearer(m.tokens["serveradmin"])); rec.Code != 204 {
		t.Errorf("server admin deleting a space: %d", rec.Code)
	}
}

// What people can see: the list of spaces, search, and links across spaces.
func TestHiddenSpacesStayHidden(t *testing.T) {
	m := newMatrix(t)
	admin := bearer(m.tokens["serveradmin"])
	md := header("Content-Type", "text/markdown")
	// A page in the private space links to the public one, and the other way round.
	m.do("PUT", "/priv/secret-plan", "# Secret plan\n\nlinks to [pub](/pub/start) with the word zeppelin", md, admin)
	m.do("PUT", "/pub/start", "# Start\n\nmentions [the plan](/priv/secret-plan)", md, admin)

	for _, persona := range personas {
		canSee := expected(persona, false).read
		// Home page and API list only readable spaces.
		list := m.do("GET", "/_api/spaces", "", m.opts(persona)...).Body.String()
		home := m.do("GET", "/", "", m.opts(persona, header("Accept", "text/markdown"))...).Body.String()
		if strings.Contains(list, `"priv"`) != canSee || strings.Contains(home, "/priv/") != canSee || !strings.Contains(list, `"pub"`) {
			t.Errorf("%s: spaces list wrong: %s | %s", persona, list, home)
		}
		// Search never returns pages of a space the caller cannot read.
		hits := m.do("GET", "/_api/search?q=zeppelin", "", m.opts(persona)...).Body.String()
		if strings.Contains(hits, "secret-plan") != canSee {
			t.Errorf("%s: search leak=%v canSee=%v: %s", persona, strings.Contains(hits, "secret-plan"), canSee, hits)
		}
		// Backlinks to a public page leave out pages the caller cannot read.
		back := m.do("GET", "/_api/spaces/pub/pages/start/backlinks", "", m.opts(persona)...).Body.String()
		if strings.Contains(back, "secret-plan") != canSee {
			t.Errorf("%s: backlinks leak: %s", persona, back)
		}
		// Rendering a link into a private space does not reveal whether the page exists.
		page := m.do("GET", "/pub/start", "", m.opts(persona, header("Accept", "text/html"))...).Body.String()
		if strings.Contains(page, `class="missing"`) && canSee {
			t.Errorf("%s: a link to an existing page is marked missing", persona)
		}
		// A private image is not served next to a page either.
		if rec := m.do("GET", "/priv/seed.png", "", m.opts(persona)...); (rec.Code != 404) && !canSee {
			t.Errorf("%s: private path answered %d", persona, rec.Code)
		}
	}
}

// The headers that advertise what a caller may do must match what they may do.
func TestCapabilityHeadersMatchPermissions(t *testing.T) {
	m := newMatrix(t)
	for _, space := range []string{"pub", "priv"} {
		for _, persona := range personas {
			p := expected(persona, space == "pub")
			if !p.read {
				continue
			}
			for _, path := range []string{"/" + space + "/start", "/" + space + "/"} {
				for _, method := range []string{"HEAD", "OPTIONS"} {
					rec := m.do(method, path, "", m.opts(persona, header("Accept", "text/markdown"))...)
					allow := strings.Split(strings.ReplaceAll(rec.Header().Get("Allow"), " ", ""), ",")
					has := func(s string) bool {
						for _, a := range allow {
							if a == s {
								return true
							}
						}
						return false
					}
					label := fmt.Sprintf("%s %s as %s", method, path, persona)
					if !has("GET") || !has("HEAD") || !has("OPTIONS") {
						t.Errorf("%s: Allow = %q lacks the read methods", label, rec.Header().Get("Allow"))
					}
					if has("POST") != p.write {
						t.Errorf("%s: Allow = %q but write=%v", label, rec.Header().Get("Allow"), p.write)
					}
					if strings.HasSuffix(path, "start") && (has("PUT") != p.write || has("DELETE") != p.write || has("PATCH") != p.write) {
						t.Errorf("%s: Allow = %q but write=%v", label, rec.Header().Get("Allow"), p.write)
					}
					if (rec.Header().Get("Accept-Patch") != "") != (p.write && strings.HasSuffix(path, "start")) {
						t.Errorf("%s: Accept-Patch = %q but write=%v", label, rec.Header().Get("Accept-Patch"), p.write)
					}
					user := `user="read"`
					if p.write {
						user = `user="read write append"`
						if !strings.HasSuffix(path, "start") {
							user = `user="read append"`
						}
					}
					pub := `public=""`
					if space == "pub" {
						pub = `public="read"`
					}
					if got, w := rec.Header().Get("WAC-Allow"), user+", "+pub; got != w {
						t.Errorf("%s: WAC-Allow = %q, want %q", label, got, w)
					}
					links := strings.Join(rec.Header().Values("Link"), " ")
					if strings.Contains(links, `rel="edit"`) != (p.write && strings.HasSuffix(path, "start")) {
						t.Errorf("%s: edit link wrong: %q", label, links)
					}
					if (rec.Header().Get("Accept-Post") != "") != p.write {
						t.Errorf("%s: Accept-Post = %q but write=%v", label, rec.Header().Get("Accept-Post"), p.write)
					}
				}
			}
			// Images: DELETE and POST are advertised only to writers.
			a := m.do("OPTIONS", "/"+space+"/_assets/"+m.assets[space], "", m.opts(persona)...)
			if strings.Contains(a.Header().Get("Allow"), "DELETE") != p.write {
				t.Errorf("%s/%s: image Allow = %q", space, persona, a.Header().Get("Allow"))
			}
			u := m.do("OPTIONS", "/"+space+"/_assets", "", m.opts(persona)...)
			if strings.Contains(u.Header().Get("Allow"), "POST") != p.write {
				t.Errorf("%s/%s: library Allow = %q", space, persona, u.Header().Get("Allow"))
			}
		}
	}
	// 401 and 403 answers carry Allow too, so a client can see what is possible.
	rec := m.do("PUT", "/pub/start", "x", header("Content-Type", "text/markdown"))
	if rec.Code != 401 || rec.Header().Get("Allow") != "GET, HEAD, OPTIONS" {
		t.Errorf("anonymous write: %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = m.do("PUT", "/pub/start", "x", header("Content-Type", "text/markdown"), bearer(m.tokens["viewer"]))
	if rec.Code != 403 || rec.Header().Get("Allow") != "GET, HEAD, OPTIONS" {
		t.Errorf("viewer write: %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

// tinyZip is a minimal archive: one page.
func tinyZip(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create("imported-page.md")
	w.Write([]byte("# Imported\n"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
