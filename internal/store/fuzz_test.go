package store

import (
	"strings"
	"testing"
	"unicode"
)

func FuzzPaths(f *testing.F) {
	for _, s := range []string{"docs/ops", "a//b", "../x", "/a/", "a/./b", "a%2Fb", "a\x00b", "_hidden", "x.md", "pic.PNG", strings.Repeat("a", 300), "é/ü", "a b", "-x", ".x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		parts, err := SplitPath(p)
		if err != nil {
			return
		}
		for _, seg := range parts {
			if !ValidSlug(seg) || strings.ContainsAny(seg, "/\\\x00") || strings.HasPrefix(seg, "_") || strings.HasPrefix(seg, ".") || seg == ".." {
				t.Fatalf("SplitPath(%q) accepted segment %q", p, seg)
			}
		}
		// Re-joining gives a path that splits the same way.
		again, err := SplitPath(strings.Join(parts, "/"))
		if err != nil || strings.Join(again, "/") != strings.Join(parts, "/") {
			t.Fatalf("not stable: %q -> %v -> %v %v", p, parts, again, err)
		}
	})
}

func FuzzSearchText(f *testing.F) {
	for _, s := range []string{"hello world", `"unbalanced`, "AND OR NOT NEAR(", "a*", "col:umn", "-x +y", "'; DROP TABLE pages; --", "\x00", "é ñ 日本語", strings.Repeat("word ", 100), "^", "{a b}", "title:x"} {
		f.Add(s)
	}
	st, err := Open(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { st.Close() })
	sp, _ := st.CreateSpace("main", "Main", true)
	sp.PutPage("a", "Alpha Page", "hello world and some more words\n", Refs{}, Cond{})
	f.Fuzz(func(t *testing.T, q string) {
		match := FTSQuery(q)
		for _, tok := range strings.Fields(match) {
			core := strings.Trim(strings.TrimSuffix(tok, "*"), `"`)
			if strings.TrimSuffix(tok, "*") == "" || !strings.HasPrefix(tok, `"`) {
				t.Fatalf("unquoted token %q in %q (from %q)", tok, match, q)
			}
			for _, r := range core {
				if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
					t.Fatalf("punctuation %q survived in %q (from %q)", r, match, q)
				}
			}
		}
		if _, err := sp.Search(q, 5); err != nil {
			t.Fatalf("Search(%q) failed: %v", q, err)
		}
	})
}

// The OR query used to find source material is made of quoted words only, so
// text from a question can never become query syntax.
func FuzzSearchAny(f *testing.F) {
	f.Add("hello world", "NEAR(")
	f.Add(`"unbalanced`, "OR NOT")
	f.Add("a*", "col:umn")
	st, err := Open(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { st.Close() })
	sp, _ := st.CreateSpace("main", "Main", true)
	sp.PutPage("a", "Alpha Page", "hello world and some more words\n", Refs{}, Cond{})
	f.Fuzz(func(t *testing.T, a, b string) {
		q := FTSQueryAny([]string{a, b})
		for _, tok := range strings.Split(q, " OR ") {
			if tok == "" {
				continue
			}
			if len(tok) < 2 || tok[0] != '"' || tok[len(tok)-1] != '"' {
				t.Fatalf("unquoted token %q in %q", tok, q)
			}
			for _, r := range tok[1 : len(tok)-1] {
				if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
					t.Fatalf("punctuation %q in %q", r, q)
				}
			}
		}
		if _, err := sp.SearchAny([]string{a, b}, 5); err != nil {
			t.Fatalf("SearchAny(%q, %q): %v", a, b, err)
		}
	})
}
