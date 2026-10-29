package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hbroek/wako/internal/auth"
	"github.com/hbroek/wako/internal/store"
)

func init() { auth.Params = auth.PasswordParams{Time: 1, MemoryKiB: 8, Threads: 1} } // fast hashing in tests

// aenv is a server with sign-in on, a controllable clock, and a few users.
type aenv struct {
	t     *testing.T
	st    *store.Store
	srv   *Server
	now   time.Time
	users map[string]*store.User
}

func (a *aenv) clock() time.Time { return a.now }

func newAuthEnv(t *testing.T, opts ...func(*AuthOptions)) *aenv {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := &aenv{t: t, st: st, now: time.Unix(1_800_000_000, 0), users: map[string]*store.User{}}
	a.srv = New(st, "")
	o := AuthOptions{Now: a.clock}
	for _, f := range opts {
		f(&o)
	}
	if err := a.srv.EnableAuth(o); err != nil {
		t.Fatal(err)
	}
	return a
}

func password(name string) string { return "password-of-" + name }

// user creates an account whose password is password(name).
func (a *aenv) user(name string, admin bool) *store.User {
	a.t.Helper()
	hash, err := auth.HashPassword(password(name))
	if err != nil {
		a.t.Fatal(err)
	}
	u, err := a.st.CreateUser(name, hash, admin)
	if err != nil {
		a.t.Fatal(err)
	}
	a.users[name] = u
	return u
}

func (a *aenv) space(slug string, public bool) *store.Space {
	a.t.Helper()
	sp, err := a.st.CreateSpace(slug, strings.ToUpper(slug[:1])+slug[1:], public)
	if err != nil {
		a.t.Fatal(err)
	}
	return sp
}

// token makes an API token for a user.
func (a *aenv) token(name string) string {
	a.t.Helper()
	secret := "wako_" + auth.RandomToken(16)
	if _, err := a.st.CreateToken(a.users[name].ID, "test", auth.HashToken(secret)); err != nil {
		a.t.Fatal(err)
	}
	return secret
}

// request options
type reqOpt func(*http.Request)

func bearer(tok string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}
func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func host(h string) reqOpt      { return func(r *http.Request) { r.Host = h } }
func remote(addr string) reqOpt { return func(r *http.Request) { r.RemoteAddr = addr } }

// jar keeps cookies between requests the way a browser would.
type jar map[string]*http.Cookie

func (j jar) apply(r *http.Request) {
	for _, c := range j {
		r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
}
func (j jar) store(rec *httptest.ResponseRecorder) {
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(j, c.Name)
		} else {
			j[c.Name] = c
		}
	}
}
func withJar(j jar) reqOpt { return func(r *http.Request) { j.apply(r) } }

// withCSRF adds the anti-forgery header from the jar, as the page script does.
func withCSRF(j jar) reqOpt {
	return func(r *http.Request) {
		if c := j[cookieCSRF]; c != nil {
			r.Header.Set("X-CSRF-Token", c.Value)
		}
	}
}

func (a *aenv) do(method, target, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	a.t.Helper()
	req := localReq(method, target, strings.NewReader(body))
	req.Host = "localhost:8888"
	req.RemoteAddr = "192.0.2.1:5555"
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	a.srv.ServeHTTP(rec, req)
	return rec
}

// asJSON sends a JSON body.
func asJSON(r *http.Request) { r.Header.Set("Content-Type", "application/json") }

// login signs in through the API and returns the browser's cookies.
func (a *aenv) login(name string) jar {
	a.t.Helper()
	j := jar{}
	rec := a.do("POST", "/_api/login", `{"name":"`+name+`","password":"`+password(name)+`"}`, asJSON)
	if rec.Code != 200 {
		a.t.Fatalf("login %s: %d %s", name, rec.Code, rec.Body.String())
	}
	j.store(rec)
	return j
}
