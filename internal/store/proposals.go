package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Proposal states.
const (
	ProposalPending   = "pending"
	ProposalAccepted  = "accepted"
	ProposalRejected  = "rejected"
	ProposalWithdrawn = "withdrawn"
)

// MaxPendingProposals bounds the queue, so one runaway client cannot fill a
// space's database with suggestions nobody reviews.
const MaxPendingProposals = 200

// ErrTooManyProposals means the review queue is full.
var ErrTooManyProposals = errors.New("too many suggestions are waiting for review")

// Proposal is a change someone suggested: the full new text of a page, written
// against a particular version of it.
type Proposal struct {
	ID          int64
	Path        string
	Title       string
	Body        string
	BaseVersion int64  // 0: the page does not exist yet
	BaseBody    string // the text of BaseVersion
	Author      string
	Source      string // "api", "mcp" or "ai:<model>"
	Note        string
	Status      string
	CreatedAt   time.Time

	DecidedBy      string
	DecidedAt      time.Time
	DecisionNote   string
	AppliedVersion int64 // the page version an accepted proposal became
}

// IsNew reports whether the proposal creates a page.
func (p *Proposal) IsNew() bool { return p.BaseVersion == 0 }

const proposalCols = `id, path, title, body, base_version, base_body, author, source, note, status, created_at,
	decided_by, decided_at, decision_note, applied_version`

func scanProposal(sc interface{ Scan(...any) error }) (*Proposal, error) {
	var p Proposal
	var created, decided int64
	if err := sc.Scan(&p.ID, &p.Path, &p.Title, &p.Body, &p.BaseVersion, &p.BaseBody, &p.Author, &p.Source, &p.Note, &p.Status,
		&created, &p.DecidedBy, &decided, &p.DecisionNote, &p.AppliedVersion); err != nil {
		return nil, err
	}
	p.CreatedAt = time.Unix(created, 0)
	if decided > 0 {
		p.DecidedAt = time.Unix(decided, 0)
	}
	return &p, nil
}

// CreateProposal queues a suggestion. The path must be a valid page path; the
// caller has already worked out the base (version and text) it was written against.
func (sp *Space) CreateProposal(p Proposal) (*Proposal, error) {
	if _, err := SplitPath(p.Path); err != nil || p.Path == "" {
		return nil, fmt.Errorf("%w: bad page path %q", ErrInvalid, p.Path)
	}
	if !utf8.ValidString(p.Body) || !utf8.ValidString(p.Note) {
		return nil, fmt.Errorf("%w: text must be UTF-8", ErrInvalid)
	}
	if p.BaseVersion < 0 {
		return nil, fmt.Errorf("%w: bad base version", ErrInvalid)
	}
	p.Note = clip(p.Note, 2000)
	p.Title = clip(strings.TrimSpace(p.Title), 300)
	if p.Title == "" {
		p.Title = p.Path
	}
	var n int
	if err := sp.db.QueryRow(`SELECT COUNT(*) FROM proposals WHERE status = 'pending'`).Scan(&n); err != nil {
		return nil, err
	}
	if n >= MaxPendingProposals {
		return nil, ErrTooManyProposals
	}
	now := sp.now()
	res, err := sp.db.Exec(`INSERT INTO proposals(path, title, body, base_version, base_body, author, source, note, status, created_at)
		VALUES(?,?,?,?,?,?,?,?,'pending',?)`,
		p.Path, p.Title, p.Body, p.BaseVersion, p.BaseBody, p.Author, p.Source, p.Note, now.Unix())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return sp.GetProposal(id)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func (sp *Space) GetProposal(id int64) (*Proposal, error) {
	p, err := scanProposal(sp.db.QueryRow(`SELECT `+proposalCols+` FROM proposals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// ListProposals returns suggestions newest first. status is "" or "all" for
// every state, otherwise one of the Proposal states; author, when not empty,
// limits the list to that person's suggestions.
func (sp *Space) ListProposals(status, author string, limit int) ([]*Proposal, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT ` + proposalCols + ` FROM proposals WHERE 1=1`
	var args []any
	if status != "" && status != "all" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if author != "" {
		q += ` AND author = ? COLLATE NOCASE`
		args = append(args, author)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := sp.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountPending is the number of suggestions waiting for review.
func (sp *Space) CountPending() (int, error) {
	var n int
	err := sp.db.QueryRow(`SELECT COUNT(*) FROM proposals WHERE status = 'pending'`).Scan(&n)
	return n, err
}

// DecideProposal closes a pending suggestion. It fails with ErrVersion if
// someone else already decided it, so two reviewers cannot both act on one.
func (sp *Space) DecideProposal(id int64, status, by, note string, appliedVersion int64) error {
	switch status {
	case ProposalAccepted, ProposalRejected, ProposalWithdrawn:
	default:
		return fmt.Errorf("%w: bad status %q", ErrInvalid, status)
	}
	res, err := sp.db.Exec(`UPDATE proposals SET status = ?, decided_by = ?, decided_at = ?, decision_note = ?, applied_version = ?
		WHERE id = ? AND status = 'pending'`, status, by, sp.now().Unix(), clip(note, 2000), appliedVersion, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := sp.GetProposal(id); err != nil {
			return err
		}
		return ErrVersion
	}
	return nil
}
