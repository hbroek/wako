package wiki

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/hbroek/wako/internal/render"
)

// Mapper says where a page ends up after a change. It returns its arguments
// for pages that do not move.
type Mapper func(space, path string) (newSpace, newPath string)

var (
	// [text](destination ...) and ![alt](...): only the destination is captured.
	linkRe = regexp.MustCompile(`(!?)\[(?:[^\[\]\\]|\\.|\[[^\]]*\])*\]\([ \t]*(<[^>\n]*>|[^\s)<]+)`)
	defRe  = regexp.MustCompile(`(?m)^ {0,3}\[[^\]]+\]:[ \t]*(<[^>\n]*>|\S+)`)
)

type edit struct {
	start, stop int
	text        string
}

// Rewrite updates the links in body, the Markdown of the page at src, for a
// change described by m. The page itself may move too (newSrc). Links are
// resolved as they were before the change (the environment's indexes), and the
// text is edited so that they resolve to where their targets now are. Relative
// links are recomputed from the page's new location, and absolute
// ones get the new path; "[ref]: destination" definitions are handled the same
// way.
//
// Nothing inside code or raw HTML is touched, and neither are external links.
func (e *Env) Rewrite(src, newSrc, body string, m Mapper) (string, bool) {
	masked := render.CodeMask(body)
	var edits []edit

	moved := newSrc != src
	rewriteDest := func(start, stop int) {
		dest := body[start:stop]
		pathPart, suffix := dest, ""
		if i := strings.IndexAny(dest, "#?"); i >= 0 {
			pathPart, suffix = dest[:i], dest[i:]
		}
		ref, internal, exists := e.Link(src, pathPart)
		if !internal || !exists {
			return
		}
		ns, np := m(ref.Space, ref.Path)
		if ns == ref.Space && np == ref.Path && !moved {
			return
		}
		// Leave alone any link that still reaches its target from where the page
		// now is; only broken ones are edited.
		if again, ok, _ := e.Link(newSrc, pathPart); ok && again.Space == ns && again.Path == np {
			return
		}
		ext := ""
		if strings.HasSuffix(pathPart, ".md") {
			ext = ".md"
		}
		var next string
		if strings.HasPrefix(pathPart, "/") {
			if ns == ref.Space && np == ref.Path {
				return // absolute links do not depend on where the source page is
			}
			next = e.Base + "/" + ns + "/" + np + ext
		} else {
			if ns != e.Space && ns != ref.Space {
				return
			}
			dir := path.Dir(newSrc)
			if dir == "." {
				dir = ""
			}
			next = relPath(dir, np) + ext
		}
		if next+suffix != dest {
			edits = append(edits, edit{start, stop, next + suffix})
		}
	}
	destRange := func(loc []int, g int) (int, int) {
		start, stop := loc[2*g], loc[2*g+1]
		if body[start] == '<' { // <destination>
			return start + 1, stop - 1
		}
		return start, stop
	}
	for _, loc := range linkRe.FindAllSubmatchIndex(masked, -1) {
		if loc[3] > loc[2] { // group 1 matched "!": an image
			continue
		}
		start, stop := destRange(loc, 2)
		rewriteDest(start, stop)
	}
	for _, loc := range defRe.FindAllSubmatchIndex(masked, -1) {
		start, stop := destRange(loc, 1)
		rewriteDest(start, stop)
	}

	if len(edits) == 0 {
		return body, false
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var out strings.Builder
	last := 0
	for _, ed := range edits {
		if ed.start < last {
			continue // overlapping matches: keep the first
		}
		out.WriteString(body[last:ed.start])
		out.WriteString(ed.text)
		last = ed.stop
	}
	out.WriteString(body[last:])
	return out.String(), true
}

// relPath is the relative link from a page whose directory is fromDir to the
// page at to, in the form a browser resolves from the page's URL.
func relPath(fromDir, to string) string {
	var f, t []string
	if fromDir != "" {
		f = strings.Split(fromDir, "/")
	}
	t = strings.Split(to, "/")
	// The last segment of the target is always written out, so it is never
	// shared with the directory: "docs" seen from docs/ is "../docs".
	i := 0
	for i < len(f) && i < len(t)-1 && f[i] == t[i] {
		i++
	}
	parts := make([]string, 0, len(f)-i+len(t)-i)
	for range f[i:] {
		parts = append(parts, "..")
	}
	parts = append(parts, t[i:]...)
	return strings.Join(parts, "/")
}
