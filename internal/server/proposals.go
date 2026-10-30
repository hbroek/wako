package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/hbroek/wako/internal/diff"
	"github.com/hbroek/wako/internal/store"
)

// Suggestions.
//
// Someone who may read a space but not write to it (a suggester, or any
// request made with a suggest-only API token) uses the ordinary page API. A PUT,
// PATCH or POST does not change the page; it queues a proposal and answers
// "202 Accepted" with the proposal's address. An editor reviews the queue at
// /{space}/_proposals and accepts or rejects each one. Accepting applies the
// change as a normal save, credited to whoever suggested it.

var sourceRe = regexp.MustCompile(`^(api|mcp|ai:[A-Za-z0-9._\-]{1,60})$`)

// proposalMu serialises accepting and rejecting, so a proposal is applied once.
var proposalMu sync.Mutex

// suggestable reports whether a write request is one that can be turned into a
// proposal: replacing, patching or creating a page. Moves, reordering,
// restoring and deleting stay with editors.
func suggestable(r *http.Request) bool {
	switch r.Method {
	case http.MethodPut, http.MethodPatch:
		return true
	case http.MethodPost:
		q := r.URL.Query()
		return !q.Has("move") && !q.Has("reorder") && !q.Has("restore")
	}
	return false
}

type proposalJSON struct {
	ID          int64  `json:"id"`
	Path        string `json:"path"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	New         bool   `json:"new"`
	BaseVersion int64  `json:"base_version"`
	Author      string `json:"author"`
	Source      string `json:"source"`
	Note        string `json:"note"`
	Created     string `json:"created"`
	DecidedBy   string `json:"decided_by,omitempty"`
	Decided     string `json:"decided,omitempty"`
	Decision    string `json:"decision_note,omitempty"`
	Applied     int64  `json:"applied_version,omitempty"`
	URL         string `json:"url"`
	Body        string `json:"body,omitempty"`
	Diff        string `json:"diff,omitempty"`
}

func (s *Server) proposalURL(sp *store.Space, id int64) string {
	return s.u(sp.Slug, "_proposals", strconv.FormatInt(id, 10))
}

func (s *Server) proposalJSON(sp *store.Space, p *store.Proposal, full bool) proposalJSON {
	out := proposalJSON{ID: p.ID, Path: p.Path, Title: p.Title, Status: p.Status, New: p.IsNew(), BaseVersion: p.BaseVersion,
		Author: p.Author, Source: p.Source, Note: p.Note, Created: p.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		DecidedBy: p.DecidedBy, Decision: p.DecisionNote, Applied: p.AppliedVersion, URL: s.proposalURL(sp, p.ID)}
	if !p.DecidedAt.IsZero() {
		out.Decided = p.DecidedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if full {
		out.Body = p.Body
		out.Diff = diff.Unified(p.BaseBody, p.Body, "a/"+p.Path, "b/"+p.Path)
	}
	return out
}

// suggest turns a PUT, PATCH or POST from someone who may only suggest into a proposal.
func (s *Server) suggest(w http.ResponseWriter, r *http.Request, sp *store.Space, path string) {
	var (
		target, title, body string
		base                *store.Page
	)
	switch r.Method {
	case http.MethodPut:
		if path == "" {
			w.Header().Set("Allow", "GET, HEAD, POST, OPTIONS")
			http.Error(w, "cannot PUT the space root", http.StatusMethodNotAllowed)
			return
		}
		if !markdownType(r) {
			http.Error(w, "Content-Type must be text/markdown", http.StatusUnsupportedMediaType)
			return
		}
		var ok bool
		if body, ok = readBody(w, r); !ok {
			return
		}
		target = path
		page, err := sp.GetPage(path)
		switch {
		case err == nil:
			base = page
		case errors.Is(err, store.ErrNotFound):
		default:
			s.fail(w, err)
			return
		}
		if im := strings.TrimSpace(r.Header.Get("If-Match")); im != "" && im != "*" {
			v, ok := parseVersion(im)
			if !ok || base == nil || base.Version != v {
				s.fail(w, store.ErrVersion)
				return
			}
		} else if im == "*" && base == nil {
			s.fail(w, store.ErrVersion)
			return
		}
		if strings.TrimSpace(r.Header.Get("If-None-Match")) == "*" && base != nil {
			s.fail(w, store.ErrVersion)
			return
		}
	case http.MethodPatch:
		w.Header().Set("Accept-Patch", "text/x-diff")
		if path == "" || !patchType(r) {
			http.Error(w, "PATCH a page with Content-Type text/x-diff (a unified diff)", http.StatusUnsupportedMediaType)
			return
		}
		v, ok := parseVersion(r.Header.Get("If-Match"))
		if !ok {
			http.Error(w, `PATCH needs If-Match with the version the diff was made against, e.g. If-Match: "v3"`, http.StatusPreconditionRequired)
			return
		}
		page, err := sp.GetPage(path)
		if err != nil {
			s.fail(w, err)
			return
		}
		if page.Version != v {
			s.fail(w, store.ErrVersion)
			return
		}
		patch, ok := readBody(w, r)
		if !ok {
			return
		}
		if body, err = diff.Apply(page.Body, patch); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		target, base = path, page
	default: // POST: a new child page
		var slug string
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			var in struct{ Slug, Title, Body string }
			raw, ok := readBody(w, r)
			if !ok {
				return
			}
			if err := json.Unmarshal([]byte(raw), &in); err != nil {
				http.Error(w, "bad JSON", http.StatusBadRequest)
				return
			}
			slug, title, body = in.Slug, in.Title, in.Body
		} else {
			if !markdownType(r) {
				http.Error(w, "Content-Type must be text/markdown or application/json", http.StatusUnsupportedMediaType)
				return
			}
			var ok bool
			if body, ok = readBody(w, r); !ok {
				return
			}
			slug = r.Header.Get("Slug")
		}
		if !store.ValidSlug(slug) {
			http.Error(w, "a valid slug is required", http.StatusBadRequest)
			return
		}
		target = strings.Trim(path+"/"+slug, "/")
		if _, err := sp.GetPage(target); err == nil {
			s.fail(w, store.ErrExists)
			return
		}
	}
	if base == nil && parentPath(target) != "" {
		if _, err := sp.GetPage(parentPath(target)); err != nil {
			s.fail(w, store.ErrParentMissing)
			return
		}
	}
	if base != nil && base.Body == body {
		w.Header().Set("ETag", etagFor(base.Version, reprMD))
		w.WriteHeader(http.StatusOK) // nothing to suggest
		return
	}
	if title == "" {
		title = titleFor(body, target)
	}
	source := "api"
	if v := r.Header.Get("X-Wako-Source"); sourceRe.MatchString(v) {
		source = v
	}
	p := store.Proposal{Path: target, Title: title, Body: body, Author: s.actor(r), Source: source,
		Note: strings.TrimSpace(r.Header.Get("X-Wako-Note"))}
	if base != nil {
		p.BaseVersion, p.BaseBody = base.Version, base.Body
	}
	made, err := sp.CreateProposal(p)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Location", s.proposalURL(sp, made.ID))
	w.Header().Set("Content-Location", s.proposalURL(sp, made.ID))
	writeJSON(w, http.StatusAccepted, s.proposalJSON(sp, made, false))
}

// ---- reviewing: /{space}/_proposals[/{id}[/{action}]] ----

type proposalRow struct {
	ID                                int64
	Path, Title, Status, Author, When string
	Source, Note                      string
	New                               bool
	URL, PageURL                      string
}

func (s *Server) proposalsRoute(w http.ResponseWriter, r *http.Request, sp *store.Space, acc Access, tail string) {
	w.Header().Set("Vary", "Accept, Authorization, Cookie")
	if !acc.Write && !acc.Suggest {
		// Readers see no suggestions: they are not public until accepted.
		s.deny(w, r)
		return
	}
	parts := strings.Split(strings.Trim(tail, "/"), "/")
	if parts[0] == "" {
		parts = nil
	}
	switch {
	case len(parts) == 0:
		s.listProposals(w, r, sp, acc)
	case len(parts) <= 2:
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || id <= 0 {
			http.NotFound(w, r)
			return
		}
		p, err := sp.GetProposal(id)
		if err != nil {
			s.fail(w, err)
			return
		}
		mine := s.actor(r) != "" && strings.EqualFold(p.Author, s.actor(r))
		if !acc.Write && !mine {
			http.NotFound(w, r) // someone else's suggestion
			return
		}
		if len(parts) == 1 {
			s.showProposal(w, r, sp, acc, p)
			return
		}
		s.decideProposal(w, r, sp, acc, p, parts[1], mine)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) listProposals(w http.ResponseWriter, r *http.Request, sp *store.Space, acc Access) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	status := r.URL.Query().Get("status")
	if status == "" {
		status = store.ProposalPending
	}
	author := ""
	if !acc.Write {
		if author = s.actor(r); author == "" {
			s.deny(w, r)
			return
		}
	}
	list, err := sp.ListProposals(status, author, 200)
	if err != nil {
		s.fail(w, err)
		return
	}
	if negotiate(r.Header.Get("Accept")) == reprJSON {
		out := make([]proposalJSON, 0, len(list))
		for _, p := range list {
			out = append(out, s.proposalJSON(sp, p, false))
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	d := s.newData(r, sp, acc, "", "Suggestions")
	d.Query = status
	for _, p := range list {
		d.Proposals = append(d.Proposals, s.proposalRow(sp, p))
	}
	d.Crumbs = s.crumbs(sp, "")
	s.renderHTML(w, "proposals", d)
}

func (s *Server) proposalRow(sp *store.Space, p *store.Proposal) proposalRow {
	return proposalRow{ID: p.ID, Path: p.Path, Title: p.Title, Status: p.Status, Author: p.Author, When: fmtTime(p.CreatedAt),
		Source: p.Source, Note: p.Note, New: p.IsNew(), URL: s.proposalURL(sp, p.ID), PageURL: s.u(sp.Slug, p.Path)}
}

func (s *Server) showProposal(w http.ResponseWriter, r *http.Request, sp *store.Space, acc Access, p *store.Proposal) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch negotiate(r.Header.Get("Accept")) {
	case reprJSON:
		writeJSON(w, http.StatusOK, s.proposalJSON(sp, p, true))
	case reprDiff:
		w.Header().Set("Content-Type", "text/x-diff; charset=utf-8")
		fmt.Fprint(w, diff.Unified(p.BaseBody, p.Body, "a/"+p.Path, "b/"+p.Path))
	case reprMD:
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8; variant=GFM")
		fmt.Fprint(w, p.Body)
	default:
		d := s.newData(r, sp, acc, "", "Suggestion for "+p.Path)
		row := s.proposalRow(sp, p)
		d.Proposal = &row
		d.DiffHTML = diffHTML(diff.Hunks(p.BaseBody, p.Body, 3))
		d.Source = p.Body
		d.Version = p.BaseVersion
		d.Decision = p.DecisionNote
		d.DecidedBy = p.DecidedBy
		if !p.DecidedAt.IsZero() {
			d.DecidedAt = fmtTime(p.DecidedAt)
		}
		if p.Status == store.ProposalPending && p.BaseVersion > 0 {
			if cur, err := sp.GetPage(p.Path); err == nil && cur.Version != p.BaseVersion {
				d.Stale = true
			}
		}
		d.CanReview = acc.Write && p.Status == store.ProposalPending
		d.CanWithdraw = !acc.Write && p.Status == store.ProposalPending
		d.Crumbs = s.crumbs(sp, "")
		s.renderHTML(w, "proposal", d)
	}
}

type decisionIn struct {
	Note string  `json:"note"`
	Body *string `json:"body"` // accept with the reviewer's own text instead
}

func (s *Server) decideProposal(w http.ResponseWriter, r *http.Request, sp *store.Space, acc Access, p *store.Proposal, action string, mine bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in decisionIn
	if r.ContentLength != 0 && !readJSON(w, r, &in) {
		return
	}
	who := s.actor(r)
	proposalMu.Lock()
	defer proposalMu.Unlock()
	switch action {
	case "accept":
		if !acc.Write {
			s.deny(w, r)
			return
		}
		page, err := s.applyProposal(sp, p.ID, who, in)
		if err != nil {
			s.proposalFail(w, err)
			return
		}
		out := s.proposalJSON(sp, mustProposal(sp, p.ID), false)
		w.Header().Set("Location", s.u(sp.Slug, page.Path))
		writeJSON(w, http.StatusOK, out)
	case "reject":
		if !acc.Write {
			s.deny(w, r)
			return
		}
		if err := sp.DecideProposal(p.ID, store.ProposalRejected, who, in.Note, 0); err != nil {
			s.proposalFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.proposalJSON(sp, mustProposal(sp, p.ID), false))
	case "withdraw":
		if !mine && !acc.Write {
			s.deny(w, r)
			return
		}
		if err := sp.DecideProposal(p.ID, store.ProposalWithdrawn, who, in.Note, 0); err != nil {
			s.proposalFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.proposalJSON(sp, mustProposal(sp, p.ID), false))
	default:
		http.NotFound(w, r)
	}
}

func mustProposal(sp *store.Space, id int64) *store.Proposal {
	p, _ := sp.GetProposal(id)
	if p == nil {
		p = &store.Proposal{ID: id}
	}
	return p
}

var errNotRebasable = errors.New("the page has changed since this was suggested, and the change no longer fits; reject it, or accept it with your own text")

func (s *Server) proposalFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNotRebasable), errors.Is(err, store.ErrVersion), errors.Is(err, store.ErrExists), errors.Is(err, store.ErrParentMissing):
		msg := err.Error()
		if errors.Is(err, store.ErrVersion) {
			msg = "this suggestion was already decided, or the page changed while you were deciding"
		}
		http.Error(w, msg, http.StatusConflict)
	case errors.Is(err, store.ErrTooManyProposals):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	default:
		s.fail(w, err)
	}
}

// applyProposal makes an accepted suggestion the page's new text. If the page
// changed since the suggestion was written, the change is replayed onto the
// current text, and refused if it no longer fits.
func (s *Server) applyProposal(sp *store.Space, id int64, reviewer string, in decisionIn) (*store.Page, error) {
	p, err := sp.GetProposal(id)
	if err != nil {
		return nil, err
	}
	if p.Status != store.ProposalPending {
		return nil, store.ErrVersion
	}
	body := p.Body
	if in.Body != nil {
		body = *in.Body
	}
	cond := store.Cond{By: p.Author, Note: acceptNote(p, reviewer, in.Note)}
	if cond.By == "" {
		cond.By = reviewer
	}
	if p.IsNew() {
		cond.MustNotExist = true
	} else {
		cur, err := sp.GetPage(p.Path)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("%w: the page no longer exists", errNotRebasable)
			}
			return nil, err
		}
		if in.Body == nil && cur.Version != p.BaseVersion {
			merged, err := diff.Apply(cur.Body, diff.Unified(p.BaseBody, p.Body, "a", "b"))
			if err != nil {
				return nil, errNotRebasable
			}
			body = merged
		}
		cond.IfMatch = &cur.Version
	}
	body, stubs := expandShortLinks(sp, p.Path, body)
	page, _, err := sp.PutPage(p.Path, titleFor(body, p.Path), body, refsOf(body), cond)
	if err != nil {
		return nil, err
	}
	if err := sp.DecideProposal(id, store.ProposalAccepted, reviewer, in.Note, page.Version); err != nil {
		return nil, err
	}
	createStubs(sp, stubs, reviewer)
	return page, nil
}

func acceptNote(p *store.Proposal, reviewer, note string) string {
	var b strings.Builder
	switch {
	case strings.HasPrefix(p.Source, "ai:"):
		fmt.Fprintf(&b, "drafted by AI (%s)", strings.TrimPrefix(p.Source, "ai:"))
	case p.Source == "mcp":
		b.WriteString("suggested through MCP")
	default:
		b.WriteString("suggestion")
	}
	if reviewer != "" {
		fmt.Fprintf(&b, ", accepted by %s", reviewer)
	}
	if p.Note != "" {
		fmt.Fprintf(&b, ": %s", clipRunes(p.Note, 120))
	}
	return b.String()
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
