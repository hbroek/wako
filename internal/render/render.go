// Package render turns Markdown into sanitised HTML.
package render

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var md = goldmark.New(
	// CommonMark plus the four GitHub Flavored Markdown extensions: tables,
	// strikethrough, task lists and bare-URL autolinks. Footnotes and the rest
	// stay off, so that syntax shows as plain text.
	goldmark.WithExtensions(extension.GFM),
	// Task list items show an emoji instead of a checkbox (this beats the
	// extension's own renderer, which has a lower priority number to lose).
	goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(taskEmoji{}, 100))),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

var policy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	// Links inside the wiki (pages, original images) are ours; only absolute
	// links to other sites get rel="nofollow".
	p.RequireNoFollowOnLinks(false)
	p.RequireNoFollowOnFullyQualifiedLinks(true)
	p.AllowAttrs("id").OnElements("h1", "h2", "h3", "h4", "h5", "h6")
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^language-[\w+-]+$`)).OnElements("code")
	// Set by HTMLWith for images that have a known size.
	p.AllowAttrs("width", "height").Matching(regexp.MustCompile(`^\d{1,5}$`)).OnElements("img")
	p.AllowAttrs("loading").Matching(regexp.MustCompile(`^(lazy|eager)$`)).OnElements("img")
	p.AllowAttrs("decoding").Matching(regexp.MustCompile(`^async$`)).OnElements("img")
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^missing$`)).OnElements("a")
	p.AllowAttrs("style").Matching(regexp.MustCompile(`^--r:\d{1,4}(\.\d{1,4})?$`)).OnElements("img")
	// The emoji that stands for a task list item's box (see taskEmoji).
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^task$`)).OnElements("span")
	p.AllowAttrs("role").Matching(regexp.MustCompile(`^img$`)).OnElements("span")
	p.AllowAttrs("aria-label").Matching(regexp.MustCompile(`^(done|not done)$`)).OnElements("span")
	// Column alignment from a table's separator row.
	p.AllowAttrs("style").Matching(regexp.MustCompile(`^text-align:\s*(left|right|center)$`)).OnElements("th", "td")
	return p
}()

// taskEmoji draws the box of a task list item as an emoji: no form control
// is ever written to a page.
type taskEmoji struct{}

func (taskEmoji) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(east.KindTaskCheckBox, func(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			if n.(*east.TaskCheckBox).IsChecked {
				w.WriteString(`<span class="task" role="img" aria-label="done">✅</span> `)
			} else {
				w.WriteString(`<span class="task" role="img" aria-label="not done">⬜</span> `)
			}
		}
		return ast.WalkContinue, nil
	})
}

// Image says how one library image appears on the page.
type Image struct {
	Src           string // URL for the <img>; may be a smaller copy
	Link          string // if set, the image links to this full-size URL
	Width, Height int    // pixel size of Src; 0 when unknown
}

// Resolver maps a bare image name to how it should be shown.
type Resolver func(name string) (Image, bool)

// bareName matches a reference that is just a library file name: no scheme,
// no directory, nothing that is a path or URL in its own right.
var bareName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Options tell Render how to resolve what a page refers to.
type Options struct {
	Images Resolver  // bare image names
	Links  LinkCheck // marks links to pages that do not exist
}

// HTML renders src to sanitised HTML.
func HTML(src string) (string, error) { return Render(src, Options{}) }

func insideLink(n ast.Node) bool {
	for p := n.Parent(); p != nil; p = p.Parent() {
		if _, ok := p.(*ast.Link); ok {
			return true
		}
	}
	return false
}

// HTMLWith renders src. Image references that are bare names are rewritten
// through resolve (names it cannot resolve stay as written). Every image gets
// decoding="async" and, except the first, loading="lazy"; a resolved image also
// gets its width and height and an aspect ratio, so the page does not shift as
// pictures load, and is wrapped in a link to the full-size picture when a
// smaller copy is shown.
func HTMLWith(src string, resolve Resolver) (string, error) {
	return Render(src, Options{Images: resolve})
}

// Render renders src with the given resolvers; see HTMLWith for what happens to images.
func Render(src string, o Options) (string, error) {
	resolve := o.Images
	b := []byte(src)
	doc := md.Parser().Parse(text.NewReader(b))
	type wrap struct {
		img  *ast.Image
		link string
	}
	var wraps []wrap
	n := 0
	ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if l, ok := node.(*ast.Link); ok && entering && o.Links != nil {
			// A link to a page that does not exist gets a class, so it can be styled.
			d := string(l.Destination)
			if !externalRe.MatchString(d) {
				if internal, exists := o.Links(strings.SplitN(strings.SplitN(d, "#", 2)[0], "?", 2)[0]); internal && !exists {
					l.SetAttributeString("class", []byte("missing"))
					if len(l.Title) == 0 {
						l.Title = []byte("This page does not exist yet")
					}
				}
			}
			return ast.WalkContinue, nil
		}
		img, ok := node.(*ast.Image)
		if !ok || !entering {
			return ast.WalkContinue, nil
		}
		first := n == 0
		n++
		if resolve != nil && bareName.Match(img.Destination) {
			if im, ok := resolve(string(img.Destination)); ok {
				img.Destination = []byte(im.Src)
				if im.Width > 0 && im.Height > 0 {
					img.SetAttributeString("width", []byte(strconv.Itoa(im.Width)))
					img.SetAttributeString("height", []byte(strconv.Itoa(im.Height)))
					img.SetAttributeString("style", []byte(fmt.Sprintf("--r:%.4f", float64(im.Width)/float64(im.Height))))
				}
				if im.Link != "" && !insideLink(img) {
					wraps = append(wraps, wrap{img, im.Link})
				}
			}
		}
		img.SetAttributeString("decoding", []byte("async"))
		if !first { // the first image is usually the largest thing in view
			img.SetAttributeString("loading", []byte("lazy"))
		}
		return ast.WalkContinue, nil
	})
	for _, w := range wraps {
		link := ast.NewLink()
		link.Destination = []byte(w.link)
		parent := w.img.Parent()
		parent.ReplaceChild(parent, w.img, link)
		link.AppendChild(link, w.img)
	}
	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, b, doc); err != nil {
		return "", err
	}
	return policy.Sanitize(buf.String()), nil
}

// ImageRefs returns the bare image names src refers to, in order of first use.
// Images inside code blocks or code spans are not references.
func ImageRefs(src string) []string {
	b := []byte(src)
	doc := md.Parser().Parse(text.NewReader(b))
	seen := map[string]bool{}
	var out []string
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if img, ok := n.(*ast.Image); ok && entering && bareName.Match(img.Destination) {
			if name := string(img.Destination); !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
		return ast.WalkContinue, nil
	})
	return out
}

// Title returns the text of the first level-1 ATX heading, or "" if none.
func Title(src string) string {
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.Trim(strings.TrimPrefix(line, "# "), "# "))
		}
	}
	return ""
}

// ---- renaming ----

// codeRanges returns the byte ranges of src that are code or raw HTML. Image
// syntax inside them is text, not an image, so it must never be rewritten.
func codeRanges(src []byte, doc ast.Node) [][2]int {
	var out [][2]int
	lines := func(n ast.Node) {
		l := n.Lines()
		if l == nil || l.Len() == 0 {
			return
		}
		out = append(out, [2]int{l.At(0).Start, l.At(l.Len() - 1).Stop})
	}
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.FencedCodeBlock:
			if l := n.Lines(); l != nil && l.Len() > 0 {
				// Include the fence lines, which may carry an info string. The
				// content starts at a line start, so the opening fence is the
				// line before it.
				start := l.At(0).Start
				if start > 0 {
					start = bytes.LastIndexByte(src[:start-1], '\n') + 1
				}
				stop := l.At(l.Len() - 1).Stop
				if stop > 0 && src[stop-1] != '\n' { // segment stops before its newline
					if e := bytes.IndexByte(src[stop:], '\n'); e >= 0 {
						stop += e + 1
					} else {
						stop = len(src)
					}
				}
				if e := bytes.IndexByte(src[stop:], '\n'); e >= 0 { // the closing fence line
					stop += e
				} else {
					stop = len(src)
				}
				out = append(out, [2]int{start, stop})
			}
		case *ast.CodeBlock, *ast.HTMLBlock:
			lines(n)
		case *ast.CodeSpan:
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				if t, ok := c.(*ast.Text); ok {
					out = append(out, [2]int{t.Segment.Start, t.Segment.Stop})
				}
			}
		case *ast.RawHTML:
			for i := 0; i < n.Segments.Len(); i++ {
				s := n.Segments.At(i)
				out = append(out, [2]int{s.Start, s.Stop})
			}
		}
		return ast.WalkContinue, nil
	})
	return out
}

// RewriteImageRef renames the image reference oldName (a bare library name) to
// newName throughout src: inline images, `![a][ref]` definitions, and nothing
// inside code or raw HTML. It then checks its own work against the parsed
// document and reports false if the result does not reference exactly what it
// should, in which case the caller must leave the text alone.
func RewriteImageRef(src, oldName, newName string) (string, bool) {
	return rewriteImage(src, oldName, newName, ImageRefs)
}

// ImageDests returns the destinations of the images in src that point at files
// rather than at other sites: bare names and relative paths such as
// "images/shot.png". Images in code are not images.
func ImageDests(src string) []string {
	b := []byte(src)
	doc := md.Parser().Parse(text.NewReader(b))
	seen := map[string]bool{}
	var out []string
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if img, ok := n.(*ast.Image); ok && entering {
			d := string(img.Destination)
			if d != "" && !externalRe.MatchString(d) && !strings.HasPrefix(d, "/") && !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
		return ast.WalkContinue, nil
	})
	return out
}

// RewriteImageDest is RewriteImageRef for any image destination as written,
// such as "images/shot.png" (used when importing plain Markdown folders).
func RewriteImageDest(src, oldDest, newName string) (string, bool) {
	return rewriteImage(src, oldDest, newName, ImageDests)
}

func rewriteImage(src, oldName, newName string, refs func(string) []string) (string, bool) {
	before := refs(src)
	has := false
	for _, n := range before {
		if n == oldName {
			has = true
		}
	}
	if !has {
		return src, true // nothing to rename
	}
	b := []byte(src)
	doc := md.Parser().Parse(text.NewReader(b))
	masked := append([]byte(nil), b...)
	for _, r := range codeRanges(b, doc) {
		for i := r[0]; i < r[1] && i < len(masked); i++ {
			masked[i] = 0
		}
	}

	old := regexp.QuoteMeta(oldName)
	title := `(?:[ \t]+(?:"[^"]*"|'[^']*'|\([^)]*\)))?`
	inline := regexp.MustCompile(`(!\[(?:[^\[\]\\]|\\.|\[[^\]]*\])*\]\([ \t]*<?)(` + old + `)(>?` + title + `[ \t]*\))`)
	def := regexp.MustCompile(`(?m)(^ {0,3}\[[^\]]+\]:[ \t]*<?)(` + old + `)(>?` + title + `[ \t]*$)`)

	type edit struct{ start, stop int }
	var edits []edit
	for _, re := range []*regexp.Regexp{inline, def} {
		for _, m := range re.FindAllSubmatchIndex(masked, -1) {
			edits = append(edits, edit{m[4], m[5]}) // group 2: the name
		}
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var out strings.Builder
	last := 0
	for _, e := range edits {
		out.Write(b[last:e.start])
		out.WriteString(newName)
		last = e.stop
	}
	out.Write(b[last:])
	result := out.String()

	// Verify: the references must be the old ones with oldName replaced.
	want := map[string]bool{}
	for _, n := range before {
		if n == oldName {
			n = newName
		}
		want[n] = true
	}
	got := map[string]bool{}
	for _, n := range refs(result) {
		got[n] = true
	}
	if len(want) != len(got) {
		return src, false
	}
	for n := range want {
		if !got[n] {
			return src, false
		}
	}
	return result, true
}
