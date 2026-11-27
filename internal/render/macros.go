package render

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Macros are written {{page.title}} and filled in when the page is shown; the
// stored Markdown keeps the macro. The variables are
//
//	page.title   the page title (front matter "title", else the first # heading, else the name)
//	page.path    the page's path, such as docs/setup
//	page.slug    the last part of the path
//	page.space   the space's name
//	page.KEY     any front matter key (page.owner for "owner: Sam")
//
// path, slug and space always come from the wiki, never from front matter. A
// name that is not known stays as written, and macros in code stay as written.
//
// A value is only ever plain text. It is put into the page after the Markdown
// has been read, as one literal piece, so it cannot start a link, a heading, an
// image or markup of any kind, and the page's HTML policy still applies on top.
// A value is not searched for further macros.

// Page says what the built-in macros stand for. Title is used when the page's
// text gives none.
type Page struct {
	Title, Path, Slug, Space string
}

var macroRe = regexp.MustCompile(`\{\{[ \t]*page\.([A-Za-z_][A-Za-z0-9_-]{0,63})[ \t]*\}\}`)

const maxExpanded = 128 << 10 // bytes of values written into one page

type macros struct {
	page Page
	meta Meta
	room int
}

func newMacros(p Page, meta Meta, body string) *macros {
	m := &macros{page: p, meta: meta, room: maxExpanded}
	if m.page.Slug == "" && p.Path != "" {
		m.page.Slug = p.Path[strings.LastIndex(p.Path, "/")+1:]
	}
	if meta["title"] == "" && m.page.Title == "" {
		if m.page.Title = Title(body); m.page.Title == "" {
			m.page.Title = m.page.Slug
		}
	}
	return m
}

func (m *macros) lookup(name string) (string, bool) {
	switch name = strings.ToLower(name); name {
	case "path":
		return m.page.Path, m.page.Path != ""
	case "slug":
		return m.page.Slug, m.page.Slug != ""
	case "space":
		return m.page.Space, m.page.Space != ""
	}
	if v := m.meta[name]; v != "" {
		return cleanValue(v), true
	}
	if name == "title" {
		return cleanValue(m.page.Title), m.page.Title != ""
	}
	return "", false
}

// expand replaces the macros in the text of doc.
func (m *macros) expand(doc ast.Node, src []byte) {
	if !bytes.Contains(src, []byte("{{")) {
		return
	}
	var parents []ast.Node
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		switch n.(type) {
		case *ast.CodeSpan, *ast.RawHTML:
			return ast.WalkSkipChildren, nil
		}
		if entering && n.FirstChild() != nil {
			parents = append(parents, n)
		}
		return ast.WalkContinue, nil
	})
	for _, p := range parents {
		mergeText(p)
		for c := p.FirstChild(); c != nil; {
			next := c.NextSibling()
			if t, ok := c.(*ast.Text); ok {
				m.expandText(t, src)
			}
			c = next
		}
	}
}

// mergeText joins neighbouring pieces of text that are next to each other in
// the source. The inline parser cuts text at characters that might have started
// markup (the underscore in page.my_key), and a macro must be seen whole.
func mergeText(parent ast.Node) {
	for c := parent.FirstChild(); c != nil; {
		t, ok := c.(*ast.Text)
		n, ok2 := c.NextSibling().(*ast.Text)
		if !ok || !ok2 || t.SoftLineBreak() || t.HardLineBreak() || t.IsRaw() != n.IsRaw() ||
			t.Segment.Stop != n.Segment.Start || n.Segment.Padding != 0 {
			c = c.NextSibling()
			continue
		}
		t.Segment.Stop = n.Segment.Stop
		t.SetSoftLineBreak(n.SoftLineBreak())
		t.SetHardLineBreak(n.HardLineBreak())
		parent.RemoveChild(parent, n)
	}
}

func (m *macros) expandText(t *ast.Text, src []byte) {
	seg := t.Segment
	v := seg.Value(src)
	hits := macroRe.FindAllSubmatchIndex(v, -1)
	if len(hits) == 0 {
		return
	}
	parent := t.Parent()
	if parent == nil {
		return
	}
	// seg.Value starts with seg.Padding spaces (from a tab) that are not in the
	// source, so an offset v[i] sits at seg.Start+i-pad, never before seg.Start.
	pad := seg.Padding
	at := func(i int) int { return seg.Start + max(i-pad, 0) }
	var pieces []ast.Node
	last, did := 0, false
	piece := func(from, to int) {
		if to <= from {
			return
		}
		s := text.NewSegment(at(from), at(to))
		if from == 0 {
			s.Padding = pad
		}
		pieces = append(pieces, ast.NewTextSegment(s))
	}
	for _, h := range hits {
		if h[0] > 0 && v[h[0]-1] == '\\' {
			continue // \{{page.title}} stands for itself
		}
		val, ok := m.lookup(string(v[h[2]:h[3]]))
		if !ok || len(val) > m.room {
			continue
		}
		m.room -= len(val)
		piece(last, h[0])
		s := ast.NewString([]byte(val))
		s.SetRaw(true) // escaped as HTML, but no entities or backslash escapes are read from it
		pieces = append(pieces, s)
		last, did = h[1], true
	}
	if !did {
		return
	}
	piece(last, len(v))
	// The line break that ended the text belongs after the last piece.
	if tail, ok := pieces[len(pieces)-1].(*ast.Text); ok {
		tail.SetSoftLineBreak(t.SoftLineBreak())
		tail.SetHardLineBreak(t.HardLineBreak())
	} else {
		pieces = append(pieces, func() ast.Node {
			e := ast.NewTextSegment(text.NewSegment(seg.Stop, seg.Stop))
			e.SetSoftLineBreak(t.SoftLineBreak())
			e.SetHardLineBreak(t.HardLineBreak())
			return e
		}())
	}
	for _, p := range pieces {
		parent.InsertBefore(parent, t, p)
	}
	parent.RemoveChild(parent, t)
}
