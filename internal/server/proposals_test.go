package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	xhtml "golang.org/x/net/html"

	"github.com/hbroek/wako/internal/auth"
	"github.com/hbroek/wako/internal/store"
)

type propEnv struct {
	*aenv
	sp                     *store.Space
	ed, sug, other, view   reqOpt
	sugName, otherSugName  string
	editorSuggestOnlyToken reqOpt
	mdT                    reqOpt
}

func newPropEnv(t *testing.T) *propEnv {
	a := newAuthEnv(t)
	for _, n := range []string{"ed", "sug", "sug2", "view"} {
		a.user(n, false)
	}
	sp := a.space("main", true)
	a.st.SetMember(sp.ID, a.users["ed"].ID, store.RoleEditor)
	a.st.SetMember(sp.ID, a.users["sug"].ID, store.RoleSuggester)
	a.st.SetMember(sp.ID, a.users["sug2"].ID, store.RoleSuggester)
	a.st.SetMember(sp.ID, a.users["view"].ID, store.RoleViewer)
	e := &propEnv{aenv: a, sp: sp, mdT: header("Content-Type", "text/markdown")}
	e.ed, e.sug, e.other, e.view = bearer(a.token("ed")), bearer(a.token("sug")), bearer(a.token("sug2")), bearer(a.token("view"))
	secret := "wako_" + strings.Repeat("s", 40)
	if _, err := a.st.CreateTokenWith(a.users["ed"].ID, "agent", auth.HashToken(secret), true); err != nil {
		t.Fatal(err)
	}
	e.editorSuggestOnlyToken = bearer(secret)
	return e
}

func propID(t *testing.T, rec *httptest.ResponseRecorder) int64 {
	t.Helper()
	var j struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil || j.ID == 0 || j.Status != "pending" {
		t.Fatalf("not a pending proposal: %d %q (%v)", rec.Code, rec.Body.String(), err)
	}
	return j.ID
}

func TestSuggestingQueuesInsteadOfWriting(t *testing.T) {
	e := newPropEnv(t)
	e.do("PUT", "/main/doc", "# Doc\n\nline one\n", e.mdT, e.ed)

	rec := e.do("PUT", "/main/doc", "# Doc\n\nline one changed\n", e.mdT, e.sug, header("X-Wako-Note", "typo"))
	if rec.Code != 202 || !strings.HasPrefix(rec.Header().Get("Location"), "/main/_proposals/") {
		t.Fatalf("PUT by a suggester: %d %s %v", rec.Code, rec.Body.String(), rec.Header())
	}
	id := propID(t, rec)
	if pg, _ := e.sp.GetPage("doc"); strings.Contains(pg.Body, "changed") || pg.Version != 1 {
		t.Fatalf("the page was changed: %+v", pg)
	}

	// A new page is only proposed, too, and so is a child, and a patch.
	if rec := e.do("PUT", "/main/fresh", "# Fresh\n", e.mdT, e.sug); rec.Code != 202 {
		t.Errorf("new page: %d", rec.Code)
	}
	if _, err := e.sp.GetPage("fresh"); err == nil {
		t.Error("a suggested new page exists already")
	}
	if rec := e.do("POST", "/main/doc", `{"slug":"kid","body":"# Kid\n"}`, header("Content-Type", "application/json"), e.sug); rec.Code != 202 {
		t.Errorf("child page: %d %s", rec.Code, rec.Body.String())
	}
	patch := "--- a\n+++ b\n@@ -1,3 +1,3 @@\n # Doc\n \n-line one\n+line 1\n"
	if rec := e.do("PATCH", "/main/doc", patch, header("Content-Type", "text/x-diff"), header("If-Match", `"v1"`), e.sug); rec.Code != 202 {
		t.Errorf("patch: %d %s", rec.Code, rec.Body.String())
	}
	// Stale or wrong preconditions are refused like for a writer.
	if rec := e.do("PUT", "/main/doc", "x", e.mdT, e.sug, header("If-Match", `"v9"`)); rec.Code != 412 {
		t.Errorf("stale If-Match: %d", rec.Code)
	}
	if rec := e.do("PUT", "/main/nowhere/deep", "x", e.mdT, e.sug); rec.Code != 409 {
		t.Errorf("missing parent: %d", rec.Code)
	}
	// Suggesting the text that is already there is not a suggestion.
	if rec := e.do("PUT", "/main/doc", "# Doc\n\nline one\n", e.mdT, e.sug); rec.Code != 200 {
		t.Errorf("no-op: %d", rec.Code)
	}
	// Moving, deleting, restoring and reordering stay with editors.
	for _, c := range [][2]string{{"DELETE", "/main/doc"}, {"POST", "/main/doc?move=x"}, {"POST", "/main/doc?restore=1"}, {"POST", "/main/?reorder"}} {
		if rec := e.do(c[0], c[1], "{}", header("Content-Type", "application/json"), e.sug); rec.Code != 403 {
			t.Errorf("%s %s by a suggester: %d", c[0], c[1], rec.Code)
		}
	}
	// A viewer and a stranger cannot suggest at all.
	if rec := e.do("PUT", "/main/doc", "x", e.mdT, e.view); rec.Code != 403 {
		t.Errorf("viewer: %d", rec.Code)
	}
	if rec := e.do("PUT", "/main/doc", "x", e.mdT); rec.Code != 401 {
		t.Errorf("anonymous: %d", rec.Code)
	}
	// The suggestion is visible to its author, and says what it is.
	rec = e.do("GET", "/main/_proposals/"+itoa(id), "", e.sug, header("Accept", "application/json"))
	var pj proposalJSON
	json.Unmarshal(rec.Body.Bytes(), &pj)
	if rec.Code != 200 || pj.Author != "sug" || pj.Note != "typo" || pj.BaseVersion != 1 || !strings.Contains(pj.Diff, "+line one changed") || pj.Source != "api" {
		t.Errorf("proposal: %d %+v", rec.Code, pj)
	}
}

func TestSuggestOnlyTokenCannotWrite(t *testing.T) {
	e := newPropEnv(t)
	e.do("PUT", "/main/doc", "# Doc\n", e.mdT, e.ed)
	// The same editor: their own token writes, the suggest-only one queues.
	if rec := e.do("PUT", "/main/doc", "# Doc\n\nby token\n", e.mdT, e.ed); rec.Code != 200 {
		t.Fatalf("editor token: %d", rec.Code)
	}
	rec := e.do("PUT", "/main/doc", "# Doc\n\nby agent\n", e.mdT, e.editorSuggestOnlyToken)
	if rec.Code != 202 {
		t.Fatalf("suggest-only token: %d %s", rec.Code, rec.Body.String())
	}
	if pg, _ := e.sp.GetPage("doc"); strings.Contains(pg.Body, "agent") {
		t.Fatal("a suggest-only token wrote a page")
	}
	for _, c := range [][2]string{{"DELETE", "/main/doc"}, {"POST", "/main/doc?move=x"}} {
		if rec := e.do(c[0], c[1], "", e.editorSuggestOnlyToken); rec.Code != 403 {
			t.Errorf("%s with a suggest-only token: %d", c[0], rec.Code)
		}
	}
	// It cannot review either, not even its own owner's queue.
	id := propID(t, rec)
	if rec := e.do("POST", "/main/_proposals/"+itoa(id)+"/accept", "{}", header("Content-Type", "application/json"), e.editorSuggestOnlyToken); rec.Code != 403 {
		t.Errorf("accept with a suggest-only token: %d", rec.Code)
	}
	// The change is attributed to the owner, with the token named in the note once accepted.
	if rec := e.do("POST", "/main/_proposals/"+itoa(id)+"/accept", "{}", header("Content-Type", "application/json"), e.ed); rec.Code != 200 {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body.String())
	}
	// Direct writes with a normal token record which token made them.
	revs, _ := e.sp.Revisions(mustPage(t, e.sp, "doc").ID)
	if len(revs) < 2 || !strings.Contains(revs[1].Note, "API token") {
		t.Errorf("revision notes: %+v", revs)
	}
}

func TestReviewingSuggestions(t *testing.T) {
	e := newPropEnv(t)
	e.do("PUT", "/main/doc", "# Doc\n\none\n\ntwo\n\nthree\n", e.mdT, e.ed)
	jsonT := header("Content-Type", "application/json")

	id := propID(t, e.do("PUT", "/main/doc", "# Doc\n\none\n\ntwo\n\nTHREE\n", e.mdT, e.sug, header("X-Wako-Note", "shout")))
	other := propID(t, e.do("PUT", "/main/doc", "# Doc\n\nuno\n\ntwo\n\nthree\n", e.mdT, e.other))

	// Who sees what.
	list := func(who reqOpt) []proposalJSON {
		rec := e.do("GET", "/main/_proposals", "", who, header("Accept", "application/json"))
		if rec.Code != 200 {
			t.Fatalf("list: %d", rec.Code)
		}
		var out []proposalJSON
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if got := list(e.ed); len(got) != 2 {
		t.Errorf("an editor sees all: %d", len(got))
	}
	if got := list(e.sug); len(got) != 1 || got[0].ID != id {
		t.Errorf("a suggester sees only their own: %+v", got)
	}
	if rec := e.do("GET", "/main/_proposals/"+itoa(other), "", e.sug); rec.Code != 404 {
		t.Errorf("someone else's suggestion: %d", rec.Code)
	}
	for _, who := range []reqOpt{e.view, nil} {
		var opts []reqOpt
		if who != nil {
			opts = append(opts, who)
		}
		if rec := e.do("GET", "/main/_proposals", "", opts...); rec.Code != 403 && rec.Code != 401 {
			t.Errorf("readers must not see suggestions: %d", rec.Code)
		}
	}
	if rec := e.do("POST", "/main/_proposals/"+itoa(id)+"/accept", "{}", jsonT, e.sug); rec.Code != 403 {
		t.Errorf("a suggester accepting their own: %d", rec.Code)
	}

	// Someone edits the page meanwhile: a different line, so the suggestion still fits.
	e.do("PUT", "/main/doc", "# Doc\n\none\n\ntwo\n\nthree\n\nfour\n", e.mdT, e.ed)
	rec := e.do("POST", "/main/_proposals/"+itoa(id)+"/accept", `{"note":"thanks"}`, jsonT, e.ed)
	if rec.Code != 200 {
		t.Fatalf("accept (rebased): %d %s", rec.Code, rec.Body.String())
	}
	pg := mustPage(t, e.sp, "doc")
	if !strings.Contains(pg.Body, "THREE") || !strings.Contains(pg.Body, "four") {
		t.Errorf("rebased text lost a change: %q", pg.Body)
	}
	revs, _ := e.sp.Revisions(pg.ID)
	if revs[0].Author != "sug" || !strings.Contains(revs[0].Note, "accepted by ed") || !strings.Contains(revs[0].Note, "shout") {
		t.Errorf("history credits: %+v", revs[0])
	}
	if rec := e.do("POST", "/main/_proposals/"+itoa(id)+"/accept", "{}", jsonT, e.ed); rec.Code != 409 {
		t.Errorf("accepting twice: %d", rec.Code)
	}
	var done proposalJSON
	json.Unmarshal(e.do("GET", "/main/_proposals/"+itoa(id), "", e.ed, header("Accept", "application/json")).Body.Bytes(), &done)
	if done.Status != "accepted" || done.DecidedBy != "ed" || done.Applied != pg.Version || done.Decision != "thanks" {
		t.Errorf("recorded decision: %+v", done)
	}

	// The other suggestion changed the line "one", which still fits too; but a change to the same line does not.
	e.do("PUT", "/main/doc", "# Doc\n\nEINS\n\ntwo\n\nTHREE\n\nfour\n", e.mdT, e.ed)
	if rec := e.do("POST", "/main/_proposals/"+itoa(other)+"/accept", "{}", jsonT, e.ed); rec.Code != 409 || !strings.Contains(rec.Body.String(), "no longer fits") {
		t.Errorf("conflicting suggestion: %d %s", rec.Code, rec.Body.String())
	}
	// The reviewer may accept with their own text instead.
	rec = e.do("POST", "/main/_proposals/"+itoa(other)+"/accept", `{"body":"# Doc\n\nEINS / uno\n"}`, jsonT, e.ed)
	if rec.Code != 200 || mustPage(t, e.sp, "doc").Body != "# Doc\n\nEINS / uno\n" {
		t.Errorf("accept with own text: %d %s", rec.Code, rec.Body.String())
	}

	// Reject and withdraw.
	r1 := propID(t, e.do("PUT", "/main/a", "# A\n", e.mdT, e.sug))
	r2 := propID(t, e.do("PUT", "/main/b", "# B\n", e.mdT, e.sug))
	if rec := e.do("POST", "/main/_proposals/"+itoa(r1)+"/reject", `{"note":"not now"}`, jsonT, e.ed); rec.Code != 200 {
		t.Errorf("reject: %d", rec.Code)
	}
	if rec := e.do("POST", "/main/_proposals/"+itoa(r2)+"/withdraw", "{}", jsonT, e.other); rec.Code != 404 && rec.Code != 403 {
		t.Errorf("withdrawing someone else's: %d", rec.Code)
	}
	if rec := e.do("POST", "/main/_proposals/"+itoa(r2)+"/withdraw", "{}", jsonT, e.sug); rec.Code != 200 {
		t.Errorf("withdraw: %d %s", rec.Code, rec.Body.String())
	}
	for _, p := range []string{"a", "b"} {
		if _, err := e.sp.GetPage(p); err == nil {
			t.Errorf("page %s exists though its suggestion was not accepted", p)
		}
	}

	// Accepting a new-page suggestion creates it; one for a page that now exists does not clobber it.
	n1 := propID(t, e.do("PUT", "/main/newpage", "# New\n", e.mdT, e.sug))
	if rec := e.do("POST", "/main/_proposals/"+itoa(n1)+"/accept", "{}", jsonT, e.ed); rec.Code != 200 || mustPage(t, e.sp, "newpage").Title != "New" {
		t.Errorf("accept a new page: %d", rec.Code)
	}
	n2 := propID(t, e.do("PUT", "/main/later", "# Later\n", e.mdT, e.sug))
	e.do("PUT", "/main/later", "# Someone was faster\n", e.mdT, e.ed)
	if rec := e.do("POST", "/main/_proposals/"+itoa(n2)+"/accept", "{}", jsonT, e.ed); rec.Code != 409 {
		t.Errorf("a new page that exists meanwhile: %d", rec.Code)
	}
	if mustPage(t, e.sp, "later").Body != "# Someone was faster\n" {
		t.Error("an accepted suggestion overwrote a page")
	}
}

func TestSuggestionPagesRenderAndAdvertise(t *testing.T) {
	e := newPropEnv(t)
	e.do("PUT", "/main/doc", "# Doc\n", e.mdT, e.ed)
	id := propID(t, e.do("PUT", "/main/doc", "# Doc\n\n<script>alert(1)</script> more\n", e.mdT, e.sug))
	j := e.login("ed")
	for _, p := range []string{"/main/_proposals", "/main/_proposals?status=all", "/main/_proposals/" + itoa(id), "/main/"} {
		rec := e.do("GET", p, "", header("Accept", "text/html"), withJar(j))
		if rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "<script>alert(1)") {
			t.Errorf("%s: the suggested text is not escaped", p)
		}
	}
	if rec := e.do("GET", "/main/", "", header("Accept", "text/html"), withJar(j)); !strings.Contains(rec.Body.String(), "1 to review") {
		t.Error("reviewers are not told how many suggestions wait")
	}
	// A suggester gets the editor, labelled as a suggestion, and the right headers.
	sj := e.login("sug")
	rec := e.do("GET", "/main/doc?edit", "", header("Accept", "text/html"), withJar(sj))
	if rec.Code != 200 {
		t.Errorf("editor for a suggester: %d", rec.Code)
	}
	rec = e.do("GET", "/main/doc", "", header("Accept", "text/html"), withJar(sj))
	if !strings.Contains(rec.Body.String(), "Suggest an edit") || strings.Contains(rec.Body.String(), "Delete") {
		t.Error("a suggester's page actions are wrong")
	}
	rec = e.do("OPTIONS", "/main/doc", "", e.sug)
	if rec.Header().Get("Wako-Mode") != "suggest" || !strings.Contains(rec.Header().Get("Allow"), "PUT") || strings.Contains(rec.Header().Get("Allow"), "DELETE") {
		t.Errorf("headers for a suggester: %v", rec.Header())
	}
	if rec := e.do("OPTIONS", "/main/doc", "", e.ed); rec.Header().Get("Wako-Mode") != "" {
		t.Error("an editor is told they only suggest")
	}
	// A suggester can use the cookie session too: a PUT with the CSRF token queues.
	if rec := e.do("PUT", "/main/doc", "# Doc\n\nfrom the browser\n", e.mdT, withJar(sj), withCSRF(sj)); rec.Code != 202 {
		t.Errorf("browser suggestion: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSuggestionQueueIsBounded(t *testing.T) {
	e := newPropEnv(t)
	for i := 0; i < store.MaxPendingProposals; i++ {
		if _, err := e.sp.CreateProposal(store.Proposal{Path: "p", Title: "p", Body: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if rec := e.do("PUT", "/main/more", "# More\n", e.mdT, e.sug); rec.Code != 429 {
		t.Errorf("a full queue: %d %s", rec.Code, rec.Body.String())
	}
}

func mustPage(t *testing.T, sp *store.Space, path string) *store.Page {
	t.Helper()
	p, err := sp.GetPage(path)
	if err != nil {
		t.Fatalf("page %s: %v", path, err)
	}
	return p
}

func TestSuggestionTextStaysTextOnReviewPages(t *testing.T) {
	e := newPropEnv(t)
	evil := `"><img src=x onerror=alert(1)><script>alert(2)</script>`
	e.do("PUT", "/main/doc", "# Doc\n", e.mdT, e.ed)
	e.sp.CreateProposal(store.Proposal{Path: "doc", Title: evil, Body: "# " + evil + "\n\n" + evil + "\n", BaseVersion: 1, BaseBody: "# Doc\n", Author: evil, Source: "api", Note: evil})
	e.sp.CreateProposal(store.Proposal{Path: "new", Title: evil, Body: evil, Author: "x", Source: "ai:" + "m", Note: evil})
	j := e.login("ed")
	for _, p := range []string{"/main/_proposals", "/main/_proposals?status=all", "/main/_proposals/1", "/main/_proposals/2", "/main/"} {
		rec := e.do("GET", p, "", header("Accept", "text/html"), withJar(j))
		if rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
			continue
		}
		doc, err := xhtml.Parse(rec.Body)
		if err != nil {
			t.Fatal(err)
		}
		var walk func(n *xhtml.Node)
		walk = func(n *xhtml.Node) {
			if n.Type == xhtml.ElementNode {
				if n.Data == "script" && !hasAttr(n, "src") {
					t.Errorf("%s: inline script", p)
				}
				if n.Data == "img" && attr(n, "src") == "x" {
					t.Errorf("%s: injected <img>", p)
				}
				for _, a := range n.Attr {
					if strings.HasPrefix(strings.ToLower(a.Key), "on") {
						t.Errorf("%s: event handler %s on <%s>", p, a.Key, n.Data)
					}
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		walk(doc)
	}
	// The text is in the page, as text.
	rec := e.do("GET", "/main/_proposals/1", "", header("Accept", "text/html"), withJar(j))
	if !strings.Contains(rec.Body.String(), "&lt;script&gt;alert(2)") {
		t.Error("the suggested text is not shown")
	}
}
