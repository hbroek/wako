package diff

import (
	"errors"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	for in, want := range map[string][]string{
		"":           nil,
		"a":          {"a"},
		"a\n":        {"a\n"},
		"a\nb":       {"a\n", "b"},
		"a\n\nb\n":   {"a\n", "\n", "b\n"},
		"\n":         {"\n"},
		"a\r\nb\r\n": {"a\r\n", "b\r\n"},
	} {
		got := Split(in)
		if strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
			t.Errorf("Split(%q) = %q, want %q", in, got, want)
		}
		if strings.Join(got, "") != in {
			t.Errorf("Split(%q) loses text", in)
		}
	}
}

func TestUnifiedFormat(t *testing.T) {
	a := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	b := "one\ntwo\nTHREE\nfour\nfive\nsix\nseven\neight\nnine\nten\neleven\n"
	want := "--- a/p\n+++ b/p\n" +
		"@@ -1,6 +1,6 @@\n one\n two\n-three\n+THREE\n four\n five\n six\n" +
		"@@ -8,3 +8,4 @@\n eight\n nine\n ten\n+eleven\n"
	if got := Unified(a, b, "a/p", "b/p"); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if Unified(a, a, "a", "b") != "" {
		t.Error("equal texts must give an empty diff")
	}
	// Adding to an empty text, removing everything, and the missing-newline marker.
	if got := Unified("", "x\n", "a", "b"); got != "--- a\n+++ b\n@@ -0,0 +1 @@\n+x\n" {
		t.Errorf("from empty: %q", got)
	}
	if got := Unified("x\ny\n", "", "a", "b"); got != "--- a\n+++ b\n@@ -1,2 +0,0 @@\n-x\n-y\n" {
		t.Errorf("to empty: %q", got)
	}
	if got := Unified("a\nb", "a\nb\n", "a", "b"); got != "--- a\n+++ b\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+b\n" {
		t.Errorf("newline at end: %q", got)
	}
}

func roundTrip(t *testing.T, a, b string) {
	t.Helper()
	p := Unified(a, b, "a", "b")
	got, err := Apply(a, p)
	if a == b {
		if p != "" {
			t.Errorf("equal texts, non-empty patch: %q", p)
		}
		return
	}
	if err != nil || got != b {
		t.Fatalf("round trip failed (err %v)\na=%q\nb=%q\npatch:\n%s\ngot=%q", err, a, b, p, got)
	}
}

func TestRoundTripCases(t *testing.T) {
	for _, c := range [][2]string{
		{"", "a"}, {"a", ""}, {"a\n", "a"}, {"a", "a\n"}, {"a\nb\nc\n", "a\nc\n"}, {"a\nc\n", "a\nb\nc\n"},
		{"a\nb\nc", "a\nb\nd"}, {"\n\n\n", "\n"}, {"x\r\ny\r\n", "x\r\nz\r\n"}, {"x\r\ny", "x\ny"},
		{"same\n", "same\n"}, {"1\n2\n3\n4\n5\n6\n7\n8\n9\n", "9\n8\n7\n6\n5\n4\n3\n2\n1\n"},
		{"+plus\n-minus\n@@ at\n \\ backslash\n", "-minus\n+plus\n@@ at\n"},
		{"héllo\nwörld\n", "héllo\nworld\n"}, {"a\n\n\nb\n", "a\n\nb\n"},
	} {
		roundTrip(t, c[0], c[1])
	}
}

func lcs(a, b []string) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] > dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	return dp[len(a)][len(b)]
}

func randomText(r *rand.Rand, lines, alphabet int) string {
	var sb strings.Builder
	for i := 0; i < lines; i++ {
		sb.WriteString(string(rune('a' + r.Intn(alphabet))))
		if i < lines-1 || r.Intn(4) > 0 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func TestRandomDiffsAreCorrectAndMinimal(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	for i := 0; i < 600; i++ {
		a := randomText(r, r.Intn(30), 2+r.Intn(5))
		b := randomText(r, r.Intn(30), 2+r.Intn(5))
		roundTrip(t, a, b)
		la, lb := Split(a), Split(b)
		var changes int
		for _, o := range script(la, lb) {
			if o.kind != eq {
				changes++
			}
		}
		if want := len(la) + len(lb) - 2*lcs(la, lb); changes != want {
			t.Fatalf("not minimal: %d edits, want %d\na=%q\nb=%q", changes, want, a, b)
		}
	}
	// Edits of a larger text: a few lines changed, inserted and removed.
	base := randomText(r, 400, 20)
	lines := Split(base)
	for i := 0; i < 50; i++ {
		edited := append([]string(nil), lines...)
		for k := 0; k < 1+r.Intn(8); k++ {
			at := r.Intn(len(edited))
			switch r.Intn(3) {
			case 0:
				edited[at] = "changed " + edited[at]
			case 1:
				edited = append(edited[:at], edited[at+1:]...)
			default:
				edited = append(edited[:at], append([]string{"new line\n"}, edited[at:]...)...)
			}
		}
		roundTrip(t, base, strings.Join(edited, ""))
	}
}

func TestVeryDifferentTextsFallBack(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 2500; i++ {
		a.WriteString("left " + string(rune('a'+i%26)) + "\n")
		b.WriteString("right " + string(rune('a'+i%26)) + "\n")
	}
	roundTrip(t, a.String(), b.String()) // still a valid, applicable diff
}

func TestApplyIsStrict(t *testing.T) {
	base := "a\nb\nc\nd\n"
	good := Unified(base, "a\nB\nc\nd\n", "a", "b")
	if got, err := Apply(base, good); err != nil || got != "a\nB\nc\nd\n" {
		t.Fatalf("good patch: %q %v", got, err)
	}
	for name, c := range map[string]struct{ base, patch string }{
		"changed base":      {"a\nX\nc\nd\n", good},
		"shifted base":      {"new\na\nb\nc\nd\n", good},
		"shorter base":      {"a\nb\n", good},
		"empty base":        {"", good},
		"no newline at end": {"a\nb\nc\nd", good},
		"not a patch":       {base, "just some text"},
		"empty patch":       {base, ""},
		"bad header":        {base, "@@ -x +y @@\n a\n"},
		"counts disagree":   {base, "@@ -1,3 +1,3 @@\n a\n-b\n+B\n"},
		"unknown line":      {base, "@@ -1,2 +1,2 @@\n a\n?b\n"},
		"overlapping hunks": {base, "@@ -2,2 +2,2 @@\n b\n-c\n+C\n@@ -1,2 +1,2 @@\n-a\n+A\n b\n"},
		"marker first":      {base, "@@ -1 +1 @@\n\\ No newline at end of file\n"},
		"past the end":      {base, "@@ -9,1 +9,1 @@\n-z\n+y\n"},
	} {
		if _, err := Apply(c.base, c.patch); err == nil || !errors.Is(err, ErrPatch) {
			t.Errorf("%s: want ErrPatch, got %v", name, err)
		}
	}
	// A patch with git-style preamble and trailing text applies; the headers are ignored.
	p := "diff --git a/x b/x\nindex 1..2 100644\n" + good + "-- \n2.30.0\n"
	if got, err := Apply(base, p); err != nil || got != "a\nB\nc\nd\n" {
		t.Errorf("git-style patch: %q %v", got, err)
	}
	// Tools that strip the trailing space of empty context lines.
	if got, err := Apply("a\n\nb\n", "@@ -1,3 +1,3 @@\n a\n\n-b\n+c\n"); err != nil || got != "a\n\nc\n" {
		t.Errorf("stripped context: %q %v", got, err)
	}
	// Two hunks apply in order, each against the original line numbers.
	long := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n"
	edit := "1\nTWO\n3\n4\n5\n6\n7\n8\n9\n10\n11\nTWELVE\n"
	if got, err := Apply(long, Unified(long, edit, "a", "b")); err != nil || got != edit {
		t.Errorf("two hunks: %q %v", got, err)
	}
}

func TestHunkLineNumbers(t *testing.T) {
	hs := Hunks("a\nb\nc\n", "a\nX\nc\n", 1)
	if len(hs) != 1 || hs[0].OldStart != 1 || hs[0].OldCount != 3 || hs[0].NewStart != 1 || hs[0].NewCount != 3 {
		t.Fatalf("hunks: %+v", hs)
	}
	var kinds string
	for _, l := range hs[0].Lines {
		kinds += string(l.Kind)
	}
	if kinds != " -+ " || hs[0].Lines[1].Old != 2 || hs[0].Lines[1].New != 0 || hs[0].Lines[2].New != 2 || hs[0].Lines[2].Old != 0 {
		t.Errorf("lines: %+v", hs[0].Lines)
	}
}

// The real diff and patch programs must read our patches and we must read theirs.
func TestAgainstSystemTools(t *testing.T) {
	diffBin, err1 := exec.LookPath("diff")
	patchBin, err2 := exec.LookPath("patch")
	if err1 != nil || err2 != nil {
		t.Skip("diff and patch are not installed")
	}
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 40; i++ {
		a, b := randomText(r, 5+r.Intn(40), 6), randomText(r, 5+r.Intn(40), 6)
		if a == b {
			continue
		}
		pa, pb := write("a.txt", a), write("b.txt", b)

		// Their patch, our Apply.
		theirs, _ := exec.Command(diffBin, "-u", pa, pb).Output() // exit status 1 means "differ"
		if got, err := Apply(a, string(theirs)); err != nil || got != b {
			t.Fatalf("our Apply of diff -u output failed (%v)\na=%q\nb=%q\n%s", err, a, b, theirs)
		}
		// Our patch, their patch program.
		target := write("target.txt", a)
		pf := write("our.patch", Unified(a, b, "a.txt", "b.txt"))
		if out, err := exec.Command(patchBin, "-s", "--no-backup-if-mismatch", target, pf).CombinedOutput(); err != nil {
			t.Fatalf("patch rejected our diff: %v\n%s\n%s", err, out, Unified(a, b, "a.txt", "b.txt"))
		}
		if got, _ := os.ReadFile(target); string(got) != b {
			t.Fatalf("patch produced %q, want %q", got, b)
		}
	}
}
