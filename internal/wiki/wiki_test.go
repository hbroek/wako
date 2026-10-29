package wiki

import (
	"strings"
	"testing"
)

func testEnv(spaces map[string][]Page, cur string) *Env {
	idx := map[string]*Index{}
	for s, pages := range spaces {
		idx[s] = NewIndex(pages)
	}
	return &Env{
		Space: cur, Base: "",
		HasSpace: func(s string) bool { _, ok := idx[s]; return ok },
		Index:    func(s string) *Index { return idx[s] },
	}
}

var site = map[string][]Page{
	"main": {
		{"docs", "Documentation"}, {"docs/ops", "Operations"}, {"docs/ops/runbook", "Runbook"},
		{"docs/dev", "Dev"}, {"docs/dev/guide", "Guide"}, {"faq", "Questions"}, {"Mixed-Case", "Mixed"},
	},
	"team": {{"notes", "Team Notes"}},
}

func TestLinkResolution(t *testing.T) {
	e := testEnv(site, "main")
	type res struct {
		ref              Ref
		internal, exists bool
	}
	for _, c := range []struct {
		src, dest string
		want      res
	}{
		{"docs/ops", "dev/guide", res{Ref{"main", "docs/dev/guide"}, true, true}}, // next to this page, in docs/
		{"docs/ops", "../faq", res{Ref{"main", "faq"}, true, true}},
		{"docs/ops", "../faq.md", res{Ref{"main", "faq"}, true, true}},
		{"docs/ops", "../faq#top", res{Ref{"main", "faq"}, true, true}},
		{"docs/ops", "../faq?x=1", res{Ref{"main", "faq"}, true, true}},
		{"docs/ops/runbook", "../ops", res{Ref{"main", "docs/ops"}, true, true}},
		{"docs/ops", "ops/runbook", res{Ref{"main", "docs/ops/runbook"}, true, true}},
		{"faq", "docs", res{Ref{"main", "docs"}, true, true}},
		{"faq", "docs/ops", res{Ref{"main", "docs/ops"}, true, true}},
		{"faq", "./docs/ops/", res{Ref{"main", "docs/ops"}, true, true}},
		{"faq", "nothing", res{Ref{"main", "nothing"}, true, false}},
		{"docs/ops", "/main/faq", res{Ref{"main", "faq"}, true, true}},
		{"docs/ops", "/team/notes", res{Ref{"team", "notes"}, true, true}},
		{"docs/ops", "/team/", res{}},
		{"docs/ops", "/elsewhere/page", res{}},
		{"docs/ops", "/_ui/app.css", res{}},
		{"docs/ops", "/main/_assets/abc/x.png", res{}},
		{"docs/ops", "https://example.com/faq", res{}},
		{"docs/ops", "mailto:a@b.c", res{}},
		{"docs/ops", "//example.com/x", res{}},
		{"docs/ops", "#section", res{}},
		{"docs/ops", "", res{}},
		{"faq", "../outside", res{}}, // leaves the space
		{"docs/ops", "%zz", res{}},
		{"docs/ops", "../Mixed-Case", res{Ref{"main", "Mixed-Case"}, true, true}},
	} {
		ref, internal, exists := e.Link(c.src, c.dest)
		if (res{ref, internal, exists}) != c.want {
			t.Errorf("Link(%q, %q) = %+v %v %v; want %+v", c.src, c.dest, ref, internal, exists, c.want)
		}
	}
	// A space the reader cannot see is "not there".
	e.Index = func(s string) *Index {
		if s == "team" {
			return nil
		}
		return NewIndex(site[s])
	}
	if _, internal, exists := e.Link("faq", "/team/notes"); !internal || exists {
		t.Errorf("unreadable space: internal=%v exists=%v", internal, exists)
	}
	// With a base path, absolute links carry it.
	e = testEnv(site, "main")
	e.Base = "/wiki"
	if ref, internal, exists := e.Link("faq", "/wiki/main/docs"); ref != (Ref{"main", "docs"}) || !internal || !exists {
		t.Errorf("base path: %+v %v %v", ref, internal, exists)
	}
	if _, internal, _ := e.Link("faq", "/main/docs"); internal {
		t.Error("link outside the base path treated as internal")
	}
}

func TestRelPath(t *testing.T) {
	for _, c := range []struct{ from, to, want string }{
		{"", "faq", "faq"},
		{"", "docs/ops", "docs/ops"},
		{"docs", "docs/ops", "ops"},
		{"docs", "docs", "../docs"},
		{"docs/ops", "docs/dev/guide", "../dev/guide"},
		{"docs/ops", "docs", "../../docs"},
		{"docs/ops", "faq", "../../faq"},
		{"a/b/c", "a/x", "../../x"},
	} {
		if got := relPath(c.from, c.to); got != c.want {
			t.Errorf("relPath(%q, %q) = %q, want %q", c.from, c.to, got, c.want)
		}
	}
}

// moveMapper models moving the subtree at from to to within one space.
func moveMapper(space, from, to string) Mapper {
	return func(s, p string) (string, string) {
		if s == space && (p == from || strings.HasPrefix(p, from+"/")) {
			return s, to + p[len(from):]
		}
		return s, p
	}
}

func TestRewriteOnMove(t *testing.T) {
	// docs/ops (and its child runbook) move to ops.
	e := testEnv(site, "main")
	m := moveMapper("main", "docs/ops", "ops")
	for _, c := range []struct{ name, src, body, want string }{
		{"relative link", "faq", "[o](docs/ops) [r](docs/ops/runbook.md)", "[o](ops) [r](ops/runbook.md)"},
		{"absolute link", "faq", "[o](/main/docs/ops \"T\") [x](/main/faq)", "[o](/main/ops \"T\") [x](/main/faq)"},
		{"fragment and query kept", "faq", "[o](docs/ops#setup) [p](docs/ops?x=1#y)", "[o](ops#setup) [p](ops?x=1#y)"},
		{"angle brackets", "faq", "[o](<docs/ops>)", "[o](<ops>)"},
		{"reference definition", "faq", "[o][r]\n\n[r]: docs/ops \"Ops\"\n", "[o][r]\n\n[r]: ops \"Ops\"\n"},
		{"unrelated links untouched", "faq", "[a](docs) [b](docs/dev/guide) [c](faq)", "[a](docs) [b](docs/dev/guide) [c](faq)"},
		{"images are not links", "faq", "![a](docs/ops)", "![a](docs/ops)"},
		{"external links", "faq", "[a](https://example.com/docs/ops) [b](mailto:docs/ops)", "[a](https://example.com/docs/ops) [b](mailto:docs/ops)"},
		{"double brackets are just text", "faq", "[[docs/ops]]", "[[docs/ops]]"},
		{"code is text", "faq", "[a](docs/ops)\n\n`[x](docs/ops)`\n\n```\n[y](docs/ops)\n```\n", "[a](ops)\n\n`[x](docs/ops)`\n\n```\n[y](docs/ops)\n```\n"},
		{"raw html is text", "faq", "<p>[x](docs/ops)</p>\n\n[a](docs/ops)\n", "<p>[x](docs/ops)</p>\n\n[a](ops)\n"},
		{"missing targets are left alone", "faq", "[x](docs/nope)", "[x](docs/nope)"},
		{"a page's own children move with it", "faq", "[r](docs/ops/runbook#steps)", "[r](ops/runbook#steps)"},
	} {
		got, changed := e.Rewrite(c.src, c.src, c.body, m)
		if got != c.want || changed != (got != c.body) {
			t.Errorf("%s:\n got %q (changed=%v)\nwant %q", c.name, got, changed, c.want)
		}
	}
}

func TestRewriteOutgoingLinksOfMovedPage(t *testing.T) {
	// The page docs/ops/runbook itself moves to ops/runbook, so its relative
	// links are recomputed from the new directory; absolute links do not
	// depend on where the page is.
	e := testEnv(site, "main")
	m := moveMapper("main", "docs/ops", "ops")
	body := "[up](../ops) [dev](../dev/guide) [home](../../faq) [abs](/main/faq) [ext](https://x.test/a)"
	got, changed := e.Rewrite("docs/ops/runbook", "ops/runbook", body, m)
	want := "[up](../ops) [dev](../docs/dev/guide) [home](../faq) [abs](/main/faq) [ext](https://x.test/a)"
	if !changed || got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	// A page that moves but whose links do not leave its subtree is untouched
	// when everything it links to moves with it.
	got, changed = e.Rewrite("docs/ops/runbook", "ops/runbook", "[self](../ops/runbook)", m)
	if changed || got != "[self](../ops/runbook)" {
		t.Errorf("sibling subtree link: %q changed=%v", got, changed)
	}
}

func TestRewriteSpaceRename(t *testing.T) {
	e := testEnv(site, "main")
	rename := func(s, p string) (string, string) {
		if s == "main" {
			return "wiki", p
		}
		return s, p
	}
	body := "[a](/main/docs) [b](docs/ops) [c](/team/notes)"
	got, changed := e.Rewrite("faq", "faq", body, rename)
	// Relative links name no space, so they stay.
	if want := "[a](/wiki/docs) [b](docs/ops) [c](/team/notes)"; !changed || got != want {
		t.Errorf("space rename:\n got %q\nwant %q", got, want)
	}
}

func TestRewriteWithBasePath(t *testing.T) {
	e := testEnv(site, "main")
	e.Base = "/wiki"
	got, _ := e.Rewrite("faq", "faq", "[o](/wiki/main/docs/ops) [x](/main/docs/ops)", moveMapper("main", "docs/ops", "ops"))
	if got != "[o](/wiki/main/ops) [x](/main/docs/ops)" {
		t.Errorf("base path: %q", got)
	}
}
