package server

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
	"testing"
	"time"
)

// What a feed reader sees.
type rssFeed struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	Channel struct {
		Title string `xml:"title"`
		// Atom's <link> must be matched before the plain one, or it would be read as the channel's link.
		Self struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
		} `xml:"http://www.w3.org/2005/Atom link"`
		Link          string `xml:"link"`
		Description   string `xml:"description"`
		LastBuildDate string `xml:"lastBuildDate"`
		Items         []struct {
			Title string `xml:"title"`
			Link  string `xml:"link"`
			GUID  struct {
				Value     string `xml:",chardata"`
				PermaLink string `xml:"isPermaLink,attr"`
			} `xml:"guid"`
			PubDate     string `xml:"pubDate"`
			Creator     string `xml:"http://purl.org/dc/elements/1.1/ creator"`
			Category    string `xml:"category"`
			Description string `xml:"description"`
		} `xml:"item"`
	} `xml:"channel"`
}

func parseFeed(t *testing.T, body string) rssFeed {
	t.Helper()
	var f rssFeed
	if err := xml.Unmarshal([]byte(body), &f); err != nil {
		t.Fatalf("the feed is not valid XML: %v\n%s", err, body)
	}
	if f.Version != "2.0" {
		t.Fatalf("version %q", f.Version)
	}
	return f
}

func titles(f rssFeed) string {
	var s []string
	for _, it := range f.Channel.Items {
		s = append(s, it.Title)
	}
	return strings.Join(s, " | ")
}

func TestFeedContent(t *testing.T) {
	a := newAuthEnv(t)
	a.st.RevisionWindow = 0
	a.user("alice", false)
	a.user("bob", false)
	sp := a.space("main", true)
	a.st.SetMember(sp.ID, a.users["alice"].ID, "editor")
	a.st.SetMember(sp.ID, a.users["bob"].ID, "editor")
	alice, bob := bearer(a.token("alice")), bearer(a.token("bob"))
	mdT := header("Content-Type", "text/markdown")
	tick := func() { a.now = a.now.Add(time.Minute); a.st.Now = a.clock }
	a.st.Now = a.clock
	do := func(method, path, body string, o ...reqOpt) {
		t.Helper()
		if rec := a.do(method, path, body, o...); rec.Code >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		tick()
	}

	do("PUT", "/main/docs", "# Docs\n\nWelcome to the documentation.\n", mdT, alice)
	do("PUT", "/main/docs/ops", "# Ops & <Runbooks>\n\nHow we run things.\n", mdT, bob)
	do("PUT", "/main/docs", "# Docs\n\nWelcome to the documentation.\n\nA new paragraph with <b>markup</b>.\n", mdT, bob)
	do("POST", "/main/docs/ops?move=ops", "", alice)
	do("POST", "/main/docs?restore=1", "", alice)
	do("DELETE", "/main/ops", "", bob)

	rec := a.do("GET", "/main/_feed", "")
	body := status(t, rec, 200)
	if ct := rec.Header().Get("Content-Type"); ct != "application/rss+xml; charset=utf-8" {
		t.Errorf("content type %q", ct)
	}
	if !strings.HasPrefix(body, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>") {
		t.Errorf("no XML declaration: %.60s", body)
	}
	f := parseFeed(t, body)
	ch := f.Channel
	if ch.Title != "Main — recent changes" || ch.Link != "http://localhost:8888/main/" || ch.Self.Href != "http://localhost:8888/main/_feed" || ch.Self.Rel != "self" || ch.Description == "" {
		t.Errorf("channel: %+v", ch)
	}
	want := []string{
		"Ops & <Runbooks> — deleted by bob",
		"Docs — restored by alice",
		"Ops & <Runbooks> — moved by alice",
		"Docs — edited by bob",
		"Ops & <Runbooks> — new page by bob",
		"Docs — new page by alice",
	}
	if got := titles(f); got != strings.Join(want, " | ") {
		t.Fatalf("items:\n got %s\nwant %s", got, strings.Join(want, " | "))
	}
	items := ch.Items
	// Dates are valid and newest first; the channel is as new as its newest item.
	var last time.Time
	for i, it := range items {
		pub, err := time.Parse(time.RFC1123Z, it.PubDate)
		if err != nil {
			t.Fatalf("pubDate %q: %v", it.PubDate, err)
		}
		if i > 0 && pub.After(last) {
			t.Errorf("not newest first at %d", i)
		}
		last = pub
	}
	if ch.LastBuildDate != items[0].PubDate {
		t.Errorf("lastBuildDate %q vs %q", ch.LastBuildDate, items[0].PubDate)
	}
	// Ids are unique and not URLs; categories say what happened; authors are credited.
	seen := map[string]bool{}
	for _, it := range items {
		if seen[it.GUID.Value] || !strings.HasPrefix(it.GUID.Value, "wako:main:") || it.GUID.PermaLink != "false" {
			t.Errorf("guid %q (permalink=%q)", it.GUID.Value, it.GUID.PermaLink)
		}
		seen[it.GUID.Value] = true
	}
	for i, cat := range []string{"deleted", "restored", "moved", "edited", "created", "created"} {
		if items[i].Category != cat {
			t.Errorf("item %d category %q, want %q", i, items[i].Category, cat)
		}
	}
	for i, who := range []string{"bob", "alice", "alice", "bob", "bob", "alice"} {
		if items[i].Creator != who {
			t.Errorf("item %d creator %q, want %q", i, items[i].Creator, who)
		}
	}
	// Links are absolute and lead to the page; a deleted page leads to the space.
	if items[2].Link != "http://localhost:8888/main/ops" || items[1].Link != "http://localhost:8888/main/docs" || items[0].Link != "http://localhost:8888/main/" {
		t.Errorf("links: %q %q %q", items[2].Link, items[1].Link, items[0].Link)
	}
	// Descriptions are HTML and show what changed.
	edit := items[3].Description
	for _, w := range []string{"<strong>bob</strong> edited", `<a href="http://localhost:8888/main/docs">Docs</a>`, "<pre>", "+A new paragraph with &lt;b&gt;markup&lt;/b&gt;."} {
		if !strings.Contains(edit, w) {
			t.Errorf("edit description lacks %q:\n%s", w, edit)
		}
	}
	if strings.Contains(edit, "<b>markup</b>") || strings.Contains(edit, "--- before") {
		t.Errorf("edit description: unescaped markup or file headers:\n%s", edit)
	}
	if c := items[5].Description; !strings.Contains(c, "created") || !strings.Contains(c, "<blockquote>Docs Welcome to the documentation.</blockquote>") {
		t.Errorf("created description: %s", c)
	}
	if m := items[2].Description; !strings.Contains(m, "moved") || !strings.Contains(m, "from <code>/docs/ops</code>") {
		t.Errorf("moved description: %s", m)
	}
	if d := items[0].Description; !strings.Contains(d, "deleted <code>/ops</code>") || strings.Contains(d, "<pre>") {
		t.Errorf("deleted description: %s", d)
	}
	if r := items[1].Description; !strings.Contains(r, "restored an earlier revision") || !strings.Contains(r, "restored revision 1") {
		t.Errorf("restored description: %s", r)
	}
	// Titles with & and < survive the XML round trip.
	if !strings.Contains(body, "Ops &amp; &lt;Runbooks&gt;") {
		t.Errorf("title not escaped in the XML")
	}
}

func TestFeedMergesBurstsAndHonoursLimits(t *testing.T) {
	e := newEnv(t) // default 10 minute window
	for i := 0; i < 5; i++ {
		expect(t, e.do("PUT", "/main/p", fmt.Sprintf("# P\n\nrevision %d", i), "Content-Type", md), map[bool]int{true: 201, false: 200}[i == 0])
	}
	f := parseFeed(t, expect(t, e.do("GET", "/main/_feed", ""), 200))
	if len(f.Channel.Items) != 1 || f.Channel.Items[0].Category != "created" {
		t.Fatalf("a burst of saves is one entry: %s", titles(f))
	}
	for i := 0; i < 70; i++ {
		expect(t, e.do("PUT", fmt.Sprintf("/main/page%d", i), "# Page", "Content-Type", md), 201)
	}
	count := func(q string) int {
		return len(parseFeed(t, expect(t, e.do("GET", "/main/_feed"+q, ""), 200)).Channel.Items)
	}
	for q, want := range map[string]int{"": 50, "?limit=5": 5, "?limit=0": 50, "?limit=abc": 50, "?limit=-3": 50, "?limit=500": 71, "?limit=71": 71} {
		if got := count(q); got != want {
			t.Errorf("limit %q: %d items, want %d", q, got, want)
		}
	}
}

func TestFeedCachingAndMethods(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/p", "# P", "Content-Type", md), 201)
	rec := e.do("GET", "/main/_feed", "")
	expect(t, rec, 200)
	tag := rec.Header().Get("ETag")
	if tag == "" || rec.Header().Get("Last-Modified") == "" || !strings.Contains(rec.Header().Get("Cache-Control"), "max-age") || !strings.HasPrefix(rec.Header().Get("Cache-Control"), "public") {
		t.Errorf("headers: %v", rec.Header())
	}
	expect(t, e.do("GET", "/main/_feed", "", "If-None-Match", tag), 304)
	expect(t, e.do("HEAD", "/main/_feed", ""), 200)
	expect(t, e.do("PUT", "/main/q", "# Q", "Content-Type", md), 201)
	if rec := e.do("GET", "/main/_feed", "", "If-None-Match", tag); rec.Code != 200 || rec.Header().Get("ETag") == tag {
		t.Errorf("a new change must give a new ETag: %d", rec.Code)
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		if rec := e.do(m, "/main/_feed", "x"); rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d %q", m, rec.Code, rec.Header().Get("Allow"))
		}
	}
	expect(t, e.do("GET", "/nospace/_feed", ""), 404)
	// An empty space has a valid, empty feed.
	empty := parseFeed(t, expect(t, e.do("GET", "/other/_feed", ""), 200))
	if len(empty.Channel.Items) != 0 || empty.Channel.LastBuildDate != "" {
		t.Errorf("empty feed: %+v", empty.Channel)
	}
}

func TestFeedLinksUseTheRightOrigin(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do("PUT", "/main/p", "# P", "Content-Type", md), 201)
	link := func(hdr ...string) string {
		f := parseFeed(t, expect(t, e.do("GET", "/main/_feed", "", hdr...), 200))
		return f.Channel.Items[0].Link
	}
	if got := link(); got != "http://localhost:8888/main/p" {
		t.Errorf("default origin: %q", got)
	}
	if got := link("X-Forwarded-Proto", "https"); got != "https://localhost:8888/main/p" {
		t.Errorf("behind a TLS proxy: %q", got)
	}
	e.h.(*Server).PublicURL = "https://wiki.example.org/"
	if got := link("X-Forwarded-Proto", "http"); got != "https://wiki.example.org/main/p" {
		t.Errorf("--public-url: %q", got)
	}
	// With a base path, links carry it.
	srv := New(e.st, "/wiki")
	e.h = srv
	f := parseFeed(t, expect(t, e.do("GET", "/wiki/main/_feed", ""), 200))
	if f.Channel.Items[0].Link != "http://localhost:8888/wiki/main/p" || f.Channel.Self.Href != "http://localhost:8888/wiki/main/_feed" {
		t.Errorf("base path: %q %q", f.Channel.Items[0].Link, f.Channel.Self.Href)
	}
}

func TestFeedAcrossSpacesAndInTheUI(t *testing.T) {
	e := newEnv(t)
	clock := time.Unix(1_800_000_000, 0)
	e.st.Now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	expect(t, e.do("PUT", "/main/a", "# Alpha", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/other/b", "# Beta", "Content-Type", md), 201)
	f := parseFeed(t, expect(t, e.do("GET", "/_feed", ""), 200))
	if f.Channel.Title != "Wako — recent changes" || titles(f) != "[other] Beta — new page | [main] Alpha — new page" {
		t.Errorf("global feed: %q: %s", f.Channel.Title, titles(f))
	}
	if g := f.Channel.Items[0].GUID.Value; !strings.HasPrefix(g, "wako:other:") {
		t.Errorf("guid %q", g)
	}
	// Browsers find the feed by itself, and people find it on the page.
	for path, feed := range map[string]string{"/main/a": "/main/_feed", "/main/": "/main/_feed", "/": "/_feed", "/other/b?history": "/other/_feed"} {
		page := expect(t, e.do("GET", path, "", "Accept", "text/html"), 200)
		want := `<link rel="alternate" type="application/rss+xml" title=`
		if !strings.Contains(page, want) || !strings.Contains(page, `href="`+feed+`"`) {
			t.Errorf("%s: no feed link to %s", path, feed)
		}
	}
	if root := expect(t, e.do("GET", "/main/", "", "Accept", "text/html"), 200); !strings.Contains(root, `href="?changes"`) || !strings.Contains(root, ">RSS</a>") {
		t.Error("space home does not offer the changes page and the feed")
	}
	page := expect(t, e.do("GET", "/main/?changes", "", "Accept", "text/html"), 200)
	for _, w := range []string{"Recent changes", "Alpha", "Page created", `href="/main/_feed"`} {
		if !strings.Contains(page, w) {
			t.Errorf("changes page lacks %q", w)
		}
	}
	if strings.Contains(page, "Beta") {
		t.Error("changes page shows another space")
	}
	// The same data as JSON.
	var list []changeJSON
	json.Unmarshal([]byte(expect(t, e.do("GET", "/_api/changes", ""), 200)), &list)
	if len(list) != 2 || list[0].Space != "other" || list[0].Action != "created" || list[0].URL != "/other/b" {
		t.Errorf("/_api/changes: %+v", list)
	}
	json.Unmarshal([]byte(expect(t, e.do("GET", "/_api/spaces/main/changes?limit=1", ""), 200)), &list)
	if len(list) != 1 || list[0].Space != "main" || list[0].Path != "a" {
		t.Errorf("space changes: %+v", list)
	}
	expect(t, e.do("GET", "/_api/spaces/nospace/changes", ""), 404)
}

func TestFeedAccess(t *testing.T) {
	a := newAuthEnv(t)
	a.user("root", true)
	a.user("member", false)
	a.user("outsider", false)
	pub, priv := a.space("pub", true), a.space("priv", false)
	a.st.SetMember(priv.ID, a.users["member"].ID, "viewer")
	_ = pub
	root := bearer(a.token("root"))
	mdT := header("Content-Type", "text/markdown")
	a.do("PUT", "/pub/open", "# Open page", mdT, root)
	a.do("PUT", "/priv/secret", "# Secret page", mdT, root)
	memberTok, outsiderTok := a.token("member"), a.token("outsider")
	basic := func(user, pass string) reqOpt {
		return header("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	}

	// Public spaces: anyone, no credentials. Private ones ask for them, and say the same about spaces that do not exist.
	status(t, a.do("GET", "/pub/_feed", ""), 200)
	for _, p := range []string{"/priv/_feed", "/nospace/_feed"} {
		rec := a.do("GET", p, "")
		if rec.Code != 401 || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), `Basic realm="wako feed"`) {
			t.Errorf("anonymous %s: %d %q", p, rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
	}
	// A feed reader sends Basic credentials with the API token as the password.
	f := parseFeed(t, status(t, a.do("GET", "/priv/_feed", "", basic("anything", memberTok)), 200))
	if titles(f) != "Secret page — new page by root" {
		t.Errorf("member's feed: %s", titles(f))
	}
	if rec := a.do("GET", "/priv/_feed", "", basic("", memberTok)); rec.Code != 200 {
		t.Errorf("empty user name: %d", rec.Code)
	}
	status(t, a.do("GET", "/priv/_feed", "", bearer(memberTok)), 200)
	if rec := a.do("GET", "/priv/_feed", "", basic("x", "wako_not-a-token")); rec.Code != 401 || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Errorf("bad token: %d", rec.Code)
	}
	// Signed in without access: the space does not exist for them.
	status(t, a.do("GET", "/priv/_feed", "", basic("x", outsiderTok)), 404)
	status(t, a.do("GET", "/nospace/_feed", "", basic("x", outsiderTok)), 404)
	// A reverse proxy's own Basic credentials are none of our business.
	status(t, a.do("GET", "/pub/_feed", "", basic("proxyuser", "proxy-secret")), 200)
	status(t, a.do("GET", "/priv/_feed", "", basic("proxyuser", "proxy-secret")), 401)
	// Basic with a token is accepted only on feeds, and only for reading.
	status(t, a.do("GET", "/priv/secret.md", "", basic("x", memberTok)), 404) // ignored: anonymous
	status(t, a.do("PUT", "/pub/new", "x", mdT, basic("x", a.token("root"))), 401)

	// The all-spaces feed shows each reader what they may read.
	feedFor := func(o ...reqOpt) string { return titles(parseFeed(t, status(t, a.do("GET", "/_feed", "", o...), 200))) }
	if got := feedFor(); strings.Contains(got, "Secret") || !strings.Contains(got, "Open page") {
		t.Errorf("anonymous global feed: %s", got)
	}
	if got := feedFor(basic("x", memberTok)); !strings.Contains(got, "Secret") || !strings.Contains(got, "Open page") {
		t.Errorf("member's global feed: %s", got)
	}
	if got := feedFor(basic("x", outsiderTok)); strings.Contains(got, "Secret") {
		t.Errorf("outsider's global feed: %s", got)
	}
	// Caching: shared for public feeds, private once credentials are involved.
	if cc := a.do("GET", "/pub/_feed", "").Header().Get("Cache-Control"); !strings.HasPrefix(cc, "public") {
		t.Errorf("public feed: %q", cc)
	}
	if cc := a.do("GET", "/priv/_feed", "", basic("x", memberTok)).Header().Get("Cache-Control"); !strings.HasPrefix(cc, "private") {
		t.Errorf("private feed: %q", cc)
	}
	if cc := a.do("GET", "/pub/_feed", "", basic("x", memberTok)).Header().Get("Cache-Control"); !strings.HasPrefix(cc, "private") {
		t.Errorf("personalised feed: %q", cc)
	}
	// The JSON list follows the same rules.
	for _, c := range []struct {
		path string
		opts []reqOpt
		want int
	}{{"/_api/spaces/priv/changes", nil, 404}, {"/_api/spaces/priv/changes", []reqOpt{bearer(memberTok)}, 200}, {"/_api/spaces/pub/changes", nil, 200}} {
		if rec := a.do("GET", c.path, "", c.opts...); rec.Code != c.want {
			t.Errorf("%s: %d, want %d", c.path, rec.Code, c.want)
		}
	}
}

func TestAuthorsAndPermissionsOfHistory(t *testing.T) {
	a := newAuthEnv(t)
	a.st.RevisionWindow = 0
	a.user("root", true)
	a.user("ed", false)
	a.user("reader", false)
	pub, priv := a.space("pub", true), a.space("priv", false)
	a.st.SetMember(pub.ID, a.users["ed"].ID, "editor")
	a.st.SetMember(pub.ID, a.users["reader"].ID, "viewer")
	a.st.SetMember(priv.ID, a.users["reader"].ID, "viewer")
	ed, reader, root := bearer(a.token("ed")), bearer(a.token("reader")), bearer(a.token("root"))
	mdT := header("Content-Type", "text/markdown")

	a.do("PUT", "/pub/p", "one\n", mdT, ed)
	a.do("PUT", "/pub/p", "two\n", mdT, root)
	a.do("PUT", "/priv/p", "private\n", mdT, root)

	var revs []revJSON
	json.Unmarshal([]byte(status(t, a.do("GET", "/pub/p?history", "", header("Accept", "application/json")), 200)), &revs)
	if len(revs) != 2 || revs[0].Author != "root" || revs[1].Author != "ed" {
		t.Errorf("authors: %+v", revs)
	}
	// Anyone who can read the page can see its history, revisions and diffs...
	for _, q := range []string{"?history", "?rev=1", "?diff=2", ".md?rev=1"} {
		status(t, a.do("GET", "/pub/p"+q, "", header("Accept", "text/html")), map[bool]int{true: 200}[true])
	}
	// ...and a private space's are as hidden as the space.
	for _, c := range []struct {
		opts []reqOpt
		want int
	}{{nil, 404}, {[]reqOpt{ed}, 404}, {[]reqOpt{reader}, 200}} {
		for _, q := range []string{"?history", "?rev=1", "?diff=1"} {
			if rec := a.do("GET", "/priv/p"+q, "", append([]reqOpt{header("Accept", "text/html")}, c.opts...)...); rec.Code != c.want {
				t.Errorf("/priv/p%s: %d, want %d", q, rec.Code, c.want)
			}
		}
		if rec := a.do("GET", "/_api/spaces/priv/pages/p/revisions", "", c.opts...); rec.Code != c.want {
			t.Errorf("revisions API: %d, want %d", rec.Code, c.want)
		}
	}
	// Restoring and patching are writes.
	patch := "@@ -1 +1 @@\n-two\n+three\n"
	for _, c := range []struct {
		name string
		opts []reqOpt
		want int
	}{{"anonymous", nil, 401}, {"viewer", []reqOpt{reader}, 403}, {"editor", []reqOpt{ed}, 200}} {
		if rec := a.do("POST", "/pub/p?restore=1", "", c.opts...); rec.Code != c.want {
			t.Errorf("%s restoring: %d, want %d", c.name, rec.Code, c.want)
		}
		if rec := a.do("PATCH", "/pub/p", patch, append([]reqOpt{header("Content-Type", "text/x-diff"), header("If-Match", `"v99"`)}, c.opts...)...); (rec.Code == 412) != (c.want == 200) || (c.want != 200 && rec.Code != c.want) {
			t.Errorf("%s patching: %d (want %d, or 412 once allowed)", c.name, rec.Code, c.want)
		}
	}
	// The restore was recorded under the editor's name.
	json.Unmarshal([]byte(status(t, a.do("GET", "/pub/p?history", "", header("Accept", "application/json")), 200)), &revs)
	if revs[0].Author != "ed" || revs[0].Action != "restored" {
		t.Errorf("restore author: %+v", revs[0])
	}
	// A move or a rename is credited to whoever did it.
	a.do("PUT", "/pub/linker", "see [p](p)", mdT, ed)
	a.do("POST", "/pub/p?move=q", "", header("X-Dummy", "1"), root)
	var changes []changeJSON
	json.Unmarshal([]byte(status(t, a.do("GET", "/_api/spaces/pub/changes", ""), 200)), &changes)
	if changes[0].Action != "edited" || changes[0].Author != "root" || !strings.Contains(changes[0].Note, "moving /p to /q") ||
		changes[1].Action != "moved" || changes[1].Author != "root" || changes[1].OldPath != "p" {
		t.Errorf("changes after a move: %+v", changes[:2])
	}
}

// With sign-in off, or for changes older than the history, nobody knows who
// made a change. The feed then says what happened, not that "someone" did it.
func TestFeedWithoutAnAuthor(t *testing.T) {
	e := newEnv(t) // sign-in off: no author is recorded
	e.st.RevisionWindow = 0
	expect(t, e.do("PUT", "/main/p", "# P\n\nfirst", "Content-Type", md), 201)
	expect(t, e.do("PUT", "/main/p", "# P\n\nsecond", "Content-Type", md), 200)
	expect(t, e.do("POST", "/main/p?move=q", ""), 200)
	expect(t, e.do("DELETE", "/main/q", ""), 204)
	body := expect(t, e.do("GET", "/main/_feed", ""), 200)
	f := parseFeed(t, body)
	if got := titles(f); got != "P — deleted | P — moved | P — edited | P — new page" {
		t.Fatalf("titles: %s", got)
	}
	for i, want := range []string{"<p>Deleted <code>/q</code>.", "<p>Moved <a ", "<p>Edited <a ", "<p>Created <a "} {
		d := f.Channel.Items[i].Description
		if !strings.HasPrefix(d, want) || strings.Contains(d, "Someone") || strings.Contains(d, "<strong>") {
			t.Errorf("entry %d: %q does not start with %q", i, d, want)
		}
		if f.Channel.Items[i].Creator != "" {
			t.Errorf("entry %d has a creator: %q", i, f.Channel.Items[i].Creator)
		}
	}
	page := expect(t, e.do("GET", "/main/?changes", "", "Accept", "text/html"), 200)
	for _, want := range []string{"Page edited", "Page created", "Page moved from /p to /q", "Page deleted /q"} {
		if !strings.Contains(page, want) {
			t.Errorf("changes page lacks %q", want)
		}
	}
	if strings.Contains(page, "Someone") {
		t.Error("changes page still says Someone")
	}
}
