package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hbroek/wako/internal/img"
	"github.com/hbroek/wako/internal/store"
)

// /_resize?w=60&img=<address>&space=<space>&sig=<signature> serves a smaller copy of a picture
// that lives on another site. Only the copy is kept (in the same in-memory cache
// as the library's smaller copies, for a day); the original is read, shrunk and
// forgotten.
//
// Fetching any address a stranger names is dangerous, so three things hold:
//   - the link must carry a signature that only a signed-in member can have made
//     (see resizeLink), so the server is not an open proxy;
//   - the server never connects to a private, loopback or link-local address, nor
//     follows a redirect into one, however the name resolves (it looks at the
//     address actually being dialled, which also defeats DNS tricks);
//   - the other site gets a few seconds and a few megabytes, and its answer must
//     really be a JPEG or PNG.

const (
	resizeFetchMax  = 10 << 20 // most bytes read from the other site
	resizeFetchTime = 8 * time.Second
	resizeMaxURL    = 2000
)

var (
	errResizeFetch  = errors.New("could not fetch that picture")
	errResizeBig    = errors.New("that picture is too large")
	errResizeType   = errors.New("only JPEG and PNG pictures can be resized")
	errResizeDenied = errors.New("that address is not allowed")
)

// blockedPrefixes are public-looking ranges that are not the public internet.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "2001:db8::/32"} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// blockedIP reports whether the server must not connect to ip.
func blockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// resizeHTTP returns the client used to read other sites' pictures.
func (s *Server) resizeHTTP() *http.Client {
	s.resizeOnce.Do(func() {
		dialer := &net.Dialer{Timeout: resizeFetchTime, Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return errResizeDenied
			}
			if ip, err := netip.ParseAddr(host); err != nil || (blockedIP(ip) && !s.allowPrivateFetch) {
				return errResizeDenied
			}
			return nil
		}}
		s.resizeClient = &http.Client{
			Timeout: resizeFetchTime,
			Transport: &http.Transport{
				Proxy:       nil, // never through a proxy picked up from the environment
				DialContext: dialer.DialContext, TLSHandshakeTimeout: resizeFetchTime,
				ResponseHeaderTimeout: resizeFetchTime, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second,
				DisableCompression: true,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return errors.New("too many redirects")
				}
				if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
					return errResizeDenied
				}
				return nil
			},
		}
	})
	return s.resizeClient
}

type resizeState struct {
	resizeOnce   sync.Once
	resizeClient *http.Client
	// allowPrivateFetch lets tests read from a server on this machine.
	allowPrivateFetch bool
	// resizeMax overrides resizeFetchMax (tests).
	resizeMax int64
}

// checkResizeURL validates the address of the picture to fetch.
func checkResizeURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > resizeMaxURL {
		return nil, errors.New("img must be an address of up to 2000 characters")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("img must be an http or https address")
	}
	return u, nil
}

// resizeSize reads w or h (exactly one) from a query.
func resizeSize(q url.Values) (dim byte, n int, err error) {
	for _, k := range []string{"w", "h"} {
		v := q.Get(k)
		if v == "" {
			continue
		}
		x, perr := strconv.Atoi(v)
		if perr != nil || !validSize(x) || dim != 0 {
			return 0, 0, fmt.Errorf("give one of w or h, with a size of %v", sizeLadder)
		}
		dim, n = k[0], x
	}
	if dim == 0 {
		return 0, 0, fmt.Errorf("give one of w or h, with a size of %v", sizeLadder)
	}
	return dim, n, nil
}

// resizeKey names a copy: the same picture at the same size, whichever space asks.
func resizeKey(dim byte, n int, raw string) string {
	return string(dim) + "|" + strconv.Itoa(n) + "|" + raw
}

// resizeSigData is what a link's signature covers: the space is part of it, so a
// link made in one space cannot be moved to another.
func resizeSigData(space string, dim byte, n int, raw string) string {
	return space + "|" + resizeKey(dim, n, raw)
}

// resizeURL is the signed address of a resized copy.
func (s *Server) resizeURL(space string, dim byte, n int, raw string) string {
	q := url.Values{}
	q.Set(string(dim), strconv.Itoa(n))
	q.Set("img", raw)
	q.Set("space", space)
	if s.auth != nil {
		q.Set("sig", s.auth.signer.Tag("resize", resizeSigData(space, dim, n, raw)))
	}
	return s.u("_resize") + "?" + q.Encode()
}

// resizeAllowed says whether pictures from other sites may be resized for a
// space: the server's switch and the space's own must both be on.
func (s *Server) resizeAllowed(sp *store.Space) error {
	switch {
	case !s.ExternalResize:
		return errors.New("resizing pictures from other sites is switched off on this server")
	case !sp.ExternalResize:
		return errors.New("resizing pictures from other sites is switched off for this space")
	}
	return nil
}

// resizeLink handles POST /_api/resize/link {"space":"main","img":"https://…","w":60}:
// it checks the address and gives back the signed /_resize address to put in a
// page of that space. Anyone who may edit or suggest in the space can ask.
func (s *Server) resizeLink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		jsonError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !s.aiAllowed(r) { // "signed in, or sign-in is off"
		w.Header().Set("WWW-Authenticate", `Bearer realm="wako"`)
		jsonError(w, http.StatusUnauthorized, "sign in to make a resized picture link")
		return
	}
	var in struct {
		Space string `json:"space"`
		Img   string `json:"img"`
		W     int    `json:"w"`
		H     int    `json:"h"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "bad JSON")
		return
	}
	sp, err := s.st.GetSpace(in.Space)
	if err != nil || !s.access(r, sp).Read {
		jsonError(w, http.StatusNotFound, "no such space")
		return
	}
	if acc := s.access(r, sp); !acc.Write && !acc.Suggest {
		jsonError(w, http.StatusForbidden, "you may not edit pages in this space")
		return
	}
	if err := s.resizeAllowed(sp); err != nil {
		jsonError(w, http.StatusForbidden, err.Error())
		return
	}
	in.Img = strings.TrimSpace(in.Img)
	u, err := checkResizeURL(in.Img)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if ip, perr := netip.ParseAddr(u.Hostname()); perr == nil && blockedIP(ip) && !s.allowPrivateFetch {
		jsonError(w, http.StatusBadRequest, errResizeDenied.Error())
		return
	}
	q := url.Values{}
	switch {
	case in.W > 0 && in.H == 0:
		q.Set("w", strconv.Itoa(in.W))
	case in.H > 0 && in.W == 0:
		q.Set("h", strconv.Itoa(in.H))
	}
	dim, n, err := resizeSize(q)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSuffix(path.Base(u.Path), path.Ext(u.Path))
	if name == "" || name == "." || name == "/" {
		name = u.Hostname()
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": s.resizeURL(sp.Slug, dim, n, in.Img), "alt": name})
}

// resize serves GET /_resize?w=60&img=<address>&space=<space>&sig=<signature>.
func (s *Server) resize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	dim, n, err := resizeSize(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw := q.Get("img")
	if _, err := checkResizeURL(raw); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	space := q.Get("space")
	if s.auth != nil && !s.auth.signer.CheckTag("resize", resizeSigData(space, dim, n, raw), q.Get("sig")) {
		http.Error(w, "this link is not signed; make it with the Image / PDF… button in the editor", http.StatusForbidden)
		return
	}
	sp, err := s.st.GetSpace(space)
	if err != nil {
		http.Error(w, "no such space", http.StatusBadRequest)
		return
	}
	if err := s.resizeAllowed(sp); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	// The key carries the day, so a copy is made again at most once a day and
	// the other site's changes show up without anyone clearing anything.
	day := time.Now().UTC().Format("2006-01-02")
	sum := sha256.Sum256([]byte(resizeKey(dim, n, raw) + "|" + day))
	id := "x" + hex.EncodeToString(sum[:16])
	etag := `"` + id + `"`
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	h.Set("Cache-Control", "public, max-age=86400")
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatch(inm, etag) {
		h.Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified) // answered without fetching anything
		return
	}
	v, err := s.variants.get(r.Context(), vkey{id, 0}, func() (*variant, error) {
		return s.buildExternal(r.Context(), raw, dim, n)
	})
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		h.Del("Cache-Control")
		switch {
		case errors.Is(err, errResizeBig):
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		case errors.Is(err, errResizeType):
			http.Error(w, err.Error(), http.StatusUnsupportedMediaType)
		default:
			s.logger().Info("resize failed", "err", err)
			http.Error(w, errResizeFetch.Error(), http.StatusBadGateway)
		}
		return
	}
	h.Set("Content-Type", v.mime)
	h.Set("ETag", etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(v.data))
}

// buildExternal reads the picture, makes the copy and lets the original go.
func (s *Server) buildExternal(ctx context.Context, raw string, dim byte, n int) (*variant, error) {
	data, err := s.fetchPicture(ctx, raw)
	if err != nil {
		return nil, err
	}
	mime, _, _ := strings.Cut(http.DetectContentType(data), ";")
	if mime != "image/jpeg" && mime != "image/png" {
		return nil, errResizeType
	}
	w, h := img.Dimensions(mime, data)
	lim := s.ImageLimits
	if lim.MaxPixels <= 0 {
		lim = img.DefaultLimits
	}
	if w <= 0 || h <= 0 || int64(w)*int64(h) > int64(lim.MaxPixels) {
		return nil, errResizeBig
	}
	out, vw, vh, err := img.Scale(data, mime, limitForSize(w, h, dim, n))
	if err != nil {
		return nil, errResizeType
	}
	return &variant{data: out, mime: mime, w: vw, h: vh}, nil
}

// fetchPicture reads at most resizeFetchMax bytes from the address.
func (s *Server) fetchPicture(ctx context.Context, raw string) ([]byte, error) {
	max := int64(resizeFetchMax)
	if s.resizeMax > 0 {
		max = s.resizeMax
	}
	ctx, cancel := context.WithTimeout(ctx, resizeFetchTime)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, errResizeFetch
	}
	req.Header.Set("User-Agent", "wako-resize")
	req.Header.Set("Accept", "image/jpeg, image/png")
	resp, err := s.resizeHTTP().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errResizeFetch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", errResizeFetch, resp.StatusCode)
	}
	if resp.ContentLength > max {
		return nil, errResizeBig
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errResizeFetch, err)
	}
	if int64(len(data)) > max {
		return nil, errResizeBig
	}
	return data, nil
}
