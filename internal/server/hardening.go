package server

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

// This file holds what every request goes through before it reaches a handler:
// security headers, refusal of cross-site changes, the host check, panic
// recovery, request ids and the access log.

// pageCSP is the Content-Security-Policy of the HTML pages. The pages load their
// own script and style only; markup styles (an image's aspect ratio) are allowed
// as attributes, and images may come from this site or any HTTPS site.
const pageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; " +
	"img-src 'self' data: https:; connect-src 'self'; form-action 'self'; base-uri 'none'; " +
	"frame-ancestors 'none'; object-src 'none'"

// apiCSP covers everything that is not a page (JSON, Markdown, feeds, zips).
const apiCSP = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return discard
}

// ---- who is the client, and was the request secure? ----

var loopbackProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

// ParseProxies reads a comma-separated list of addresses and networks
// ("10.0.0.0/8, 192.168.1.5").
func ParseProxies(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(list, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
		} else if a, err2 := netip.ParseAddr(f); err2 == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		} else {
			return nil, err
		}
	}
	return out, nil
}

func (s *Server) proxies() []netip.Prefix {
	if s.TrustedProxies != nil {
		return s.TrustedProxies
	}
	return loopbackProxies
}

func (s *Server) trustedPeer(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host == "" || host == "@" { // a Unix socket: only local programs can connect
		return true
	}
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	return s.addrTrusted(a.Unmap())
}

func (s *Server) addrTrusted(a netip.Addr) bool {
	for _, p := range s.proxies() {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientIP is the address of whoever is really asking. Behind a trusted proxy
// that is the last address in X-Forwarded-For that is not itself a proxy.
func (s *Server) clientIP(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if !s.trustedPeer(r) {
		return peer
	}
	fwd := r.Header.Values("X-Forwarded-For")
	var hops []string
	for _, h := range fwd {
		for _, f := range strings.Split(h, ",") {
			if f = strings.TrimSpace(f); f != "" {
				hops = append(hops, f)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hops[i])
		if err != nil {
			return peer // garbage: do not guess
		}
		if !s.addrTrusted(a.Unmap()) {
			return a.String()
		}
	}
	return peer
}

// isHTTPS reports whether the client's connection is encrypted: this one, or the
// one a trusted proxy terminated.
func (s *Server) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return s.trustedPeer(r) && strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]), "https")
}

// requestHost is the host (and port) the client asked for.
func (s *Server) requestHost(r *http.Request) string {
	if s.trustedPeer(r) {
		if h := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); h != "" {
			return h
		}
	}
	return r.Host
}

func hostname(hostport string) string {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}

// ---- checks ----

// hostAllowed guards a server that has sign-in off against DNS rebinding: a web
// page could otherwise point its own name at 127.0.0.1 and read or change the
// wiki from the visitor's browser. Only names that are clearly local, IP
// addresses, the public URL's host and --allowed-host names are served.
func (s *Server) hostAllowed(r *http.Request) bool {
	if s.auth != nil {
		return true
	}
	h := hostname(s.requestHost(r))
	switch {
	case h == "":
		return false
	case h == "localhost", strings.HasSuffix(h, ".localhost"):
		return true
	case net.ParseIP(h) != nil:
		return true
	}
	if u, err := url.Parse(s.PublicURL); err == nil && u.Host != "" && hostname(u.Host) == h {
		return true
	}
	for _, a := range s.AllowedHosts {
		if hostname(a) == h {
			return true
		}
	}
	return false
}

// crossSite reports whether a request that changes something comes from a page
// on another site. Browsers say so with Sec-Fetch-Site and Origin; programs
// (curl, the wako command) send neither and are not affected.
func (s *Server) crossSite(r *http.Request) bool {
	if !unsafeMethod(r.Method) {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return true
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return true // "null" and other opaque origins
	}
	if strings.EqualFold(u.Host, s.requestHost(r)) {
		return false
	}
	if p, err := url.Parse(s.PublicURL); err == nil && p.Host != "" && strings.EqualFold(u.Host, p.Host) {
		return false
	}
	for _, a := range s.AllowedHosts {
		if strings.EqualFold(u.Host, a) || hostname(u.Host) == hostname(a) && !strings.Contains(a, ":") {
			return false
		}
	}
	return true
}

// secure sets the headers every response carries. Handlers may override them.
func (s *Server) secure(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), interest-cohort=()")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Content-Security-Policy", apiCSP)
	if s.isHTTPS(r) {
		h.Set("Strict-Transport-Security", "max-age=15552000")
	}
	if strings.HasPrefix(strings.TrimPrefix(r.URL.Path, s.base), "/_api/") {
		h.Set("Cache-Control", "no-store") // account data and listings must not sit in shared caches
	}
}

// ---- the wrapper ----

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer (for deadlines).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func requestID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ServeHTTP is the whole server: checks, then sign-in, then the routes.
func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	start, id, user := time.Now(), requestID(), ""
	w := &statusWriter{ResponseWriter: rw}
	w.Header().Set("X-Request-Id", id)
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v) // the handler meant to cut the connection
			}
			s.logger().Error("panic while serving a request", "request_id", id, "method", r.Method, "path", r.URL.Path,
				"panic", v, "stack", string(debug.Stack()))
			if w.status == 0 {
				http.Error(w, "internal error (request "+id+")", http.StatusInternalServerError)
			}
		}
		s.accessLog(r, w, id, user, start)
	}()

	s.secure(w, r)
	if !s.hostAllowed(r) {
		http.Error(w, "this server has sign-in turned off, so it only answers to local names and IP addresses; "+
			"start it with --allowed-host NAME (or --public-url) to serve another name", http.StatusMisdirectedRequest)
		return
	}
	if s.crossSite(r) {
		http.Error(w, "refused: this change was requested by a page on another site", http.StatusForbidden)
		return
	}
	r, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if p := s.principal(r); p != nil {
		user = p.user.Name
	}
	s.route(w, r)
}

func (s *Server) accessLog(r *http.Request, w *statusWriter, id, user string, start time.Time) {
	if s.Logger == nil || !s.AccessLog {
		return
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	level := slog.LevelInfo
	switch p := strings.TrimPrefix(r.URL.Path, s.base); {
	case status >= 500:
		level = slog.LevelError
	case p == "/healthz" || strings.HasPrefix(p, "/_ui/app.") || strings.HasPrefix(p, "/_ui/theme.") || p == "/_ui/icon.svg":
		level = slog.LevelDebug // not worth a line each
	}
	attrs := []slog.Attr{slog.String("request_id", id), slog.String("method", r.Method), slog.String("path", r.URL.Path),
		slog.Int("status", status), slog.Int64("bytes", w.bytes), slog.Duration("took", time.Since(start).Round(time.Microsecond)),
		slog.String("ip", s.clientIP(r))}
	if user != "" {
		attrs = append(attrs, slog.String("user", user))
	}
	s.Logger.LogAttrs(r.Context(), level, "request", attrs...)
}

// HTTPServer returns the net/http server to run s with: sensible limits on how
// long a client may take to send headers and a request, and on their size.
// Handlers that move a lot of data (import, export) lift the deadlines for
// their own request.
func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}
