package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/hbroek/wako/internal/mcp"
	"github.com/hbroek/wako/internal/version"
)

// /_mcp serves the Model Context Protocol over HTTP ("streamable HTTP", POST
// only), so an AI assistant can use the wiki with just an API token:
//
//	claude mcp add --transport http wako https://wiki.example.com/_mcp --header "Authorization: Bearer wako_..."
//
// The tools run as ordinary API requests made with that same token (see
// localDoer), so they can do exactly what the token can: read what its owner
// may read, write where they may write, and only suggest where the token is
// suggest-only or the owner is a suggester.

const maxMCPBody = 4 << 20

func (s *Server) mcpEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "this endpoint takes JSON-RPC messages by POST", http.StatusMethodNotAllowed)
		return
	}
	if s.auth != nil {
		p := s.principal(r)
		if p == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wako"`)
			http.Error(w, "authentication required: send an API token as a Bearer token", http.StatusUnauthorized)
			return
		}
		if p.token == nil {
			http.Error(w, "use an API token (Authorization: Bearer ...), not a browser session", http.StatusForbidden)
			return
		}
	}
	if ct := strings.ToLower(r.Header.Get("Content-Type")); !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPBody))
	if err != nil {
		http.Error(w, "message too large", http.StatusRequestEntityTooLarge)
		return
	}
	srv := &mcp.Server{Doer: &localDoer{s: s, outer: r}, Version: version.Version}
	reply := srv.Handle(r.Context(), body)
	if reply == nil {
		w.WriteHeader(http.StatusAccepted) // only notifications
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(reply)
}

// localDoer runs a tool's API request through this server's own handler, as a
// fresh request carrying the caller's Authorization header, so every check
// (token, space rights, suggest-only, access log) applies as for any client.
type localDoer struct {
	s     *Server
	outer *http.Request
}

type memResponse struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (m *memResponse) Header() http.Header { return m.h }
func (m *memResponse) WriteHeader(c int) {
	if m.status == 0 {
		m.status = c
	}
}
func (m *memResponse) Write(b []byte) (int, error) {
	m.WriteHeader(http.StatusOK)
	if m.body.Len() > 8<<20 {
		return len(b), nil // a tool never needs more than this
	}
	return m.body.Write(b)
}

func (d *localDoer) Do(ctx context.Context, method, path string, header http.Header, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+d.outer.Host+d.s.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	// The caller's identity and network address, exactly as the outer request had them.
	for _, k := range []string{"Authorization", "X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "User-Agent"} {
		if v := d.outer.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	req.Host, req.RemoteAddr = d.outer.Host, d.outer.RemoteAddr
	if d.outer.TLS != nil {
		req.TLS = d.outer.TLS
	}
	rec := &memResponse{h: http.Header{}}
	d.s.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return rec.status, rec.h, rec.body.Bytes(), nil
}
