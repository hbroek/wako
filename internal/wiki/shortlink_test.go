package wiki

import (
	"regexp"
	"testing"
)

func TestExpandShortLinks(t *testing.T) {
	ix := NewIndex([]Page{
		{"setup", "Setup Guide"}, {"docs", "Docs"}, {"docs/install", "Install"}, {"docs/setup", "Setup"},
	})
	ok := regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`).MatchString
	for _, c := range []struct{ src, in, want string }{
		{"notes", "See [[new page]].", "See [new page](new-page)."},
		{"docs/intro", "[[new page]]", "[new page](new-page)"},
		{"docs/intro", "[[Install]]", "[Install](install)"},
		{"notes", "[[install]]", "[install](docs/install)"},
		{"docs/intro", "[[setup]]", "[setup](setup)"},        // the sibling docs/setup beats /setup
		{"notes", "[[setup guide]]", "[setup guide](setup)"}, // by title
		{"notes", "[[Setup Guide|the guide]]", "[the guide](setup)"},
		{"notes", "[[docs/Install]]", "[docs/Install](docs/install)"},
		{"a/b/c", "[[docs/new one]]", "[docs/new one](../../docs/new-one)"},
		{"notes", "`[[code]]` and [[x y]]", "`[[code]]` and [x y](x-y)"},
		{"notes", "```\n[[fenced]]\n```\n[[after]]", "```\n[[fenced]]\n```\n[after](after)"},
		{"notes", `\[[escaped]] ![[image]]`, `\[[escaped]] ![[image]]`},
		{"notes", "[[a.md]] [[!!]] [[open", "[[a.md]] [[!!]] [[open"},
		{"notes", "[[one]] and [[two]]", "[one](one) and [two](two)"},
	} {
		if got, _ := ExpandShortLinks(c.src, c.in, ix, func(s string) bool { return ok(s) && s[len(s)-3:] != ".md" }); got != c.want {
			t.Errorf("%s: %q\n got %q\nwant %q", c.src, c.in, got, c.want)
		}
	}
}

func TestExpandShortLinksReportsNewPages(t *testing.T) {
	ix := NewIndex([]Page{{"docs", "Docs"}})
	valid := func(s string) bool { return s != "" }
	_, stubs := ExpandShortLinks("docs", "[[Docs]] [[New Thing]] [[new thing]] [[docs/Deep one]] [[docs]]", ix, valid)
	want := []Stub{{"new-thing", "New Thing"}, {"docs/deep-one", "Deep one"}}
	if len(stubs) != len(want) || stubs[0] != want[0] || stubs[1] != want[1] {
		t.Errorf("stubs %v, want %v", stubs, want)
	}
}
