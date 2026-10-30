package wiki

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

var (
	shortRe    = regexp.MustCompile(`\[\[([^\[\]\n|]+?)(?:\|([^\[\]\n]*))?\]\]`)
	codeSpanRe = regexp.MustCompile("`+[^`]*`+")
	slugBadRe  = regexp.MustCompile(`[^a-z0-9._-]+`)
	fenceRe    = regexp.MustCompile("^ {0,3}(```|~~~)")
)

// Paths lists the paths of every page, sorted.
func (ix *Index) Paths() []string {
	out := make([]string, 0, len(ix.titles))
	for p := range ix.titles {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// slugOf turns words into a page slug: "New page" is "new-page".
func slugOf(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Trim(slugBadRe.ReplaceAllString(s, "-"), "-._")
}

// ExpandShortLinks rewrites the [[Page name]] and [[Page name|label]] shortcuts
// in body, written on the page at src, into ordinary Markdown links to a page of
// the same space, so the stored text reads the same in any Markdown tool.
//
// A name matches an existing page by title or by its last path segment, ignoring
// case; with several matches the one beside src wins, then the shortest path.
// A name with "/" is a path from the space root. A name that matches nothing
// points at a new page next to src (which then shows as a link to a missing
// page). Shortcuts in code, images (![[x]]), and names that cannot be a valid
// slug, are left as written. valid says whether a segment is usable as a slug.
func ExpandShortLinks(src, body string, ix *Index, valid func(string) bool) string {
	if !strings.Contains(body, "[[") {
		return body
	}
	dir := path.Dir(src)
	if dir == "." {
		dir = ""
	}
	paths := ix.Paths()
	resolve := func(name string) (string, bool) {
		if strings.Contains(name, "/") {
			var segs []string
			for _, seg := range strings.Split(strings.Trim(name, "/"), "/") {
				seg = slugOf(seg)
				if seg == "" || !valid(seg) {
					return "", false
				}
				segs = append(segs, seg)
			}
			want := strings.Join(segs, "/")
			for _, p := range paths {
				if strings.EqualFold(p, want) {
					return p, true
				}
			}
			return want, true
		}
		slug := slugOf(name)
		if slug == "" || !valid(slug) {
			return "", false
		}
		best, bestScore := "", 0
		for _, p := range paths {
			last := p[strings.LastIndex(p, "/")+1:]
			if !strings.EqualFold(last, slug) && !strings.EqualFold(ix.titles[p], strings.TrimSpace(name)) {
				continue
			}
			score := 1
			if path.Dir(p) == "." && dir == "" || path.Dir(p) == dir {
				score = 2
			}
			if score > bestScore || score == bestScore && len(p) < len(best) {
				best, bestScore = p, score
			}
		}
		if best != "" {
			return best, true
		}
		return strings.TrimPrefix(dir+"/"+slug, "/"), true
	}

	expand := func(m string) string {
		sub := shortRe.FindStringSubmatch(m)
		name, label := strings.TrimSpace(sub[1]), strings.TrimSpace(sub[2])
		target, ok := resolve(name)
		if !ok {
			return m
		}
		if label == "" {
			label = name
		}
		return "[" + label + "](" + relPath(dir, target) + ")"
	}

	lines := strings.SplitAfter(body, "\n")
	fence := ""
	for i, line := range lines {
		if m := fenceRe.FindStringSubmatch(line); m != nil {
			switch {
			case fence == "":
				fence = m[1]
			case fence == m[1]:
				fence = ""
			}
			continue
		}
		if fence != "" || !strings.Contains(line, "[[") {
			continue
		}
		// Work on the stretches between code spans only.
		var out strings.Builder
		last := 0
		for _, loc := range codeSpanRe.FindAllStringIndex(line, -1) {
			out.WriteString(expandProse(line[last:loc[0]], expand))
			out.WriteString(line[loc[0]:loc[1]])
			last = loc[1]
		}
		out.WriteString(expandProse(line[last:], expand))
		lines[i] = out.String()
	}
	return strings.Join(lines, "")
}

// expandProse applies f to each shortcut in s, except an escaped (\[[) or image (![[) one.
func expandProse(s string, f func(string) string) string {
	if !strings.Contains(s, "[[") {
		return s
	}
	var out strings.Builder
	last := 0
	for _, loc := range shortRe.FindAllStringIndex(s, -1) {
		out.WriteString(s[last:loc[0]])
		if loc[0] > 0 && (s[loc[0]-1] == '\\' || s[loc[0]-1] == '!') {
			out.WriteString(s[loc[0]:loc[1]])
		} else {
			out.WriteString(f(s[loc[0]:loc[1]]))
		}
		last = loc[1]
	}
	out.WriteString(s[last:])
	return out.String()
}
