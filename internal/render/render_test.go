package render

import (
	"reflect"
	"strings"
	"testing"
)

func TestImageRefs(t *testing.T) {
	src := "![a](shot.png) text ![b](diagram-2.svg) ![again](shot.png)\n\n" +
		"![abs](/main/_assets/x/y.png) ![url](https://example.com/a.png) ![dir](sub/a.png) ![up](../a.png)\n\n" +
		"`![code](span.png)`\n\n```\n![fenced](fence.png)\n```\n\n[link](notimage.png)\n"
	want := []string{"shot.png", "diagram-2.svg"}
	if got := ImageRefs(src); !reflect.DeepEqual(got, want) {
		t.Fatalf("ImageRefs = %v, want %v", got, want)
	}
}

func TestHTMLWithResolver(t *testing.T) {
	resolve := func(name string, _ Size) (Image, bool) {
		switch name {
		case "shot.png":
			return Image{Src: "/main/_assets/abc/shot.png", Width: 640, Height: 480}, true
		case "big.jpg":
			return Image{Src: "/main/_assets/def/big.jpg?w=800", Link: "/main/_assets/def/big.jpg", Width: 800, Height: 400}, true
		}
		return Image{}, false
	}
	html, err := HTMLWith("![a](shot.png) ![b](missing.png) ![c](https://x.test/c.png) ![d](big.jpg)", resolve)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`src="/main/_assets/abc/shot.png"`, `width="640"`, `height="480"`, `style="--r:1.3333"`,
		`src="missing.png"`, `src="https://x.test/c.png"`,
		`<a href="/main/_assets/def/big.jpg"`, `src="/main/_assets/def/big.jpg?w=800"`, `style="--r:2.0000"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %s in %s", want, html)
		}
	}
	if strings.Count(html, `decoding="async"`) != 4 {
		t.Errorf("every image needs decoding=async: %s", html)
	}
	// Only the first image is eager.
	if strings.Count(html, `loading="lazy"`) != 3 || strings.Contains(strings.SplitN(html, "<img", 3)[1], "loading") {
		t.Errorf("lazy loading: %s", html)
	}
	if strings.Contains(strings.SplitN(html, "<img", 2)[0], "nofollow") {
		t.Errorf("internal links must not be nofollow: %s", html)
	}
	if ext, _ := HTMLWith("[x](https://example.com/)", nil); !strings.Contains(ext, `rel="nofollow"`) {
		t.Errorf("external links should stay nofollow: %s", ext)
	}
	// An image that is already a link is not wrapped a second time.
	html, _ = HTMLWith("[![d](big.jpg)](https://example.com/page)", resolve)
	if strings.Count(html, "<a ") != 1 || !strings.Contains(html, `href="https://example.com/page"`) {
		t.Errorf("nested link: %s", html)
	}
	// Images in code blocks are text, not images.
	if html, _ := HTMLWith("```\n![d](big.jpg)\n```", resolve); strings.Contains(html, "<img") {
		t.Errorf("code block rendered an image: %s", html)
	}
	// Nothing a resolver returns can smuggle script URLs or attributes past the sanitiser.
	evil := func(string, Size) (Image, bool) {
		return Image{Src: "javascript:alert(1)", Link: "javascript:alert(2)", Width: 1, Height: 1}, true
	}
	if html, _ := HTMLWith("![a](x.png)", evil); strings.Contains(html, "javascript:") {
		t.Fatalf("unsanitised: %s", html)
	}
	// User-written attribute syntax is not honoured: style cannot be injected.
	html, _ = HTMLWith("![a](x.png){style=\"background:url(x)\" onerror=\"x()\"}", nil)
	tag := html[strings.Index(html, "<img"):]
	tag = tag[:strings.Index(tag, ">")]
	if strings.Contains(tag, "onerror") || strings.Contains(tag, "background") {
		t.Fatalf("attribute injection: %s", html)
	}
}

func TestRewriteImageRef(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"inline", "![a](shot.png)", "![a](diagram.png)"},
		{"title", `![a](shot.png "A title")`, `![a](diagram.png "A title")`},
		{"angle brackets", "![a](<shot.png>)", "![a](<diagram.png>)"},
		{"repeated", "![a](shot.png) and ![b](shot.png)", "![a](diagram.png) and ![b](diagram.png)"},
		{"empty alt", "![](shot.png)", "![](diagram.png)"},
		{"brackets in alt", "![a [b] c](shot.png)", "![a [b] c](diagram.png)"},
		{"inside a link", "[![a](shot.png)](https://example.com)", "[![a](diagram.png)](https://example.com)"},
		{"reference style", "![a][r]\n\n[r]: shot.png \"T\"\n", "![a][r]\n\n[r]: diagram.png \"T\"\n"},
		{"other names are untouched", "![a](myshot.png) ![b](shot.png.bak) ![c](shot-2.png) ![d](shot.png)",
			"![a](myshot.png) ![b](shot.png.bak) ![c](shot-2.png) ![d](diagram.png)"},
		{"not a regex", "![a](shotxpng) ![b](shot.png)", "![a](shotxpng) ![b](diagram.png)"},
		{"links and paths are not images", "[l](shot.png) ![p](dir/shot.png) ![u](https://x.test/shot.png)",
			"[l](shot.png) ![p](dir/shot.png) ![u](https://x.test/shot.png)"},
		{"code span", "![a](shot.png) and `![b](shot.png)`", "![a](diagram.png) and `![b](shot.png)`"},
		{"fenced code", "![a](shot.png)\n\n```md\n![b](shot.png)\n```\n\n![c](shot.png)\n",
			"![a](diagram.png)\n\n```md\n![b](shot.png)\n```\n\n![c](diagram.png)\n"},
		{"tilde fence", "~~~\n![b](shot.png)\n~~~\n![c](shot.png)\n", "~~~\n![b](shot.png)\n~~~\n![c](diagram.png)\n"},
		{"indented code", "![a](shot.png)\n\n    ![b](shot.png)\n", "![a](diagram.png)\n\n    ![b](shot.png)\n"},
		{"raw html", "<div>![b](shot.png)</div>\n\n![a](shot.png)\n", "<div>![b](shot.png)</div>\n\n![a](diagram.png)\n"},
		{"not referenced", "no images here ![x](other.png)", "no images here ![x](other.png)"},
		{"only in code", "```\n![b](shot.png)\n```\n", "```\n![b](shot.png)\n```\n"},
	}
	for _, c := range cases {
		got, ok := RewriteImageRef(c.in, "shot.png", "diagram.png")
		if !ok || got != c.want {
			t.Errorf("%s:\n got %q (ok=%v)\nwant %q", c.name, got, ok, c.want)
		}
		// Whatever was rewritten, the references now agree with the new name.
		for _, n := range ImageRefs(got) {
			if n == "shot.png" {
				t.Errorf("%s: still references shot.png", c.name)
			}
		}
	}
	// Renaming onto a name the page already uses merges the two.
	got, ok := RewriteImageRef("![a](shot.png) ![b](diagram.png)", "shot.png", "diagram.png")
	if !ok || got != "![a](diagram.png) ![b](diagram.png)" {
		t.Errorf("merge: %q %v", got, ok)
	}
	// Characters that are special in a regular expression are matched literally.
	got, ok = RewriteImageRef("![a](aXb.png) ![b](a.b.png)", "a.b.png", "x.png")
	if !ok || got != "![a](aXb.png) ![b](x.png)" {
		t.Errorf("literal match: %q %v", got, ok)
	}
}

func TestRewriteFenceBoundaries(t *testing.T) {
	// No blank lines around the fence, and a fence that ends the document.
	for in, want := range map[string]string{
		"![a](shot.png)\n```\n![b](shot.png)\n```\n![c](shot.png)":               "![a](diagram.png)\n```\n![b](shot.png)\n```\n![c](diagram.png)",
		"```\n![b](shot.png)\n```":                                               "```\n![b](shot.png)\n```",
		"![a](shot.png)\n````\n```\n![b](shot.png)\n```\n````\n![c](shot.png)\n": "![a](diagram.png)\n````\n```\n![b](shot.png)\n```\n````\n![c](diagram.png)\n",
		"```\n![b](shot.png)\n":                                                  "```\n![b](shot.png)\n", // unterminated fence runs to the end
	} {
		if got, ok := RewriteImageRef(in, "shot.png", "diagram.png"); !ok || got != want {
			t.Errorf("%q\n got %q (ok=%v)\nwant %q", in, got, ok, want)
		}
	}
}

// ---- portable Markdown ----

func TestOnlyStandardSyntaxForLinks(t *testing.T) {
	// [[double brackets]] are not Markdown, so they stay text.
	html, err := HTML("See [[My new page]] and [[docs/ops|label]].")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "[[My new page]]") || !strings.Contains(html, "[[docs/ops|label]]") || strings.Contains(html, "<a ") {
		t.Errorf("wiki syntax was interpreted: %s", html)
	}
	// Soft line breaks are spaces, as in CommonMark; only two spaces or a
	// backslash make a hard break.
	html, _ = HTML("one\ntwo  \nthree\\\nfour")
	if strings.Count(html, "<br") != 2 || !strings.Contains(html, "one\ntwo") {
		t.Errorf("line breaks: %s", html)
	}
}

func TestMissingLinksAreMarked(t *testing.T) {
	check := func(dest string) (bool, bool) {
		switch dest {
		case "here", "docs/here":
			return true, true
		case "gone", "docs/gone":
			return true, false
		}
		return false, false // not a page
	}
	html, err := Render("[a](here) [b](gone) [c](docs/gone#part) [d](https://example.com/gone) [e](#top) [f](gone \"Own title\") ![i](gone)", Options{Links: check})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<a href="here">a</a>`,
		`<a href="gone" title="This page does not exist yet" class="missing">b</a>`,
		`<a href="docs/gone#part" title="This page does not exist yet" class="missing">c</a>`,
		`<a href="https://example.com/gone" rel="nofollow">d</a>`,
		`<a href="#top">e</a>`,
		`title="Own title"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %s in\n%s", want, html)
		}
	}
	if strings.Count(html, `class="missing"`) != 3 { // b, c and f; images are not links
		t.Errorf("wrong number of marked links: %s", html)
	}
	// Without a checker nothing is marked; the class cannot be injected by text either.
	if html, _ := HTML("[b](gone)"); strings.Contains(html, "missing") {
		t.Errorf("unchecked link marked: %s", html)
	}
	if html, _ := HTML("<a class=\"missing x\" href=\"u\">raw</a>"); strings.Contains(html, `class="missing x"`) {
		t.Errorf("class injected through raw HTML: %s", html)
	}
}

func TestLinksExtraction(t *testing.T) {
	src := "[a](docs/ops) [b](../faq.md#top) [c](/main/faq?x=1) [d](https://example.com) [e](mailto:x@y.z) [f](#here) [g](//cdn.test/x) [a again](docs/ops)\n\n" +
		"![img](shot.png) [ref][r] [ref2]\n\n[r]: other/page\n\n" +
		"[[not a link]]\n\n`[x](code)`\n\n```\n[y](fenced)\n```\n"
	want := []string{"docs/ops", "../faq.md", "/main/faq", "other/page"}
	if got := Links(src); !reflect.DeepEqual(got, want) {
		t.Fatalf("Links =\n%v\nwant\n%v", got, want)
	}
}

func TestGFMAndNothingElse(t *testing.T) {
	cases := []struct {
		name, in        string
		absent, present []string
	}{
		{"table", "| a | b |\n|:--|--:|\n| 1 | 2 |\n", nil, []string{"<table", "<th", `text-align:left`, `text-align:right`, "<td"}},
		{"strikethrough", "this is ~~gone~~ text", []string{"~~"}, []string{"<del>gone</del>"}},
		{"task list", "- [ ] todo\n- [x] done\n", []string{"[ ]", "[x]", "<input"}, []string{"⬜</span> todo", "✅</span> done", `aria-label="done"`}},
		{"no inputs", "- [x] a\n\n<input type=checkbox checked onfocus=alert(1)>\n", []string{"onfocus", "<input"}, nil},
		{"emails", "write to a.b@example.com", nil, []string{`<a href="mailto:a.b@example.com"`}},
		{"autolink ends", "(see https://example.com/a).", nil, []string{`<a href="https://example.com/a"`}},
		// Everything CommonMark has still works.
		{"basics", "# T\n\n*em* **strong** `code` [l](u)\n\n> q\n\n1. a\n2. b\n\n---\n\n    indented\n", nil,
			[]string{"<h1", "<em>em</em>", "<strong>strong</strong>", "<code>code</code>", `<a href="u">`, "<blockquote>", "<ol>", "<hr", "<pre>"}},
		{"bare url", "see https://example.com/a and www.example.com", nil, []string{`<a href="https://example.com/a"`, `<a href="http://www.example.com"`}},
		{"footnote", "Text.[^1]\n\n[^1]: The note.\n", []string{"footnote", "<sup", "<li id"}, []string{"[^1]"}},
		// Angle-bracket autolinks are part of CommonMark itself.
		{"angle autolink", "<https://example.com/a>", nil, []string{`<a href="https://example.com/a"`}},
	}
	for _, c := range cases {
		html, err := HTML(c.in)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range c.absent {
			if strings.Contains(html, s) {
				t.Errorf("%s: unexpected %q in %s", c.name, s, html)
			}
		}
		for _, s := range c.present {
			if !strings.Contains(html, s) {
				t.Errorf("%s: missing %q in %s", c.name, s, html)
			}
		}
	}
}

func TestImageDestsAndPathRewrite(t *testing.T) {
	src := "![a](shot.png) ![b](images/pic%20one.png) ![c](<images/pic one.png>) ![d](./up/../x.png) ![e](https://x.test/e.png) ![f](/abs.png)\n\n" +
		"[not an image](link.png)\n\n`![code](c.png)`\n\n```\n![fence](f.png)\n```\n"
	want := []string{"shot.png", "images/pic%20one.png", "images/pic one.png", "./up/../x.png"}
	if got := ImageDests(src); !reflect.DeepEqual(got, want) {
		t.Fatalf("ImageDests = %v\nwant %v", got, want)
	}
	got, ok := RewriteImageDest(src, "images/pic%20one.png", "pic-one.png")
	if !ok || !strings.Contains(got, "![b](pic-one.png)") || !strings.Contains(got, "![c](<images/pic one.png>)") || !strings.Contains(got, "![a](shot.png)") {
		t.Errorf("rewrite: %q %v", got, ok)
	}
	if got, ok := RewriteImageDest(src, "images/pic one.png", "pic-one.png"); !ok || !strings.Contains(got, "![c](<pic-one.png>)") || !strings.Contains(got, "![b](images/pic%20one.png)") {
		t.Errorf("angle brackets: %q %v", got, ok)
	}
	// Code stays as written, and names that merely contain the path are untouched.
	if got, _ := RewriteImageDest("![x](a/b.png) ![y](a/b.png.bak) ![z](xa/b.png)\n\n`![c](a/b.png)`", "a/b.png", "n.png"); got != "![x](n.png) ![y](a/b.png.bak) ![z](xa/b.png)\n\n`![c](a/b.png)`" {
		t.Errorf("exactness: %q", got)
	}
	// Bare names still go through the stricter RewriteImageRef.
	if got, ok := RewriteImageRef(src, "shot.png", "diagram.png"); !ok || !strings.Contains(got, "![a](diagram.png)") {
		t.Errorf("RewriteImageRef: %q %v", got, ok)
	}
}

func TestFragmentSizes(t *testing.T) {
	var got []Size
	res := func(name string, size Size) (Image, bool) { got = append(got, size); return Image{}, false }
	// A library name takes #w= and the older ?w= alike.
	for _, src := range []string{"![a](p.jpg#w=400)", "![a](p.jpg?w=400)"} {
		got = nil
		if _, err := HTMLWith(src, res); err != nil || len(got) != 1 || got[0] != (Size{W: 400}) {
			t.Errorf("%s: sizes %v, err %v", src, got, err)
		}
	}
	if refs := ImageRefs("![a](p.jpg#h=300)"); len(refs) != 1 || refs[0] != "p.jpg" {
		t.Errorf("refs = %v", refs)
	}
	// Another address loses the fragment and gets the size as an attribute.
	out, _ := HTMLWith("![a](https://example.com/logo.svg#w=400) ![b](https://example.com/b.png#h=90) ![c](https://example.com/c.png#top)", nil)
	for _, want := range []string{`src="https://example.com/logo.svg"`, `width="400"`, `src="https://example.com/b.png"`, `style="--h:90px"`, `c.png#top`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	if strings.Contains(out, "#w=") || strings.Contains(out, "#h=") {
		t.Errorf("fragment left in %s", out)
	}
	// Renaming keeps either form.
	if got, ok := RewriteImageRef("![a](p.jpg#w=400)", "p.jpg", "q.jpg"); !ok || got != "![a](q.jpg#w=400)" {
		t.Errorf("rename = %q %v", got, ok)
	}
}
