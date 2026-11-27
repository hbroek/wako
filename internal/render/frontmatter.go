package render

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// Front matter is a block at the very start of a page:
//
//	---
//	title: Release plan
//	owner: Sam
//	tags: [a, b]
//	kanban: true
//	---
//
// It is never shown on the page. Its keys become macro variables
// ({{page.owner}}), `title` replaces the page's title, and `kanban: true` turns
// the page into a board (the kanban extension reads that itself).
//
// This is a small, flat subset of YAML on purpose: "key: value" lines whose
// values are plain, single- or double-quoted text, a [flow, list], a block of
// "- items" or a | / > block of text. Lists and blocks are joined into one text
// value. Anything nested is ignored, and anything that is not of this shape
// means the block is not front matter at all (it stays ordinary Markdown, so a
// page that opens with a horizontal rule keeps working). No anchors, tags or
// other YAML features exist, so there is nothing to expand or to bomb.

const (
	maxFrontMatter = 16 << 10 // bytes between the fences
	maxMetaKeys    = 64
	maxMetaValue   = 2000 // characters
)

var metaKey = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]{0,63}):(?:[ \t]+(.*))?$`)

// Meta is the front matter of a page, with keys in lower case.
type Meta map[string]string

func isFence(line string) bool {
	return strings.TrimRight(line, " \t\r\n") == "---"
}

// FrontMatter reads the front matter at the start of src. end is the offset of
// the first byte after the block (0 when there is none).
func FrontMatter(src string) (meta Meta, end int) {
	first, _, found := strings.Cut(src, "\n")
	if !found || !isFence(first) {
		return nil, 0
	}
	meta = Meta{}
	var (
		key   string   // the key a following list or block belongs to
		items []string // "- item" lines, or the lines of a | or > block
		block bool     // key opened a | or > block
	)
	flush := func() {
		if key != "" && len(items) > 0 {
			sep := ", "
			if block {
				sep = " "
			}
			meta.set(key, strings.Join(items, sep))
		}
		key, items, block = "", nil, false
	}
	for pos := len(first) + 1; pos < len(src); {
		line := src[pos:]
		next := len(src)
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line, next = line[:i], pos+i+1
		}
		if next > maxFrontMatter {
			return nil, 0
		}
		t := strings.TrimRight(line, " \t\r")
		switch {
		case t == "---" || t == "...":
			flush()
			return meta, next
		case strings.TrimSpace(t) == "" || strings.HasPrefix(strings.TrimSpace(t), "#"):
			// blank or a comment
		case key != "" && (t[0] == ' ' || t[0] == '\t' || strings.HasPrefix(t, "- ") || t == "-"):
			s := strings.TrimSpace(t)
			switch {
			case block:
				items = append(items, s)
			case strings.HasPrefix(s, "- ") || s == "-":
				items = append(items, scalar(strings.TrimSpace(strings.TrimPrefix(s, "-"))))
			}
			// other indented text is a nested map or a wrapped value: ignored
		default:
			m := metaKey.FindStringSubmatch(t)
			if m == nil {
				return nil, 0 // not front matter
			}
			flush()
			v := strings.TrimSpace(m[2])
			switch {
			case v == "":
				key = strings.ToLower(m[1])
			case v[0] == '|' || v[0] == '>':
				key, block = strings.ToLower(m[1]), true
			case v[0] == '{':
				// a flow map: nothing to expose
			case v[0] == '[':
				meta.set(m[1], flowList(v))
			default:
				meta.set(m[1], scalar(v))
			}
		}
		pos = next
	}
	return nil, 0 // never closed
}

func (m Meta) set(key, val string) {
	key = strings.ToLower(key)
	if _, have := m[key]; !have && len(m) >= maxMetaKeys {
		return
	}
	if val = cleanValue(val); val != "" {
		m[key] = val
	} else {
		delete(m, key)
	}
}

// cleanValue makes a value safe to show: one line, no control characters, no
// characters that reorder text, and a bounded length.
func cleanValue(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		switch {
		case r == utf8.RuneError, unicode.IsControl(r):
			r = ' '
		case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069, r == 0x200E, r == 0x200F, r == 0x061C:
			continue // bidirectional controls
		}
		if n++; n > maxMetaValue {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// scalar reads one value: quoted text, or plain text up to a " #" comment.
func scalar(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	switch v[0] {
	case '"':
		for i := 1; i < len(v); i++ {
			if v[i] == '\\' {
				i++
			} else if v[i] == '"' {
				if s, err := strconv.Unquote(v[:i+1]); err == nil {
					return s
				}
				return v[1:i]
			}
		}
		return strings.Trim(v, `"`)
	case '\'':
		var b strings.Builder
		for i := 1; i < len(v); i++ {
			if v[i] == '\'' {
				if i+1 < len(v) && v[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				return b.String()
			}
			b.WriteByte(v[i])
		}
		return strings.Trim(v, `'`)
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	if v == "~" || v == "null" {
		return ""
	}
	return strings.TrimSpace(v)
}

// flowList reads "[a, b, 'c d']" as "a, b, c d".
func flowList(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.LastIndex(v, "]"); i >= 0 {
		v = v[:i]
	}
	v = strings.TrimPrefix(v, "[")
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = scalar(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

// StripFrontMatter returns src without its front matter.
func StripFrontMatter(src string) string {
	if _, end := FrontMatter(src); end > 0 {
		return src[end:]
	}
	return src
}

// frontMatterNode is the block the parser makes for front matter, so that the
// rest of the page is parsed (and rendered, and searched for links and
// pictures) without it. It renders as nothing.
type frontMatterNode struct {
	ast.BaseBlock
	end int
}

var kindFrontMatter = ast.NewNodeKind("FrontMatter")

func (n *frontMatterNode) Kind() ast.NodeKind { return kindFrontMatter }
func (n *frontMatterNode) IsRaw() bool        { return true }
func (n *frontMatterNode) Dump(src []byte, level int) {
	ast.DumpHelper(n, src, level, nil, nil)
}

type frontMatterParser struct{}

func (frontMatterParser) Trigger() []byte { return []byte{'-'} }

func (frontMatterParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, seg := reader.PeekLine()
	if seg.Start != 0 || parent.Kind() != ast.KindDocument || !isFence(string(line)) {
		return nil, parser.NoChildren
	}
	_, end := FrontMatter(string(reader.Source()))
	if end == 0 {
		return nil, parser.NoChildren
	}
	return &frontMatterNode{end: end}, parser.NoChildren
}

func (frontMatterParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	_, seg := reader.PeekLine()
	reader.AdvanceToEOL()
	if seg.Stop >= node.(*frontMatterNode).end {
		return parser.Close
	}
	return parser.Continue | parser.NoChildren
}

func (frontMatterParser) Close(node ast.Node, reader text.Reader, pc parser.Context) {
	node.Parent().RemoveChild(node.Parent(), node)
}

func (frontMatterParser) CanInterruptParagraph() bool { return false }
func (frontMatterParser) CanAcceptIndentedLine() bool { return false }
