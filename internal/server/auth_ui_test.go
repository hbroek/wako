package server

import (
	"strings"
	"testing"
	"time"

	"github.com/hbroek/wako/internal/auth"
	"github.com/hbroek/wako/internal/store"
)

func html(extra ...reqOpt) []reqOpt { return append([]reqOpt{header("Accept", "text/html")}, extra...) }

func TestWhatEachVisitorSees(t *testing.T) {
	a := newAuthEnv(t)
	a.user("root", true)
	a.user("ed", false)
	a.user("reader", false)
	sp := a.space("docs", true)
	a.st.SetMember(sp.ID, a.users["ed"].ID, "editor")
	a.st.SetMember(sp.ID, a.users["reader"].ID, "viewer")
	a.do("PUT", "/docs/page", "# Page", header("Content-Type", "text/markdown"), bearer(a.tokenFor("root")))

	type view struct {
		who                                                        string
		opts                                                       []reqOpt
		signIn, signOut, users, newSpace, edit, settings, newChild bool
	}
	jr, je, jv := a.login("root"), a.login("ed"), a.login("reader")
	for _, v := range []view{
		{"anonymous", nil, true, false, false, false, false, false, false},
		{"viewer", []reqOpt{withJar(jv)}, false, true, false, false, false, false, false},
		{"editor", []reqOpt{withJar(je)}, false, true, false, false, true, false, true},
		{"administrator", []reqOpt{withJar(jr)}, false, true, true, true, true, true, true},
	} {
		page := a.do("GET", "/docs/page", "", html(v.opts...)...).Body.String()
		has := func(s string) bool { return strings.Contains(page, s) }
		if has(`/_ui/login?next=`) != v.signIn || has(`id="logout"`) != v.signOut || has(`>Users</a>`) != v.users ||
			has(`+ New space`) != v.newSpace || has(`id="edit-link"`) != v.edit || has(`>Settings</a>`) != v.settings || has(`New child page`) != v.newChild {
			t.Errorf("%s sees the wrong controls", v.who)
		}
		// The sign-in link brings you back to where you were.
		if v.signIn && !has(`/_ui/login?next=%2Fdocs%2Fpage`) {
			t.Errorf("%s: sign-in link does not return to the page", v.who)
		}
		// The editor form is not served to people who cannot save.
		edit := a.do("GET", "/docs/page?edit", "", html(v.opts...)...).Body.String()
		if strings.Contains(edit, `id="src"`) != v.edit {
			t.Errorf("%s: ?edit served=%v", v.who, strings.Contains(edit, `id="src"`))
		}
		create := a.do("GET", "/docs/page?create", "", html(v.opts...)...).Body.String()
		if strings.Contains(create, `id="src"`) != v.newChild {
			t.Errorf("%s: ?create served=%v", v.who, strings.Contains(create, `id="src"`))
		}
	}
	// Pages that need an account send an anonymous visitor to sign in, and back.
	rec := a.do("GET", "/_ui/account", "")
	if rec.Code != 303 || rec.Header().Get("Location") != "/_ui/login?next=%2F_ui%2Faccount" {
		t.Errorf("account page for anonymous: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := a.do("GET", "/_ui/users", ""); rec.Code != 303 {
		t.Errorf("users page for anonymous: %d", rec.Code)
	}
	if rec := a.do("GET", "/_ui/users", "", withJar(jv)); rec.Code != 403 {
		t.Errorf("users page for an ordinary user: %d", rec.Code)
	}
	for _, p := range []string{"/_ui/users", "/_ui/account", "/_ui/new-space"} {
		if rec := a.do("GET", p, "", withJar(jr)); rec.Code != 200 {
			t.Errorf("%s for an administrator: %d", p, rec.Code)
		}
	}
	// The sign-in page, and what it does for someone already signed in.
	if page := status(t, a.do("GET", "/_ui/login?next=%2Fdocs%2Fpage", "", html()...), 200); !strings.Contains(page, `id="login"`) || !strings.Contains(page, `data-next="/docs/page"`) {
		t.Errorf("login page: %s", page)
	}
	if rec := a.do("GET", "/_ui/login?next=%2Fdocs%2Fpage", "", withJar(jv)); rec.Code != 303 || rec.Header().Get("Location") != "/docs/page" {
		t.Errorf("login page when signed in: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// Static assets and the health check need no sign-in.
	for _, p := range []string{"/_ui/app.css", "/_ui/app.js", "/healthz"} {
		if rec := a.do("GET", p, ""); rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	// The script sends the CSRF header.
	if js := a.do("GET", "/_ui/app.js", "").Body.String(); !strings.Contains(js, "X-CSRF-Token") || !strings.Contains(js, "wako_csrf") {
		t.Error("app.js does not send the CSRF token")
	}
}

func TestSignInRedirectsStayOnTheSite(t *testing.T) {
	a := newAuthEnv(t)
	for next, want := range map[string]string{
		"/docs/page":             "/docs/page",
		"/docs/page?edit":        "/docs/page?edit",
		"":                       "/",
		"//evil.example/x":       "/",
		"https://evil.example/x": "/",
		"/\\evil.example":        "/",
		"javascript:alert(1)":    "/",
		"/ok\r\nSet-Cookie: a=b": "/",
		"relative/path":          "/",
		"/docs/\x00":             "/",
		"/docs/\x7f":             "/",
		"/docs/\tx":              "/",
	} {
		if got := a.srv.safeNext(next); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", next, got, want)
		}
	}
	a.srv.base = "/wiki"
	for next, want := range map[string]string{"/wiki/docs": "/wiki/docs", "/docs": "/wiki/", "": "/wiki/"} {
		if got := a.srv.safeNext(next); got != want {
			t.Errorf("with a base path, safeNext(%q) = %q, want %q", next, got, want)
		}
	}
}

func TestBootstrapAndReset(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	created, generated, err := Bootstrap(st, "admin", "")
	if err != nil || !created || len(generated) < 12 {
		t.Fatalf("bootstrap: %v %v %q", created, err, generated)
	}
	u, _ := st.GetUserByName("admin")
	if !u.IsAdmin || !auth.VerifyPassword(generated, u.PasswordHash) || strings.Contains(u.PasswordHash, generated) {
		t.Fatalf("administrator: %+v", u)
	}
	// Only when there is none: a second start changes nothing.
	if created, gen, err := Bootstrap(st, "other", "another-password"); err != nil || created || gen != "" {
		t.Errorf("second bootstrap: %v %q %v", created, gen, err)
	}
	if _, err := st.GetUserByName("other"); err == nil {
		t.Error("second bootstrap created a user")
	}

	// A chosen password is used as given, and validated.
	st2, _ := store.Open(t.TempDir())
	defer st2.Close()
	if _, _, err := Bootstrap(st2, "admin", "short"); err == nil {
		t.Error("weak bootstrap password accepted")
	}
	if _, _, err := Bootstrap(st2, "bad name", "long-enough-pw"); err == nil {
		t.Error("bad administrator name accepted")
	}
	if created, gen, err := Bootstrap(st2, "boss", "chosen-password"); err != nil || !created || gen != "" {
		t.Errorf("chosen password: %v %q %v", created, gen, err)
	}
	// An existing ordinary user with that name is promoted instead.
	st3, _ := store.Open(t.TempDir())
	defer st3.Close()
	hash, _ := auth.HashPassword("old-password")
	st3.CreateUser("sam", hash, false)
	if created, _, err := Bootstrap(st3, "sam", "new-password-1"); err != nil || !created {
		t.Fatalf("promote: %v %v", created, err)
	}
	if sam, _ := st3.GetUserByName("sam"); !sam.IsAdmin || !auth.VerifyPassword("new-password-1", sam.PasswordHash) {
		t.Errorf("promoted user: %+v", sam)
	}

	// Resetting a password ends the sessions.
	now := a0()
	st.CreateSession(u.ID, "r", now, now.Add(time.Hour))
	gen, err := ResetPassword(st, "admin", "")
	if err != nil || gen == "" {
		t.Fatalf("reset: %q %v", gen, err)
	}
	u, _ = st.GetUserByName("admin")
	if !auth.VerifyPassword(gen, u.PasswordHash) {
		t.Error("reset password does not verify")
	}
	if _, _, rotated, err := st.RefreshSession("r", "x", now, now.Add(time.Hour), 1); err == nil || rotated {
		t.Error("sessions survived a password reset")
	}
	if _, err := ResetPassword(st, "admin", "short"); err == nil {
		t.Error("weak reset accepted")
	}
	// A name that does not exist becomes a new administrator (recovery).
	if gen, err := ResetPassword(st, "recovery", "recovery-pass-1"); err != nil || gen != "" {
		t.Fatalf("recovery: %q %v", gen, err)
	}
	if r, _ := st.GetUserByName("recovery"); !r.IsAdmin {
		t.Error("recovery account is not an administrator")
	}
}

func a0() time.Time { return time.Unix(1_800_000_000, 0) }
