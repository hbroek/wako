package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	xhtml "golang.org/x/net/html"

	"github.com/hbroek/wako/internal/store"
)

// ---- headers and the content security policy ----

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/p", "# P\n\n![x](shot.png)\n", "Content-Type", md), 201)
	e.upload("main", "shot.png", pngBytes(t, 3))
	feed := "/main/_feed"
	for name, c := range map[string]struct {
		path, accept string
		csp          string
	}{
		"html page":   {"/main/p", "text/html", pageCSP},
		"home":        {"/", "text/html", pageCSP},
		"missing":     {"/main/nope", "text/html", pageCSP},
		"markdown":    {"/main/p.md", "", apiCSP},
		"json":        {"/_api/spaces", "", apiCSP},
		"feed":        {feed, "", apiCSP},
		"plain error": {"/main/p?rev=9", "text/markdown", apiCSP},
		"static":      {"/_ui/app.js", "", apiCSP},
	} {
		rec := e.do("GET", c.path, "", "Accept", c.accept)
		h := rec.Header()
		for k, want := range map[string]string{
			"X-Content-Type-Options":       "nosniff",
			"X-Frame-Options":              "DENY",
			"Referrer-Policy":              "same-origin",
			"Cross-Origin-Opener-Policy":   "same-origin",
			"Cross-Origin-Resource-Policy": "same-origin",
			"Content-Security-Policy":      c.csp,
		} {
			if h.Get(k) != want {
				t.Errorf("%s: %s = %q, want %q", name, k, h.Get(k), want)
			}
		}
		if !strings.Contains(h.Get("Permissions-Policy"), "camera=()") {
			t.Errorf("%s: Permissions-Policy %q", name, h.Get("Permissions-Policy"))
		}
		if h.Get("Strict-Transport-Security") != "" {
			t.Errorf("%s: HSTS over plain HTTP", name)
		}
	}
	// Uploaded pictures keep their own, stricter policy.
	up := e.upload("main", "shot2.png", pngBytes(t, 8))
	var a assetJSON
	json.Unmarshal(up.Body.Bytes(), &a)
	if csp := e.do("GET", a.URL, "").Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("image CSP: %q", csp)
	}
	// Account and listing data must not sit in shared caches; pages and feeds decide for themselves.
	if cc := e.do("GET", "/_api/spaces", "").Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("API cache control %q", cc)
	}
	if cc := e.do("GET", "/_api/spaces/main/export", "").Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("export cache control %q", cc)
	}
	if cc := e.do("GET", feed, "").Header().Get("Cache-Control"); !strings.HasPrefix(cc, "public") {
		t.Errorf("feed cache control %q", cc)
	}
	// HSTS only for connections that are encrypted, directly or at a trusted proxy.
	if hs := e.do("GET", "/main/p", "", "X-Forwarded-Proto", "https").Header().Get("Strict-Transport-Security"); !strings.HasPrefix(hs, "max-age=") {
		t.Errorf("HSTS behind a TLS proxy: %q", hs)
	}
	// Every response has an id to quote in a bug report, and they differ.
	r1, r2 := e.do("GET", "/healthz", ""), e.do("GET", "/healthz", "")
	if len(r1.Header().Get("X-Request-Id")) != 16 || r1.Header().Get("X-Request-Id") == r2.Header().Get("X-Request-Id") {
		t.Errorf("request ids: %q %q", r1.Header().Get("X-Request-Id"), r2.Header().Get("X-Request-Id"))
	}
}

// The policy forbids inline script, so no page may contain any.
func TestPagesNeedNoInlineScript(t *testing.T) {
	a := newAuthEnv(t)
	a.user("root", true)
	a.space("main", true)
	root := bearer(a.token("root"))
	mdT := header("Content-Type", "text/markdown")
	a.do("PUT", "/main/docs", "# Docs\n\n![x](shot.png) <b>raw</b> [l](https://example.com)\n", mdT, root)
	a.do("PUT", "/main/docs/child", "# Child\n", mdT, root)
	a.do("POST", "/main/_assets?name=shot.png", string(pngBytes(t, 4)), header("Content-Type", "image/png"), root)
	a.do("PUT", "/main/docs", "# Docs\n\n![x](shot.png) <b>raw</b>\n", mdT, root)
	j := a.login("root")
	pages := []string{"/", "/main/", "/main/docs", "/main/docs?edit", "/main/docs?create", "/main/nope", "/main/nope?new", "/main/docs?history",
		"/main/docs?rev=1", "/main/docs?diff=1", "/main/?settings", "/main/?changes", "/main/?broken", "/main/_assets/", "/_ui/search?q=docs",
		"/_ui/login", "/_ui/account", "/_ui/users", "/_ui/new-space"}
	for _, p := range pages {
		opts := []reqOpt{header("Accept", "text/html"), withJar(j)}
		if p == "/_ui/login" {
			opts = opts[:1] // the sign-in page sends signed-in visitors away; look at it as a stranger
		}
		rec := a.do("GET", p, "", opts...)
		if rec.Code != 200 && rec.Code != 404 {
			t.Errorf("%s: %d", p, rec.Code)
			continue
		}
		if csp := rec.Header().Get("Content-Security-Policy"); csp != pageCSP {
			t.Errorf("%s: CSP %q", p, csp)
		}
		doc, err := xhtml.Parse(rec.Body)
		if err != nil {
			t.Fatal(err)
		}
		var walk func(n *xhtml.Node)
		walk = func(n *xhtml.Node) {
			if n.Type == xhtml.ElementNode {
				if n.Data == "script" {
					hasSrc := false
					for _, at := range n.Attr {
						hasSrc = hasSrc || at.Key == "src"
					}
					if !hasSrc || n.FirstChild != nil {
						t.Errorf("%s: inline <script>", p)
					}
				}
				if n.Data == "style" {
					t.Errorf("%s: inline <style>", p)
				}
				for _, at := range n.Attr {
					v := strings.ToLower(strings.TrimSpace(at.Val))
					switch {
					case strings.HasPrefix(at.Key, "on"):
						t.Errorf("%s: <%s %s=…> event handler", p, n.Data, at.Key)
					case (at.Key == "href" || at.Key == "src" || at.Key == "action") && (strings.HasPrefix(v, "javascript:") || strings.HasPrefix(v, "data:text") || strings.HasPrefix(v, "vbscript:")):
						t.Errorf("%s: <%s %s=%q>", p, n.Data, at.Key, at.Val)
					case at.Key == "style" && n.Data != "img":
						t.Errorf("%s: style attribute on <%s> (only images carry one)", p, n.Data)
					}
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		walk(doc)
	}
}

// ---- cross-site requests ----

func TestCrossSiteChangesAreRefused(t *testing.T) {
	for _, mode := range []string{"sign-in off", "signed in"} {
		var do func(method, path, body string, o ...reqOpt) *httptest.ResponseRecorder
		var tok reqOpt = func(*http.Request) {}
		var srv *Server
		if mode == "sign-in off" {
			e := newEnv(t)
			srv = e.h.(*Server)
			do = func(method, path, body string, o ...reqOpt) *httptest.ResponseRecorder {
				req := localReq(method, path, strings.NewReader(body))
				for _, f := range o {
					f(req)
				}
				rec := httptest.NewRecorder()
				e.h.ServeHTTP(rec, req)
				return rec
			}
			expect(t, e.do("PUT", "/main/p", "v1", "Content-Type", md), 201)
		} else {
			a := newAuthEnv(t)
			a.user("root", true)
			a.space("main", true)
			srv = a.srv
			tok = bearer(a.token("root"))
			do = func(method, path, body string, o ...reqOpt) *httptest.ResponseRecorder {
				return a.do(method, path, body, append(o, tok)...)
			}
			do("PUT", "/main/p", "v1", header("Content-Type", "text/markdown"))
		}
		_ = srv
		body := func() string { return status(t, do("GET", "/main/p.md", ""), 200) }
		mdT := header("Content-Type", "text/markdown")

		// A page on another site posts a form to the wiki: refused, nothing changes.
		for name, o := range map[string][]reqOpt{
			"foreign Origin":           {header("Origin", "https://evil.example")},
			"opaque Origin":            {header("Origin", "null")},
			"Sec-Fetch-Site":           {header("Sec-Fetch-Site", "cross-site")},
			"same-site Sec-Fetch-Site": {header("Sec-Fetch-Site", "same-site")},
			"foreign port":             {header("Origin", "http://localhost:9999")},
			"garbage Origin":           {header("Origin", "::::")},
		} {
			for _, c := range []struct{ method, path, b string }{
				{"POST", "/main/p?restore=1", ""}, {"PUT", "/main/p", "defaced"}, {"DELETE", "/main/p", ""},
				{"POST", "/main/p?move=moved", ""}, {"POST", "/_api/spaces", `{"slug":"x"}`},
			} {
				rec := do(c.method, c.path, c.b, append([]reqOpt{mdT}, o...)...)
				if rec.Code != 403 || !strings.Contains(rec.Body.String(), "another site") {
					t.Errorf("%s / %s %s %s: %d %s", mode, name, c.method, c.path, rec.Code, strings.TrimSpace(rec.Body.String()))
				}
			}
		}
		if got := body(); got != "v1" {
			t.Errorf("%s: a refused request changed the page: %q", mode, got)
		}
		// The wiki's own pages, programs, and reading are all fine.
		for name, o := range map[string][]reqOpt{
			"no headers (curl, wako)": nil,
			"same Origin":             {header("Origin", "http://localhost:8888")},
			"same-origin fetch":       {header("Sec-Fetch-Site", "same-origin"), header("Origin", "http://localhost:8888")},
			"typed address":           {header("Sec-Fetch-Site", "none")},
		} {
			if rec := do("PUT", "/main/p", "from "+name, append([]reqOpt{mdT}, o...)...); rec.Code != 200 {
				t.Errorf("%s / %s: %d %s", mode, name, rec.Code, rec.Body.String())
			}
		}
		if rec := do("GET", "/main/p", "", header("Origin", "https://evil.example"), header("Sec-Fetch-Site", "cross-site")); rec.Code != 200 {
			t.Errorf("%s: reading is never refused on these grounds: %d", mode, rec.Code)
		}
		// The configured public address and extra hosts are "this site" too.
		srv.PublicURL = "https://wiki.example.org"
		srv.AllowedHosts = []string{"wiki.lan"}
		for _, origin := range []string{"https://wiki.example.org", "http://wiki.lan", "https://wiki.lan:8443"} {
			if rec := do("PUT", "/main/p", "via "+origin, mdT, header("Origin", origin)); rec.Code != 200 {
				t.Errorf("%s: Origin %s: %d", mode, origin, rec.Code)
			}
		}
		// A trusted proxy tells us the host the browser used.
		if mode == "signed in" {
			if rec := do("PUT", "/main/p", "proxied", mdT, header("Origin", "https://wiki.corp"), header("X-Forwarded-Host", "wiki.corp"), remote("127.0.0.1:9")); rec.Code != 200 {
				t.Errorf("%s: Origin matching X-Forwarded-Host from a trusted proxy: %d", mode, rec.Code)
			}
			if rec := do("PUT", "/main/p", "spoofed", mdT, header("Origin", "https://wiki.corp"), header("X-Forwarded-Host", "wiki.corp"), remote("203.0.113.5:1")); rec.Code != 403 {
				t.Errorf("%s: X-Forwarded-Host from an untrusted peer must not count: %d", mode, rec.Code)
			}
		}
	}
}

// ---- the host check for servers without sign-in ----

func TestHostCheckProtectsAgainstDNSRebinding(t *testing.T) {
	e := newEnv(t) // sign-in off
	srv := e.h.(*Server)
	get := func(hostHeader string, o ...reqOpt) int {
		req := localReq("GET", "/main/", nil)
		req.Host = hostHeader
		for _, f := range o {
			f(req)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, ok := range []string{"localhost", "localhost:8888", "LOCALHOST", "wiki.localhost", "127.0.0.1", "127.0.0.1:9", "[::1]:8888", "[::1]", "192.168.1.20:8888", "10.0.0.5", "[fe80::1]:80"} {
		if c := get(ok); c != 200 {
			t.Errorf("Host %q: %d", ok, c)
		}
	}
	for _, bad := range []string{"evil.example", "evil.example:8888", "attacker.com.localhost.evil.example", "wiki.corp", "", "localhost.evil.example"} {
		if c := get(bad); c != 421 {
			t.Errorf("Host %q: %d, want 421", bad, c)
		}
	}
	// A message that tells the operator what to do.
	req := localReq("GET", "/", nil)
	req.Host = "wiki.corp"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "--allowed-host") {
		t.Errorf("message: %s", rec.Body.String())
	}
	// Allowed by configuration, by the public address, or by a trusted proxy's say-so.
	srv.AllowedHosts = []string{"wiki.corp", "Notes.Example.com:8080"}
	srv.PublicURL = "https://wiki.example.org/"
	for _, ok := range []string{"wiki.corp", "wiki.corp:8888", "notes.example.com", "wiki.example.org"} {
		if c := get(ok); c != 200 {
			t.Errorf("configured Host %q: %d", ok, c)
		}
	}
	if c := get("internal.example", header("X-Forwarded-Host", "wiki.corp")); c != 200 {
		t.Errorf("X-Forwarded-Host from a trusted proxy: %d", c)
	}
	if c := get("internal.example", header("X-Forwarded-Host", "wiki.corp"), remote("203.0.113.5:1")); c != 421 {
		t.Errorf("X-Forwarded-Host from a stranger: %d", c)
	}
	// With sign-in on the host does not matter: nothing is gained by rebinding.
	a := newAuthEnv(t)
	a.space("main", true)
	if rec := a.do("GET", "/main/", "", host("evil.example")); rec.Code != 200 {
		t.Errorf("signed-in server: %d", rec.Code)
	}
}

// ---- which proxies are believed ----

func TestTrustedProxies(t *testing.T) {
	s := &Server{}
	req := func(remoteAddr string, hdr ...string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remoteAddr
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Add(hdr[i], hdr[i+1])
		}
		return r
	}
	// By default only this machine is a proxy.
	for _, c := range []struct {
		name, want string
		r          *http.Request
	}{
		{"direct client", "203.0.113.9", req("203.0.113.9:5000")},
		{"stranger lying about X-Forwarded-For", "203.0.113.9", req("203.0.113.9:5000", "X-Forwarded-For", "1.2.3.4")},
		{"local proxy", "198.51.100.7", req("127.0.0.1:5000", "X-Forwarded-For", "198.51.100.7")},
		{"local proxy, several hops", "198.51.100.7", req("[::1]:5000", "X-Forwarded-For", "9.9.9.9, 198.51.100.7, 127.0.0.2")},
		{"client sent its own X-Forwarded-For first", "198.51.100.7", req("127.0.0.1:1", "X-Forwarded-For", "6.6.6.6", "X-Forwarded-For", "198.51.100.7")},
		{"proxy without the header", "127.0.0.1", req("127.0.0.1:5000")},
		{"garbage in the header", "127.0.0.1", req("127.0.0.1:5000", "X-Forwarded-For", "not-an-ip")},
		{"a Unix socket", "198.51.100.7", req("", "X-Forwarded-For", "198.51.100.7")},
		{"IPv4-mapped IPv6 loopback", "198.51.100.7", req("[::ffff:127.0.0.1]:5000", "X-Forwarded-For", "198.51.100.7")},
	} {
		if got := s.clientIP(c.r); got != c.want {
			t.Errorf("%s: clientIP = %q, want %q", c.name, got, c.want)
		}
	}
	// HTTPS and the host are believed from the same peers only.
	if s.isHTTPS(req("203.0.113.9:1", "X-Forwarded-Proto", "https")) || !s.isHTTPS(req("127.0.0.1:1", "X-Forwarded-Proto", "https")) ||
		s.isHTTPS(req("127.0.0.1:1", "X-Forwarded-Proto", "http")) || !s.isHTTPS(req("127.0.0.1:1", "X-Forwarded-Proto", "HTTPS, http")) {
		t.Error("isHTTPS")
	}
	if got := s.requestHost(req("127.0.0.1:1", "X-Forwarded-Host", "wiki.corp, other")); got != "wiki.corp" {
		t.Errorf("requestHost behind a proxy: %q", got)
	}
	if got := s.requestHost(req("203.0.113.9:1", "X-Forwarded-Host", "wiki.corp")); got != "example.com" {
		t.Errorf("requestHost from a stranger: %q", got)
	}
	// Configured proxies.
	ps, err := ParseProxies("10.0.0.0/8, 192.168.1.5 ,2001:db8::/32")
	if err != nil || len(ps) != 3 {
		t.Fatalf("ParseProxies: %v %v", ps, err)
	}
	s.TrustedProxies = ps
	if got := s.clientIP(req("10.1.2.3:1", "X-Forwarded-For", "198.51.100.7")); got != "198.51.100.7" {
		t.Errorf("configured proxy: %q", got)
	}
	if got := s.clientIP(req("127.0.0.1:1", "X-Forwarded-For", "198.51.100.7")); got != "127.0.0.1" {
		t.Errorf("loopback is no longer trusted once other proxies are configured: %q", got)
	}
	if got := s.clientIP(req("192.168.1.5:1", "X-Forwarded-For", "198.51.100.7, 10.9.9.9")); got != "198.51.100.7" {
		t.Errorf("proxy chain: %q", got)
	}
	if _, err := ParseProxies("10.0.0.0/8, nonsense"); err == nil {
		t.Error("garbage accepted")
	}
	if ps, err := ParseProxies(""); err != nil || len(ps) != 0 {
		t.Errorf("empty list: %v %v", ps, err)
	}
}

// Behind a proxy, one attacker cannot lock everyone out, and cannot dodge the
// limit by inventing addresses.
func TestLoginLimitUsesTheRealClient(t *testing.T) {
	a := newAuthEnv(t)
	a.user("alice", false)
	a.user("bob", false)
	attempt := func(name, pw, peer string, hdr ...reqOpt) int {
		return a.do("POST", "/_api/login", `{"name":"`+name+`","password":"`+pw+`"}`, append([]reqOpt{asJSON, remote(peer)}, hdr...)...).Code
	}
	// Through the local proxy, client A fails five times against alice...
	for i := 0; i < 5; i++ {
		if c := attempt("alice", "wrong-password", "127.0.0.1:1000", header("X-Forwarded-For", "198.51.100.1")); c != 401 {
			t.Fatalf("attempt %d: %d", i, c)
		}
	}
	if c := attempt("alice", password("alice"), "127.0.0.1:1001", header("X-Forwarded-For", "198.51.100.1")); c != 429 {
		t.Errorf("client A should be blocked: %d", c)
	}
	// ...and client B, behind the same proxy, is not.
	if c := attempt("alice", password("alice"), "127.0.0.1:1002", header("X-Forwarded-For", "198.51.100.2")); c != 200 {
		t.Errorf("client B must be unaffected: %d", c)
	}
	// A stranger who sets X-Forwarded-For gets no new identity per request.
	for i := 0; i < 5; i++ {
		attempt("bob", "wrong-password", "203.0.113.7:1000", header("X-Forwarded-For", fmt.Sprintf("10.0.0.%d", i)))
	}
	if c := attempt("bob", password("bob"), "203.0.113.7:1000", header("X-Forwarded-For", "10.0.0.99")); c != 429 {
		t.Errorf("a forged X-Forwarded-For dodged the limit: %d", c)
	}
	// Cookies are Secure only when a trusted proxy says the client used HTTPS.
	secure := func(peer string) (int, bool) {
		rec := a.do("POST", "/_api/login", `{"name":"alice","password":"`+password("alice")+`"}`, asJSON, host("wiki.example.com"),
			header("X-Forwarded-Proto", "https"), remote(peer))
		c := cookieByName(rec, cookieAccess)
		return rec.Code, c != nil && c.Secure
	}
	if code, sec := secure("127.0.0.1:9"); code != 200 || !sec {
		t.Errorf("behind a trusted proxy: %d secure=%v", code, sec)
	}
	if code, _ := secure("203.0.113.9:9"); code != 400 {
		t.Errorf("a stranger claiming HTTPS must not unlock cookies on a plain-HTTP host: %d", code)
	}
}

// ---- panics, logging, limits ----

func TestPanicsBecomeErrorPages(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{Logger: slog.New(slog.NewJSONHandler(&logs, nil)), AccessLog: true} // no store: any real request panics
	req := localReq("GET", "/main/page", nil)
	req.Header.Set("Authorization", "Bearer secret-token-value")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	id := rec.Header().Get("X-Request-Id")
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), id) || strings.Contains(rec.Body.String(), "nil pointer") {
		t.Errorf("panic response: %d %q", rec.Code, rec.Body.String())
	}
	out := logs.String()
	if !strings.Contains(out, "panic while serving a request") || !strings.Contains(out, id) || !strings.Contains(out, "goroutine") {
		t.Errorf("the panic must be logged with its stack and request id:\n%s", out)
	}
	if strings.Contains(out, "secret-token-value") {
		t.Error("an Authorization header reached the log")
	}
}

func TestAccessLog(t *testing.T) {
	var logs bytes.Buffer
	a := newAuthEnv(t)
	a.srv.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	a.srv.AccessLog = true
	a.user("alice", true)
	a.space("main", true)
	tok := a.token("alice")
	a.do("PUT", "/main/p", "# P", header("Content-Type", "text/markdown"), bearer(tok), header("User-Agent", "test"))
	a.do("GET", "/_api/search?q=secret-search-term", "", bearer(tok))
	a.do("GET", "/healthz", "")
	a.do("GET", "/_ui/app.js", "")
	a.do("GET", "/main/nope", "", header("Authorization", "Basic c2VjcmV0OnBhc3N3b3Jk"))

	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		lines = append(lines, m)
	}
	if len(lines) != 3 { // healthz and static files are debug noise
		t.Fatalf("%d log lines, want 3:\n%s", len(lines), logs.String())
	}
	put := lines[0]
	if put["method"] != "PUT" || put["path"] != "/main/p" || put["status"] != float64(201) || put["user"] != "alice" ||
		put["msg"] != "request" || len(put["request_id"].(string)) != 16 || put["ip"] != "192.0.2.1" {
		t.Errorf("PUT line: %v", put)
	}
	if lines[1]["path"] != "/_api/search" || lines[2]["status"] != float64(404) {
		t.Errorf("lines: %v %v", lines[1], lines[2])
	}
	out := logs.String()
	for _, secret := range []string{tok, "secret-search-term", "c2VjcmV0", "password"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q reached the log", secret)
		}
	}
	// With the access log off, nothing is written.
	logs.Reset()
	a.srv.AccessLog = false
	a.do("GET", "/main/", "")
	if logs.Len() != 0 {
		t.Errorf("access log off, but: %s", logs.String())
	}
}

func TestRealServerLimits(t *testing.T) {
	e := newEnv(t)
	s := e.h.(*Server)
	hs := s.HTTPServer("")
	if hs.ReadHeaderTimeout == 0 || hs.ReadTimeout == 0 || hs.WriteTimeout == 0 || hs.IdleTimeout == 0 || hs.MaxHeaderBytes != 64<<10 {
		t.Errorf("limits: %+v", hs)
	}
	hs.ReadHeaderTimeout = 300 * time.Millisecond
	ts := httptest.NewUnstartedServer(s)
	ts.Config = hs
	ts.Start()
	defer ts.Close()

	// Huge headers are refused (431) rather than read.
	req, _ := http.NewRequest("GET", ts.URL+"/healthz", nil)
	req.Header.Set("X-Padding", strings.Repeat("a", 100<<10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("oversized headers: %d", resp.StatusCode)
	}
	// A client that sends its headers a drop at a time is cut off.
	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("GET /healthz HTTP/1.1\r\nHost: localhost\r\n"))
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	io.Copy(io.Discard, conn) // returns when the server closes the connection
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("a slow client held the connection for %v", took)
	}
	// Normal requests still work.
	if resp, err := http.Get(ts.URL + "/healthz"); err != nil || resp.StatusCode != 200 {
		t.Errorf("healthz: %v %v", resp, err)
	}
}

// Whatever is put in the address, the server answers with a status, never a crash.
func FuzzRequestPaths(f *testing.F) {
	for _, p := range []string{"/main/docs", "/main/", "/", "/_api/spaces/main/tree", "/main/a%2Fb", "/main/..%2F..%2Fetc", "/main/%00", "/main/_assets/x",
		"/_ui/../x", "/main/p?rev=999999999999999999999", "/main/p?diff=-1&from=x", "/main/p?restore=1", "/main/p.md.md", "/main//p", "/main/é", "/\x7f",
		"/_api/spaces/main/pages/a/b/c/backlinks", "/main/_feed?limit=NaN", "/_api/search?q=%22%28", "/main/p?move=../../x", "/main/p?reorder"} {
		f.Add(p, "GET")
		f.Add(p, "POST")
		f.Add(p, "PUT")
	}
	st, err := store.Open(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { st.Close() })
	st.CreateSpace("main", "Main", true)
	srv := New(st, "")
	f.Fuzz(func(t *testing.T, target, method string) {
		if !strings.HasPrefix(target, "/") || strings.ContainsAny(target, " \r\n") || len(target) > 2000 {
			return
		}
		switch method {
		case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
		default:
			return
		}
		u, err := url.ParseRequestURI(target)
		if err != nil {
			return
		}
		req := httptest.NewRequest(method, target, strings.NewReader("# x\n"))
		req.URL = u
		req.Host, req.RemoteAddr = "localhost:8888", "127.0.0.1:1"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code >= 500 {
			t.Fatalf("%s %s -> %d %s", method, target, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s %s: missing security headers", method, target)
		}
	})
}

func TestIconIsServedAndLinked(t *testing.T) {
	env := newEnv(t)
	rec := env.do("GET", "/_ui/icon.svg", "")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/svg+xml" || !strings.Contains(rec.Body.String(), "<svg") {
		t.Fatalf("icon: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if strings.Contains(rec.Body.String(), "<script") {
		t.Fatal("the icon must not contain script")
	}
	if png := env.do("GET", "/_ui/apple-touch-icon.png", ""); png.Code != 200 || png.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(png.Body.String(), "\x89PNG") {
		t.Fatalf("apple-touch-icon: %d", png.Code)
	}
	if fav := env.do("GET", "/favicon.ico", ""); fav.Code != 200 || !strings.HasPrefix(fav.Body.String(), "\x89PNG") {
		t.Fatalf("/favicon.ico: %d", fav.Code)
	}
	page := env.do("GET", "/main/", "")
	if !strings.Contains(page.Body.String(), `rel="icon" type="image/svg+xml"`) {
		t.Fatal("pages do not link the icon")
	}
}

// Titles, space names and Markdown with markup in them come out as text, on every page that shows them.
func TestHostileStringsStayText(t *testing.T) {
	a := newAuthEnv(t)
	a.user("root", true)
	sp := a.space("main", true)
	evil := []string{
		`<script>alert(1)</script>`, `"><img src=x onerror=alert(1)>`, `' onmouseover='alert(1)`, `</title><script>alert(2)</script>`,
		`<svg onload=alert(3)>`, "</textarea><script>alert(4)</script>", `javascript:alert(5)`, `&lt;b&gt;`,
	}
	title := `"><img src=x onerror=alert(1)> <script>alert(1)</script>`
	if _, err := a.st.UpdateSpace("main", store.SpaceUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	root := bearer(a.token("root"))
	mdT := header("Content-Type", "text/markdown")
	for i, e := range evil {
		body := "# " + e + "\n\n" + e + "\n\n[" + e + "](" + e + ")\n\n![" + e + "](x.png \"" + e + "\")\n\n`" + e + "`\n\n```\n" + e + "\n```\n\n> " + e + "\n"
		path := "/main/p" + string(rune('a'+i))
		if rec := a.do("PUT", path, body, mdT, root); rec.Code != 201 {
			t.Fatalf("PUT %s: %d %s", path, rec.Code, rec.Body.String())
		}
		a.do("PUT", path, body+"\nsecond\n", mdT, root)
	}
	_ = sp
	j := a.login("root")
	var pages []string
	for i := range evil {
		p := "/main/p" + string(rune('a'+i))
		pages = append(pages, p, p+"?edit", p+"?history", p+"?rev=1", p+"?diff=1")
	}
	pages = append(pages, "/", "/main/", "/main/?settings", "/main/?changes", "/main/?broken", "/_ui/search?q=alert", "/_ui/users")
	for _, p := range pages {
		rec := a.do("GET", p, "", header("Accept", "text/html"), withJar(j))
		if rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
			continue
		}
		doc, err := xhtml.Parse(rec.Body)
		if err != nil {
			t.Fatal(err)
		}
		var walk func(n *xhtml.Node)
		walk = func(n *xhtml.Node) {
			if n.Type == xhtml.ElementNode {
				if n.Data == "script" && !hasAttr(n, "src") {
					t.Errorf("%s: inline script element", p)
				}
				if n.Data == "svg" || (n.Data == "img" && attr(n, "src") == "x") {
					t.Errorf("%s: injected <%s>", p, n.Data)
				}
				for _, at := range n.Attr {
					if strings.HasPrefix(strings.ToLower(at.Key), "on") {
						t.Errorf("%s: event handler attribute %s on <%s>", p, at.Key, n.Data)
					}
					if (at.Key == "href" || at.Key == "src") && strings.HasPrefix(strings.ToLower(strings.TrimSpace(at.Val)), "javascript:") {
						t.Errorf("%s: javascript: URL on <%s>", p, n.Data)
					}
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		walk(doc)
	}
}

func attr(n *xhtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *xhtml.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}
