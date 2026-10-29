package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hbroek/wako/internal/mcp"
	"github.com/hbroek/wako/internal/version"
)

// mcp serves the wiki to an AI assistant over standard input and output. The
// assistant starts this program; everything it does is an API request made
// with the token given here.
func (a *app) mcp(args []string) error {
	if _, err := a.command("mcp", args, nil); err != nil {
		return err
	}
	if a.token == "" {
		fmt.Fprintln(a.err, "wako mcp: no token; only public spaces can be read (set WAKO_TOKEN, or --token)")
	}
	srv := &mcp.Server{
		Doer:    &mcp.HTTPDoer{Base: a.server, Token: a.token, Client: &http.Client{Timeout: 60 * time.Second}},
		Version: version.Version,
	}
	return srv.ServeStdio(context.Background(), a.in, a.out)
}

type suggestionRow struct {
	ID          int64  `json:"id"`
	Path        string `json:"path"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	New         bool   `json:"new"`
	Author      string `json:"author"`
	Source      string `json:"source"`
	Note        string `json:"note"`
	Created     string `json:"created"`
	DecidedBy   string `json:"decided_by"`
	Decision    string `json:"decision_note"`
	BaseVersion int64  `json:"base_version"`
	Diff        string `json:"diff"`
}

func (a *app) suggestions(args []string) error {
	var status string
	if _, err := a.command("suggestions", args, func(fs *flag.FlagSet) { fs.StringVar(&status, "status", "pending", "") }); err != nil {
		return err
	}
	if err := a.needSpace(); err != nil {
		return err
	}
	var list []suggestionRow
	if err := a.getJSON("/"+a.space+"/_proposals?status="+url.QueryEscape(status), &list); err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(a.err, "no suggestions")
		return nil
	}
	for _, p := range list {
		kind := "edit"
		if p.New {
			kind = "new"
		}
		who := p.Author
		if p.Source != "" && p.Source != "api" {
			who += " (" + p.Source + ")"
		}
		fmt.Fprintf(a.out, "#%-4d %-9s %-4s /%s  by %s", p.ID, p.Status, kind, p.Path, who)
		if p.Note != "" {
			fmt.Fprintf(a.out, "  — %s", p.Note)
		}
		fmt.Fprintln(a.out)
	}
	return nil
}

// suggestion shows one suggestion as a diff, or accepts, rejects or withdraws it.
func (a *app) suggestion(args []string) error {
	var note string
	ops, err := a.command("suggestion", args, func(fs *flag.FlagSet) { fs.StringVar(&note, "note", "", "") })
	if err != nil {
		return err
	}
	if len(ops) < 1 || len(ops) > 2 {
		return usageErr("usage: wako suggestion <id> [accept|reject|withdraw] [--note TEXT]")
	}
	if err := a.needSpace(); err != nil {
		return err
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(ops[0], "#"), 10, 64)
	if err != nil || id <= 0 {
		return usageErr("the suggestion number must be a number, as in 'wako suggestion 12'")
	}
	base := fmt.Sprintf("/%s/_proposals/%d", a.space, id)
	if len(ops) == 1 {
		var p suggestionRow
		if err := a.getJSON(base, &p); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "#%d %s /%s by %s (%s)\n", p.ID, p.Status, p.Path, p.Author, p.Source)
		if p.Note != "" {
			fmt.Fprintf(a.out, "note: %s\n", p.Note)
		}
		if p.Decision != "" {
			fmt.Fprintf(a.out, "decision: %s (%s)\n", p.Decision, p.DecidedBy)
		}
		fmt.Fprintf(a.out, "\n%s", p.Diff)
		return nil
	}
	switch ops[1] {
	case "accept", "reject", "withdraw":
	default:
		return usageErr("the action must be accept, reject or withdraw")
	}
	data, _ := json.Marshal(map[string]string{"note": note})
	resp, err := a.call("POST", base+"/"+ops[1], bytes.NewReader(data), "Content-Type", "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	fmt.Fprintf(a.out, "suggestion #%d: %s\n", id, map[string]string{"accept": "accepted", "reject": "rejected", "withdraw": "withdrawn"}[ops[1]])
	return nil
}

var citeLink = regexp.MustCompile(`\[(\d{1,3})\]\([^)\s]*\)`)

func (a *app) postJSON(p string, in, out any) error {
	data, _ := json.Marshal(in)
	resp, err := a.call("POST", p, bytes.NewReader(data), "Content-Type", "application/json", "Accept", "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// ask puts a question to the wiki.
func (a *app) ask(args []string) error {
	var all bool
	ops, err := a.command("ask", args, func(fs *flag.FlagSet) { fs.BoolVar(&all, "all", false, "") })
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		return usageErr("usage: wako ask <question...> [--all]")
	}
	space := a.space
	if all {
		space = ""
	} else if err := a.needSpace(); err != nil {
		return err
	}
	var res struct {
		Answer  string `json:"answer"`
		Sources []struct {
			N           int
			Space, Path string
			Title, URL  string
		} `json:"sources"`
	}
	if err := a.postJSON("/_api/ai/ask", map[string]string{"question": strings.Join(ops, " "), "space": space}, &res); err != nil {
		return err
	}
	fmt.Fprintln(a.out, citeLink.ReplaceAllString(res.Answer, "[$1]")) // a terminal has no use for the links; the sources are listed below
	if len(res.Sources) > 0 {
		fmt.Fprintln(a.out, "\nSources:")
		for _, s := range res.Sources {
			fmt.Fprintf(a.out, "  [%d] %s/%s  %s\n", s.N, s.Space, s.Path, s.Title)
		}
	}
	return nil
}

// draft has a page written, and can save (or suggest) it.
func (a *app) draft(args []string) error {
	var pagePath string
	var save bool
	ops, err := a.command("draft", args, func(fs *flag.FlagSet) {
		fs.StringVar(&pagePath, "path", "", "")
		fs.BoolVar(&save, "save", false, "")
	})
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		return usageErr("usage: wako draft <instruction...> [--path PAGE] [--save]")
	}
	if err := a.needSpace(); err != nil {
		return err
	}
	if save && pagePath == "" {
		return usageErr("--save needs --path: where should the page go?")
	}
	instruction := strings.Join(ops, " ")
	var res struct {
		Path        string `json:"path"`
		Markdown    string `json:"markdown"`
		Exists      bool   `json:"exists"`
		BaseVersion int64  `json:"base_version"`
		Model       string `json:"model"`
		Truncated   bool   `json:"truncated"`
	}
	if err := a.postJSON("/_api/ai/draft", map[string]string{"space": a.space, "instruction": instruction, "path": pagePath}, &res); err != nil {
		return err
	}
	if res.Truncated {
		fmt.Fprintln(a.err, "wako: the draft was cut off at the model's length limit")
	}
	if !save {
		fmt.Fprint(a.out, res.Markdown)
		return nil
	}
	hdr := []string{"Content-Type", "text/markdown", "X-Wako-Source", "ai:" + sanitizeModel(res.Model), "X-Wako-Note", asciiNote("from the instruction: "+instruction, 200)}
	if res.Exists {
		hdr = append(hdr, "If-Match", fmt.Sprintf(`"v%d"`, res.BaseVersion))
	} else {
		hdr = append(hdr, "If-None-Match", "*")
	}
	resp, err := a.request("PUT", a.pageURL(pagePath), strings.NewReader(res.Markdown), hdr...)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPreconditionFailed {
		return fmt.Errorf("not saved: the page changed while it was being drafted (or exists already); the draft was:\n\n%s", res.Markdown)
	}
	if err := a.check(resp); err != nil {
		return err
	}
	if resp.StatusCode == http.StatusAccepted {
		var j struct{ ID int64 }
		json.NewDecoder(resp.Body).Decode(&j)
		fmt.Fprintf(a.out, "suggested /%s as suggestion #%d; an editor has to accept it\n", pagePath, j.ID)
		return nil
	}
	fmt.Fprintf(a.out, "saved /%s (version %s), drafted by %s\n", pagePath, versionOf(resp), res.Model)
	return nil
}

func sanitizeModel(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return -1
	}, s)
}

func asciiNote(s string, max int) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return ' '
		}
		return r
	}, s)), " ")
	if len(s) > max {
		s = s[:max]
	}
	return s
}
