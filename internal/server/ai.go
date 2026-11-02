package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hbroek/wako/internal/ai"
	"github.com/hbroek/wako/internal/auth"
	"github.com/hbroek/wako/internal/render"
	"github.com/hbroek/wako/internal/store"
)

// AI features: ask the wiki a question, and have a page drafted. Both are off
// unless an administrator configures a model (EnableAI) and switches them on
// for a space, because they send page text to the model's service.
//
//	GET  /_api/ai          what is available to the caller
//	POST /_api/ai/ask      {"question", "space"?}  -> an answer built only from pages the caller may read, with sources
//	POST /_api/ai/draft    {"space", "instruction", "path"?} -> Markdown for a page; nothing is saved
//	POST /_api/ai/edit     {"space", "instruction", "before", "selected", "after"} -> text to replace or insert in the editor (ai_edit.go)
//
// A draft is only text. Saving it is an ordinary write by the person who asked
// (a PUT that they may or may not be allowed to make), so editors publish it
// and suggesters queue it for review, like anything else they write.

// Limits on what goes to the model.
const (
	aiMaxQuestion = 2000
	aiMaxInstr    = 4000
	askPages      = 6
	askPageChars  = 6000
	askTotalChars = 30000
	draftPages    = 4
	draftPageChar = 4000
	draftTotal    = 16000
	aiConcurrent  = 4
)

// EnableAI turns the AI features on, using p, with each user limited to
// perHour requests per hour. A nil p leaves them off.
func (s *Server) EnableAI(p ai.Provider, perHour int) {
	s.AI = p
	if perHour <= 0 {
		perHour = 30
	}
	s.aiLim = auth.NewLimiter(perHour, time.Hour)
	s.aiWork = make(chan struct{}, aiConcurrent)
}

// allowSlowReply lifts the server's write deadline for this request, so a slow
// model does not get the connection cut before its answer arrives.
func allowSlowReply(w http.ResponseWriter) {
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(aiTimeout + 15*time.Second))
}

// aiTimeout is how long one request may wait for the model.
const aiTimeout = 120 * time.Second

func (s *Server) aiAPI(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case len(parts) == 0 && r.Method == http.MethodGet:
		s.aiStatus(w, r)
	case len(parts) == 1 && parts[0] == "ask" && r.Method == http.MethodPost:
		s.aiAsk(w, r)
	case len(parts) == 1 && parts[0] == "draft" && r.Method == http.MethodPost:
		s.aiDraft(w, r)
	case len(parts) == 1 && parts[0] == "edit" && r.Method == http.MethodPost:
		s.aiEdit(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "not found", http.StatusNotFound)
	}
}

type aiStatusJSON struct {
	Enabled bool     `json:"enabled"`
	Model   string   `json:"model,omitempty"`
	Spaces  []string `json:"spaces"` // spaces the caller may ask about (readable, with AI switched on)
	Draft   []string `json:"draft"`  // spaces the caller may draft pages in
}

func (s *Server) aiStatus(w http.ResponseWriter, r *http.Request) {
	out := aiStatusJSON{Spaces: []string{}, Draft: []string{}}
	if s.AI != nil && s.aiAllowed(r) {
		out.Enabled, out.Model = true, s.AI.Model()
		spaces, _ := s.readableSpaces(r)
		for _, sp := range spaces {
			if !sp.AIEnabled {
				continue
			}
			out.Spaces = append(out.Spaces, sp.Slug)
			if acc := s.access(r, sp); acc.Write || acc.Suggest {
				out.Draft = append(out.Draft, sp.Slug)
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// aiAllowed: with sign-in on, only signed-in users may use the model (it costs money).
func (s *Server) aiAllowed(r *http.Request) bool {
	return s.auth == nil || s.principal(r) != nil
}

// aiGate runs the checks every AI request needs. It writes the refusal and
// returns false if the request may not go on, and otherwise returns a function
// to call when the model is actually about to be used (it counts the request
// against the caller's hourly limit and takes a slot).
func (s *Server) aiGate(w http.ResponseWriter, r *http.Request) (use func() (release func(), ok bool), ok bool) {
	if s.AI == nil {
		jsonError(w, http.StatusServiceUnavailable, "AI is not set up on this server")
		return nil, false
	}
	if !s.aiAllowed(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="wako"`)
		jsonError(w, http.StatusUnauthorized, "sign in to use the AI features")
		return nil, false
	}
	key := "ip:" + s.clientIP(r)
	if p := s.principal(r); p != nil {
		key = "u:" + p.user.Name
	}
	if blocked, wait := s.aiLim.Blocked(key); blocked {
		w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		jsonError(w, http.StatusTooManyRequests, fmt.Sprintf("you have reached your limit of AI requests for now; try again in %d minutes", int(wait.Minutes())+1))
		return nil, false
	}
	return func() (func(), bool) {
		select {
		case s.aiWork <- struct{}{}:
		default:
			jsonError(w, http.StatusTooManyRequests, "the AI is busy with other requests; try again in a moment")
			return nil, false
		}
		s.aiLim.Fail(key)
		return func() { <-s.aiWork }, true
	}, true
}

type aiSource struct {
	N     int    `json:"n"`
	Space string `json:"space"`
	Path  string `json:"path"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// gathered is a page selected as source material.
type gathered struct {
	src  aiSource
	doc  ai.Doc
	rank float64
}

// retrieve finds the pages in spaces that best match the words, within a budget
// of characters. skip is a page (space/path) to leave out.
func (s *Server) retrieve(spaces []*store.Space, words []string, maxPages, perPage, total int, skip string) []gathered {
	var found []gathered
	for _, sp := range spaces {
		hits, err := sp.SearchAny(words, maxPages*2)
		if err != nil {
			s.logger().Warn("AI retrieval", "space", sp.Slug, "err", err)
			continue
		}
		for _, h := range hits {
			if sp.Slug+"/"+h.Path == skip {
				continue
			}
			pg, err := sp.GetPage(h.Path)
			if err != nil {
				continue
			}
			text := pg.Body
			if utf8.RuneCountInString(text) > perPage {
				text = string([]rune(text)[:perPage]) + "\n[…the page continues]"
			}
			url := s.u(sp.Slug, pg.Path)
			found = append(found, gathered{
				src:  aiSource{Space: sp.Slug, Path: pg.Path, Title: pg.Title, URL: url},
				doc:  ai.Doc{Link: url, Title: pg.Title, Text: text},
				rank: h.Score,
			})
		}
	}
	// bm25 scores are better when lower.
	for i := 1; i < len(found); i++ {
		for j := i; j > 0 && found[j].rank < found[j-1].rank; j-- {
			found[j], found[j-1] = found[j-1], found[j]
		}
	}
	var out []gathered
	used := 0
	for _, g := range found {
		if len(out) == maxPages || used+len(g.doc.Text) > total {
			break
		}
		used += len(g.doc.Text)
		g.src.N = len(out) + 1
		out = append(out, g)
	}
	return out
}

func sourcesOf(g []gathered) ([]aiSource, []ai.Doc, []string) {
	srcs, docs, urls := make([]aiSource, 0, len(g)), make([]ai.Doc, 0, len(g)), make([]string, 0, len(g))
	for _, x := range g {
		srcs, docs, urls = append(srcs, x.src), append(docs, x.doc), append(urls, x.src.URL)
	}
	return srcs, docs, urls
}

// aiSpaces resolves which spaces a request is about: the named one (it must be
// readable and have AI switched on), or every such space.
func (s *Server) aiSpaces(w http.ResponseWriter, r *http.Request, named string, needEdit bool) ([]*store.Space, bool) {
	if named != "" {
		sp, err := s.st.GetSpace(named)
		if err != nil || !s.access(r, sp).Read {
			jsonError(w, http.StatusNotFound, "there is no such space")
			return nil, false
		}
		if needEdit {
			if acc := s.access(r, sp); !acc.Write && !acc.Suggest {
				jsonError(w, http.StatusForbidden, "you may not edit or suggest changes in this space")
				return nil, false
			}
		}
		if !sp.AIEnabled {
			jsonError(w, http.StatusForbidden, "AI features are switched off for this space; a server administrator can switch them on in its settings")
			return nil, false
		}
		return []*store.Space{sp}, true
	}
	all, _ := s.readableSpaces(r)
	var out []*store.Space
	for _, sp := range all {
		if sp.AIEnabled {
			out = append(out, sp)
		}
	}
	if len(out) == 0 {
		jsonError(w, http.StatusForbidden, "AI features are not switched on for any space you can read")
		return nil, false
	}
	return out, true
}

var imageTag = regexp.MustCompile(`<img\b[^>]*>`)
var externalImage = regexp.MustCompile(`!\[([^\]]*)\]\(\s*<?(?:https?:)?//[^)]*\)`)
var externalImageAddr = regexp.MustCompile(`^!\[[^\]]*\]\(\s*<?([^)\s>]*)`)
var sizeFragment = regexp.MustCompile(`#[wh]=?[0-9]{2,4}$`)

// dropNewImages removes images the model made up from other sites (a page could
// make it emit one whose address carries the conversation away). A picture whose
// address is already in the text it was given stays, and its size may change:
// ![a](https://example.com/a.jpg#h=60) is the same picture as ...a.jpg.
func dropNewImages(text string, given ...string) string {
	have := strings.Join(given, "\n")
	return externalImage.ReplaceAllStringFunc(text, func(m string) string {
		if a := externalImageAddr.FindStringSubmatch(m); a != nil {
			if base := sizeFragment.ReplaceAllString(a[1], ""); base != "" && strings.Contains(have, base) {
				return m
			}
		}
		return externalImage.ReplaceAllString(m, "$1")
	})
}

// aiFailure words a model failure for the user and logs the cause.
func (s *Server) aiFailure(w http.ResponseWriter, err error, what string) {
	var ae *ai.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		jsonError(w, http.StatusGatewayTimeout, "the AI took too long to answer; try again, or ask something narrower")
	case errors.Is(err, context.Canceled):
		jsonError(w, 499, "cancelled")
	case errors.As(err, &ae):
		s.logger().Warn("AI request failed", "what", what, "status", ae.Status, "err", ae.Message)
		jsonError(w, http.StatusBadGateway, ae.Message)
	default:
		s.logger().Error("AI request failed", "what", what, "err", err)
		jsonError(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *Server) aiLog(r *http.Request, kind, space string, res *ai.Result, took time.Duration, pages int) {
	s.logger().Info("ai", "kind", kind, "space", space, "user", s.actor(r), "model", res.Model, "pages", pages,
		"tokens_in", res.InputTokens, "tokens_out", res.OutputTokens, "took", took.Round(time.Millisecond))
}

type askIn struct {
	Question string `json:"question"`
	Space    string `json:"space"`
}

type askOut struct {
	Answer    string     `json:"answer"` // Markdown, with [n] markers linked to the sources
	HTML      string     `json:"html"`
	Sources   []aiSource `json:"sources"`
	Model     string     `json:"model,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
}

func (s *Server) aiAsk(w http.ResponseWriter, r *http.Request) {
	use, ok := s.aiGate(w, r)
	if !ok {
		return
	}
	var in askIn
	if !readJSON(w, r, &in) {
		return
	}
	in.Question = strings.TrimSpace(in.Question)
	if in.Question == "" || utf8.RuneCountInString(in.Question) > aiMaxQuestion {
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("ask a question of up to %d characters", aiMaxQuestion))
		return
	}
	spaces, ok := s.aiSpaces(w, r, in.Space, false)
	if !ok {
		return
	}
	found := s.retrieve(spaces, ai.Keywords(in.Question), askPages, askPageChars, askTotalChars, "")
	if len(found) == 0 { // nothing to base an answer on: say so, without asking the model
		writeJSON(w, http.StatusOK, askOut{Answer: "I could not find anything in the wiki about that.", HTML: "<p>I could not find anything in the wiki about that.</p>", Sources: []aiSource{}})
		return
	}
	srcs, docs, urls := sourcesOf(found)
	release, ok := use()
	if !ok {
		return
	}
	defer release()
	allowSlowReply(w)
	ctx, cancel := context.WithTimeout(r.Context(), aiTimeout)
	defer cancel()
	start := time.Now()
	res, err := s.AI.Complete(ctx, ai.AskPrompt(in.Question, docs))
	if err != nil {
		s.aiFailure(w, err, "ask")
		return
	}
	s.aiLog(r, "ask", in.Space, res, time.Since(start), len(docs))
	md := ai.LinkCitations(strings.TrimSpace(res.Text), urls)
	if res.Truncated {
		md += "\n\n_(The answer was cut off at the length limit.)_"
	}
	html, err := render.HTML(md)
	if err != nil {
		s.fail(w, err)
		return
	}
	// A page could try to make the model emit an image whose address carries the
	// conversation to another site; answers never show images.
	html = imageTag.ReplaceAllString(html, "")
	writeJSON(w, http.StatusOK, askOut{Answer: md, HTML: html, Sources: srcs, Model: res.Model, Truncated: res.Truncated})
}

type draftIn struct {
	Space       string `json:"space"`
	Instruction string `json:"instruction"`
	Path        string `json:"path"`
}

type draftOut struct {
	Path        string     `json:"path,omitempty"`
	Title       string     `json:"title"`
	Markdown    string     `json:"markdown"`
	Exists      bool       `json:"exists"`       // the page is there already: the draft is a rewrite of it
	BaseVersion int64      `json:"base_version"` // its version, to send as If-Match when saving
	Sources     []aiSource `json:"sources"`
	Model       string     `json:"model"`
	Truncated   bool       `json:"truncated,omitempty"`
}

func (s *Server) aiDraft(w http.ResponseWriter, r *http.Request) {
	use, ok := s.aiGate(w, r)
	if !ok {
		return
	}
	var in draftIn
	if !readJSON(w, r, &in) {
		return
	}
	in.Instruction = strings.TrimSpace(in.Instruction)
	if in.Space == "" || in.Instruction == "" || utf8.RuneCountInString(in.Instruction) > aiMaxInstr {
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("name a space and describe the page in up to %d characters", aiMaxInstr))
		return
	}
	in.Path = strings.Trim(strings.TrimSuffix(strings.Trim(in.Path, "/"), ".md"), "/")
	if in.Path != "" {
		if _, err := store.SplitPath(in.Path); err != nil {
			jsonError(w, http.StatusBadRequest, "that is not a valid page path")
			return
		}
	}
	spaces, ok := s.aiSpaces(w, r, in.Space, true)
	if !ok {
		return
	}
	sp := spaces[0]
	var existing string
	out := draftOut{Path: in.Path, Sources: []aiSource{}}
	if in.Path != "" {
		if pg, err := sp.GetPage(in.Path); err == nil {
			existing, out.Exists, out.BaseVersion = pg.Body, true, pg.Version
		}
	}
	words := ai.Keywords(in.Instruction + " " + strings.NewReplacer("/", " ", "-", " ", "_", " ").Replace(in.Path))
	found := s.retrieve(spaces, words, draftPages, draftPageChar, draftTotal, sp.Slug+"/"+in.Path)
	srcs, docs, _ := sourcesOf(found)
	release, ok := use()
	if !ok {
		return
	}
	defer release()
	allowSlowReply(w)
	ctx, cancel := context.WithTimeout(r.Context(), aiTimeout)
	defer cancel()
	start := time.Now()
	res, err := s.AI.Complete(ctx, ai.DraftPrompt(in.Instruction, existing, docs, imageNames(sp)))
	if err != nil {
		s.aiFailure(w, err, "draft")
		return
	}
	s.aiLog(r, "draft", in.Space, res, time.Since(start), len(docs))
	md := dropNewImages(ai.CleanDraft(res.Text), existing)
	if len(md) > maxBody {
		jsonError(w, http.StatusBadGateway, "the draft came out too long; ask for something smaller")
		return
	}
	out.Markdown, out.Sources, out.Model, out.Truncated = md, srcs, res.Model, res.Truncated
	out.Title = titleFor(md, "")
	writeJSON(w, http.StatusOK, out)
}

// aiPage serves the browser page for asking and drafting.
func (s *Server) aiPage(w http.ResponseWriter, r *http.Request) {
	d := s.newData(r, nil, Access{}, "", "Ask the wiki")
	d.AIOn = s.AI != nil && s.aiAllowed(r)
	d.SearchSpace = r.URL.Query().Get("space")
	if p := r.URL.Query().Get("path"); p != "" {
		d.Path = p
	}
	s.renderHTML(w, "ask", d)
}

// imageNames lists the image files in a space's library, for the model to
// refer to by name.
func imageNames(sp *store.Space) []string {
	list, err := sp.ListAssets("", 200)
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range list {
		if strings.HasPrefix(a.Mime, "image/") {
			out = append(out, a.Name)
		}
	}
	return out
}
