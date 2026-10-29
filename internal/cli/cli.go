// Package cli is the wako command-line client. It speaks the server's REST API
// and links no database code, so the binary stays small.
package cli

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hbroek/wako/internal/version"
)

const usage = `wako - a command-line client for a Wako server

Usage: wako [--server URL] [--token TOKEN] [--space SPACE] <command> [options] [arguments]

Settings (flags, or the environment variables WAKO_SERVER, WAKO_TOKEN, WAKO_SPACE):
  --server   address of the server           (default http://127.0.0.1:8888)
  --token    an API token, made on your Account page; reading public spaces needs none
  --space    the space the command works in

Commands:
  spaces                         list the spaces you can see
  ls [path]                      show the page tree (or the part below a page)
  get <path> [--rev N]           print a page's Markdown
  put <path> [file]              save a page from a file, or from standard input
        --create                 fail if the page already exists
        --if-match VERSION       fail unless the page is at this version (e.g. 3)
  rm <path> [-r]                 delete a page (-r: with the pages below it)
  mv <from> <to>                 move or rename a page; links to it are updated
  search <words...> [--all] [--limit N]
  upload <image>...              add images to the space's media library
  export [-o file.zip]           download the space as a zip (- for standard output)
  import [options] <zip|folder>  add pages and images from an export or a folder of Markdown
        --conflict skip|overwrite   what to do with pages that already exist (default skip)
        --under PATH             import below this page
        --create                 create the space first if it does not exist
        --title TEXT             the new space's title (default: made from its name)
  ask <question...> [--all]      ask the wiki; the answer comes from pages you may read, with sources
  draft <instruction...> [--path P] [--save]
                                 have a page drafted (printed; --save saves it as P, or suggests it if
                                 you may only suggest)
  suggestions [--status S]       list suggested changes (pending, accepted, rejected, all)
  suggestion <id> [accept|reject|withdraw] [--note TEXT]
                                 show a suggestion's changes, or decide it
  mcp                            serve this wiki to an AI assistant (Model Context Protocol, over
                                 standard input and output); see the README
  version                        print the version
  help                           this text
`

type app struct {
	server, token, space string
	in                   io.Reader
	out, err             io.Writer
	http                 *http.Client
}

type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func usageErr(format string, a ...any) error { return &exitError{2, fmt.Sprintf(format, a...)} }

// Run executes one wako command and returns the process exit code: 0 on
// success, 1 when the server or the files refuse, 2 for mistakes in the command.
func Run(args []string, in io.Reader, out, errw io.Writer, getenv func(string) string) int {
	a := &app{in: in, out: out, err: errw, http: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Minute}}}
	env := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	a.server, a.token, a.space = env("WAKO_SERVER", "http://127.0.0.1:8888"), env("WAKO_TOKEN", ""), env("WAKO_SPACE", "")

	globals := flag.NewFlagSet("wako", flag.ContinueOnError)
	globals.SetOutput(io.Discard)
	a.bind(globals)
	if err := globals.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, usage)
			return 0
		}
		fmt.Fprintf(errw, "wako: %v\n\n%s", err, usage)
		return 2
	}
	rest := globals.Args()
	if len(rest) == 0 {
		fmt.Fprint(errw, usage)
		return 2
	}
	cmd, rest := rest[0], rest[1:]
	commands := map[string]func([]string) error{
		"spaces": a.spaces, "ls": a.ls, "get": a.get, "put": a.put, "rm": a.rm, "mv": a.mv,
		"search": a.search, "upload": a.upload, "export": a.export, "import": a.importZip,
		"suggestions": a.suggestions, "suggestion": a.suggestion, "mcp": a.mcp, "ask": a.ask, "draft": a.draft,
	}
	if cmd == "version" || cmd == "--version" {
		fmt.Fprintln(out, "wako "+version.String())
		return 0
	}
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Fprint(out, usage)
		return 0
	}
	run, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(errw, "wako: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	a.server = strings.TrimRight(a.server, "/")
	if err := run(rest); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			fmt.Fprintf(errw, "wako: %s\n", ee.msg)
			return ee.code
		}
		fmt.Fprintf(errw, "wako: %v\n", err)
		return 1
	}
	return 0
}

// bind adds the settings to a flag set, so they work before and after the command.
func (a *app) bind(fs *flag.FlagSet) {
	fs.StringVar(&a.server, "server", a.server, "")
	fs.StringVar(&a.token, "token", a.token, "")
	fs.StringVar(&a.space, "space", a.space, "")
}

// command parses a command's own flags (and the settings), returning its arguments.
func (a *app) command(name string, args []string, setup func(*flag.FlagSet)) ([]string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	a.bind(fs)
	if setup != nil {
		setup(fs)
	}
	// Flags may come after the operands ("get docs --rev 2").
	var operands []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageErr("%s: %v (see 'wako help')", name, err)
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		operands = append(operands, args[0])
		args = args[1:]
	}
	a.server = strings.TrimRight(a.server, "/")
	return operands, nil
}

func (a *app) needSpace() error {
	if a.space == "" {
		return usageErr("which space? use --space NAME or set WAKO_SPACE (see 'wako spaces')")
	}
	return nil
}

// ---- HTTP ----

func (a *app) request(method, p string, body io.Reader, hdr ...string) (*http.Response, error) {
	req, err := http.NewRequest(method, a.server+p, body)
	if err != nil {
		return nil, fmt.Errorf("bad server address %q: %v", a.server, err)
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	req.Header.Set("User-Agent", "wako-cli")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %v", a.server, unwrap(err))
	}
	return resp, nil
}

func unwrap(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// reason reads the server's explanation out of an error response.
func reason(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var j struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &j) == nil && j.Error != "" {
		return j.Error
	}
	return strings.TrimSpace(string(b))
}

// check turns an error response into an error that says what to do about it.
func (a *app) check(resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}
	msg := reason(resp)
	switch resp.StatusCode {
	case 401:
		if a.token == "" {
			return fmt.Errorf("you are not signed in: this needs an API token (make one on your Account page, then --token or WAKO_TOKEN)")
		}
		return fmt.Errorf("the server did not accept the token (it may have been revoked)")
	case 403:
		return fmt.Errorf("not allowed: your account does not have the right role for this")
	case 404:
		if low := strings.ToLower(msg); msg == "" || low == "not found" || strings.Contains(low, "404 page not found") {
			return fmt.Errorf("not found")
		}
		return fmt.Errorf("not found: %s", msg)
	}
	return fmt.Errorf("%s: %s", resp.Status, orStatus(msg, resp))
}

func orStatus(msg string, resp *http.Response) string {
	if msg == "" {
		return resp.Status
	}
	return msg
}

func (a *app) call(method, p string, body io.Reader, hdr ...string) (*http.Response, error) {
	resp, err := a.request(method, p, body, hdr...)
	if err != nil {
		return nil, err
	}
	if err := a.check(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

func (a *app) getJSON(p string, v any) error {
	resp, err := a.call("GET", p, nil, "Accept", "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// pagePath cleans a page argument: "/docs/ops.md" and "docs/ops" are the same page.
func pagePath(arg string) string {
	arg = strings.Trim(arg, "/")
	return strings.TrimSuffix(arg, ".md")
}

func (a *app) pageURL(arg string) string {
	segs := strings.Split(pagePath(arg), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "/" + a.space + "/" + strings.Join(segs, "/")
}

// ---- commands ----

func (a *app) spaces(args []string) error {
	if _, err := a.command("spaces", args, nil); err != nil {
		return err
	}
	var list []struct {
		Slug, Title string
		PublicRead  bool `json:"public_read"`
	}
	if err := a.getJSON("/_api/spaces", &list); err != nil {
		return err
	}
	for _, s := range list {
		vis := "private"
		if s.PublicRead {
			vis = "public"
		}
		fmt.Fprintf(a.out, "%-20s %-8s %s\n", s.Slug, vis, s.Title)
	}
	return nil
}

type node struct {
	Path, Title string
	Children    []*node
}

func (a *app) ls(args []string) error {
	ops, err := a.command("ls", args, nil)
	if err != nil {
		return err
	}
	if err := a.needSpace(); err != nil {
		return err
	}
	var nodes []*node
	if err := a.getJSON("/_api/spaces/"+a.space+"/tree", &nodes); err != nil {
		return err
	}
	start := ""
	if len(ops) > 0 {
		start = pagePath(ops[0])
	}
	var walk func(ns []*node, depth int, show bool)
	found := start == ""
	walk = func(ns []*node, depth int, show bool) {
		for _, n := range ns {
			here := show || n.Path == start
			if here {
				found = true
				fmt.Fprintf(a.out, "%s%s  %s\n", strings.Repeat("  ", depth), n.Path, n.Title)
			}
			d := depth
			if here {
				d++
			}
			walk(n.Children, d, here)
		}
	}
	walk(nodes, 0, start == "")
	if !found {
		return fmt.Errorf("no page %q in %s", start, a.space)
	}
	return nil
}

func (a *app) get(args []string) error {
	var rev string
	ops, err := a.command("get", args, func(fs *flag.FlagSet) { fs.StringVar(&rev, "rev", "", "") })
	if err != nil {
		return err
	}
	if len(ops) != 1 {
		return usageErr("usage: wako get <path> [--rev N]")
	}
	if err := a.needSpace(); err != nil {
		return err
	}
	p := a.pageURL(ops[0]) + ".md"
	if rev != "" {
		p += "?rev=" + url.QueryEscape(rev)
	}
	resp, err := a.call("GET", p, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(a.out, resp.Body)
	return err
}

// versionOf reads the page version out of an ETag such as "v3".
func versionOf(resp *http.Response) string {
	return strings.Trim(strings.TrimPrefix(strings.Trim(resp.Header.Get("ETag"), `"`), "v"), `"`)
}

func (a *app) put(args []string) error {
	var create bool
	var ifMatch string
	ops, err := a.command("put", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&create, "create", false, "")
		fs.StringVar(&ifMatch, "if-match", "", "")
	})
	if err != nil {
		return err
	}
	if len(ops) < 1 || len(ops) > 2 {
		return usageErr("usage: wako put <path> [file]   (reads standard input without a file)")
	}
	if err := a.needSpace(); err != nil {
		return err
	}
	var body io.Reader = a.in
	if len(ops) == 2 && ops[1] != "-" {
		f, err := os.Open(ops[1])
		if err != nil {
			return err
		}
		defer f.Close()
		body = f
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	hdr := []string{"Content-Type", "text/markdown"}
	if create {
		hdr = append(hdr, "If-None-Match", "*")
	}
	if ifMatch != "" {
		hdr = append(hdr, "If-Match", `"v`+strings.Trim(strings.TrimPrefix(strings.Trim(ifMatch, `"`), "v"), `"`)+`"`)
	}
	resp, err := a.request("PUT", a.pageURL(ops[0]), bytes.NewReader(data), hdr...)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 412 {
		return fmt.Errorf("not saved: the page is not in the state you expected (%s)", map[bool]string{true: "it already exists", false: "someone changed it, or the version is wrong"}[create])
	}
	if resp.StatusCode == 409 {
		return fmt.Errorf("not saved: %s", reason(resp))
	}
	if err := a.check(resp); err != nil {
		return err
	}
	if resp.StatusCode == http.StatusAccepted {
		var j struct{ ID int64 }
		json.NewDecoder(resp.Body).Decode(&j)
		fmt.Fprintf(a.out, "suggested /%s as suggestion #%d; an editor has to accept it (wako suggestion %d)\n", pagePath(ops[0]), j.ID, j.ID)
		return nil
	}
	verb := "updated"
	if resp.StatusCode == http.StatusCreated {
		verb = "created"
	}
	fmt.Fprintf(a.out, "%s /%s (version %s)\n", verb, pagePath(ops[0]), versionOf(resp))
	return nil
}

func (a *app) rm(args []string) error {
	var recursive bool
	ops, err := a.command("rm", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&recursive, "r", false, "")
		fs.BoolVar(&recursive, "recursive", false, "")
	})
	if err != nil {
		return err
	}
	if len(ops) != 1 {
		return usageErr("usage: wako rm <path> [-r]")
	}
	if err := a.needSpace(); err != nil {
		return err
	}
	p := a.pageURL(ops[0])
	if recursive {
		p += "?recursive=true"
	}
	resp, err := a.request("DELETE", p, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 409 {
		return fmt.Errorf("%s has pages below it: use -r to delete them too", pagePath(ops[0]))
	}
	if err := a.check(resp); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "deleted /%s\n", pagePath(ops[0]))
	return nil
}

func (a *app) mv(args []string) error {
	ops, err := a.command("mv", args, nil)
	if err != nil {
		return err
	}
	if len(ops) != 2 {
		return usageErr("usage: wako mv <from> <to>")
	}
	if err := a.needSpace(); err != nil {
		return err
	}
	resp, err := a.request("POST", a.pageURL(ops[0])+"?move="+url.QueryEscape(pagePath(ops[1])), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 409 {
		return fmt.Errorf("cannot move: %s", reason(resp))
	}
	if err := a.check(resp); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "moved /%s to /%s\n", pagePath(ops[0]), pagePath(ops[1]))
	return nil
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func (a *app) search(args []string) error {
	var all bool
	var limit int
	ops, err := a.command("search", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&all, "all", false, "")
		fs.IntVar(&limit, "limit", 10, "")
	})
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		return usageErr("usage: wako search <words...> [--all] [--limit N]")
	}
	p := "/_api/search"
	if !all {
		if err := a.needSpace(); err != nil {
			return err
		}
		p = "/_api/spaces/" + a.space + "/search"
	}
	var hits []struct {
		Space, Path, Title string
		SnippetHTML        string `json:"snippet_html"`
	}
	if err := a.getJSON(fmt.Sprintf("%s?limit=%d&q=%s", p, limit, url.QueryEscape(strings.Join(ops, " "))), &hits); err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Fprintln(a.err, "no pages match")
		return nil
	}
	for _, h := range hits {
		where := "/" + h.Path
		if all {
			where = h.Space + where
		}
		snip := strings.Join(strings.Fields(html.UnescapeString(tagRe.ReplaceAllString(h.SnippetHTML, ""))), " ")
		fmt.Fprintf(a.out, "%s  %s\n    %s\n", where, h.Title, snip)
	}
	return nil
}

func (a *app) upload(args []string) error {
	ops, err := a.command("upload", args, nil)
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		return usageErr("usage: wako upload <image>...")
	}
	if err := a.needSpace(); err != nil {
		return err
	}
	failed := 0
	for _, file := range ops {
		data, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(a.err, "wako: %v\n", err)
			failed++
			continue
		}
		ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(file)))
		if ct == "" {
			ct = http.DetectContentType(data)
		}
		resp, err := a.request("POST", "/"+a.space+"/_assets?name="+url.QueryEscape(filepath.Base(file)), bytes.NewReader(data), "Content-Type", ct)
		if err != nil {
			return err
		}
		if err := a.check(resp); err != nil {
			fmt.Fprintf(a.err, "wako: %s: %v\n", file, err)
			resp.Body.Close()
			failed++
			continue
		}
		var res struct{ Name string }
		json.NewDecoder(resp.Body).Decode(&res)
		status := "added"
		if resp.StatusCode == http.StatusOK {
			status = "already in the library"
		}
		resp.Body.Close()
		fmt.Fprintf(a.out, "%s  (%s)  use it as  ![%s](%s)\n", res.Name, status, strings.TrimSuffix(res.Name, path.Ext(res.Name)), res.Name)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d files were not uploaded", failed, len(ops))
	}
	return nil
}

func (a *app) export(args []string) error {
	var outFile string
	_, err := a.command("export", args, func(fs *flag.FlagSet) { fs.StringVar(&outFile, "o", "", "") })
	if err != nil {
		return err
	}
	if err := a.needSpace(); err != nil {
		return err
	}
	resp, err := a.call("GET", "/_api/spaces/"+a.space+"/export", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if outFile == "-" {
		_, err = io.Copy(a.out, resp.Body)
		return err
	}
	if outFile == "" {
		outFile = a.space + "-" + time.Now().Format("2006-01-02") + ".zip"
		if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
			outFile = filepath.Base(params["filename"])
		}
	}
	f, err := os.Create(outFile)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(outFile)
		return fmt.Errorf("download failed: %v", err)
	}
	fmt.Fprintf(a.err, "wrote %s (%d bytes)\n", outFile, n)
	return nil
}

// zipFolder packs a folder of Markdown files and images, skipping hidden files.
func zipFolder(dir string) (*os.File, error) {
	tmp, err := os.CreateTemp("", "wako-import-*.zip")
	if err != nil {
		return nil, err
	}
	zw := zip.NewWriter(tmp)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil // symbolic links and special files are left out
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		_, err = io.Copy(w, src)
		return err
	})
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return tmp, nil
}

func (a *app) importZip(args []string) error {
	var conflict, under, title string
	var create bool
	ops, err := a.command("import", args, func(fs *flag.FlagSet) {
		fs.StringVar(&title, "title", "", "")
		fs.StringVar(&conflict, "conflict", "skip", "")
		fs.StringVar(&under, "under", "", "")
		fs.BoolVar(&create, "create", false, "")
	})
	if err != nil {
		return err
	}
	if len(ops) != 1 {
		return usageErr("usage: wako import [--conflict skip|overwrite] [--under PATH] [--create [--title TEXT]] <zip|folder>")
	}
	if err := a.needSpace(); err != nil {
		return err
	}
	info, err := os.Stat(ops[0])
	if err != nil {
		return err
	}
	var f *os.File
	if info.IsDir() {
		if f, err = zipFolder(ops[0]); err != nil {
			return err
		}
		defer os.Remove(f.Name())
	} else if f, err = os.Open(ops[0]); err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}

	if create {
		var list []struct{ Slug string }
		if err := a.getJSON("/_api/spaces", &list); err != nil {
			return err
		}
		exists := false
		for _, s := range list {
			exists = exists || s.Slug == a.space
		}
		if !exists {
			if title == "" {
				title = strings.Title(strings.ReplaceAll(a.space, "-", " "))
			}
			body, _ := json.Marshal(map[string]any{"slug": a.space, "title": title, "public_read": true})
			resp, err := a.call("POST", "/_api/spaces", bytes.NewReader(body), "Content-Type", "application/json")
			if err != nil {
				return fmt.Errorf("creating space %s: %v", a.space, err)
			}
			resp.Body.Close()
			fmt.Fprintf(a.err, "created space %s (%s)\n", a.space, title)
		}
		f.Seek(0, io.SeekStart)
	}

	q := url.Values{"conflict": {conflict}}
	if under != "" {
		q.Set("under", under)
	}
	req, err := http.NewRequest("POST", a.server+"/_api/spaces/"+a.space+"/import?"+q.Encode(), f)
	if err != nil {
		return err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", "application/zip")
	req.Header.Set("User-Agent", "wako-cli")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %v", a.server, unwrap(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var rep struct {
		PagesCreated   int `json:"pages_created"`
		PagesUpdated   int `json:"pages_updated"`
		PagesSkipped   int `json:"pages_skipped"`
		Placeholders   int `json:"placeholders"`
		ImagesAdded    int `json:"images_added"`
		ImagesExisting int `json:"images_existing"`
		Warnings       []string
		Error          string
		Report         *struct {
			PagesCreated int `json:"pages_created"`
		}
	}
	json.Unmarshal(raw, &rep)
	if resp.StatusCode >= 400 {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		err := a.check(resp)
		if rep.Error != "" && resp.StatusCode != 401 && resp.StatusCode != 403 && resp.StatusCode != 404 {
			err = errors.New(rep.Error)
		}
		return err
	}
	fmt.Fprintf(a.out, "%d pages created, %d updated, %d skipped", rep.PagesCreated, rep.PagesUpdated, rep.PagesSkipped)
	if rep.Placeholders > 0 {
		fmt.Fprintf(a.out, ", %d empty parent pages added", rep.Placeholders)
	}
	fmt.Fprintf(a.out, "\n%d images added, %d already in the library\n", rep.ImagesAdded, rep.ImagesExisting)
	for _, w := range rep.Warnings {
		fmt.Fprintf(a.err, "warning: %s\n", w)
	}
	return nil
}
