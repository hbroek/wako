package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hbroek/wako/internal/img"
	"github.com/hbroek/wako/internal/render"
	"github.com/hbroek/wako/internal/store"
)

var (
	assetIDRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	bareNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

type assetJSON struct {
	ID     string `json:"id"`
	Name   string `json:"name"` // what Markdown writes: ![alt](name)
	Mime   string `json:"mime"`
	Size   int64  `json:"size"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	URL    string `json:"url"`
	UsedBy *int   `json:"used_by,omitempty"`

	PagesUpdated *int `json:"pages_updated,omitempty"` // rename: pages whose Markdown changed
	PagesAliased *int `json:"pages_aliased,omitempty"` // rename: pages that keep the old name as an alias
}

func (s *Server) assetURL(sp *store.Space, id, name string) string {
	return s.u(sp.Slug, "_assets", id, name)
}

// assetCaps advertises what the caller may do with an asset (see PLAN.md 2a).
func (s *Server) assetCaps(w http.ResponseWriter, sp *store.Space, acc Access, upload bool) {
	h := w.Header()
	h.Set("Vary", "Accept, Authorization, Cookie")
	allow, user := []string{"GET", "HEAD"}, "read"
	if upload {
		if acc.Write {
			allow, user = append(allow, "POST"), "read append"
			h.Set("Accept-Post", "image/png, image/jpeg, image/gif, image/webp, image/svg+xml, application/pdf, multipart/form-data")
		}
	} else if acc.Write {
		allow, user = append(allow, "POST", "DELETE"), "read write append" // POST ?rename=
	}
	h.Set("Allow", strings.Join(append(allow, "OPTIONS"), ", "))
	pub := ""
	if sp.PublicRead {
		pub = "read"
	}
	h.Set("WAC-Allow", fmt.Sprintf(`user="%s", public="%s"`, user, pub))
}

// assets handles the media library: /{space}/_assets (list, upload),
// /{space}/_assets/{name} (lookup by name) and /{space}/_assets/{id}[/{name}].
func (s *Server) assets(w http.ResponseWriter, r *http.Request, sp *store.Space, acc Access, rest string) {
	rest = strings.Trim(rest, "/")
	if rest == "" {
		s.assetCaps(w, sp, acc, true)
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			s.listAssets(w, r, sp, acc)
		case http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			if !acc.Write {
				s.deny(w, r)
				return
			}
			s.uploadAsset(w, r, sp)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	id, _, _ := strings.Cut(rest, "/")
	if !assetIDRe.MatchString(id) {
		// Not an id: look the library entry up by name and send the caller to
		// its immutable URL.
		if r.Method != http.MethodGet && r.Method != http.MethodHead || strings.Contains(rest, "/") || !bareNameRe.MatchString(id) {
			http.NotFound(w, r)
			return
		}
		a, err := sp.FindAssetByName(id)
		if err != nil {
			s.fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.Redirect(w, r, s.assetURL(sp, a.ID, a.Name), http.StatusFound)
		return
	}
	s.assetCaps(w, sp, acc, false)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.serveAsset(w, r, sp, id, true)
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPost:
		if !acc.Write {
			s.deny(w, r)
			return
		}
		s.renameAsset(w, r, sp, id)
	case http.MethodDelete:
		if !acc.Write {
			s.deny(w, r)
			return
		}
		if err := sp.DeleteAsset(id, r.URL.Query().Get("force") == "true"); err != nil {
			s.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// renameAllowed lists the extensions a library name may have for each type.
var renameAllowed = map[string][]string{
	"image/png": {".png"}, "image/jpeg": {".jpg", ".jpeg"}, "image/gif": {".gif"},
	"image/webp": {".webp"}, "image/svg+xml": {".svg"}, "application/pdf": {".pdf"},
}

// checkRename validates a requested library name. A name without an extension
// gets the type's usual one; a name with the wrong extension is refused.
func checkRename(name, mime string) (string, error) {
	name = strings.TrimSpace(name)
	if len(name) > 100 || !bareNameRe.MatchString(name) {
		return "", errors.New("a name uses letters, digits, '.', '-' and '_', and starts with a letter or digit")
	}
	ext := strings.ToLower(path.Ext(name))
	if ext == "" {
		return name + renameAllowed[mime][0], nil
	}
	for _, ok := range renameAllowed[mime] {
		if ext == ok {
			return name, nil
		}
	}
	return "", fmt.Errorf("this image is %s, so its name must end in %s", mime, strings.Join(renameAllowed[mime], " or "))
}

// renameAsset handles POST /{space}/_assets/{id}?rename=new-name.png. Every page
// that uses the image has the name rewritten in its Markdown.
func (s *Server) renameAsset(w http.ResponseWriter, r *http.Request, sp *store.Space, id string) {
	want := r.URL.Query().Get("rename")
	if want == "" {
		http.Error(w, "POST needs ?rename=<new name>", http.StatusBadRequest)
		return
	}
	info, err := sp.AssetInfo(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	name, err := checkRename(want, info.Mime)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := sp.RenameAsset(id, name, render.RewriteImageRef, s.actor(r))
	if err != nil {
		if errors.Is(err, store.ErrExists) {
			http.Error(w, fmt.Sprintf("the library already has an image named %q", name), http.StatusConflict)
			return
		}
		s.fail(w, err)
		return
	}
	a := res.Asset
	url := s.assetURL(sp, a.ID, a.Name)
	w.Header().Set("Location", url)
	writeJSON(w, http.StatusOK, assetJSON{ID: a.ID, Name: a.Name, Mime: a.Mime, Size: a.Size, Width: a.Width,
		Height: a.Height, URL: url, PagesUpdated: &res.Updated, PagesAliased: &res.Aliased})
}

// serveAsset sends an asset, or a smaller copy of it when ?w= asks for one.
// immutable is true for URLs that contain the content hash.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, sp *store.Space, id string, immutable bool) {
	var dim byte
	size := 0
	for _, k := range []string{"w", "h", "s"} {
		ws := r.URL.Query().Get(k)
		if ws == "" {
			continue
		}
		n, err := strconv.Atoi(ws)
		if err != nil || !validSize(n) || dim != 0 {
			http.Error(w, fmt.Sprintf("give one of w, h or s, with a size of %v", sizeLadder), http.StatusBadRequest)
			return
		}
		dim, size = k[0], n
	}
	info, err := sp.AssetInfo(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	width := 0 // longest side of the copy asked for
	if dim != 0 {
		width = limitFor(info, dim, size)
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	if info.Mime == "application/pdf" {
		// A PDF is not a web page, so there is nothing to sandbox, and browsers
		// refuse to show a PDF in a sandboxed response. It is served inline under
		// its own name, and nosniff keeps it from being taken for anything else.
		h.Set("Content-Security-Policy", "frame-ancestors 'self'")
		h.Set("Content-Disposition", `inline; filename="`+info.Name+`"`)
	}
	visibility := "public"
	if !sp.PublicRead {
		visibility = "private"
	} else {
		// Anyone may read this space (the response says so in WAC-Allow and is cached as public),
		// so another site may show its pictures: a feed reader showing an entry of this wiki,
		// for one. Everything private keeps the same-origin policy every response starts with.
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	}
	if immutable {
		h.Set("Cache-Control", visibility+", max-age=31536000, immutable")
	} else {
		// A page-relative URL can point at another image after an edit, so the
		// browser revalidates (a cheap 304 by ETag) instead of caching forever.
		h.Set("Cache-Control", visibility+", no-cache")
	}

	if width > 0 && scalable(info, width, minFor(dim, size)) {
		etag := fmt.Sprintf(`"%s-w%d"`, id, width)
		if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatch(inm, etag) {
			h.Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified) // answered without building the copy
			return
		}
		v, err := s.variants.get(r.Context(), vkey{id, width}, func() (*variant, error) {
			return buildVariant(sp, info, width)
		})
		if err != nil {
			if r.Context().Err() == nil {
				s.fail(w, err)
			}
			return
		}
		if !v.original {
			h.Set("Content-Type", v.mime)
			h.Set("ETag", etag)
			http.ServeContent(w, r, "", info.CreatedAt, bytes.NewReader(v.data))
			return
		}
	}
	a, err := sp.GetAsset(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	h.Set("Content-Type", a.Mime)
	h.Set("ETag", `"`+a.ID+`"`)
	http.ServeContent(w, r, "", a.CreatedAt, bytes.NewReader(a.Data))
}

// uploadAsset accepts multipart/form-data (field "file") or a raw image body
// with the file name in ?name=.
func (s *Server) uploadAsset(w http.ResponseWriter, r *http.Request, sp *store.Space) {
	limit := s.MaxUpload
	if limit <= 0 {
		limit = 25 << 20
	}
	var data []byte
	name := r.URL.Query().Get("name")
	r.Body = http.MaxBytesReader(w, r.Body, limit+(64<<10)) // room for multipart framing
	body := r.Body
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt == "multipart/form-data" {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, "upload too large or malformed", http.StatusRequestEntityTooLarge)
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, `multipart field "file" is required`, http.StatusBadRequest)
			return
		}
		defer f.Close()
		name = hdr.Filename
		data, err = io.ReadAll(io.LimitReader(f, limit+1))
		if err != nil {
			http.Error(w, "unreadable upload", http.StatusBadRequest)
			return
		}
	} else {
		var err error
		if data, err = io.ReadAll(body); err != nil {
			http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
			return
		}
	}
	if int64(len(data)) > limit {
		http.Error(w, fmt.Sprintf("upload exceeds %d bytes", limit), http.StatusRequestEntityTooLarge)
		return
	}
	if len(data) == 0 {
		http.Error(w, "empty upload", http.StatusBadRequest)
		return
	}
	mimeType, ok := img.Sniff(data, name)
	if !ok {
		http.Error(w, "unsupported file type: only png, jpeg, gif, webp and svg images, and PDF files, are accepted", http.StatusUnsupportedMediaType)
		return
	}
	lim := s.ImageLimits
	switch r.URL.Query().Get("mode") {
	case "":
	case "original":
		lim.Mode = img.Original
	case "compact":
		lim.Mode = img.Compact
	default:
		http.Error(w, "mode must be compact or original", http.StatusBadRequest)
		return
	}
	// Cleaning and shrinking decode the whole picture, so they share the work
	// slots with variant building.
	if err := acquire(r.Context(), s.work); err != nil {
		return
	}
	res, err := img.Process(data, mimeType, lim)
	<-s.work
	if err != nil {
		s.fail(w, err)
		return
	}
	a, created, err := sp.PutAsset(img.CleanName(name, res.Mime), res.Mime, res.Data, res.Width, res.Height)
	if err != nil {
		s.fail(w, err)
		return
	}
	url := s.assetURL(sp, a.ID, a.Name)
	w.Header().Set("Location", url)
	w.Header().Set("ETag", `"`+a.ID+`"`)
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, assetJSON{ID: a.ID, Name: a.Name, Mime: a.Mime, Size: a.Size, Width: a.Width, Height: a.Height, URL: url})
}

type assetView struct {
	ID, Name, Mime, URL string
	Size                int64
	Width, Height       int
	UsedBy              int
	Thumb               string // URL of the small copy, if one exists
	ThumbW, ThumbH      int
}

// listAssets shows the media library: JSON for clients, a gallery for browsers.
func (s *Server) listAssets(w http.ResponseWriter, r *http.Request, sp *store.Space, acc Access) {
	list, err := sp.ListAssets(r.URL.Query().Get("q"), 0)
	if err != nil {
		s.fail(w, err)
		return
	}
	switch negotiate(r.Header.Get("Accept")) {
	case reprJSON:
		out := make([]assetJSON, 0, len(list))
		for _, a := range list {
			used := a.UsedBy
			out = append(out, assetJSON{ID: a.ID, Name: a.Name, Mime: a.Mime, Size: a.Size, Width: a.Width, Height: a.Height,
				URL: s.assetURL(sp, a.ID, a.Name), UsedBy: &used})
		}
		writeJSON(w, http.StatusOK, out)
	case reprHTML:
		d := s.newData(r, sp, acc, "", "Media")
		d.Crumbs = []link{{Title: sp.Title, URL: s.u(sp.Slug) + "/"}, {Title: "Media", URL: s.u(sp.Slug, "_assets") + "/"}}
		d.Query = r.URL.Query().Get("q")
		for _, a := range list {
			v := assetView{ID: a.ID, Name: a.Name, Mime: a.Mime, URL: s.assetURL(sp, a.ID, a.Name),
				Size: a.Size, Width: a.Width, Height: a.Height, UsedBy: a.UsedBy}
			v.Thumb, v.ThumbW, v.ThumbH = v.URL, a.Width, a.Height
			if scalable(a, widthThumb, minFor('s', widthThumb)) {
				v.Thumb = v.URL + "?s=" + strconv.Itoa(widthThumb)
				v.ThumbW, v.ThumbH = img.Fitted(a.Width, a.Height, widthThumb)
			}
			d.Assets = append(d.Assets, v)
		}
		if n, used, quota, err := sp.AssetUsage(); err == nil {
			d.Usage = fmt.Sprintf("%d file%s, %s", n, plural(n), humanSize(used))
			if quota >= 0 {
				d.Usage += " of " + humanSize(quota)
			}
		}
		s.renderHTML(w, "library", d)
	default:
		http.Error(w, "not acceptable", http.StatusNotAcceptable)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// GCAssets removes unreferenced assets older than minAge from every space.
func GCAssets(st *store.Store, minAge time.Duration) (count int, bytes int64, err error) {
	spaces, err := st.ListSpaces()
	if err != nil {
		return 0, 0, err
	}
	var errs []error
	for _, sp := range spaces {
		n, b, err := sp.GCAssets(minAge)
		if err != nil {
			errs = append(errs, fmt.Errorf("space %s: %w", sp.Slug, err))
			continue
		}
		count, bytes = count+n, bytes+b
	}
	return count, bytes, errors.Join(errs...)
}
