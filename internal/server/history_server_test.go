package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hbroek/wako/internal/diff"
)

// An env whose every save is its own revision, so history is easy to count.
func newHistEnv(t *testing.T) *env {
	e := newEnv(t)
	e.st.RevisionWindow = 0
	return e
}

func etag(e *env, path string) string {
	return e.do("HEAD", path, "", "Accept", md).Header().Get("ETag")
}

func TestHistoryRevisionsAndDiffs(t *testing.T) {
	e := newHistEnv(t)
	expect(t, e.do("PUT", "/main/p", "# P\n\nfirst\n", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/p", "# P\n\nsecond <b>bold</b> & more\n", "Content-Type", md), 200)
	expect(t, e.do("PUT", "/main/p", "# P renamed\n\nthird\n", "Content-Type", md), 200)

	// The list, as JSON and as a page.
	var revs []revJSON
	if err := json.Unmarshal([]byte(expect(t, e.do("GET", "/main/p?history", "", "Accept", "application/json"), 200)), &revs); err != nil {
		t.Fatal(err)
	}
	if len(revs) != 3 || revs[0].Version != 3 || !revs[0].Current || revs[1].Current || revs[2].Version != 1 || revs[2].Action != "created" || revs[0].Title != "P renamed" {
		t.Fatalf("revisions: %+v", revs)
	}
	var api []revJSON
	json.Unmarshal([]byte(expect(t, e.do("GET", "/_api/spaces/main/pages/p/revisions", ""), 200)), &api)
	if len(api) != 3 || api[1].Version != 2 {
		t.Errorf("API list: %+v", api)
	}
	page := expect(t, e.do("GET", "/main/p?history", "", "Accept", "text/html"), 200)
	for _, want := range []string{"History of", `href="/main/p?rev=2"`, `href="/main/p?diff=2"`, `data-restore="1"`, "current", "created the page"} {
		if !strings.Contains(page, want) {
			t.Errorf("history page missing %q", want)
		}
	}
	if strings.Contains(page, `data-restore="3"`) || strings.Contains(page, `?diff=1"`) {
		t.Error("the current revision cannot be restored, and the first has nothing to compare with")
	}
	expect(t, e.do("GET", "/main/p?history", "", "Accept", "image/png"), 406)

	// Viewing an old revision.
	if got := expect(t, e.do("GET", "/main/p.md?rev=1", ""), 200); got != "# P\n\nfirst\n" {
		t.Errorf("rev 1 as markdown: %q", got)
	}
	if got := expect(t, e.do("GET", "/main/p?rev=2", "", "Accept", md), 200); got != "# P\n\nsecond <b>bold</b> & more\n" {
		t.Errorf("rev 2 via Accept: %q", got)
	}
	rec := e.do("GET", "/main/p.md?rev=2", "")
	if rec.Header().Get("ETag") != `"v2"` {
		t.Errorf("revision etag %q", rec.Header().Get("ETag"))
	}
	expect(t, e.do("GET", "/main/p.md?rev=2", "", "If-None-Match", `"v2"`), 304)
	var one struct {
		revJSON
		Body string
		Path string
	}
	json.Unmarshal([]byte(expect(t, e.do("GET", "/main/p?rev=1", "", "Accept", "application/json"), 200)), &one)
	if one.Version != 1 || one.Body != "# P\n\nfirst\n" || one.Current || one.Path != "p" {
		t.Errorf("revision JSON: %+v", one)
	}
	old := expect(t, e.do("GET", "/main/p?rev=1", "", "Accept", "text/html"), 200)
	for _, want := range []string{"revision 1", "first", `data-restore="1"`, "Current version", `?diff=1`} {
		if !strings.Contains(old, want) {
			t.Errorf("revision page missing %q", want)
		}
	}
	// An old revision is not an editable page.
	for _, bad := range []string{`id="edit-link"`, `id="backlinks"`, `id="move"`, `data-dialog`} {
		if strings.Contains(old, bad) {
			t.Errorf("revision page offers %s", bad)
		}
	}
	// The current revision says so and has nothing to restore.
	if cur := expect(t, e.do("GET", "/main/p?rev=3", "", "Accept", "text/html"), 200); !strings.Contains(cur, "This is the current text") || strings.Contains(cur, "data-restore") {
		t.Error("current revision banner")
	}
	if msg := expect(t, e.do("GET", "/main/p?rev=9", "", "Accept", md), 404); !strings.Contains(msg, "no such revision") {
		t.Errorf("missing revision: %s", msg)
	}
	expect(t, e.do("GET", "/main/p?rev=abc", "", "Accept", md), 400)
	expect(t, e.do("GET", "/main/p?rev=0", "", "Accept", md), 400)
	expect(t, e.do("GET", "/main/nope?history", "", "Accept", "application/json"), 404)
	expect(t, e.do("GET", "/_api/spaces/main/pages/nope/revisions", ""), 404)

	// Diffs: of a revision against the one before, or against any other.
	d := expect(t, e.do("GET", "/main/p?diff=2", "", "Accept", "text/x-diff"), 200)
	if !strings.HasPrefix(d, "--- p (revision 1)\n+++ p (revision 2)\n@@") || !strings.Contains(d, "-first\n") || !strings.Contains(d, "+second <b>bold</b> & more\n") {
		t.Errorf("diff:\n%s", d)
	}
	if got, err := diff.Apply("# P\n\nfirst\n", d); err != nil || got != "# P\n\nsecond <b>bold</b> & more\n" {
		t.Errorf("the diff must apply to revision 1: %q %v", got, err)
	}
	if rec := e.do("GET", "/main/p?diff=2", "", "Accept", "text/x-diff"); !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/x-diff") {
		t.Errorf("diff type %q", rec.Header().Get("Content-Type"))
	}
	wide := expect(t, e.do("GET", "/_api/spaces/main/pages/p/diff?from=1&to=3", ""), 200)
	if !strings.Contains(wide, "-first") || !strings.Contains(wide, "+third") || !strings.Contains(wide, "+# P renamed") {
		t.Errorf("wide diff: %s", wide)
	}
	if def := expect(t, e.do("GET", "/_api/spaces/main/pages/p/diff", ""), 200); !strings.Contains(def, "(revision 3)") || !strings.Contains(def, "(revision 2)") {
		t.Errorf("default diff compares the current revision with the one before: %s", def)
	}
	first := expect(t, e.do("GET", "/main/p?diff=1", "", "Accept", "text/x-diff"), 200)
	if !strings.HasPrefix(first, "--- /dev/null\n+++ p (revision 1)\n") || !strings.Contains(first, "+first") {
		t.Errorf("first revision's diff: %s", first)
	}
	// The same diff as a page, with every character escaped.
	html := expect(t, e.do("GET", "/main/p?diff=2", "", "Accept", "text/html"), 200)
	for _, want := range []string{`class="diff"`, `<tr class="del">`, `<tr class="add">`, "second &lt;b&gt;bold&lt;/b&gt; &amp; more", "All revisions"} {
		if !strings.Contains(html, want) {
			t.Errorf("diff page missing %q", want)
		}
	}
	if strings.Contains(html, "<b>bold</b>") {
		t.Error("diff text not escaped")
	}
	expect(t, e.do("GET", "/main/p?diff=9", "", "Accept", "text/x-diff"), 404)
	// A diff only makes sense for ?diff.
	expect(t, e.do("GET", "/main/p", "", "Accept", "text/x-diff"), 406)
	expect(t, e.do("GET", "/main/", "", "Accept", "text/x-diff"), 406)

	// Comparing two texts, for the editor's conflict view.
	post := func(body string, hdr ...string) string {
		return expect(t, e.do("POST", "/_api/spaces/main/diff", body, append([]string{"Content-Type", "application/json"}, hdr...)...), 200)
	}
	res := post(`{"from":"a\nb\n","to":"a\n<script>alert(1)</script>\n"}`)
	var got struct {
		Changed bool
		HTML    string
	}
	if err := json.Unmarshal([]byte(res), &got); err != nil || !got.Changed || strings.Contains(got.HTML, "<script>") || !strings.Contains(got.HTML, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("text diff: %s", res)
	}
	if res := post(`{"from":"same","to":"same"}`); !strings.Contains(res, `"changed":false`) || !strings.Contains(res, "No differences") {
		t.Errorf("equal texts: %s", res)
	}
	expect(t, e.do("POST", "/_api/spaces/main/diff", "x", "Content-Type", "text/plain"), 415)
	expect(t, e.do("POST", "/_api/spaces/main/diff", "{", "Content-Type", "application/json"), 400)
	expect(t, e.do("POST", "/_api/spaces/nospace/diff", `{}`, "Content-Type", "application/json"), 404)
}

func TestRestore(t *testing.T) {
	e := newHistEnv(t)
	expect(t, e.do("PUT", "/main/other", "# Other", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/p", "# P\n\nsee [other](other)\n", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/p", "# P\n\nno links now\n", "Content-Type", md), 200)
	if got := describe(backlinks(t, e, "main", "other")); got != "" {
		t.Fatalf("backlinks before restoring: %s", got)
	}

	rec := e.do("POST", "/main/p?restore=1", "")
	expect(t, rec, 200)
	if rec.Header().Get("ETag") != `"v3"` || rec.Header().Get("Location") != "/main/p" {
		t.Errorf("restore response: %v", rec.Header())
	}
	if got := md_(t, e, "main", "p"); got != "# P\n\nsee [other](other)\n" {
		t.Errorf("restored text: %q", got)
	}
	// What the text refers to is restored too.
	if got := describe(backlinks(t, e, "main", "other")); got != "main:p" {
		t.Errorf("backlinks after restoring: %s", got)
	}
	// History grows; nothing is lost.
	var revs []revJSON
	json.Unmarshal([]byte(expect(t, e.do("GET", "/main/p?history", "", "Accept", "application/json"), 200)), &revs)
	if len(revs) != 3 || revs[0].Action != "restored" || revs[0].Note != "restored revision 1" {
		t.Fatalf("history after restoring: %+v", revs)
	}
	if got := expect(t, e.do("GET", "/main/p.md?rev=2", ""), 200); got != "# P\n\nno links now\n" {
		t.Errorf("revision 2 must survive a restore: %q", got)
	}
	// Restoring the text that is current already does nothing.
	rec = e.do("POST", "/main/p?restore=3", "")
	expect(t, rec, 200)
	if rec.Header().Get("ETag") != `"v3"` {
		t.Errorf("no-op restore changed the version: %v", rec.Header())
	}
	// Preconditions and errors.
	expect(t, e.do("POST", "/main/p?restore=2", "", "If-Match", `"v1"`), 412)
	expect(t, e.do("POST", "/main/p?restore=2", "", "If-Match", `"v3"`), 200)
	expect(t, e.do("POST", "/main/p?restore=99", ""), 404)
	expect(t, e.do("POST", "/main/p?restore=x", ""), 400)
	expect(t, e.do("POST", "/main/nope?restore=1", ""), 404)
	expect(t, e.do("POST", "/main/?restore=1", ""), 400)
}

func TestPatch(t *testing.T) {
	e := newHistEnv(t)
	base := "# Title\n\nline one\nline two\nline three\n"
	expect(t, e.do("PUT", "/main/p", base, "Content-Type", md), 201)
	patchType := "Content-Type"
	do := func(body string, hdr ...string) *httptest.ResponseRecorder {
		return e.do("PATCH", "/main/p", body, hdr...)
	}
	changed := "# Title\n\nline one\nLINE TWO\nline three\nline four\n"
	p := diff.Unified(base, changed, "a", "b")

	// A diff against the current version applies and makes a new version.
	rec := do(p, patchType, "text/x-diff", "If-Match", `"v1"`)
	expect(t, rec, 200)
	if rec.Header().Get("ETag") != `"v2"` || rec.Header().Get("Location") != "/main/p" {
		t.Errorf("patch response: %v", rec.Header())
	}
	if got := md_(t, e, "main", "p"); got != changed {
		t.Fatalf("patched text: %q", got)
	}
	// The same diff against the old version is stale: 412, and nothing changes.
	expect(t, do(p, patchType, "text/x-diff", "If-Match", `"v1"`), 412)
	// A diff that does not fit the current text is 409 with a reason.
	other := diff.Unified("# Title\n\nsomething else\n", "# Title\n\nsomething else entirely\n", "a", "b")
	if msg := expect(t, do(other, patchType, "text/x-diff", "If-Match", `"v2"`), 409); !strings.Contains(msg, "does not") {
		t.Errorf("409 message: %s", msg)
	}
	expect(t, do("this is not a diff", patchType, "text/x-diff", "If-Match", `"v2"`), 409)
	expect(t, do("", patchType, "text/x-diff", "If-Match", `"v2"`), 409)
	// Required: the right type and the version it was made against.
	rec = do(p, patchType, "text/plain", "If-Match", `"v2"`)
	expect(t, rec, 415)
	if rec.Header().Get("Accept-Patch") != "text/x-diff" {
		t.Errorf("415 without Accept-Patch: %v", rec.Header())
	}
	expect(t, do(p, patchType, "text/x-diff"), 428)
	expect(t, do(p, patchType, "text/x-diff", "If-Match", "*"), 428)
	expect(t, do(p, patchType, "text/x-diff", "If-Match", `"garbage"`), 412)
	expect(t, do(p, patchType, "application/json", "If-Match", `"v2"`), 415)
	expect(t, e.do("PATCH", "/main/nope", p, patchType, "text/x-diff", "If-Match", `"v1"`), 404)
	expect(t, e.do("PATCH", "/main/", p, patchType, "text/x-diff", "If-Match", `"v1"`), 405)
	// A diff that changes nothing is accepted and leaves the version alone.
	rec = do("--- a\n+++ b\n@@ -3 +3 @@\n-line one\n+line one\n", patchType, "text/x-diff", "If-Match", `"v2"`)
	expect(t, rec, 200)
	if rec.Header().Get("ETag") != `"v2"` {
		t.Errorf("no-op patch: %v", rec.Header())
	}

	// The patch is a real edit: the title, links and history follow the new text.
	expect(t, e.do("PUT", "/main/target", "# Target", "Content-Type", md), 201)
	cur := md_(t, e, "main", "p")
	next := strings.Replace(cur, "# Title", "# A new title", 1) + "\nSee [t](target).\n"
	expect(t, do(diff.Unified(cur, next, "a", "b"), patchType, "text/x-diff", "If-Match", `"v2"`), 200)
	if got := describe(backlinks(t, e, "main", "target")); got != "main:p" {
		t.Errorf("a patch must update the links: %s", got)
	}
	if !strings.Contains(expect(t, e.do("GET", "/main/p?history", "", "Accept", "application/json"), 200), `"title":"A new title"`) {
		t.Error("title not updated")
	}

	// A diff written by the real diff(1): CRLF text and a missing final newline.
	expect(t, e.do("PUT", "/main/crlf", "a\r\nb\r\nc", "Content-Type", md), 201)
	crlf := diff.Unified("a\r\nb\r\nc", "a\r\nB\r\nc\r\nd", "a", "b")
	expect(t, e.do("PATCH", "/main/crlf", crlf, patchType, "text/x-diff", "If-Match", `"v1"`), 200)
	if got := md_(t, e, "main", "crlf"); got != "a\r\nB\r\nc\r\nd" {
		t.Errorf("crlf: %q", got)
	}
	// Oversized patches are refused.
	expect(t, do(strings.Repeat("x", 5<<20), patchType, "text/x-diff", "If-Match", `"v3"`), 413)
}

// Two people edit the same page at once: nobody's work is silently overwritten.
func TestSimultaneousEditsNeverOverwriteSilently(t *testing.T) {
	e := newHistEnv(t)
	expect(t, e.do("PUT", "/main/p", "one\ntwo\nthree\n", "Content-Type", md), 201)
	opened := etag(e, "/main/p") // both open the editor at version 1

	expect(t, e.do("PUT", "/main/p", "one\nTWO (alice)\nthree\n", "Content-Type", md, "If-Match", opened), 200)
	rec := e.do("PUT", "/main/p", "one\ntwo\nTHREE (bob)\n", "Content-Type", md, "If-Match", opened)
	expect(t, rec, 412)
	if got := md_(t, e, "main", "p"); got != "one\nTWO (alice)\nthree\n" {
		t.Fatalf("alice's work was lost: %q", got)
	}
	// Bob's editor fetches the current text and shows the difference.
	mine := "one\ntwo\nTHREE (bob)\n"
	res := expect(t, e.do("POST", "/_api/spaces/main/diff",
		fmt.Sprintf(`{"from":%q,"to":%q}`, md_(t, e, "main", "p"), mine), "Content-Type", "application/json"), 200)
	if !strings.Contains(res, "TWO (alice)") || !strings.Contains(res, "THREE (bob)") {
		t.Errorf("conflict diff: %s", res)
	}
	// He decides to save over it, knowingly, against the version he just saw.
	current := etag(e, "/main/p")
	expect(t, e.do("PUT", "/main/p", mine, "Content-Type", md, "If-Match", current), 200)
	// ...and alice's text is still in the history.
	if got := expect(t, e.do("GET", "/main/p.md?rev=2", ""), 200); got != "one\nTWO (alice)\nthree\n" {
		t.Errorf("alice's revision: %q", got)
	}
	// The same race with patches: both made against version 1; one wins, one is refused.
	expect(t, e.do("PUT", "/main/q", "a\nb\nc\n", "Content-Type", md), 201)
	d1 := diff.Unified("a\nb\nc\n", "A\nb\nc\n", "a", "b")
	d2 := diff.Unified("a\nb\nc\n", "a\nb\nC\n", "a", "b")
	expect(t, e.do("PATCH", "/main/q", d1, "Content-Type", "text/x-diff", "If-Match", `"v1"`), 200)
	expect(t, e.do("PATCH", "/main/q", d2, "Content-Type", "text/x-diff", "If-Match", `"v1"`), 412)
	if got := md_(t, e, "main", "q"); got != "A\nb\nc\n" {
		t.Errorf("after the race: %q", got)
	}
}

func TestEditorOffersAConflictView(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/p", "x", "Content-Type", md), 201)
	f := expect(t, e.do("GET", "/main/p?edit", ""), 200)
	for _, want := range []string{`id="conflict"`, `data-diff="/_api/spaces/main/diff"`, `id="conflict-mine"`, `id="conflict-theirs"`, `id="conflict-keep"`} {
		if !strings.Contains(f, want) {
			t.Errorf("editor missing %s", want)
		}
	}
	js := expect(t, e.do("GET", "/_ui/app.js", ""), 200)
	if !strings.Contains(js, "showConflict") || !strings.Contains(js, "data-restore") && !strings.Contains(js, "[data-restore]") {
		t.Error("script lacks the conflict and restore handling")
	}
}

func TestPatchAdvertisement(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/p", "x", "Content-Type", md), 201)
	h := e.do("OPTIONS", "/main/p", "").Header()
	if !strings.Contains(h.Get("Allow"), "PATCH") || h.Get("Accept-Patch") != "text/x-diff" {
		t.Errorf("page caps: %v", h)
	}
	if root := e.do("OPTIONS", "/main/", "").Header(); strings.Contains(root.Get("Allow"), "PATCH") || root.Get("Accept-Patch") != "" {
		t.Errorf("the space root has no text to patch: %v", root)
	}
}

// A browser editor presses Ctrl+S over and over, so its quick saves are one
// revision. A program makes each call on purpose, so every call is kept.
func TestOnlyBrowserSavesAreMerged(t *testing.T) {
	a := newAuthEnv(t) // default 10 minute window
	a.user("alice", false)
	sp := a.space("s", true)
	a.st.SetMember(sp.ID, a.users["alice"].ID, "editor")
	tok := bearer(a.token("alice"))
	mdT := header("Content-Type", "text/markdown")
	count := func(path string) int {
		var revs []revJSON
		json.Unmarshal([]byte(status(t, a.do("GET", "/s/"+path+"?history", "", header("Accept", "application/json")), 200)), &revs)
		return len(revs)
	}

	// With an API token: every call is a revision.
	status(t, a.do("PUT", "/s/api", "1", mdT, tok), 201)
	status(t, a.do("PUT", "/s/api", "2", mdT, tok), 200)
	status(t, a.do("PUT", "/s/api", "3", mdT, tok), 200)
	patch := diff.Unified("3", "3\nx\n", "a", "b")
	status(t, a.do("PATCH", "/s/api", patch, header("Content-Type", "text/x-diff"), header("If-Match", `"v3"`), tok), 200)
	status(t, a.do("POST", "/s/api?restore=1", "", tok), 200)
	if n := count("api"); n != 5 {
		t.Errorf("API saves: %d revisions, want 5", n)
	}

	// From the browser editor: the quick ones merge, a later one does not.
	j := a.login("alice")
	put := func(body string) {
		t.Helper()
		if rec := a.do("PUT", "/s/ui", body, mdT, withJar(j), withCSRF(j)); rec.Code > 201 {
			t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
		}
	}
	put("1")
	put("2")
	put("3")
	if n := count("ui"); n != 1 {
		t.Errorf("a burst of browser saves: %d revisions, want 1", n)
	}
	a.now = a.now.Add(11 * time.Minute)
	a.st.Now = a.clock
	put("4")
	if n := count("ui"); n != 2 {
		t.Errorf("a save after the window: %d revisions, want 2", n)
	}
	// And the burst really was merged: the text of the first revision is the last save before the window.
	if got := expect2(t, a.do("GET", "/s/ui.md?rev=3", "")); got != "3" {
		t.Errorf("merged revision text: %q", got)
	}
}

func expect2(t *testing.T, rec *httptest.ResponseRecorder) string { return status(t, rec, 200) }
