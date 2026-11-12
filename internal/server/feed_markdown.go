package server

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/hbroek/wako/internal/store"
)

// The change feed carries each entry twice: as HTML in <description>, for any
// feed reader, and as Markdown in <source:markdown> (the "source" namespace of
// scripting.com, written as Scripting News writes it: plain escaped text, not
// CDATA), for readers that render Markdown themselves and can show the
// pictures and links of a page's opening, which HTML descriptions lose.

const (
	sourceNS    = "https://source.scripting.com/"
	excerptMD   = 1200 // characters of a new page's Markdown shown in a feed entry
	excerptHard = 1600 // a single block longer than this is cut where it stands
)

// mdRef matches a Markdown link or image: [text](dest) and ![alt](dest).
var mdRef = regexp.MustCompile(`(!?)\[([^\]]*)\]\(([^)\s]+)\)`)

// bareImage is how a page names a library file: a name with an optional size.
var bareImage = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)([?#][wh]=?[0-9]{2,4})?$`)

var absoluteDest = regexp.MustCompile(`^(?i:[a-z][a-z0-9+.-]*:)`)

// feedMarkdown makes the Markdown of a page's opening readable away from the
// wiki: library pictures and attachments, and links to other pages, get
// absolute addresses (a bare "photo.jpg" means nothing in a feed reader).
func (s *Server) feedMarkdown(lc *linkCtx, sp *store.Space, src string, pageID int64, md, origin string) string {
	bound, _ := sp.Bindings(pageID)
	env := lc.env(sp.Slug)
	asset := func(name string) *store.AssetInfo {
		if a, ok := bound[name]; ok {
			return a
		}
		if a, err := sp.FindAssetByName(name); err == nil {
			return a
		}
		return nil
	}
	return mdRef.ReplaceAllStringFunc(md, func(m string) string {
		g := mdRef.FindStringSubmatch(m)
		bang, text, dest := g[1], g[2], g[3]
		if absoluteDest.MatchString(dest) || strings.HasPrefix(dest, "#") || strings.HasPrefix(dest, "//") {
			return m
		}
		if bang == "!" && strings.HasPrefix(dest, "/") { // an address on this site, written without the host
			return bang + "[" + text + "](" + origin + dest + ")"
		}
		if bn := bareImage.FindStringSubmatch(dest); bn != nil && (bang == "!" || strings.HasSuffix(strings.ToLower(bn[1]), ".pdf")) {
			if a := asset(bn[1]); a != nil {
				return bang + "[" + text + "](" + origin + s.assetURL(sp, a.ID, a.Name) + bn[2] + ")"
			}
			return m
		}
		if bang == "" {
			if ref, internal, exists := env.Link(src, dest); internal && exists {
				frag := ""
				if i := strings.Index(dest, "#"); i >= 0 {
					frag = dest[i:]
				}
				return "[" + text + "](" + origin + s.u(ref.Space, ref.Path) + frag + ")"
			}
		}
		return m
	})
}

// markdownExcerpt is the start of a page: whole blocks up to about max
// characters. A block that alone is far longer is cut where it stands, and an
// open code fence is closed. cut says whether anything was left out.
func markdownExcerpt(body string, max int) (text string, cut bool) {
	body = strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	if utf8.RuneCountInString(body) <= max {
		return body, false
	}
	var out []string
	size, fence := 0, ""
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if fence == "" && (strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")) {
			fence = t[:3]
		} else if fence != "" && strings.HasPrefix(t, fence) {
			fence = ""
		}
		n := utf8.RuneCountInString(line) + 1
		if fence == "" && t == "" && size > 0 && size+nextBlock(lines[i+1:]) > max {
			return strings.TrimSpace(strings.Join(out, "\n")), true
		}
		if size+n > excerptHard {
			text = strings.TrimSpace(strings.Join(out, "\n"))
			if fence != "" {
				text += "\n" + fence
			}
			return text, true
		}
		out = append(out, line)
		size += n
	}
	return strings.TrimSpace(strings.Join(out, "\n")), false
}

// nextBlock is the length of the block that starts at the first of lines.
func nextBlock(lines []string) int {
	n := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" && n > 0 {
			break
		}
		n += utf8.RuneCountInString(l) + 1
	}
	return n
}

// fenced wraps text in a code fence long enough not to be closed by the text.
func fenced(lang, text string) string {
	fence, run := "```", 0
	for _, r := range text {
		if r == '`' {
			run++
			for len(fence) <= run {
				fence += "`"
			}
		} else {
			run = 0
		}
	}
	return fence + lang + "\n" + text + "\n" + fence
}

// mdEscape keeps a title from being read as Markdown inside a link's brackets.
func mdEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`, "*", `\*`, "_", `\_`, "`", "\\`", "<", `\<`).Replace(s)
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// xmlText escapes text for element content and drops the characters XML cannot carry.
func xmlText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n':
			return r
		case r < 0x20 || r == 0xFFFE || r == 0xFFFF:
			return -1
		}
		return r
	}, strings.ToValidUTF8(strings.ReplaceAll(s, "\r\n", "\n"), "\uFFFD"))
	return xmlEscaper.Replace(s)
}
