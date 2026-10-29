package diff

import "testing"

func FuzzApplyNeverPanics(f *testing.F) {
	f.Add("a\nb\nc\n", "--- a\n+++ b\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n")
	f.Add("a", "@@ -1 +1 @@\n-a\n\\ No newline at end of file\n+b\n")
	f.Add("", "@@ -0,0 +1 @@\n+x\n")
	f.Add("x\n", "@@ -1,2 +1,2 @@\n x\n-\n+y\n@@ -99 +99 @@\n-q\n+r\n")
	f.Fuzz(func(t *testing.T, base, patch string) {
		out, err := Apply(base, patch)
		if err != nil && out != "" {
			t.Fatalf("an error came with output %q", out)
		}
	})
}

// For any two texts, the diff turns one into the other, and says nothing for equal texts.
func FuzzDiffRoundTrip(f *testing.F) {
	f.Add("a\nb\nc\n", "a\nB\nc\nd")
	f.Add("", "x")
	f.Add("same\n", "same\n")
	f.Add("a\r\nb", "a\nb\r\n")
	f.Fuzz(func(t *testing.T, a, b string) {
		p := Unified(a, b, "a", "b")
		if (p == "") != (a == b) {
			t.Fatalf("empty diff for different texts, or a diff for equal ones: %q %q -> %q", a, b, p)
		}
		if a == b {
			return
		}
		got, err := Apply(a, p)
		if err != nil || got != b {
			t.Fatalf("round trip failed: %v\na=%q\nb=%q\npatch=%q\ngot=%q", err, a, b, p, got)
		}
	})
}
