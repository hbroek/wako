package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hbroek/wako/internal/auth"
	"github.com/hbroek/wako/internal/store"
)

// AuthOptions configure sign-in. Without EnableAuth the server runs open: every
// caller is a full administrator (this is what --disable-auth does).
type AuthOptions struct {
	AccessTTL     time.Duration // how long an access token lives; default 15 minutes
	RefreshTTL    time.Duration // how long a session lasts without use; default 7 days
	AllowInsecure bool          // set cookies over plain HTTP on any host
	Now           func() time.Time
}

const (
	cookieAccess  = "wako_at"
	cookieRefresh = "wako_rt"
	cookieCSRF    = "wako_csrf"
	refreshGrace  = 30 * time.Second
)

type authState struct {
	signer        *auth.Signer
	accessTTL     time.Duration
	refreshTTL    time.Duration
	allowInsecure bool
	now           func() time.Time
	perUser       *auth.Limiter // failures per (client address, user name)
	perAddr       *auth.Limiter // failures per client address
}

// EnableAuth turns on sign-in, roles and the anonymous/private-space rules.
func (s *Server) EnableAuth(o AuthOptions) error {
	if o.AccessTTL <= 0 {
		o.AccessTTL = 15 * time.Minute
	}
	if o.RefreshTTL <= 0 {
		o.RefreshTTL = 7 * 24 * time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	secret, err := s.st.SetSettingIfAbsent("signing_key", auth.RandomToken(32))
	if err != nil {
		return err
	}
	signer, err := auth.NewSigner([]byte(secret))
	if err != nil {
		return err
	}
	a := &authState{signer: signer, accessTTL: o.AccessTTL, refreshTTL: o.RefreshTTL, allowInsecure: o.AllowInsecure, now: o.Now,
		perUser: auth.NewLimiter(5, 15*time.Minute), perAddr: auth.NewLimiter(30, 15*time.Minute)}
	a.perUser.SetClock(o.Now)
	a.perAddr.SetClock(o.Now)
	s.auth = a
	return nil
}

// principal is who is making the request.
type principal struct {
	user   *store.User
	sid    int64 // the session, for cookie sign-in; 0 for API tokens
	cookie bool
	token  *store.APIToken // set when the request came with an API token
	roles  map[int64]store.Role
}

type ctxKey struct{}

func (s *Server) principal(r *http.Request) *principal {
	p, _ := r.Context().Value(ctxKey{}).(*principal)
	return p
}

// ---- who may do what ----

// access resolves the caller's rights in sp:
//
//	server administrator -> everything
//	member               -> what their role allows
//	anyone else          -> read only, and only if the space is public
//
// Without sign-in enabled everyone is an administrator.
func (s *Server) access(r *http.Request, sp *store.Space) Access {
	a := s.rawAccess(r, sp)
	// A token that can only suggest never writes or manages anything, whatever its owner may do.
	if p := s.principal(r); p != nil && p.token != nil && p.token.SuggestOnly && (a.Write || a.Suggest) {
		return Access{Read: a.Read, Suggest: true}
	}
	if a.Write {
		a.Suggest = true
	}
	return a
}

func (s *Server) rawAccess(r *http.Request, sp *store.Space) Access {
	if s.auth == nil {
		return Access{Read: true, Write: true, Admin: true}
	}
	p := s.principal(r)
	if p != nil {
		if p.user.IsAdmin {
			return Access{Read: true, Write: true, Admin: true}
		}
		if p.roles == nil {
			p.roles, _ = s.st.UserRoles(p.user.ID)
			if p.roles == nil {
				p.roles = map[int64]store.Role{}
			}
		}
		switch p.roles[sp.ID] {
		case store.RoleAdmin:
			return Access{Read: true, Write: true, Admin: true}
		case store.RoleEditor:
			return Access{Read: true, Write: true}
		case store.RoleSuggester:
			return Access{Read: true, Suggest: true}
		case store.RoleViewer:
			return Access{Read: true}
		}
	}
	return Access{Read: sp.PublicRead}
}

// isAdmin reports whether the caller is a server administrator: they create and
// delete spaces and manage users.
func (s *Server) isAdmin(r *http.Request) bool {
	if s.auth == nil {
		return true
	}
	p := s.principal(r)
	return p != nil && p.user.IsAdmin
}

// deny answers a request the caller may not make: 401 if they are not signed in
// (so a client knows to authenticate), 403 if they are.
func (s *Server) deny(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil && s.principal(r) == nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="wako"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	http.Error(w, "forbidden", http.StatusForbidden)
}

// ---- authentication ----

func unsafeMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// authenticate identifies the caller from an API token or the session cookies
// and attaches them to the request. It refuses (and writes the response) when a
// token is invalid or a cookie-authenticated change lacks its CSRF token.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	a := s.auth
	if a == nil {
		return r, true
	}
	var p *principal
	// Only the Bearer scheme is ours. Other schemes (a reverse proxy's own Basic
	// authentication, say) are none of our business and are ignored.
	if h := r.Header.Get("Authorization"); len(h) >= 6 && strings.EqualFold(h[:6], "bearer") && (len(h) == 6 || h[6] == ' ') {
		tok := strings.TrimSpace(h[6:])
		u, t, err := s.st.LookupToken(auth.HashToken(tok))
		if tok == "" || err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wako", error="invalid_token"`)
			http.Error(w, "invalid API token", http.StatusUnauthorized)
			return r, false
		}
		p = &principal{user: u, token: t}
	} else if user, pass, ok := r.BasicAuth(); ok && user != "" || ok && pass != "" {
		// Feed readers can only send Basic credentials: the API token is the
		// password. Elsewhere Basic belongs to a reverse proxy and is ignored.
		if isFeedPath(r.URL.Path) && (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.HasPrefix(pass, "wako_") {
			u, t, err := s.st.LookupToken(auth.HashToken(pass))
			if err != nil {
				s.challengeFeed(w)
				return r, false
			}
			p = &principal{user: u, token: t}
		} else {
			p = s.cookiePrincipal(w, r)
		}
	} else {
		p = s.cookiePrincipal(w, r)
	}
	if p != nil && p.cookie && unsafeMethod(r.Method) && !a.signer.CheckCSRF(p.sid, r.Header.Get("X-CSRF-Token")) {
		http.Error(w, "missing or invalid CSRF token", http.StatusForbidden)
		return r, false
	}
	if p == nil {
		return r, true
	}
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)), true
}

// cookiePrincipal signs the caller in from cookies: a valid access token, or
// failing that a refresh token, which starts a new access token.
func (s *Server) cookiePrincipal(w http.ResponseWriter, r *http.Request) *principal {
	a := s.auth
	now := a.now()
	if c, err := r.Cookie(cookieAccess); err == nil {
		if uid, sid, ok := a.signer.Verify(c.Value, now); ok {
			if u, err := s.st.SessionUser(sid, now); err == nil && u.ID == uid {
				if _, err := r.Cookie(cookieCSRF); err != nil {
					s.setCSRFCookie(w, r, sid)
				}
				return &principal{user: u, sid: sid, cookie: true}
			}
		}
	}
	c, err := r.Cookie(cookieRefresh)
	if err != nil || c.Value == "" {
		return nil
	}
	fresh := auth.RandomToken(32)
	sid, u, rotated, err := s.st.RefreshSession(auth.HashToken(c.Value), auth.HashToken(fresh), now, now.Add(a.refreshTTL), refreshGrace)
	if err != nil {
		s.clearCookies(w, r)
		return nil
	}
	if !rotated {
		fresh = "" // a request that raced a rotation: keep the cookie the browser already has
	}
	s.setSessionCookies(w, r, u.ID, sid, fresh)
	return &principal{user: u, sid: sid, cookie: true}
}

func (s *Server) cookiePath() string {
	if s.base == "" {
		return "/"
	}
	return s.base
}

func loopbackHost(hostport string) bool {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	h = strings.Trim(h, "[]")
	return h == "localhost" || strings.HasSuffix(h, ".localhost") || h == "127.0.0.1" || h == "::1"
}

// cookieSecure says whether cookies get the Secure flag, and whether they may
// be set at all: over plain HTTP only for localhost or with --allow-insecure.
func (s *Server) cookieSecure(r *http.Request) (secure, ok bool) {
	if s.isHTTPS(r) {
		return true, true
	}
	return false, s.auth.allowInsecure || loopbackHost(s.requestHost(r))
}

func (s *Server) cookie(r *http.Request, name, value string, maxAge time.Duration, httpOnly bool) *http.Cookie {
	secure, _ := s.cookieSecure(r)
	return &http.Cookie{Name: name, Value: value, Path: s.cookiePath(), MaxAge: int(maxAge / time.Second),
		HttpOnly: httpOnly, Secure: secure, SameSite: http.SameSiteLaxMode}
}

func (s *Server) setCSRFCookie(w http.ResponseWriter, r *http.Request, sid int64) {
	if _, ok := s.cookieSecure(r); ok {
		http.SetCookie(w, s.cookie(r, cookieCSRF, s.auth.signer.CSRF(sid), s.auth.refreshTTL, false))
	}
}

// setSessionCookies issues a fresh access token and CSRF token, and the refresh
// token too when one is given.
func (s *Server) setSessionCookies(w http.ResponseWriter, r *http.Request, uid, sid int64, refresh string) {
	if _, ok := s.cookieSecure(r); !ok {
		return
	}
	a := s.auth
	http.SetCookie(w, s.cookie(r, cookieAccess, a.signer.Sign(uid, sid, a.now().Add(a.accessTTL)), a.accessTTL, true))
	if refresh != "" {
		http.SetCookie(w, s.cookie(r, cookieRefresh, refresh, a.refreshTTL, true))
	}
	s.setCSRFCookie(w, r, sid)
}

func (s *Server) clearCookies(w http.ResponseWriter, r *http.Request) {
	for _, n := range []string{cookieAccess, cookieRefresh, cookieCSRF} {
		c := s.cookie(r, n, "", 0, n != cookieCSRF)
		c.MaxAge = -1
		http.SetCookie(w, c)
	}
}

// ---- helpers shared by the account handlers ----

func (s *Server) clientAddr(r *http.Request) string { return s.clientIP(r) }

// readJSON decodes a JSON request body. Requiring the JSON content type keeps
// plain cross-site form posts out of the account endpoints.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(v); err != nil {
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return false
	}
	return true
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// safeNext keeps sign-in redirects on this site.
func (s *Server) safeNext(next string) string {
	home := s.base + "/"
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return home
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f { // control characters, including line breaks
			return home
		}
	}
	if s.base != "" && next != s.base && !strings.HasPrefix(next, s.base+"/") {
		return home
	}
	return next
}

// actor is the name recorded as the author of a change: the signed-in user, or
// "" when sign-in is off.
func (s *Server) actor(r *http.Request) string {
	if p := s.principal(r); p != nil {
		return p.user.Name
	}
	return ""
}

// cond adds who is writing to a write's preconditions. Saves from the browser
// editor (a session cookie), or from anyone when sign-in is off, may be merged
// into one revision when they come close together; saves with an API token never are.
func (s *Server) cond(r *http.Request, c store.Cond) store.Cond {
	c.By = s.actor(r)
	p := s.principal(r)
	if c.Note == "" {
		c.Note = s.writeNote(r, p)
	}
	c.Coalesce = s.auth == nil || (p != nil && p.cookie)
	return c
}

// writeNote says in the history how a write was made, beyond who made it: by an
// AI assistant or MCP, with which token, and the reason they gave. The source
// and reason headers are the client's own statement, shown as such.
func (s *Server) writeNote(r *http.Request, p *principal) string {
	var parts []string
	if v := r.Header.Get("X-Wako-Source"); sourceRe.MatchString(v) {
		switch {
		case strings.HasPrefix(v, "ai:"):
			parts = append(parts, "drafted by AI ("+strings.TrimPrefix(v, "ai:")+")")
		case v == "mcp":
			parts = append(parts, "via MCP")
		}
	}
	if p != nil && p.token != nil {
		parts = append(parts, "via API token "+strconv.Quote(p.token.Name))
	}
	if n := clipRunes(strings.Join(strings.Fields(r.Header.Get("X-Wako-Note")), " "), 200); n != "" {
		parts = append(parts, n)
	}
	return strings.Join(parts, " · ")
}
