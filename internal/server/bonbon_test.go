package server

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/hbroek/wako/internal/store"
)

func TestBonbonDiscoveryFile(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/.well-known/bonbon.json", "")
	body := expect(t, rec, 200)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type %q", ct)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil || got["urlSearch"] != "/_search" {
		t.Errorf("bonbon.json = %q (%v)", body, err)
	}
	expect(t, e.do("POST", "/.well-known/bonbon.json", "{}"), 405)
	expect(t, e.do("GET", "/.well-known/other.json", ""), 404)
}

func TestSiteSearchAnswersMarkdown(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/rust-async", "# Async in Rust\n\nFutures and **async** runtimes.", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/go", "# Go\n\nGoroutines and channels.", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/other/notes", "# Notes\n\nRust async notes, in another space.", "Content-Type", md), 201)

	rec := e.do("GET", "/_search?q=RUST+async", "", "Accept", md)
	body := expect(t, rec, 200)
	if ct := rec.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("content type %q", ct)
	}
	if !strings.Contains(body, "### [Async in Rust](/main/rust-async)") || !strings.Contains(body, "](/other/notes)") {
		t.Errorf("matches missing:\n%s", body)
	}
	if strings.Contains(body, "/main/go") {
		t.Errorf("a page without both words matched:\n%s", body)
	}
	if strings.Contains(body, "<") && !strings.Contains(body, `\<`) || strings.Contains(body, "<mark") {
		t.Errorf("HTML in the answer:\n%s", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ") {
			t.Errorf("a # or ## heading: %q", line)
		}
	}

	// It answers Markdown whatever the client asks for.
	rec = e.do("GET", "/_search?q=goroutines", "", "Accept", "text/html")
	if got := expect(t, rec, 200); !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/markdown") || !strings.Contains(got, "/main/go") {
		t.Errorf("html asked: %q %s", rec.Header().Get("Content-Type"), got)
	}

	// Nothing found is still a 200 with words.
	if got := expect(t, e.do("GET", "/_search?q=zebra", "", "Accept", md), 200); !strings.Contains(got, `No pages match "zebra".`) {
		t.Errorf("no match: %q", got)
	}
}

func TestSiteSearchNeedsWordsAndMethod(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("GET", "/_search", "", "Accept", md), 400)
	expect(t, e.do("GET", "/_search?q=++%09", "", "Accept", md), 400)
	expect(t, e.do("POST", "/_search?q=x", "x"), 405)
	expect(t, e.do("GET", "/_search/x?q=x", ""), 404)
	// A very long query is cut, not refused or slow.
	expect(t, e.do("GET", "/_search?q="+strings.Repeat("word+", 5000), "", "Accept", md), 200)
	// Strange input never breaks the search syntax.
	for _, q := range []string{`"`, `*`, `a AND`, `NEAR(`, `title:x`, `-`, `\`, `'; DROP TABLE pages;--`} {
		expect(t, e.do("GET", "/_search?q="+url.QueryEscape(q), "", "Accept", md), 200)
	}
}

func TestSiteSearchEscapesWhatPagesSay(t *testing.T) {
	e := newEnv(t)
	body := "# [evil](http://evil.example) <script>x()</script>\n\nneedle ![i](http://evil.example/p.png) [x](http://evil.example) <b>bold</b> | a | b |\n# not a heading"
	expect(t, e.do("PUT", "/main/tricky", body, "Content-Type", md), 201)
	got := expect(t, e.do("GET", "/_search?q=needle", "", "Accept", md), 200)
	// Every character that could start markup must arrive with a backslash.
	unescaped := regexp.MustCompile(`(^|[^\\])(\]\(http|!\[|<[a-z/])`)
	for _, line := range strings.Split(got, "\n") {
		if unescaped.MatchString(line) {
			t.Errorf("page text became markup: %q\n%s", line, got)
		}
	}
	links := regexp.MustCompile(`\]\(([^)]*)\)`).FindAllStringSubmatch(got, -1)
	for _, l := range links {
		if !strings.HasPrefix(l[1], "/") {
			t.Errorf("a link that is not a wiki address: %q\n%s", l[1], got)
		}
	}
}

func TestSiteSearchPages(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 45; i++ {
		expect(t, e.do("PUT", fmt.Sprintf("/main/p%02d", i), fmt.Sprintf("# Page %d\n\ncommonword here", i), "Content-Type", md), 201)
	}
	links := func(s string) int { return strings.Count(s, "### [") }
	first := expect(t, e.do("GET", "/_search?q=commonword", "", "Accept", md), 200)
	if links(first) != 20 || !strings.Contains(first, "[More results](/_search?q=commonword&page=2)") {
		t.Fatalf("page 1: %d matches\n%s", links(first), first)
	}
	second := expect(t, e.do("GET", "/_search?q=commonword&page=2", "", "Accept", md), 200)
	third := expect(t, e.do("GET", "/_search?q=commonword&page=3", "", "Accept", md), 200)
	if links(second) != 20 || links(third) != 5 || strings.Contains(third, "More results") {
		t.Fatalf("pages 2 and 3: %d, %d", links(second), links(third))
	}
	if strings.Contains(second, "](/main/p") && strings.Contains(first, strings.SplitN(strings.SplitN(second, "](", 2)[1], ")", 2)[0]) {
		t.Errorf("page 2 repeats page 1")
	}
	if got := expect(t, e.do("GET", "/_search?q=commonword&page=9", "", "Accept", md), 200); !strings.Contains(got, "No more pages") {
		t.Errorf("past the end: %q", got)
	}
	// A page that is not a number is page 1.
	if got := expect(t, e.do("GET", "/_search?q=commonword&page=x", "", "Accept", md), 200); links(got) != 20 {
		t.Errorf("page=x gave %d matches", links(got))
	}
}

func TestSiteSearchRespectsPrivateSpaces(t *testing.T) {
	a := newAuthEnv(t)
	a.user("alice", false)
	a.user("bob", false)
	open := a.space("open", true)
	secret := a.space("secret", false)
	a.st.SetMember(secret.ID, a.users["alice"].ID, "editor")
	for sp, body := range map[*store.Space]string{open: "# Public\n\nlaunchcode is public", secret: "# Plans\n\nlaunchcode is hidden"} {
		if _, _, err := sp.PutPage("page", titleOfBody(body), body, store.Refs{}, store.Cond{By: "alice"}); err != nil {
			t.Fatal(err)
		}
	}
	search := func(opts ...reqOpt) string {
		return expect(t, a.do("GET", "/_search?q=launchcode", "", append(opts, header("Accept", md))...), 200)
	}
	if got := search(); !strings.Contains(got, "/open/page") || strings.Contains(got, "/secret/page") {
		t.Errorf("anonymous:\n%s", got)
	}
	if got := search(bearer(a.token("bob"))); strings.Contains(got, "/secret/page") {
		t.Errorf("a token of someone who is not a member:\n%s", got)
	}
	if got := search(bearer(a.token("alice"))); !strings.Contains(got, "/secret/page") || !strings.Contains(got, "/open/page") {
		t.Errorf("a member's token:\n%s", got)
	}
	// The discovery file needs no sign-in.
	expect(t, a.do("GET", "/.well-known/bonbon.json", ""), 200)
}

func titleOfBody(b string) string {
	t, _, _ := strings.Cut(strings.TrimPrefix(b, "# "), "\n")
	return t
}
