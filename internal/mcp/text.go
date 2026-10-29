package mcp

import (
	"html"
	"strings"
)

// SnippetText turns a search excerpt (HTML with <mark> around matches) into
// plain text, marking the matches with **.
func SnippetText(h string) string {
	h = strings.NewReplacer("<mark>", "**", "</mark>", "**").Replace(h)
	var b strings.Builder
	in := false
	for _, r := range h {
		switch {
		case r == '<':
			in = true
		case r == '>' && in:
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(html.UnescapeString(b.String())), " ")
}
