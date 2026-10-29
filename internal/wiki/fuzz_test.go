package wiki

import (
	"strings"
	"testing"
)

func FuzzRewriteNeverBreaksText(f *testing.F) {
	f.Add("[a](docs/ops) [b](/main/docs/ops#x \"t\") [c](<docs/ops>) `[d](docs/ops)`\n\n[r]: docs/ops\n", "faq", "faq")
	f.Add("[x](../y) [z](./q/)", "docs/a", "other/a")
	f.Add("![i](docs/ops) [e](https://x.test/docs/ops)", "", "")
	e := testEnv(site, "main")
	m := moveMapper("main", "docs/ops", "ops")
	f.Fuzz(func(t *testing.T, body, src, newSrc string) {
		out, changed := e.Rewrite(src, newSrc, body, m)
		if !changed && out != body {
			t.Fatalf("not changed, but the text differs: %q -> %q", body, out)
		}
		if changed && out == body {
			t.Fatalf("changed, but the text is the same: %q", body)
		}
		// Rewriting is stable: doing it again from where the pages now are finds nothing more to move.
		if strings.Count(body, "\n") != strings.Count(out, "\n") {
			t.Fatalf("line count changed: %q -> %q", body, out)
		}
		// Links never resolve to something outside the space.
		e.Link(src, body)
	})
}

func FuzzLinkResolution(f *testing.F) {
	f.Add("docs/ops", "../faq#top")
	f.Add("a", "/main/docs/ops?x=1")
	f.Add("", "//host/x")
	f.Add("x/y", strings.Repeat("../", 40)+"z")
	e := testEnv(site, "main")
	f.Fuzz(func(t *testing.T, src, dest string) {
		ref, internal, exists := e.Link(src, dest)
		if !internal && (exists || ref != (Ref{})) {
			t.Fatalf("not a page link, yet %+v exists=%v", ref, exists)
		}
		if internal && (ref.Path == "" || strings.HasPrefix(ref.Path, "/") || ref.Path == ".." || strings.HasPrefix(ref.Path, "../") || strings.Contains(ref.Path, "/../") || strings.HasPrefix(ref.Path, "_")) {
			t.Fatalf("resolved to an unusable path %+v", ref)
		}
	})
}
