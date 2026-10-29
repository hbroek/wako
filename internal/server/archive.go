package server

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hbroek/wako/internal/archive"
	"github.com/hbroek/wako/internal/store"
)

const defaultMaxImport = 256 << 20

// longRequest lifts the server's timeouts for one request: moving a whole space
// takes longer than a page save.
func longRequest(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(30 * time.Minute)
	rc.SetReadDeadline(deadline)
	rc.SetWriteDeadline(deadline)
}

// spaceAdminFor finds the space for an administrative request and checks that
// the caller may administer it: unreadable spaces are 404, and readers without
// the right get 401 or 403.
func (s *Server) spaceAdminFor(w http.ResponseWriter, r *http.Request, slug string) (*store.Space, bool) {
	sp, err := s.st.GetSpace(slug)
	if err != nil || !s.access(r, sp).Read {
		http.NotFound(w, r)
		return nil, false
	}
	if !s.access(r, sp).Admin {
		s.deny(w, r)
		return nil, false
	}
	return sp, true
}

// exportSpace answers GET /_api/spaces/{space}/export with a zip of the space.
func (s *Server) exportSpace(w http.ResponseWriter, r *http.Request, slug string) {
	sp, ok := s.spaceAdminFor(w, r, slug)
	if !ok {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := sp.Now()
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.zip"`, sp.Slug, now.UTC().Format("2006-01-02")))
	h.Set("Cache-Control", "private, no-store")
	if r.Method == http.MethodHead {
		return
	}
	longRequest(w)
	if err := archive.Export(w, sp, now); err != nil {
		// The response has started, so all that can be done is to stop it.
		s.logger().Error("export failed", "space", sp.Slug, "err", err)
		panic(http.ErrAbortHandler)
	}
}

// spool copies the uploaded zip (raw body, or the "file" part of a form) to a
// temporary file, since a zip must be read at random.
func (s *Server) spool(w http.ResponseWriter, r *http.Request) (*os.File, int64, error) {
	limit := s.MaxImport
	if limit <= 0 {
		limit = defaultMaxImport
	}
	body := io.Reader(http.MaxBytesReader(w, r.Body, limit+(1<<20)))
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		mr, err := r.MultipartReader()
		if err != nil {
			return nil, 0, fmt.Errorf("%w: %v", archive.ErrBadArchive, err)
		}
		var part *multipart.Part
		for {
			if part, err = mr.NextPart(); err != nil {
				return nil, 0, fmt.Errorf("%w: the form has no \"file\" part", archive.ErrBadArchive)
			}
			if part.FormName() == "file" {
				break
			}
		}
		body = part
	}
	f, err := os.CreateTemp("", "wako-import-*.zip")
	if err != nil {
		return nil, 0, err
	}
	n, err := io.Copy(f, io.LimitReader(body, limit+1))
	if err == nil && n > limit {
		err = archive.ErrTooLarge
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		err = archive.ErrTooLarge
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, err
	}
	return f, n, nil
}

// importSpace handles POST /_api/spaces/{space}/import[?conflict=skip|overwrite&under=path].
func (s *Server) importSpace(w http.ResponseWriter, r *http.Request, slug string) {
	sp, ok := s.spaceAdminFor(w, r, slug)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	longRequest(w)
	f, size, err := s.spool(w, r)
	if err != nil {
		s.importFail(w, err)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	zr, err := zip.NewReader(f, size)
	if err != nil {
		s.importFail(w, fmt.Errorf("%w: this is not a zip file (%v)", archive.ErrBadArchive, err))
		return
	}
	max := s.MaxImport
	if max <= 0 {
		max = defaultMaxImport
	}
	imgMax := s.MaxUpload
	if imgMax <= 0 {
		imgMax = 25 << 20
	}
	q := r.URL.Query()
	rep, err := archive.Import(zr, sp, archive.Options{
		By: s.actor(r), Conflict: q.Get("conflict"), Under: q.Get("under"),
		Limits: s.ImageLimits, MaxImageBytes: imgMax, MaxPageBytes: maxBody, MaxTotalBytes: 4 * max,
	})
	if err != nil {
		// What was imported before the problem stays; say so.
		if rep != nil {
			writeJSON(w, importStatus(err), map[string]any{"error": err.Error(), "report": rep})
			return
		}
		s.importFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func importStatus(err error) int {
	switch {
	case errors.Is(err, archive.ErrBadArchive):
		return http.StatusBadRequest
	case errors.Is(err, archive.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, store.ErrQuota):
		return http.StatusInsufficientStorage
	}
	return http.StatusInternalServerError
}

func (s *Server) importFail(w http.ResponseWriter, err error) {
	status := importStatus(err)
	if status == http.StatusInternalServerError {
		s.fail(w, err)
		return
	}
	msg := err.Error()
	if errors.Is(err, store.ErrQuota) {
		msg = "this space's media quota is full; delete unused images first"
	}
	jsonError(w, status, strings.TrimSpace(msg))
}
