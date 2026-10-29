package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hbroek/wako/internal/store"
)

// Nothing from a private space may reach someone who is not allowed to read it:
// not through a page, an old revision, the APIs, search, the feeds or the export.
// A member of the space is the control that the content is really there.
func TestPrivateSpaceNeverLeaks(t *testing.T) {
	a := newAuthEnv(t)
	a.user("alice", false)
	a.user("bob", false)
	secret := a.space("secret", false)
	pub := a.space("pub", true)
	a.st.SetMember(secret.ID, a.users["alice"].ID, store.RoleEditor)
	a.st.SetMember(pub.ID, a.users["bob"].ID, store.RoleEditor)

	const marker, title = "XYZZY-hidden-words", "Zorblax Plans"
	if _, _, err := secret.PutPage("plans", title, "# "+title+"\n\n"+marker+" and more\n", store.Refs{}, store.Cond{By: "alice"}); err != nil {
		t.Fatal(err)
	}
	secret.PutPage("plans", title, "# "+title+"\n\n"+marker+" edited\n", store.Refs{}, store.Cond{By: "alice"})
	secret.CreateProposal(store.Proposal{Path: "plans", Title: title, Body: "# " + title + "\n\n" + marker + " proposed\n", BaseVersion: 2, BaseBody: "x", Author: "alice"})
	pub.PutPage("home", "Public home", "# Public\n\nlinks to [the plans](/secret/plans)\n", store.Refs{}, store.Cond{By: "bob"})

	inSpace := []string{
		"/secret/plans", "/secret/plans.md", "/secret/plans?rev=1", "/secret/plans?history", "/secret/plans?diff&from=1&to=2",
		"/secret/plans?edit", "/secret/", "/secret/_proposals", "/secret/_proposals/1", "/secret/_proposals?status=all", "/secret/_feed", "/secret/_feed?changes", "/secret/nothing-here",
		"/_api/spaces/secret/tree", "/_api/spaces/secret/search?q=Zorblax", "/_api/spaces/secret/changes",
		"/_api/spaces/secret/export", "/_api/spaces/secret/links/broken", "/_api/spaces/secret/pages/plans/backlinks",
		"/_ui/search?q=Zorblax&space=secret",
	}
	everywhere := []string{
		"/", "/_api/spaces", "/_api/ai", "/_api/search?q=Zorblax", "/_api/search?q=XYZZY", "/_ui/search?q=Zorblax", "/_ui/search?q=XYZZY",
		"/_feed", "/_api/changes", "/pub/home", "/pub/", "/pub/_feed", "/_api/spaces/pub/tree", "/_api/spaces/pub/changes",
		"/_api/spaces/pub/pages/home/links",
	}
	leaks := func(rec *httptest.ResponseRecorder) bool {
		b := rec.Body.String()
		return strings.Contains(b, marker) || strings.Contains(b, title)
	}
	for _, who := range []string{"anonymous", "bob"} {
		var opts []reqOpt
		if who == "bob" {
			opts = append(opts, bearer(a.token("bob")))
		}
		for _, accept := range []string{"text/html", "text/markdown", "application/json", "*/*"} {
			for _, p := range inSpace {
				rec := a.do("GET", p, "", append([]reqOpt{header("Accept", accept)}, opts...)...)
				if leaks(rec) {
					t.Errorf("%s GET %s (Accept %s) leaked: %d %.200s", who, p, accept, rec.Code, rec.Body.String())
				}
				if strings.HasPrefix(p, "/_ui/search") && rec.Code == 200 {
					// An empty result page is fine, if it cannot be told apart from searching a space that does not exist.
					ghost := a.do("GET", strings.ReplaceAll(p, "secret", "ghost"), "", append([]reqOpt{header("Accept", accept)}, opts...)...)
					if strings.ReplaceAll(rec.Body.String(), "secret", "ghost") != ghost.Body.String() {
						t.Errorf("%s: searching the private space differs from searching a missing one", who)
					}
					continue
				}
				if rec.Code == 200 {
					t.Errorf("%s GET %s (Accept %s) answered 200 for a space they cannot read", who, p, accept)
				}
			}
			for _, p := range everywhere {
				rec := a.do("GET", p, "", append([]reqOpt{header("Accept", accept)}, opts...)...)
				if leaks(rec) {
					t.Errorf("%s GET %s (Accept %s) leaked: %d %.200s", who, p, accept, rec.Code, rec.Body.String())
				}
				listsSpaces := p == "/" || p == "/_api/spaces" || p == "/_api/ai" || strings.Contains(p, "search")
				if listsSpaces && (strings.Contains(rec.Body.String(), "/secret") || strings.Contains(rec.Body.String(), `"secret"`)) {
					t.Errorf("%s GET %s (Accept %s) mentions the private space: %.200s", who, p, accept, rec.Body.String())
				}
			}
		}
		// Writing is refused too, and changes nothing.
		for _, m := range []string{"PUT", "POST", "DELETE", "PATCH"} {
			rec := a.do(m, "/secret/plans", "# hacked\n", append([]reqOpt{header("Content-Type", "text/markdown")}, opts...)...)
			if rec.Code < 400 || rec.Code >= 500 {
				t.Errorf("%s %s /secret/plans -> %d", who, m, rec.Code)
			}
		}
		// A page of the public space that links to the private one reveals only the link text it contains itself.
		rec := a.do("GET", "/pub/home", "", append([]reqOpt{header("Accept", "text/html")}, opts...)...)
		if rec.Code != 200 || leaks(rec) {
			t.Errorf("%s: public page: %d, leaks=%v", who, rec.Code, leaks(rec))
		}
	}
	p, _ := secret.GetPage("plans")
	if strings.Contains(p.Body, "hacked") {
		t.Fatal("a refused write changed the page")
	}

	// Control: a member sees it all.
	alice := bearer(a.token("alice"))
	for _, p := range []string{"/secret/plans", "/secret/plans.md", "/_api/search?q=Zorblax", "/_ui/search?q=Zorblax", "/_feed"} {
		rec := a.do("GET", p, "", alice, header("Accept", "text/markdown"))
		if rec.Code != 200 || !leaks(rec) {
			t.Errorf("alice GET %s: %d, sees the content=%v (the test would pass for the wrong reason)", p, rec.Code, leaks(rec))
		}
	}
}
