package server

import (
	"strings"
	"testing"
)

func TestFrontMatterMacrosAndBoardsOnPages(t *testing.T) {
	e := newEnv(t)
	md := "text/markdown"
	src := "---\ntitle: Release plan\nowner: <b>Sam</b>\n---\n# Heading\n\nBy {{page.owner}} in {{page.space}} at {{page.path}}: {{page.title}}.\n\n" +
		"<!-- kanban -->\n## To do\n- [ ] ship {{page.slug}}\n\n## Done\n- [x] write\n<!-- /kanban -->\n"
	expect(t, e.do("PUT", "/main/plan", src, "Content-Type", md), 201)

	// The stored text is exactly what was written: nothing is expanded or removed.
	if got := expect(t, e.do("GET", "/main/plan.md", ""), 200); got != src {
		t.Errorf("stored text changed:\n%q", got)
	}
	page := expect(t, e.do("GET", "/main/plan", "", "Accept", "text/html"), 200)
	for _, want := range []string{
		"By &lt;b&gt;Sam&lt;/b&gt; in main at plan: Release plan.",
		`<div class="kanban">`, `<li class="kanban-card"><span class="task"`, "ship plan",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, bad := range []string{"owner:", "<b>Sam", "{{page", "kanban -->"} {
		if strings.Contains(page, bad) {
			t.Errorf("page shows %q", bad)
		}
	}

	if i := strings.Index(page, `<div class="kanban">`); i < 0 || strings.Contains(page[i:i+600], "<input") {
		t.Error("a card shows a form control")
	}
	// The page is known by the title its front matter gives.
	if j := expect(t, e.do("GET", "/main/plan", "", "Accept", "application/json"), 200); !strings.Contains(j, `"title":"Release plan"`) {
		t.Errorf("the page does not use the front matter title: %s", j)
	}

	// The editor preview knows which page it belongs to.
	prev := expect(t, e.do("POST", "/_api/spaces/main/render?page=plan", "{{page.path}} / {{page.title}}", "Content-Type", md), 200)
	if !strings.Contains(prev, "plan / plan") {
		t.Errorf("preview = %s", prev)
	}
	if edit := expect(t, e.do("GET", "/main/plan?edit", "", "Accept", "text/html"), 200); !strings.Contains(edit, "{{page.title}}") || !strings.Contains(edit, "kanban") {
		t.Error("the cheat sheet does not explain macros and boards")
	}
	if css := expect(t, e.do("GET", "/_ui/app.css", ""), 200); !strings.Contains(css, ".kanban-column") {
		t.Error("stylesheet lacks the board rules")
	}
}
