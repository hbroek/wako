package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hbroek/wako/internal/ai"
)

// POST /_api/ai/edit is the assistant under the editor. The editor sends the
// page as it is on screen, in three parts (before, selected, after), and gets
// back a text and where to put it. Nothing is saved: the browser changes the
// editor's text, the person can undo it, and saving is the usual write.
//
// Where the text goes is decided here, not by the model:
//   - changing existing text replaces the selection, or the whole page;
//   - new content goes after the selection or at the cursor when the instruction
//     says "here", "insert" or "cursor", and to the end of the page otherwise.

const (
	editWholePage = 30000 // longer pages are not sent whole
	editMaxSel    = 20000
)

type editIn struct {
	Space       string `json:"space"`
	Instruction string `json:"instruction"`
	Before      string `json:"before"`
	Selected    string `json:"selected"`
	After       string `json:"after"`
}

type editOut struct {
	Mode      string `json:"mode"`  // "replace" or "insert"
	Place     string `json:"place"` // replace: "selection" or "page"; insert: "cursor" or "end"
	Text      string `json:"text"`
	Model     string `json:"model"`
	Truncated bool   `json:"truncated,omitempty"`
}

// changeWord marks an instruction that changes what is there, for a reply that lacks its tag.
var changeWord = regexp.MustCompile(`(?i)\b(fix|correct|proofread|rewrite|rephrase|reword|translate|shorten|simplify|improve|polish|reformat|rename|replace|edit|change|clean)`)

var hereWord = regexp.MustCompile(`(?i)\b(here|insert|inserted|inserting|cursor)\b`)

func (s *Server) aiEdit(w http.ResponseWriter, r *http.Request) {
	use, ok := s.aiGate(w, r)
	if !ok {
		return
	}
	var in editIn
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*maxBody+aiMaxInstr*4+4096)).Decode(&in); err != nil {
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return
	}
	in.Instruction = strings.TrimSpace(in.Instruction)
	if in.Space == "" || in.Instruction == "" || utf8.RuneCountInString(in.Instruction) > aiMaxInstr {
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("say what to do, in up to %d characters", aiMaxInstr))
		return
	}
	page := utf8.RuneCountInString(in.Before) + utf8.RuneCountInString(in.Selected) + utf8.RuneCountInString(in.After)
	if len(in.Before)+len(in.Selected)+len(in.After) > maxBody {
		jsonError(w, http.StatusRequestEntityTooLarge, "this page is too large for the assistant")
		return
	}
	if utf8.RuneCountInString(in.Selected) > editMaxSel {
		jsonError(w, http.StatusRequestEntityTooLarge, "the selection is too long for the assistant; select a smaller part")
		return
	}
	spaces, ok := s.aiSpaces(w, r, in.Space, true)
	if !ok {
		return
	}
	long := page > editWholePage
	release, ok := use()
	if !ok {
		return
	}
	defer release()
	allowSlowReply(w)
	ctx, cancel := context.WithTimeout(r.Context(), aiTimeout)
	defer cancel()
	start := time.Now()
	res, err := s.AI.Complete(ctx, ai.EditPrompt(in.Instruction, in.Before, in.Selected, in.After, long || in.Selected != "", imageNames(spaces[0])))
	if err != nil {
		s.aiFailure(w, err, "edit")
		return
	}
	s.aiLog(r, "edit", in.Space, res, time.Since(start), 0)
	mode, text := ai.ParseEdit(res.Text)
	if mode == "" { // small models forget the tag; the wording of the instruction is the next best guide
		mode = "new"
		if changeWord.MatchString(in.Instruction) {
			mode = "replace"
		}
	}
	replace := mode == "replace"
	text = externalImage.ReplaceAllString(text, "$1")
	out := editOut{Text: text, Model: res.Model, Truncated: res.Truncated}
	switch {
	case replace && in.Selected == "" && long:
		jsonError(w, http.StatusUnprocessableEntity, "this page is too long to change in one go; select the part you want changed")
		return
	case replace && res.Truncated:
		// Applying half a rewrite would delete the rest of the text.
		jsonError(w, http.StatusUnprocessableEntity, "the answer hit the length limit before it was finished, so nothing was changed; select a smaller part")
		return
	case replace && strings.TrimSpace(text) == "":
		jsonError(w, http.StatusBadGateway, "the AI sent back nothing; try rephrasing")
		return
	case replace:
		orig := in.Selected
		out.Mode, out.Place = "replace", "selection"
		if orig == "" {
			orig, out.Place = in.Before+in.After, "page"
		}
		out.Text = keepEdges(orig, dropEcho(orig, text))
	default:
		if strings.TrimSpace(text) == "" {
			jsonError(w, http.StatusBadGateway, "the AI sent back nothing; try rephrasing")
			return
		}
		out.Mode, out.Place = "insert", "end"
		if in.Selected != "" || hereWord.MatchString(in.Instruction) {
			out.Place = "cursor" // after the selection, if there is one
		}
	}
	if len(out.Text) > maxBody {
		jsonError(w, http.StatusBadGateway, "the result came out too long; ask for something smaller")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// keepEdges gives text the whitespace that surrounded orig at both ends, so
// that fixing a page does not drop its final newline or a selection's indent.
func keepEdges(orig, text string) string {
	body := strings.TrimLeftFunc(strings.TrimRightFunc(orig, unicode.IsSpace), unicode.IsSpace)
	lead := orig[:len(orig)-len(strings.TrimLeftFunc(orig, unicode.IsSpace))]
	trail := orig[len(strings.TrimRightFunc(orig, unicode.IsSpace)):]
	if body == "" {
		lead, trail = "", ""
	}
	return lead + strings.TrimSpace(text) + trail
}

// dropEcho cuts off what some models add after a rewrite of a selection: the
// surrounding text again, in a code fence the selection itself did not have.
func dropEcho(orig, text string) string {
	if strings.Contains(orig, "```") {
		return text
	}
	if i := strings.Index(text, "\n```"); i >= 0 && strings.TrimSpace(text[:i]) != "" {
		return text[:i]
	}
	return text
}
