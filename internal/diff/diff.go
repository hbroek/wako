// Package diff compares texts line by line, writes unified diffs, and applies
// them. Texts are compared exactly, including line endings and whether the last
// line ends in a newline.
package diff

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Split cuts s into lines, each keeping its terminating "\n". The last line has
// none if s does not end in one.
func Split(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			lines = append(lines, s)
			break
		}
		lines = append(lines, s[:i+1])
		s = s[i+1:]
	}
	return lines
}

type kind byte

const (
	eq  kind = ' '
	del kind = '-'
	ins kind = '+'
)

// maxEdits bounds the work of the shortest-edit-script search. Texts that
// differ by more than this many lines are shown as replaced wholesale, which is
// a valid (if unhelpful) diff.
const maxEdits = 1500

type op struct {
	kind kind
	a, b int // indexes into the two line slices (a for eq and del, b for eq and ins)
}

// script returns the edit script turning a into b.
func script(a, b []string) []op {
	// Common beginning and end need no searching.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var ops []op
	for i := 0; i < pre; i++ {
		ops = append(ops, op{eq, i, i})
	}
	midA, midB := a[pre:len(a)-suf], b[pre:len(b)-suf]
	for _, o := range middle(midA, midB) {
		switch o.kind {
		case eq:
			ops = append(ops, op{eq, o.a + pre, o.b + pre})
		case del:
			ops = append(ops, op{del, o.a + pre, 0})
		case ins:
			ops = append(ops, op{ins, 0, o.b + pre})
		}
	}
	for i := 0; i < suf; i++ {
		ops = append(ops, op{eq, len(a) - suf + i, len(b) - suf + i})
	}
	return ops
}

// middle is Myers' O(ND) shortest edit script on the part that differs.
func middle(a, b []string) []op {
	n, m := len(a), len(b)
	switch {
	case n == 0 && m == 0:
		return nil
	case n == 0:
		out := make([]op, m)
		for i := range out {
			out[i] = op{ins, 0, i}
		}
		return out
	case m == 0:
		out := make([]op, n)
		for i := range out {
			out[i] = op{del, i, 0}
		}
		return out
	}
	limit := n + m
	if limit > maxEdits {
		limit = maxEdits
	}
	off := limit + 1
	v := make([]int, 2*off+2)
	var trace [][]int // trace[d] holds v[-d-1 .. d+1] as it was at the start of round d
	for d := 0; d <= limit; d++ {
		snap := make([]int, 2*d+3)
		copy(snap, v[off-d-1:off+d+2])
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				return backtrack(trace, n, m)
			}
		}
	}
	// Too different to search: replace everything.
	out := make([]op, 0, n+m)
	for i := 0; i < n; i++ {
		out = append(out, op{del, i, 0})
	}
	for j := 0; j < m; j++ {
		out = append(out, op{ins, 0, j})
	}
	return out
}

func backtrack(trace [][]int, n, m int) []op {
	var rev []op
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		w := trace[d]
		at := func(k int) int { return w[k+d+1] }
		k := x - y
		var prevK int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := at(prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			rev = append(rev, op{eq, x, y})
		}
		if d > 0 {
			if x == prevX {
				y--
				rev = append(rev, op{ins, 0, y})
			} else {
				x--
				rev = append(rev, op{del, x, 0})
			}
		}
		x, y = prevX, prevY
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// Line is one line of a hunk.
type Line struct {
	Kind      byte   // ' ' context, '-' removed, '+' added
	Text      string // without the line ending
	NoNewline bool   // the line has no terminating newline
	Old, New  int    // 1-based line numbers; 0 where the line is not in that text
}

// Hunk is a run of changes with its surrounding context.
type Hunk struct {
	OldStart, OldCount int
	NewStart, NewCount int
	Lines              []Line
}

// Hunks returns the differences between a and b with the given lines of context.
// It is empty if the texts are equal.
func Hunks(a, b string, context int) []Hunk {
	la, lb := Split(a), Split(b)
	ops := script(la, lb)
	// Find the changed stretches, then pad each with context and merge close ones.
	type span struct{ from, to int } // indexes into ops, [from, to)
	var spans []span
	for i := 0; i < len(ops); {
		if ops[i].kind == eq {
			i++
			continue
		}
		j := i
		for j < len(ops) && ops[j].kind != eq {
			j++
		}
		spans = append(spans, span{i, j})
		i = j
	}
	if len(spans) == 0 {
		return nil
	}
	var merged []span
	for _, s := range spans {
		from, to := s.from-context, s.to+context
		if from < 0 {
			from = 0
		}
		if to > len(ops) {
			to = len(ops)
		}
		if n := len(merged); n > 0 && from <= merged[n-1].to {
			merged[n-1].to = to
		} else {
			merged = append(merged, span{from, to})
		}
	}
	// Line numbers at the start of ops[i].
	oldAt := make([]int, len(ops)+1)
	newAt := make([]int, len(ops)+1)
	for i, o := range ops {
		oldAt[i+1], newAt[i+1] = oldAt[i], newAt[i]
		if o.kind != ins {
			oldAt[i+1]++
		}
		if o.kind != del {
			newAt[i+1]++
		}
	}
	var hunks []Hunk
	for _, s := range merged {
		h := Hunk{OldStart: oldAt[s.from] + 1, NewStart: newAt[s.from] + 1}
		for i := s.from; i < s.to; i++ {
			o := ops[i]
			l := Line{Kind: byte(o.kind)}
			var raw string
			switch o.kind {
			case eq:
				raw, l.Old, l.New = la[o.a], oldAt[i]+1, newAt[i]+1
				h.OldCount++
				h.NewCount++
			case del:
				raw, l.Old = la[o.a], oldAt[i]+1
				h.OldCount++
			case ins:
				raw, l.New = lb[o.b], newAt[i]+1
				h.NewCount++
			}
			l.Text, l.NoNewline = strings.TrimSuffix(raw, "\n"), !strings.HasSuffix(raw, "\n")
			h.Lines = append(h.Lines, l)
		}
		if h.OldCount == 0 {
			h.OldStart-- // a range of no lines is placed after the line before it
		}
		if h.NewCount == 0 {
			h.NewStart--
		}
		hunks = append(hunks, h)
	}
	return hunks
}

func rng(start, count int) string {
	if count == 1 {
		return strconv.Itoa(start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

// Unified returns a unified diff turning a into b (three lines of context), or
// "" if they are equal. nameA and nameB label the two sides.
func Unified(a, b, nameA, nameB string) string {
	hunks := Hunks(a, b, 3)
	if len(hunks) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", nameA, nameB)
	for _, h := range hunks {
		fmt.Fprintf(&sb, "@@ -%s +%s @@\n", rng(h.OldStart, h.OldCount), rng(h.NewStart, h.NewCount))
		for _, l := range h.Lines {
			sb.WriteByte(l.Kind)
			sb.WriteString(l.Text)
			sb.WriteByte('\n')
			if l.NoNewline {
				sb.WriteString("\\ No newline at end of file\n")
			}
		}
	}
	return sb.String()
}

// ---- applying ----

// ErrPatch is wrapped by every error from Apply.
var ErrPatch = errors.New("patch does not apply")

func patchErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPatch, fmt.Sprintf(format, args...))
}

type pline struct {
	kind      byte
	text      string
	noNewline bool
}

type phunk struct {
	oldStart, oldCount, newStart, newCount int
	lines                                  []pline
}

func parseRange(s string) (start, count int, err error) {
	count = 1
	a, b, found := strings.Cut(s, ",")
	if start, err = strconv.Atoi(a); err != nil {
		return 0, 0, err
	}
	if found {
		if count, err = strconv.Atoi(b); err != nil {
			return 0, 0, err
		}
	}
	if start < 0 || count < 0 {
		return 0, 0, errors.New("negative range")
	}
	return start, count, nil
}

func parseHunkHeader(line string) (phunk, error) {
	// @@ -a[,b] +c[,d] @@ optional text
	f := strings.Fields(line)
	if len(f) < 4 || f[0] != "@@" || !strings.HasPrefix(f[1], "-") || !strings.HasPrefix(f[2], "+") || f[3] != "@@" && !strings.HasPrefix(f[3], "@@") {
		return phunk{}, patchErr("bad hunk header %q", line)
	}
	var h phunk
	var err error
	if h.oldStart, h.oldCount, err = parseRange(f[1][1:]); err != nil {
		return phunk{}, patchErr("bad hunk header %q", line)
	}
	if h.newStart, h.newCount, err = parseRange(f[2][1:]); err != nil {
		return phunk{}, patchErr("bad hunk header %q", line)
	}
	return h, nil
}

func parsePatch(patch string) ([]phunk, error) {
	var hunks []phunk
	var cur *phunk
	lines := strings.Split(patch, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	counts := func(h *phunk) (o, n int) {
		for _, l := range h.lines {
			if l.kind != '+' {
				o++
			}
			if l.kind != '-' {
				n++
			}
		}
		return
	}
	finish := func() error {
		if cur == nil {
			return nil
		}
		if o, n := counts(cur); o != cur.oldCount || n != cur.newCount {
			return patchErr("hunk at line %d has %d old and %d new lines, its header says %d and %d",
				cur.oldStart, o, n, cur.oldCount, cur.newCount)
		}
		hunks = append(hunks, *cur)
		cur = nil
		return nil
	}
	for _, line := range lines {
		// A hunk ends when its header's line counts are used up, as patch(1)
		// reads it. What follows (a mail signature, say) is not part of it.
		if cur != nil && !strings.HasPrefix(line, "\\") && !strings.HasPrefix(line, "@@") {
			if o, n := counts(cur); o == cur.oldCount && n == cur.newCount {
				if err := finish(); err != nil {
					return nil, err
				}
			}
		}
		switch {
		case strings.HasPrefix(line, "@@"):
			if err := finish(); err != nil {
				return nil, err
			}
			h, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			cur = &h
		case cur == nil:
			// File headers, and anything between or after the hunks, are ignored.
		case strings.HasPrefix(line, "\\"):
			if len(cur.lines) == 0 {
				return nil, patchErr("unexpected %q", line)
			}
			cur.lines[len(cur.lines)-1].noNewline = true
		case line == "":
			// Some tools strip the trailing space of an empty context line.
			cur.lines = append(cur.lines, pline{kind: ' '})
		case line[0] == ' ' || line[0] == '-' || line[0] == '+':
			cur.lines = append(cur.lines, pline{kind: line[0], text: line[1:]})
		default:
			return nil, patchErr("unexpected line %q inside a hunk", line)
		}
	}
	if err := finish(); err != nil {
		return nil, err
	}
	return hunks, nil
}

// Apply applies a unified diff to base. The diff must fit exactly where it
// says: every context and removed line has to match. It never guesses.
func Apply(base, patch string) (string, error) {
	hunks, err := parsePatch(patch)
	if err != nil {
		return "", err
	}
	if len(hunks) == 0 {
		return "", patchErr("no changes found in the patch")
	}
	lines := Split(base)
	var out strings.Builder
	pos := 0 // next unread line of base
	for n, h := range hunks {
		at := h.oldStart - 1 // index of the first old line
		if h.oldCount == 0 {
			at = h.oldStart // an empty range sits after the line it names
		}
		if at < pos {
			return "", patchErr("hunk %d overlaps the one before it", n+1)
		}
		if at > len(lines) {
			return "", patchErr("hunk %d starts at line %d, but the text has only %d lines", n+1, h.oldStart, len(lines))
		}
		for ; pos < at; pos++ {
			out.WriteString(lines[pos])
		}
		for _, l := range h.lines {
			switch l.kind {
			case ' ', '-':
				if pos >= len(lines) {
					return "", patchErr("hunk %d expects line %d, which does not exist", n+1, pos+1)
				}
				have := lines[pos]
				text, hasNL := strings.TrimSuffix(have, "\n"), strings.HasSuffix(have, "\n")
				if text != l.text || hasNL == l.noNewline {
					return "", patchErr("hunk %d does not match line %d (found %q)", n+1, pos+1, text)
				}
				if l.kind == ' ' {
					out.WriteString(have)
				}
				pos++
			case '+':
				out.WriteString(l.text)
				if !l.noNewline {
					out.WriteByte('\n')
				}
			}
		}
	}
	for ; pos < len(lines); pos++ {
		out.WriteString(lines[pos])
	}
	return out.String(), nil
}
