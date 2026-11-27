package render

import (
	"strconv"
	"strings"
	"testing"
)

func TestFrontMatterKeys(t *testing.T) {
	src := "---\n" +
		"title: \"Release \\\"0.2\\\"\"  # comment\n" +
		"Owner: Sam\n" +
		"quote: 'it''s'\n" +
		"tags: [a, 'b c', \"d\"]\n" +
		"people:\n  - Ann\n  - Bo\n" +
		"notes: |\n  one\n  two\n" +
		"nested:\n  x: 1\n" +
		"empty:\n" +
		"---\nbody\n"
	meta, end := FrontMatter(src)
	if src[end:] != "body\n" {
		t.Fatalf("body after front matter = %q", src[end:])
	}
	want := Meta{"title": `Release "0.2"`, "owner": "Sam", "quote": "it's", "tags": "a, b c, d", "people": "Ann, Bo", "notes": "one two"}
	if len(meta) != len(want) {
		t.Fatalf("meta = %v, want %v", meta, want)
	}
	for k, v := range want {
		if meta[k] != v {
			t.Errorf("%s = %q, want %q", k, meta[k], v)
		}
	}
}

func TestNotFrontMatter(t *testing.T) {
	for _, src := range []string{
		"---\nplain words\n---\nbody",   // not key: value lines
		"---\ntitle: x\nno close",       // never closed
		"\n---\ntitle: x\n---\nbody",    // not at the very start
		"--- \nx\n",                     // one fence only
		"---\n# heading\n\n- item\n---", // a rule, text, a rule
		"- - -\ntitle: x\n---\n",        // a different kind of rule
	} {
		if _, end := FrontMatter(src); end != 0 {
			t.Errorf("%q read as front matter", src)
		}
		out, _ := HTML(src)
		if !strings.Contains(out, "<hr") && !strings.Contains(out, "<h2") {
			t.Errorf("%q lost its rule: %s", src, out)
		}
	}
}

func TestFrontMatterIsHiddenAndTitles(t *testing.T) {
	src := "---\ntitle: Plan\nowner: Sam\n---\n# Heading\n\ntext\n"
	out, err := HTML(src)
	if err != nil || strings.Contains(out, "owner") || strings.Contains(out, "<hr") || !strings.Contains(out, "<h1") {
		t.Fatalf("rendered %q, %v", out, err)
	}
	if got := Title(src); got != "Plan" {
		t.Errorf("Title = %q, want the front matter title", got)
	}
	if got := Title("---\n# just a comment\nk: v\n---\n# Real\n"); got != "Real" {
		t.Errorf("Title = %q, a YAML comment is not a heading", got)
	}
	if got := Title("---\nowner: Sam\n---\n# Real\n"); got != "Real" {
		t.Errorf("Title = %q, want the heading when the front matter has none", got)
	}
	if StripFrontMatter(src) != "# Heading\n\ntext\n" {
		t.Errorf("StripFrontMatter = %q", StripFrontMatter(src))
	}
	// Pictures and links named inside front matter are not references.
	if refs := ImageRefs("---\nimg: ![x](a.png)\n---\n![y](b.png)"); len(refs) != 1 || refs[0] != "b.png" {
		t.Errorf("ImageRefs = %v", refs)
	}
}

func TestMacros(t *testing.T) {
	page := Page{Path: "docs/setup", Space: "main"}
	cases := []struct{ name, in, want string }{
		{"title from heading", "# Setup guide\n\n{{page.title}}", "<p>Setup guide</p>"},
		{"title from name", "{{page.title}}", "<p>setup</p>"},
		{"title from front matter", "---\ntitle: Plan\n---\n# Heading\n\n{{ page.title }}", "<p>Plan</p>"},
		{"built ins", "{{page.path}} {{page.slug}} {{page.space}}", "<p>docs/setup setup main</p>"},
		{"any key", "---\nOwner: Sam\nmy_key: v\nmy-key2: w\n---\n{{page.owner}} {{page.my_key}} {{page.my-key2}}", "<p>Sam v w</p>"},
		{"unknown stays", "{{page.nope}} {{other.title}} {{title}}", "<p>{{page.nope}} {{other.title}} {{title}}</p>"},
		{"code stays", "`{{page.path}}`\n\n```\n{{page.path}}\n```\n", "<code>{{page.path}}</code>"},
		{"escaped", `\{{page.path}}`, "<p>{{page.path}}</p>"},
		{"in emphasis and links", "**{{page.slug}}** [{{page.space}}](x)", `<strong>setup</strong> <a href="x">main</a>`},
		{"in headings", "## About {{page.slug}}", "About setup</h2>"},
		{"in a table", "| a |\n|---|\n| {{page.slug}} |\n", "<td>setup</td>"},
		{"built ins win", "---\npath: elsewhere\nspace: other\n---\n{{page.path}} {{page.space}}", "<p>docs/setup main</p>"},
		{"not again", "---\na: '{{page.path}}'\n---\n{{page.a}}", "<p>{{page.path}}</p>"},
	}
	for _, c := range cases {
		out, err := Render(c.in, Options{Page: page})
		if err != nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %q gave %q (%v), want it to contain %q", c.name, c.in, out, err, c.want)
		}
	}
}

// A value is plain text: whatever it holds, it cannot become markup.
func TestMacroValuesCannotInject(t *testing.T) {
	values := []string{
		`<script>alert(1)</script>`,
		`<img src=x onerror=alert(1)>`,
		`[x](javascript:alert(1))`,
		`![x](https://evil.example/p.png)`,
		`**bold** _em_ # heading`,
		"x\n\n# heading\n\n- list",
		`"><svg onload=alert(1)>`,
		`&lt;b&gt; &amp; &#60;i&#62;`,
		`\<b\>`,
		"<!-- kanban -->",
		"a‮b",
	}
	for _, v := range values {
		forms := []string{strconv.Quote(v)}
		if !strings.Contains(v, "\n") {
			forms = append(forms, "'"+strings.ReplaceAll(v, "'", "''")+"'", v)
		}
		for _, form := range forms {
			src := "---\ntitle: " + form + "\nk: " + form + "\n---\n{{page.k}}\n\n{{page.title}}\n\n[{{page.k}}](u)\n\n![{{page.k}}](p.png)\n\n- [ ] {{page.k}}\n\n| h |\n|---|\n| {{page.k}} |\n"
			out, err := Render(src, Options{Page: Page{Path: "a", Space: "s"}})
			if err != nil {
				t.Fatal(err)
			}
			if bad := unsafeHTML(out); bad != "" {
				t.Errorf("value %q: unsafe output %s in %s", v, bad, out)
			}
			for _, tag := range []string{"<script", "<svg", "<img src=x", "<h1", "<h2", "<em>", "<strong>", `src="https://evil`, `href="javascript`} {
				if strings.Contains(out, tag) {
					t.Errorf("value %q produced %q: %s", v, tag, out)
				}
			}
			if strings.Contains(out, "‮") {
				t.Errorf("value %q kept a bidirectional control", v)
			}
		}
	}
}

func TestMacroExpansionIsBounded(t *testing.T) {
	big := strings.Repeat("x", 1900)
	src := "---\nk: " + big + "\n---\n" + strings.Repeat("{{page.k}} ", 5000)
	out, err := Render(src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 400<<10 {
		t.Errorf("a %d byte page grew to %d bytes", len(src), len(out))
	}
	if !strings.Contains(out, "{{page.k}}") {
		t.Error("expansion never stopped")
	}
	long := "---\nk: " + strings.Repeat("é", 5000) + "\n---\n"
	if m, _ := FrontMatter(long); len([]rune(m["k"])) != maxMetaValue {
		t.Errorf("value kept %d characters", len([]rune(m["k"])))
	}
}

func TestKanbanBoards(t *testing.T) {
	board := "## To do\n- [ ] one\n\n## Done\n- [x] two\n"
	for name, src := range map[string]string{
		"markers":      "<!-- kanban -->\n" + board + "<!-- /kanban -->\n",
		"front matter": "---\nkanban: true\n---\n" + board,
		"obsidian":     "---\nkanban-plugin: board\n---\n\n" + board,
	} {
		out, err := HTML(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`<div class="kanban">`, `<section class="kanban-column">`, `<ul class="kanban-cards">`,
			`<li class="kanban-card"><span class="task"`, `kanban-card kanban-card-done`, "⬜</span> one", "✅</span> two"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: no %q in %s", name, want, out)
			}
		}
		if strings.Contains(out, "<input") || strings.Contains(out, "kanban:") || strings.Contains(out, "<!--") {
			t.Errorf("%s: left something behind: %s", name, out)
		}
	}
	// Without a request the same text stays an ordinary page.
	if out, _ := HTML(board); strings.Contains(out, "kanban") || !strings.Contains(out, "<h2") {
		t.Errorf("a page that did not ask for a board became one: %s", out)
	}
	// A card is ordinary page text: macros work, markup is still cleaned.
	out, _ := Render("<!-- kanban -->\n## A\n- [ ] {{page.slug}} <script>alert(1)</script> **b**\n<!-- /kanban -->", Options{Page: Page{Path: "x/y"}})
	if !strings.Contains(out, "y ") || strings.Contains(out, "<script") || !strings.Contains(out, "<strong>b</strong>") {
		t.Errorf("card = %s", out)
	}
}
