package render

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Links in pages are ordinary Markdown links, [text](destination). This file
// finds them and, when rendering, marks the ones that lead nowhere.

var externalRe = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9+.\-]*:|//|#|\?)`)

// Links returns the destinations in src that may point at wiki pages (with any
// #fragment or ?query removed), in order of first use. External URLs, anchors
// and images are not included, and neither is anything in code.
func Links(src string) []string {
	b := []byte(src)
	doc := md.Parser().Parse(text.NewReader(b))
	seen := map[string]bool{}
	var out []string
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		l, ok := n.(*ast.Link)
		if !ok || !entering {
			return ast.WalkContinue, nil
		}
		d := string(l.Destination)
		if externalRe.MatchString(d) {
			return ast.WalkContinue, nil
		}
		if i := strings.IndexAny(d, "#?"); i >= 0 {
			d = d[:i]
		}
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
		return ast.WalkContinue, nil
	})
	return out
}

// LinkCheck says, for a link destination as written, whether it points at a
// page (internal) and whether that page exists.
type LinkCheck func(dest string) (internal, exists bool)

// CodeMask returns src with code and raw HTML blanked out (bytes set to 0), so
// patterns matched against it cannot hit text that is not Markdown.
func CodeMask(src string) []byte {
	b := []byte(src)
	doc := md.Parser().Parse(text.NewReader(b))
	masked := bytes.Clone(b)
	for _, r := range codeRanges(b, doc) {
		for i := r[0]; i < r[1] && i < len(masked); i++ {
			masked[i] = 0
		}
	}
	return masked
}
