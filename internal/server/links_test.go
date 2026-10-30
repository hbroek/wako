package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func putPage(t *testing.T, e *env, space, path, body string) {
	t.Helper()
	rec := e.do("PUT", "/"+space+"/"+path, body, "Content-Type", md)
	if rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("PUT /%s/%s: %d %s", space, path, rec.Code, rec.Body.String())
	}
}

func md_(t *testing.T, e *env, space, path string) string {
	t.Helper()
	return expect(t, e.do("GET", "/"+space+"/"+path+".md", ""), 200)
}

func backlinks(t *testing.T, e *env, space, path string) []linkHit {
	t.Helper()
	var hits []linkHit
	body := expect(t, e.do("GET", "/_api/spaces/"+space+"/pages/"+path+"/backlinks", ""), 200)
	if err := json.Unmarshal([]byte(body), &hits); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return hits
}

func brokenOf(t *testing.T, e *env, space string) []brokenLink {
	t.Helper()
	var out []brokenLink
	body := expect(t, e.do("GET", "/_api/spaces/"+space+"/links/broken", ""), 200)
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return out
}

func describe(hits []linkHit) string {
	var s []string
	for _, h := range hits {
		s = append(s, h.Space+":"+h.Path)
	}
	return strings.Join(s, ",")
}

func TestLinksInPages(t *testing.T) {
	e := newEnv(t)
	putPage(t, e, "main", "docs", "# Docs")
	putPage(t, e, "main", "docs/ops", "# Operations")
	putPage(t, e, "other", "notes", "# Team Notes")
	putPage(t, e, "main", "home",
		"# Home\n\n[ops](docs/ops) [abs](/main/docs/ops) [team](/other/notes) [new](my-new-page) [gone](docs/nope) [ext](https://example.com/x) [top](#top)\n")
	html := expect(t, e.do("GET", "/main/home", "", "Accept", "text/html"), 200)
	for _, want := range []string{
		`<a href="docs/ops">ops</a>`,
		`<a href="/main/docs/ops">abs</a>`,
		`<a href="/other/notes">team</a>`,
		`<a href="my-new-page" title="This page does not exist yet" class="missing">new</a>`,
		`<a href="docs/nope" title="This page does not exist yet" class="missing">gone</a>`,
		`<a href="https://example.com/x" rel="nofollow">ext</a>`,
		`<a href="#top">top</a>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %s\n%s", want, html)
		}
	}
	if strings.Count(html, `class="missing"`) != 2 {
		t.Errorf("only the two broken links should be marked: %s", html)
	}
	// A broken link leads to the page that offers to create it.
	if page := expect(t, e.do("GET", "/main/my-new-page", "", "Accept", "text/html"), 404); !strings.Contains(page, "Create this page") {
		t.Errorf("no create offer: %s", page)
	}
	// The Markdown view is exactly what was saved.
	if got := md_(t, e, "main", "home"); !strings.Contains(got, "[ops](docs/ops)") {
		t.Errorf("markdown: %s", got)
	}
	// The live preview marks missing links relative to the page being edited.
	prev := expect(t, e.do("POST", "/_api/spaces/main/render?page=docs/ops", "[a](../docs) [b](nothing)", "Content-Type", md), 200)
	if strings.Count(prev, `class="missing"`) != 1 || !strings.Contains(prev, `href="nothing" title`) {
		t.Errorf("preview: %s", prev)
	}
	// A page that is not saved yet has no place to resolve relative links from.
	if prev := expect(t, e.do("POST", "/_api/spaces/main/render", "[b](nothing)", "Content-Type", md), 200); strings.Contains(prev, "missing") {
		t.Errorf("preview without a page: %s", prev)
	}
}

func TestBacklinks(t *testing.T) {
	e := newEnv(t)
	putPage(t, e, "main", "docs", "# Docs")
	putPage(t, e, "main", "docs/ops", "# Operations")
	putPage(t, e, "main", "docs/b", "relative: [ops](ops)")
	putPage(t, e, "main", "c", "absolute: [ops](/main/docs/ops) and again [ops](docs/ops#top)")
	putPage(t, e, "main", "docs/ops/self", "self: [ops](/main/docs/ops) [up](../ops)")
	putPage(t, e, "main", "d", "in code only: `[ops](docs/ops)` and https://example.com/docs/ops and `[[docs/ops]]`")
	putPage(t, e, "other", "x", "cross-space: [ops](/main/docs/ops)")
	putPage(t, e, "other", "y", "[ops](docs/ops)") // relative: inside "other", where docs/ops does not exist

	if got := describe(backlinks(t, e, "main", "docs/ops")); got != "main:c,main:docs/b,main:docs/ops/self,other:x" {
		t.Fatalf("backlinks: %s", got)
	}
	hits := backlinks(t, e, "main", "docs/ops")
	if hits[0].Title != "c" || hits[0].URL != "/main/c" || hits[3].SpaceTitle != "other" {
		t.Errorf("hit details: %+v", hits)
	}
	if got := describe(backlinks(t, e, "main", "docs")); got != "" {
		t.Errorf("docs has no backlinks, got %s", got)
	}
	for _, h := range backlinks(t, e, "main", "docs/ops") {
		if h.Space == "main" && h.Path == "docs/ops" {
			t.Error("page listed as linking to itself")
		}
	}
	// Edits and deletes are reflected at once.
	putPage(t, e, "main", "c", "no link now")
	expect(t, e.do("DELETE", "/main/docs/b", ""), 204)
	if got := describe(backlinks(t, e, "main", "docs/ops")); got != "main:docs/ops/self,other:x" {
		t.Errorf("after edits: %s", got)
	}
	expect(t, e.do("GET", "/_api/spaces/main/pages/nothing/backlinks", ""), 404)
	expect(t, e.do("GET", "/_api/spaces/nospace/pages/c/backlinks", ""), 404)
	if page := expect(t, e.do("GET", "/main/docs/ops", "", "Accept", "text/html"), 200); !strings.Contains(page, `id="backlinks"`) ||
		!strings.Contains(page, `/_api/spaces/main/pages/docs/ops/backlinks`) {
		t.Error("no backlinks panel")
	}
}

func TestBrokenLinks(t *testing.T) {
	e := newEnv(t)
	putPage(t, e, "main", "docs", "# Docs")
	putPage(t, e, "main", "docs/ok", "# OK")
	putPage(t, e, "main", "docs/p",
		"[good](ok) [abs good](/main/docs/ok) [bad](gone) [abs](/main/docs/gone2) [other](/other/nope) [ext](https://x.test/y) [anchor](#a) ![img](shot.png) `[[text only]]`")
	got := brokenOf(t, e, "main")
	targets := map[string]string{}
	for _, b := range got {
		targets[b.Target] = b.CreateURL
		if b.Path != "docs/p" || b.URL != "/main/docs/p" {
			t.Errorf("source: %+v", b)
		}
	}
	want := map[string]string{"gone": "/main/docs/gone?new", "/main/docs/gone2": "/main/docs/gone2?new", "/other/nope": "/other/nope?new"}
	if len(targets) != len(want) {
		t.Fatalf("broken links: %v", targets)
	}
	for k, v := range want {
		if targets[k] != v {
			t.Errorf("%s -> %q, want %q", k, targets[k], v)
		}
	}
	// Creating a target heals the link: nothing is stale.
	putPage(t, e, "main", "docs/gone", "# Gone")
	for _, b := range brokenOf(t, e, "main") {
		if b.Target == "gone" {
			t.Error("link still reported after its target was created")
		}
	}
	expect(t, e.do("DELETE", "/main/docs/ok", ""), 204)
	if n := len(brokenOf(t, e, "main")); n != 4 { // gone2, nope, and the two links to ok
		t.Errorf("deleting a target should break the links to it: %d", n)
	}
	page := expect(t, e.do("GET", "/main/?broken", "", "Accept", "text/html"), 200)
	if !strings.Contains(page, "Broken links") || !strings.Contains(page, "<code>gone2</code>") && !strings.Contains(page, "/main/docs/gone2") || !strings.Contains(page, "Create page") {
		t.Errorf("report page: %s", page)
	}
	if root := expect(t, e.do("GET", "/main/", "", "Accept", "text/html"), 200); !strings.Contains(root, `href="?broken"`) {
		t.Error("root page does not link to the report")
	}
	expect(t, e.do("GET", "/_api/spaces/nospace/links/broken", ""), 404)
}

func TestMovingPagesKeepsLinksWorking(t *testing.T) {
	e := newEnv(t)
	putPage(t, e, "main", "docs", "# Docs")
	putPage(t, e, "main", "docs/ops", "# Operations")
	putPage(t, e, "main", "docs/ops/runbook", "# Runbook\n\n[back](../ops) [dev](../dev/guide) [home](../../faq) [abs](/main/faq) ![x](shot.png)\n")
	putPage(t, e, "main", "docs/dev", "# Dev")
	putPage(t, e, "main", "docs/dev/guide", "# Guide\n\nsee [runbook](../ops/runbook) and [steps](/main/docs/ops/runbook#steps)")
	putPage(t, e, "main", "faq", "# FAQ\n\n[ops](docs/ops) [abs](/main/docs/ops/runbook.md#x) [dev](docs/dev)\n\n```\n[c](docs/ops)\n```\n")
	putPage(t, e, "other", "x", "elsewhere: [ops](/main/docs/ops)")
	before := e.do("GET", "/main/faq", "", "Accept", md).Header().Get("ETag")

	rec := e.do("POST", "/main/docs/ops?move=ops", "")
	expect(t, rec, 200)
	if rec.Header().Get("Location") != "/main/ops" {
		t.Fatalf("location %q", rec.Header().Get("Location"))
	}

	// Inbound links follow the page and its child; untouched ones stay as they were.
	wantFAQ := "# FAQ\n\n[ops](ops) [abs](/main/ops/runbook.md#x) [dev](docs/dev)\n\n```\n[c](docs/ops)\n```\n"
	if got := md_(t, e, "main", "faq"); got != wantFAQ {
		t.Fatalf("faq:\n%q\nwant\n%q", got, wantFAQ)
	}
	if got := md_(t, e, "main", "docs/dev/guide"); got != "# Guide\n\nsee [runbook](../../ops/runbook) and [steps](/main/ops/runbook#steps)" {
		t.Fatalf("guide: %q", got)
	}
	// The moved page's own relative links are recomputed from its new place.
	wantRunbook := "# Runbook\n\n[back](../ops) [dev](../docs/dev/guide) [home](../faq) [abs](/main/faq) ![x](shot.png)\n"
	if got := md_(t, e, "main", "ops/runbook"); got != wantRunbook {
		t.Fatalf("runbook:\n%q\nwant\n%q", got, wantRunbook)
	}
	// Everything still resolves, and backlinks find the new paths.
	if broken := brokenOf(t, e, "main"); len(broken) != 0 {
		t.Fatalf("broken after move: %+v", broken)
	}
	if got := describe(backlinks(t, e, "main", "ops")); got != "main:faq,main:ops/runbook" {
		t.Errorf("backlinks of the moved page: %s", got)
	}
	if got := describe(backlinks(t, e, "main", "ops/runbook")); got != "main:docs/dev/guide,main:faq" {
		t.Errorf("backlinks of the moved child: %s", got)
	}
	html := expect(t, e.do("GET", "/main/docs/dev/guide", "", "Accept", "text/html"), 200)
	if !strings.Contains(html, `href="/main/ops/runbook#steps"`) || !strings.Contains(html, `href="../../ops/runbook"`) || strings.Contains(html, "missing") {
		t.Errorf("guide html: %s", html)
	}
	// Rewritten pages have a new version, so stale editors see a conflict.
	if after := e.do("GET", "/main/faq", "", "Accept", md).Header().Get("ETag"); after == before {
		t.Error("faq version did not change")
	}
	// Links from other spaces are not rewritten; they show up as broken there.
	if got := md_(t, e, "other", "x"); got != "elsewhere: [ops](/main/docs/ops)" {
		t.Errorf("other space was edited: %q", got)
	}
	if b := brokenOf(t, e, "other"); len(b) != 1 || b[0].Target != "/main/docs/ops" {
		t.Errorf("other space's report: %+v", b)
	}
	// Moving back restores everything.
	expect(t, e.do("POST", "/main/ops?move=docs/ops", ""), 200)
	if got := md_(t, e, "main", "faq"); got != "# FAQ\n\n[ops](docs/ops) [abs](/main/docs/ops/runbook.md#x) [dev](docs/dev)\n\n```\n[c](docs/ops)\n```\n" {
		t.Errorf("faq after moving back: %q", got)
	}
	if got := md_(t, e, "main", "docs/ops/runbook"); got != "# Runbook\n\n[back](../ops) [dev](../dev/guide) [home](../../faq) [abs](/main/faq) ![x](shot.png)\n" {
		t.Errorf("runbook after moving back: %q", got)
	}
	if broken := brokenOf(t, e, "main"); len(broken) != 0 {
		t.Errorf("broken after moving back: %+v", broken)
	}
}

func TestRenamingAPageRewritesLinksToIt(t *testing.T) {
	e := newEnv(t)
	putPage(t, e, "main", "ops", "# Operations")
	putPage(t, e, "main", "a", "[x](ops) [y](/main/ops) [z](ops.md) [w](./ops/)")
	expect(t, e.do("POST", "/main/ops?move=operations", ""), 200)
	if got := md_(t, e, "main", "a"); got != "[x](operations) [y](/main/operations) [z](operations.md) [w](operations)" {
		t.Fatalf("a: %q", got)
	}
	if b := brokenOf(t, e, "main"); len(b) != 0 {
		t.Errorf("broken: %+v", b)
	}
}

func TestRenamingASpaceRewritesItsOwnAbsoluteLinks(t *testing.T) {
	e := newEnv(t)
	putPage(t, e, "main", "docs", "# Docs")
	putPage(t, e, "main", "a", "[abs](/main/docs) [rel](docs) [other](/other/x)")
	putPage(t, e, "other", "x", "# X\n\n[main](/main/docs)")
	expect(t, e.do("PUT", "/_api/spaces/main", `{"slug":"wiki"}`, "Content-Type", "application/json"), 200)
	if got := md_(t, e, "wiki", "a"); got != "[abs](/wiki/docs) [rel](docs) [other](/other/x)" {
		t.Fatalf("a: %q", got)
	}
	if b := brokenOf(t, e, "wiki"); len(b) != 0 {
		t.Errorf("broken in renamed space: %+v", b)
	}
	// Other spaces keep the old name. The link now starts with a name that is no
	// longer a space, so it is an ordinary site path and is not reported.
	if got := md_(t, e, "other", "x"); !strings.Contains(got, "(/main/docs)") {
		t.Errorf("other space edited: %q", got)
	}
	if b := brokenOf(t, e, "other"); len(b) != 0 {
		t.Errorf("other space report: %+v", b)
	}
}

func TestMovingUnderBasePath(t *testing.T) {
	e := newEnv(t)
	e.h = New(e.st, "/wiki")
	call := func(method, target, body string, want int) string {
		t.Helper()
		return expect(t, e.do(method, "/wiki"+target, body, "Content-Type", md), want)
	}
	call("PUT", "/main/docs", "# Docs", 201)
	call("PUT", "/main/a", "[abs](/wiki/main/docs) [x](/main/docs)", 201)
	call("POST", "/main/docs?move=guide", "", 200)
	// Only the link that carries the base path is ours to rewrite.
	if got := call("GET", "/main/a.md", "", 200); got != "[abs](/wiki/main/guide) [x](/main/docs)" {
		t.Errorf("base path: %q", got)
	}
}

func TestSearchAPI(t *testing.T) {
	e := newEnv(t)
	putPage(t, e, "main", "ops", "# Operations\n\nHow we run the production servers.")
	putPage(t, e, "main", "deploy", "# Deploying\n\nThe deployment script is running every night, see operations.")
	putPage(t, e, "other", "ops2", "# Team runbook\n\nOperations for the team, running things.")

	type hit = hitJSON
	search := func(url string) []hit {
		t.Helper()
		var hits []hit
		body := expect(t, e.do("GET", url, ""), 200)
		if err := json.Unmarshal([]byte(body), &hits); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		return hits
	}
	ids := func(hits []hit) string {
		var s []string
		for _, h := range hits {
			s = append(s, h.Space+":"+h.Path)
		}
		return strings.Join(s, ",")
	}

	// One space, best match first: the title match outranks the body match.
	hits := search("/_api/spaces/main/search?q=operations")
	if ids(hits) != "main:ops,main:deploy" {
		t.Fatalf("main: %s", ids(hits))
	}
	h := hits[0]
	if h.URL != "/main/ops" || h.Title != "Operations" || h.SpaceTitle != "main" ||
		!strings.Contains(h.TitleHTML, "<mark>Operations</mark>") {
		t.Errorf("hit: %+v", h)
	}
	if !strings.Contains(hits[1].SnippetHTML, "<mark>operations</mark>") {
		t.Errorf("snippet: %s", hits[1].SnippetHTML)
	}
	// All spaces, merged.
	all := search("/_api/search?q=operations")
	if len(all) != 3 || all[2].Space == "" {
		t.Fatalf("all: %s", ids(all))
	}
	// Restricting to a space with ?space=, limits, typing prefixes, stemming.
	if got := ids(search("/_api/search?q=operations&space=other")); got != "other:ops2" {
		t.Errorf("space filter: %s", got)
	}
	if got := len(search("/_api/search?q=operations&limit=1")); got != 1 {
		t.Errorf("limit: %d", got)
	}
	if got := ids(search("/_api/spaces/main/search?q=produc")); got != "main:ops" {
		t.Errorf("prefix: %s", got)
	}
	if got := ids(search("/_api/spaces/main/search?q=run")); got != "main:ops,main:deploy" && got != "main:deploy,main:ops" {
		t.Errorf("stem: %s", got)
	}
	if got := search("/_api/search?q="); len(got) != 0 {
		t.Errorf("empty query: %v", got)
	}
	if got := search(`/_api/search?q=%22%28%29*%20NEAR%2F2`); got == nil {
		t.Error("odd input must be an empty list, not an error")
	}
	expect(t, e.do("GET", "/_api/spaces/nospace/search?q=x", ""), 404)
	expect(t, e.do("GET", "/_api/search?q=x&space=nospace", ""), 404)
	// Index follows edits, moves and deletes.
	putPage(t, e, "main", "ops", "# Operations\n\nOnly databases now.")
	if got := ids(search("/_api/spaces/main/search?q=production")); got != "" {
		t.Errorf("stale index: %s", got)
	}
	expect(t, e.do("POST", "/main/ops?move=db", ""), 200)
	if got := ids(search("/_api/spaces/main/search?q=databases")); got != "main:db" {
		t.Errorf("after move: %s", got)
	}
	expect(t, e.do("DELETE", "/main/db", ""), 204)
	if got := ids(search("/_api/spaces/main/search?q=databases")); got != "" {
		t.Errorf("after delete: %s", got)
	}
}

func TestSearchOutputIsEscaped(t *testing.T) {
	e := newEnv(t)
	putPage(t, e, "main", "x", "# <script>alert(1)</script> Title\n\nneedle <img src=x onerror=alert(2)> & more")
	body := expect(t, e.do("GET", "/_api/search?q=needle", ""), 200)
	if strings.Contains(body, "<script") || strings.Contains(body, "<img") {
		t.Errorf("unescaped markup in JSON: %s", body)
	}
	page := expect(t, e.do("GET", "/_ui/search?q=needle", "", "Accept", "text/html"), 200)
	if strings.Contains(page, "<script>alert") || strings.Contains(page, "<img src=x") || !strings.Contains(page, "<mark>needle</mark>") {
		t.Errorf("search page: %s", page)
	}
}

func TestSearchUI(t *testing.T) {
	e := newEnv(t)
	putPage(t, e, "main", "ops", "# Operations\n\nproduction servers")
	putPage(t, e, "other", "o", "# Other\n\nproduction notes")
	page := expect(t, e.do("GET", "/_ui/search?q=production", "", "Accept", "text/html"), 200)
	for _, want := range []string{"Operations", "Other", `href="/main/ops"`, `href="/other/o"`, "<mark>production</mark>"} {
		if !strings.Contains(page, want) {
			t.Errorf("search page missing %q", want)
		}
	}
	scoped := expect(t, e.do("GET", "/_ui/search?q=production&space=other", "", "Accept", "text/html"), 200)
	if strings.Contains(scoped, `href="/main/ops"`) || !strings.Contains(scoped, `href="/other/o"`) {
		t.Errorf("scoped search: %s", scoped)
	}
	if none := expect(t, e.do("GET", "/_ui/search?q=zzzz", "", "Accept", "text/html"), 200); !strings.Contains(none, "No pages match") {
		t.Errorf("no results: %s", none)
	}
	// Every page offers the header form and the Ctrl+Shift+F pane.
	pg := expect(t, e.do("GET", "/main/ops", "", "Accept", "text/html"), 200)
	for _, want := range []string{`action="/_ui/search"`, `name="space" value="main"`, `id="search-pane"`, `data-api="/_api/search"`, `id="sp-all"`} {
		if !strings.Contains(pg, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if home := expect(t, e.do("GET", "/", "", "Accept", "text/html"), 200); !strings.Contains(home, `id="search-pane"`) || strings.Contains(home, `id="sp-all"`) {
		t.Error("home page search")
	}
}
