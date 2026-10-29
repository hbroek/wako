package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hbroek/wako/internal/auth"
)

func status(t *testing.T, rec *httptest.ResponseRecorder, want int) string {
	t.Helper()
	b := rec.Body.String()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d: %s", rec.Code, want, strings.TrimSpace(b))
	}
	return b
}

func cookieByName(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestLoginSetsSafeCookies(t *testing.T) {
	a := newAuthEnv(t)
	a.user("alice", false)
	body := `{"name":"alice","password":"` + password("alice") + `"}`

	rec := a.do("POST", "/_api/login", body, asJSON)
	if got := status(t, rec, 200); !strings.Contains(got, `"name":"alice"`) || strings.Contains(got, "hash") || strings.Contains(got, "password") {
		t.Fatalf("login response: %s", got)
	}
	at, rt, cs := cookieByName(rec, cookieAccess), cookieByName(rec, cookieRefresh), cookieByName(rec, cookieCSRF)
	if at == nil || rt == nil || cs == nil {
		t.Fatalf("cookies: %v", rec.Result().Cookies())
	}
	for _, c := range []*http.Cookie{at, rt} {
		if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge <= 0 {
			t.Errorf("%s cookie: %+v", c.Name, c)
		}
	}
	if cs.HttpOnly {
		t.Error("the CSRF cookie must be readable by the page script")
	}
	if at.MaxAge != 15*60 || rt.MaxAge != 7*24*3600 {
		t.Errorf("lifetimes: access %d, refresh %d", at.MaxAge, rt.MaxAge)
	}
	if at.Secure {
		t.Error("localhost over HTTP should not require Secure")
	}
	// Over HTTPS (here: via the proxy header) cookies are Secure.
	rec = a.do("POST", "/_api/login", body, asJSON, host("wiki.example.com"), header("X-Forwarded-Proto", "https"), remote("127.0.0.1:9"))
	status(t, rec, 200)
	for _, c := range rec.Result().Cookies() {
		if !c.Secure {
			t.Errorf("%s not Secure over HTTPS", c.Name)
		}
	}
	// Plain HTTP to a real host is refused, unless allowed.
	rec = a.do("POST", "/_api/login", body, asJSON, host("wiki.example.com"))
	if status(t, rec, 400); !strings.Contains(rec.Body.String(), "allow-insecure") || len(rec.Result().Cookies()) != 0 {
		t.Errorf("plain HTTP login: %s %v", rec.Body.String(), rec.Result().Cookies())
	}
	b := newAuthEnv(t, func(o *AuthOptions) { o.AllowInsecure = true })
	b.user("alice", false)
	if rec := b.do("POST", "/_api/login", body, asJSON, host("wiki.example.com")); rec.Code != 200 || cookieByName(rec, cookieAccess).Secure {
		t.Errorf("--allow-insecure: %d", rec.Code)
	}
	// With a base path the cookies are scoped to it.
	c := newAuthEnv(t)
	c.srv.base = "/wiki"
	c.user("alice", false)
	if rec := c.do("POST", "/wiki/_api/login", body, asJSON); cookieByName(rec, cookieAccess).Path != "/wiki" {
		t.Errorf("cookie path: %v", rec.Result().Cookies())
	}
}

func TestLoginRejections(t *testing.T) {
	a := newAuthEnv(t)
	a.user("alice", false)
	for name, c := range map[string]struct {
		method, body string
		opts         []reqOpt
		want         int
	}{
		"wrong password": {"POST", `{"name":"alice","password":"nope-nope"}`, []reqOpt{asJSON}, 401},
		"unknown user":   {"POST", `{"name":"ghost","password":"whatever-1"}`, []reqOpt{asJSON}, 401},
		"empty":          {"POST", `{}`, []reqOpt{asJSON}, 401},
		"not json":       {"POST", `name=alice&password=x`, []reqOpt{header("Content-Type", "application/x-www-form-urlencoded")}, 415},
		"bad json":       {"POST", `{`, []reqOpt{asJSON}, 400},
		"GET":            {"GET", ``, nil, 405},
	} {
		rec := a.do(c.method, "/_api/login", c.body, c.opts...)
		if rec.Code != c.want || len(rec.Result().Cookies()) != 0 {
			t.Errorf("%s: %d %s (cookies %v)", name, rec.Code, rec.Body.String(), rec.Result().Cookies())
		}
	}
	// The same message for a wrong password and an unknown name.
	r1 := a.do("POST", "/_api/login", `{"name":"alice","password":"nope-nope"}`, asJSON).Body.String()
	r2 := a.do("POST", "/_api/login", `{"name":"ghost","password":"nope-nope"}`, asJSON).Body.String()
	if r1 != r2 {
		t.Errorf("responses differ: %q vs %q", r1, r2)
	}
	// Names ignore case.
	if rec := a.do("POST", "/_api/login", `{"name":"ALICE","password":"`+password("alice")+`"}`, asJSON); rec.Code != 200 {
		t.Errorf("case-insensitive name: %d", rec.Code)
	}
}

func TestSessionLifecycle(t *testing.T) {
	a := newAuthEnv(t)
	a.user("alice", false)
	j := a.login("alice")

	me := func(opts ...reqOpt) *httptest.ResponseRecorder {
		rec := a.do("GET", "/_api/me", "", append([]reqOpt{withJar(j)}, opts...)...)
		j.store(rec)
		return rec
	}
	if got := status(t, me(), 200); !strings.Contains(got, `"alice"`) {
		t.Fatalf("me: %s", got)
	}

	// Changes need the CSRF token.
	md := header("Content-Type", "text/markdown")
	a.space("s", true)
	a.st.SetMember(1, a.users["alice"].ID, "editor")
	if rec := a.do("PUT", "/s/page", "x", md, withJar(j)); rec.Code != 403 || !strings.Contains(rec.Body.String(), "CSRF") {
		t.Errorf("cookie write without CSRF: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.do("PUT", "/s/page", "x", md, withJar(j), header("X-CSRF-Token", "forged")); rec.Code != 403 {
		t.Errorf("forged CSRF: %d", rec.Code)
	}
	if rec := a.do("PUT", "/s/page", "x", md, withJar(j), withCSRF(j)); rec.Code != 201 {
		t.Errorf("cookie write with CSRF: %d %s", rec.Code, rec.Body.String())
	}
	// A different session's token does not work.
	other := a.login("alice")
	if rec := a.do("PUT", "/s/page2", "x", md, withJar(j), withCSRF(other)); rec.Code != 403 {
		t.Errorf("another session's CSRF token: %d", rec.Code)
	}
	// Reads and safe methods never need it.
	if rec := a.do("GET", "/s/page.md", "", withJar(j)); rec.Code != 200 {
		t.Errorf("read: %d", rec.Code)
	}

	// The access token expires after 15 minutes; the refresh token renews it
	// without the user doing anything, and rotates itself.
	oldRefresh := j[cookieRefresh].Value
	a.now = a.now.Add(16 * time.Minute)
	rec := me()
	status(t, rec, 200)
	if cookieByName(rec, cookieAccess) == nil || cookieByName(rec, cookieRefresh) == nil || j[cookieRefresh].Value == oldRefresh {
		t.Fatalf("expected new access and refresh cookies after expiry: %v", rec.Result().Cookies())
	}
	// Requests already in flight with the old refresh token still succeed for a
	// moment (and do not set a refresh cookie, so the browser keeps the new one).
	stale := jar{cookieRefresh: &http.Cookie{Name: cookieRefresh, Value: oldRefresh}}
	rec = a.do("GET", "/_api/me", "", withJar(stale))
	status(t, rec, 200)
	if cookieByName(rec, cookieRefresh) != nil {
		t.Error("a raced request must not replace the refresh cookie")
	}
	a.now = a.now.Add(31 * time.Second)
	if rec := a.do("GET", "/_api/me", "", withJar(stale)); rec.Code != 401 {
		t.Errorf("old refresh token after the grace period: %d", rec.Code)
	}
	// ...and a rejected refresh token is cleared from the browser.
	rec = a.do("GET", "/_api/me", "", withJar(stale))
	if c := cookieByName(rec, cookieRefresh); c == nil || c.MaxAge >= 0 {
		t.Errorf("dead cookie not cleared: %v", rec.Result().Cookies())
	}

	// A session that stays idle for a week ends.
	a.now = a.now.Add(8 * 24 * time.Hour)
	if rec := me(); rec.Code != 401 {
		t.Errorf("idle session: %d", rec.Code)
	}

	// Tampered cookies are just ignored.
	j2 := a.login("alice")
	j2[cookieAccess].Value = j2[cookieAccess].Value[:len(j2[cookieAccess].Value)-2] + "xx"
	delete(j2, cookieRefresh)
	if rec := a.do("GET", "/_api/me", "", withJar(j2)); rec.Code != 401 {
		t.Errorf("tampered access token: %d", rec.Code)
	}

	// Signing out ends the session at once, even though the access token has not expired.
	j3 := a.login("alice")
	saved := jar{cookieAccess: j3[cookieAccess]}
	rec = a.do("POST", "/_api/logout", "", withJar(j3), withCSRF(j3))
	status(t, rec, 204)
	for _, n := range []string{cookieAccess, cookieRefresh, cookieCSRF} {
		if c := cookieByName(rec, n); c == nil || c.MaxAge >= 0 {
			t.Errorf("%s not cleared on sign-out", n)
		}
	}
	if rec := a.do("GET", "/_api/me", "", withJar(saved)); rec.Code != 401 {
		t.Errorf("access cookie still works after sign-out: %d", rec.Code)
	}
	// Signing out needs the CSRF token too, so another site cannot do it.
	j4 := a.login("alice")
	if rec := a.do("POST", "/_api/logout", "", withJar(j4)); rec.Code != 403 {
		t.Errorf("logout without CSRF: %d", rec.Code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	a := newAuthEnv(t)
	a.user("alice", false)
	a.user("bob", false)
	bad := func(name, addr string) int {
		return a.do("POST", "/_api/login", `{"name":"`+name+`","password":"wrong-password"}`, asJSON, remote(addr)).Code
	}
	for i := 0; i < 5; i++ {
		if c := bad("alice", "198.51.100.7:1000"); c != 401 {
			t.Fatalf("attempt %d: %d", i, c)
		}
	}
	// Blocked now, and even the right password is refused: no oracle.
	rec := a.do("POST", "/_api/login", `{"name":"alice","password":"`+password("alice")+`"}`, asJSON, remote("198.51.100.7:1001"))
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("after 5 failures: %d retry=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	// Other users and other addresses are not affected.
	if c := a.do("POST", "/_api/login", `{"name":"bob","password":"`+password("bob")+`"}`, asJSON, remote("198.51.100.7:1002")).Code; c != 200 {
		t.Errorf("another user from the same address: %d", c)
	}
	if c := a.do("POST", "/_api/login", `{"name":"alice","password":"`+password("alice")+`"}`, asJSON, remote("203.0.113.9:1")).Code; c != 200 {
		t.Errorf("same user from another address: %d", c)
	}
	// The window passes.
	a.now = a.now.Add(16 * time.Minute)
	if c := a.do("POST", "/_api/login", `{"name":"alice","password":"`+password("alice")+`"}`, asJSON, remote("198.51.100.7:1003")).Code; c != 200 {
		t.Errorf("after the window: %d", c)
	}
	// An address that tries many names is blocked as a whole.
	for i := 0; i < 30; i++ {
		bad("guess"+strings.Repeat("x", i), "192.0.2.50:1")
	}
	if c := a.do("POST", "/_api/login", `{"name":"bob","password":"`+password("bob")+`"}`, asJSON, remote("192.0.2.50:2")).Code; c != 429 {
		t.Errorf("address flooding names: %d", c)
	}
	// A success clears the user's counter.
	for i := 0; i < 4; i++ {
		bad("bob", "192.0.2.77:1")
	}
	a.do("POST", "/_api/login", `{"name":"bob","password":"`+password("bob")+`"}`, asJSON, remote("192.0.2.77:2"))
	for i := 0; i < 4; i++ {
		if c := bad("bob", "192.0.2.77:3"); c != 401 {
			t.Errorf("counter not reset by a successful login: %d", c)
		}
	}
}

func TestAPITokens(t *testing.T) {
	a := newAuthEnv(t)
	a.user("alice", false)
	sp := a.space("s", false)
	a.st.SetMember(sp.ID, a.users["alice"].ID, "editor")
	j := a.login("alice")
	md := header("Content-Type", "text/markdown")

	rec := a.do("POST", "/_api/tokens", `{"name":"ci"}`, asJSON, withJar(j), withCSRF(j))
	var created struct {
		ID    int64
		Name  string
		Token string
	}
	json.Unmarshal([]byte(status(t, rec, 201)), &created)
	if !strings.HasPrefix(created.Token, "wako_") || len(created.Token) < 40 || created.Name != "ci" {
		t.Fatalf("token: %+v", created)
	}
	// The listing never shows the secret again.
	list := status(t, a.do("GET", "/_api/tokens", "", withJar(j)), 200)
	if strings.Contains(list, created.Token) || !strings.Contains(list, `"ci"`) {
		t.Errorf("list: %s", list)
	}
	// It authenticates without cookies or CSRF, with the owner's rights.
	if rec := a.do("PUT", "/s/page", "# P", md, bearer(created.Token)); rec.Code != 201 {
		t.Errorf("write with token: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.do("GET", "/_api/me", "", bearer(created.Token)); rec.Code != 200 {
		t.Errorf("me with token: %d", rec.Code)
	}
	if rec := a.do("GET", "/_api/users", "", bearer(created.Token)); rec.Code != 403 {
		t.Errorf("token must not exceed its owner's rights: %d", rec.Code)
	}
	// Tokens cannot manage tokens or change passwords.
	if rec := a.do("POST", "/_api/tokens", `{"name":"more"}`, asJSON, bearer(created.Token)); rec.Code != 403 {
		t.Errorf("token creating a token: %d", rec.Code)
	}
	if rec := a.do("PUT", "/_api/me", `{"current_password":"x","new_password":"yyyyyyyy"}`, asJSON, bearer(created.Token)); rec.Code != 403 {
		t.Errorf("token changing the password: %d", rec.Code)
	}
	// A bad token fails loudly, even for a public read, instead of silently acting as anonymous.
	a.space("open", true)
	rec = a.do("GET", "/open/", "", bearer("wako_not-a-token"))
	if rec.Code != 401 || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "invalid_token") {
		t.Errorf("bad token: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	if rec := a.do("GET", "/open/", "", header("Authorization", "Bearer ")); rec.Code != 401 {
		t.Errorf("empty bearer: %d", rec.Code)
	}
	// Other schemes are not ours: treated as anonymous.
	if rec := a.do("GET", "/open/", "", header("Authorization", "Basic abc")); rec.Code != 200 {
		t.Errorf("basic auth header: %d", rec.Code)
	}
	// Revoking works at once, and only for the owner.
	b := a.user("bob", false)
	_ = b
	jb := a.login("bob")
	if rec := a.do("DELETE", "/_api/tokens/"+itoa(created.ID), "", withJar(jb), withCSRF(jb)); rec.Code != 404 {
		t.Errorf("revoking someone else's token: %d", rec.Code)
	}
	status(t, a.do("DELETE", "/_api/tokens/"+itoa(created.ID), "", withJar(j), withCSRF(j)), 204)
	if rec := a.do("GET", "/_api/me", "", bearer(created.Token)); rec.Code != 401 {
		t.Errorf("revoked token: %d", rec.Code)
	}
	// Names are required.
	if rec := a.do("POST", "/_api/tokens", `{"name":"  "}`, asJSON, withJar(j), withCSRF(j)); rec.Code != 400 {
		t.Errorf("empty name: %d", rec.Code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestChangingYourPassword(t *testing.T) {
	a := newAuthEnv(t)
	a.user("alice", false)
	j, other := a.login("alice"), a.login("alice")
	put := func(cur, next string) *httptest.ResponseRecorder {
		return a.do("PUT", "/_api/me", `{"current_password":"`+cur+`","new_password":"`+next+`"}`, asJSON, withJar(j), withCSRF(j))
	}
	if rec := put("wrong-password", "brand-new-pass"); rec.Code != 403 {
		t.Errorf("wrong current password: %d", rec.Code)
	}
	if rec := put(password("alice"), "short"); rec.Code != 400 {
		t.Errorf("weak new password: %d", rec.Code)
	}
	rec := put(password("alice"), "brand-new-pass")
	status(t, rec, 204)
	j.store(rec)
	if rec := a.do("GET", "/_api/me", "", withJar(j)); rec.Code != 200 {
		t.Errorf("this browser must stay signed in: %d", rec.Code)
	}
	if rec := a.do("GET", "/_api/me", "", withJar(other)); rec.Code != 401 {
		t.Errorf("other sessions must end: %d", rec.Code)
	}
	if rec := a.do("POST", "/_api/login", `{"name":"alice","password":"`+password("alice")+`"}`, asJSON); rec.Code != 401 {
		t.Errorf("old password: %d", rec.Code)
	}
	if rec := a.do("POST", "/_api/login", `{"name":"alice","password":"brand-new-pass"}`, asJSON); rec.Code != 200 {
		t.Errorf("new password: %d", rec.Code)
	}
}

func TestUserManagement(t *testing.T) {
	a := newAuthEnv(t)
	a.user("root", true)
	a.user("alice", false)
	jr, ja := a.login("root"), a.login("alice")
	call := func(j jar, method, path, body string) *httptest.ResponseRecorder {
		return a.do(method, path, body, asJSON, withJar(j), withCSRF(j))
	}

	if rec := call(ja, "GET", "/_api/users", ""); rec.Code != 403 {
		t.Errorf("ordinary user listing users: %d", rec.Code)
	}
	if rec := a.do("GET", "/_api/users", ""); rec.Code != 401 {
		t.Errorf("anonymous listing users: %d", rec.Code)
	}
	list := status(t, call(jr, "GET", "/_api/users", ""), 200)
	if strings.Contains(list, "hash") || !strings.Contains(list, `"root"`) || !strings.Contains(list, `"alice"`) {
		t.Errorf("list: %s", list)
	}

	status(t, call(jr, "POST", "/_api/users", `{"name":"carol","password":"carols-password"}`), 201)
	for name, c := range map[string]struct {
		body string
		want int
	}{
		"duplicate (any case)": {`{"name":"CAROL","password":"carols-password"}`, 409},
		"bad name":             {`{"name":"a b","password":"carols-password"}`, 400},
		"empty name":           {`{"name":"","password":"carols-password"}`, 400},
		"weak password":        {`{"name":"dave","password":"short"}`, 400},
	} {
		if rec := call(jr, "POST", "/_api/users", c.body); rec.Code != c.want {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if rec := a.do("POST", "/_api/login", `{"name":"carol","password":"carols-password"}`, asJSON); rec.Code != 200 {
		t.Errorf("new user cannot sign in: %d", rec.Code)
	}

	// Promote, demote, and the last-administrator rules.
	status(t, call(jr, "PUT", "/_api/users/alice", `{"is_admin":true}`), 200)
	if rec := call(ja, "GET", "/_api/users", ""); rec.Code != 200 {
		t.Errorf("promoted user: %d", rec.Code)
	}
	status(t, call(jr, "PUT", "/_api/users/alice", `{"is_admin":false}`), 200)
	if rec := call(jr, "PUT", "/_api/users/root", `{"is_admin":false}`); rec.Code != 409 {
		t.Errorf("demoting the last admin: %d", rec.Code)
	}
	if rec := call(jr, "DELETE", "/_api/users/root", ""); rec.Code != 409 {
		t.Errorf("deleting the last admin: %d", rec.Code)
	}

	// An administrator setting a password ends that user's sessions.
	status(t, call(jr, "PUT", "/_api/users/alice", `{"password":"changed-by-admin"}`), 200)
	if rec := a.do("GET", "/_api/me", "", withJar(ja)); rec.Code != 401 {
		t.Errorf("session survived an admin password reset: %d", rec.Code)
	}
	if rec := a.do("POST", "/_api/login", `{"name":"alice","password":"changed-by-admin"}`, asJSON); rec.Code != 200 {
		t.Errorf("reset password: %d", rec.Code)
	}
	if rec := call(jr, "PUT", "/_api/users/alice", `{"password":"x"}`); rec.Code != 400 {
		t.Errorf("weak reset: %d", rec.Code)
	}

	// Deleting a user removes everything of theirs.
	sp := a.space("s", false)
	carol, _ := a.st.GetUserByName("carol")
	a.st.SetMember(sp.ID, carol.ID, "editor")
	tok := a.token2(carol.ID)
	status(t, call(jr, "DELETE", "/_api/users/carol", ""), 204)
	if rec := a.do("GET", "/_api/me", "", bearer(tok)); rec.Code != 401 {
		t.Errorf("token of a deleted user: %d", rec.Code)
	}
	if m, _ := a.st.ListMembers(sp.ID); len(m) != 0 {
		t.Errorf("membership of a deleted user: %+v", m)
	}
	if rec := call(jr, "DELETE", "/_api/users/carol", ""); rec.Code != 404 {
		t.Errorf("deleting twice: %d", rec.Code)
	}
}

func (a *aenv) token2(uid int64) string {
	secret := "wako_" + auth.RandomToken(16)
	a.st.CreateToken(uid, "t", auth.HashToken(secret))
	return secret
}

func TestMemberManagement(t *testing.T) {
	a := newAuthEnv(t)
	a.user("root", true)
	a.user("boss", false)
	a.user("worker", false)
	a.user("newbie", false)
	sp := a.space("team", false)
	a.st.SetMember(sp.ID, a.users["boss"].ID, "admin")
	jb, jw := a.login("boss"), a.login("worker")
	md := header("Content-Type", "text/markdown")
	call := func(j jar, method, path, body string) *httptest.ResponseRecorder {
		return a.do(method, path, body, asJSON, withJar(j), withCSRF(j))
	}

	// Before: worker has no access to the private space at all.
	if rec := a.do("GET", "/team/", "", withJar(jw)); rec.Code != 404 {
		t.Fatalf("outsider sees a private space: %d", rec.Code)
	}
	status(t, call(jb, "PUT", "/_api/spaces/team/members/worker", `{"role":"viewer"}`), 200)
	if rec := a.do("GET", "/team/", "", withJar(jw)); rec.Code != 200 {
		t.Errorf("viewer cannot read: %d", rec.Code)
	}
	if rec := a.do("PUT", "/team/p", "x", md, withJar(jw), withCSRF(jw)); rec.Code != 403 {
		t.Errorf("viewer can write: %d", rec.Code)
	}
	// The change applies to a signed-in session straight away.
	status(t, call(jb, "PUT", "/_api/spaces/team/members/worker", `{"role":"editor"}`), 200)
	if rec := a.do("PUT", "/team/p", "x", md, withJar(jw), withCSRF(jw)); rec.Code != 201 {
		t.Errorf("editor cannot write: %d", rec.Code)
	}
	// ...but editors cannot manage members or settings.
	if rec := call(jw, "PUT", "/_api/spaces/team/members/newbie", `{"role":"viewer"}`); rec.Code != 403 {
		t.Errorf("editor adding a member: %d", rec.Code)
	}
	list := status(t, call(jb, "GET", "/_api/spaces/team/members", ""), 200)
	if !strings.Contains(list, `{"name":"worker","role":"editor"}`) || !strings.Contains(list, `"boss"`) {
		t.Errorf("members: %s", list)
	}
	for name, c := range map[string]struct {
		path, body string
		want       int
	}{
		"unknown user": {"/_api/spaces/team/members/ghost", `{"role":"viewer"}`, 404},
		"bad role":     {"/_api/spaces/team/members/newbie", `{"role":"owner"}`, 400},
		"no role":      {"/_api/spaces/team/members/newbie", `{}`, 400},
	} {
		if rec := call(jb, "PUT", c.path, c.body); rec.Code != c.want {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	// Removing a member takes the access away at once.
	status(t, call(jb, "DELETE", "/_api/spaces/team/members/worker", ""), 204)
	if rec := a.do("GET", "/team/", "", withJar(jw)); rec.Code != 404 {
		t.Errorf("removed member still sees the space: %d", rec.Code)
	}
	if rec := call(jb, "DELETE", "/_api/spaces/team/members/worker", ""); rec.Code != 404 {
		t.Errorf("removing twice: %d", rec.Code)
	}
	// A space administrator may change public access and the title, but not delete the space.
	status(t, call(jb, "PUT", "/_api/spaces/team", `{"public_read":true,"title":"Team"}`), 200)
	if rec := a.do("GET", "/team/", ""); rec.Code != 200 {
		t.Errorf("public space for anonymous: %d", rec.Code)
	}
	if rec := call(jb, "DELETE", "/_api/spaces/team", ""); rec.Code != 403 {
		t.Errorf("space admin deleting the space: %d", rec.Code)
	}
	// Deleting a space removes its memberships.
	status(t, a.do("DELETE", "/_api/spaces/team", "", asJSON, bearer(a.tokenFor("root"))), 204)
	if roles, _ := a.st.UserRoles(a.users["boss"].ID); len(roles) != 0 {
		t.Errorf("memberships of a deleted space: %v", roles)
	}
}

func (a *aenv) tokenFor(name string) string { return a.token2(a.users[name].ID) }

func TestOpenModeWithoutAuth(t *testing.T) {
	e := newEnv(t) // no EnableAuth
	for _, p := range []string{"/_api/login", "/_api/me", "/_api/users", "/_api/tokens", "/_api/logout"} {
		if rec := e.do("POST", p, "{}", "Content-Type", "application/json"); rec.Code != 404 {
			t.Errorf("%s with sign-in off: %d", p, rec.Code)
		}
	}
	if rec := e.do("GET", "/_ui/login", ""); rec.Code != 303 {
		t.Errorf("login page with sign-in off: %d", rec.Code)
	}
	// Everything works for everyone, including with a bogus Authorization header.
	expect(t, e.do("PUT", "/main/p", "x", "Content-Type", md, "Authorization", "Bearer junk"), 201)
	expect(t, e.do("PUT", "/_api/spaces/main", `{"title":"T"}`, "Content-Type", "application/json"), 200)
}
