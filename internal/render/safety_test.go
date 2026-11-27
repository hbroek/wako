package render

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

var forbiddenTags = map[string]bool{
	"script": true, "style": true, "iframe": true, "frame": true, "frameset": true, "object": true, "embed": true, "applet": true,
	"form": true, "input": true, "button": true, "textarea": true, "select": true, "option": true, "meta": true, "link": true,
	"base": true, "svg": true, "math": true, "template": true, "noscript": true, "audio": true, "video": true, "source": true,
	"track": true, "canvas": true, "dialog": true, "slot": true, "portal": true, "title": true, "xmp": true, "plaintext": true,
	"listing": true, "isindex": true, "marquee": true, "bgsound": true,
}

// unsafeHTML returns a description of everything in rendered output that could run
// script or load something active, or "" if the output is safe to put in a page.
func unsafeHTML(out string) string {
	doc, err := html.Parse(strings.NewReader(out))
	if err != nil {
		return "unparseable: " + err.Error()
	}
	var bad []string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if forbiddenTags[n.Data] {
				bad = append(bad, "<"+n.Data+">")
			}
			for _, a := range n.Attr {
				key, val := strings.ToLower(a.Key), strings.ToLower(strings.Join(strings.Fields(a.Val), ""))
				switch {
				case strings.HasPrefix(key, "on"):
					bad = append(bad, "handler "+a.Key)
				case key == "href" || key == "src" || key == "action" || key == "formaction" || key == "poster" || key == "background" || key == "data" || key == "xlink:href" || key == "srcset":
					if strings.HasPrefix(val, "javascript:") || strings.HasPrefix(val, "vbscript:") || strings.HasPrefix(val, "data:") || strings.HasPrefix(val, "livescript:") || strings.HasPrefix(val, "mhtml:") {
						bad = append(bad, key+"="+a.Val)
					}
				case key == "style":
					if (n.Data == "th" || n.Data == "td") && (val == "text-align:left" || val == "text-align:right" || val == "text-align:center") {
						break
					}
					if n.Data != "img" || !strings.HasPrefix(val, "--r:") || strings.ContainsAny(val, ";(){}<>\\/@") {
						bad = append(bad, "style="+a.Val)
					}
				case key == "srcdoc" || key == "formmethod" || key == "http-equiv" || key == "is":
					bad = append(bad, key)
				case key == "class" && !(a.Val == "missing" || a.Val == "task" && n.Data == "span" || strings.HasPrefix(a.Val, "language-") || kanbanClass(n.Data, a.Val)):
					bad = append(bad, "class="+a.Val)
				case key == "target" || key == "rel" && n.Data != "a":
					bad = append(bad, key+"="+a.Val)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return strings.Join(bad, ", ")
}

// A collection of well-known ways to smuggle script into Markdown and HTML.
var attacks = []string{
	`<script>alert(1)</script>`,
	`<SCRIPT SRC=//evil.example/x.js></SCRIPT>`,
	`<img src=x onerror=alert(1)>`,
	`<img src="x" onerror="alert(1)" onload="alert(2)">`,
	`<IMG SRC=javascript:alert('XSS')>`,
	`<img src=" &#14;  javascript:alert(1);">`,
	`<img src=x:alert(alt) onerror=eval(src) alt=0>`,
	`<svg onload=alert(1)>`,
	`<svg><script>alert(1)</script></svg>`,
	`<svg><animate onbegin=alert(1) attributeName=x dur=1s>`,
	`<math><mi xlink:href="javascript:alert(1)">x</mi></math>`,
	`<iframe src="javascript:alert(1)"></iframe>`,
	`<iframe srcdoc="<script>alert(1)</script>"></iframe>`,
	`<object data="javascript:alert(1)"></object>`,
	`<embed src="data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==">`,
	`<form action="javascript:alert(1)"><input type=submit></form>`,
	`<form><button formaction="javascript:alert(1)">x</button></form>`,
	`<input autofocus onfocus=alert(1)>`,
	`<body onload=alert(1)>`,
	`<details open ontoggle=alert(1)>`,
	`<meta http-equiv="refresh" content="0;url=javascript:alert(1)">`,
	`<link rel=stylesheet href="javascript:alert(1)">`,
	`<base href="javascript:alert(1)//">`,
	`<style>@import 'javascript:alert(1)';</style>`,
	`<style>body{background:url(javascript:alert(1))}</style>`,
	`<div style="background:url(javascript:alert(1))">x</div>`,
	`<div style="width:expression(alert(1))">x</div>`,
	`<a href="javascript:alert(1)">x</a>`,
	`<a href="  JaVaScRiPt:alert(1)">x</a>`,
	"<a href=\"java\tscript:alert(1)\">x</a>",
	"<a href=\"java&#x0A;script:alert(1)\">x</a>",
	`<a href="&#106;avascript:alert(1)">x</a>`,
	`<a href="data:text/html,<script>alert(1)</script>">x</a>`,
	`<a href="vbscript:msgbox(1)">x</a>`,
	`<a href="x" target="_blank" rel="opener">x</a>`,
	`<video src=x onerror=alert(1)></video>`,
	`<audio><source src=x onerror=alert(1)></audio>`,
	`<template><script>alert(1)</script></template>`,
	`<noscript><p title="</noscript><img src=x onerror=alert(1)>">`,
	`<xmp><script>alert(1)</script></xmp>`,
	`<textarea><script>alert(1)</script></textarea>`,
	`<title><script>alert(1)</script></title>`,
	`<marquee onstart=alert(1)>x</marquee>`,
	`<x onclick=alert(1)>click</x>`,
	`<a href=x onmouseover=alert(1)>x</a>`,
	`<p onclick="alert(1)">x</p>`,
	`<table background="javascript:alert(1)"><tr><td>x</td></tr></table>`,
	`<div id=x tabindex=0 onfocus=alert(1)>x</div>`,
	`<scr<script>ipt>alert(1)</scr</script>ipt>`,
	`<<script>alert(1)//<</script>`,
	`[x](javascript:alert(1))`,
	`[x](JAVASCRIPT:alert(1))`,
	`[x](&#106;avascript:alert(1))`,
	`[x](<javascript:alert(1)>)`,
	"[x](java\nscript:alert(1))",
	`[x](vbscript:msgbox(1))`,
	`[x](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)`,
	`[x]( javascript:alert(1))`,
	`[x][r]` + "\n\n" + `[r]: javascript:alert(1)`,
	`![x](javascript:alert(1))`,
	`![x](x" onerror="alert(1))`,
	`![x](x "title" onerror="alert(1)")`,
	`![x](data:image/svg+xml;base64,PHN2ZyBvbmxvYWQ9YWxlcnQoMSk+)`,
	`<javascript:alert(1)>`,
	`<data:text/html,<script>alert(1)</script>>`,
	`<https://example.com/"onmouseover="alert(1)>`,
	"`<script>alert(1)</script>`" + "\n\n```html\n<script>alert(1)</script>\n```",
	"[x](http://a.example \"t\" onclick=\"alert(1)\")",
	"![x](shot.png){onerror=alert(1)}",
	"![x](shot.png){style=\"background:url(javascript:alert(1))\"}",
	"# <script>alert(1)</script>\n\n## <img src=x onerror=alert(1)>",
	"> <script>alert(1)</script>\n\n- <img src=x onerror=alert(1)>\n\n1. [x](javascript:alert(1))",
	"| a |\n|---|\n| <script>alert(1)</script> |",
	"<div>\n\n<script>alert(1)</script>\n\n</div>",
	"<div><script>alert(1)</script></div>\nmore text",
	"<!--[if gte IE 4]><script>alert(1)</script><![endif]-->",
	"<!--><script>alert(1)</script>-->",
	"<![CDATA[<script>alert(1)</script>]]>",
	"<?xml version=\"1.0\"?><script>alert(1)</script>",
	"\x00<script>alert(1)</script>",
	"<scr\x00ipt>alert(1)</scr\x00ipt>",
	"<img src=\"\x01javascript:alert(1)\">",
	"<a href=\" javascript:alert(1)\">x</a>",
	"<a href=\"javascript\u0000:alert(1)\">x</a>",
	"<a href=\"jav&#x09;ascript:alert(1)\">x</a>",
	"<IMG SRC=&#106;&#97;&#118;&#97;&#115;&#99;&#114;&#105;&#112;&#116;&#58;&#97;&#108;&#101;&#114;&#116;&#40;&#49;&#41;>",
}

func TestKnownAttacksAreNeutralised(t *testing.T) {
	resolvers := Options{
		Images: func(name string, _ Size) (Image, bool) {
			return Image{Src: "/m/_assets/abc/" + name, Width: 10, Height: 10}, true
		},
		Links: func(string) (bool, bool) { return true, false },
	}
	for _, src := range attacks {
		for name, opts := range map[string]Options{"plain": {}, "with resolvers": resolvers} {
			out, err := Render(src, opts)
			if err != nil {
				t.Fatalf("%q: %v", src, err)
			}
			if bad := unsafeHTML(out); bad != "" {
				t.Errorf("%s: %q\n -> %s\n unsafe: %s", name, src, out, bad)
			}
		}
	}
}

func TestGoodMarkdownSurvivesTheSanitiser(t *testing.T) {
	out, _ := Render("# T\n\n*e* **s** `c` [l](https://example.com) [r](rel/path#frag) ![i](https://example.com/i.png)\n\n> q\n\n```go\nx := 1\n```\n\n1. a\n2. b\n\n<b>bold</b> <i>it</i> <br> <sub>s</sub>\n\n<div>block</div>\n", Options{})
	for _, want := range []string{"<h1", "<em>e</em>", "<strong>s</strong>", "<code>c</code>", `href="https://example.com"`, `href="rel/path#frag"`,
		`src="https://example.com/i.png"`, "<blockquote>", `class="language-go"`, "<ol>"} {
		if !strings.Contains(out, want) {
			t.Errorf("lost %q in %s", want, out)
		}
	}
	if bad := unsafeHTML(out); bad != "" {
		t.Errorf("unsafe: %s", bad)
	}
	// Raw HTML is not shown at all (safe by default, and the Markdown stays plain).
	for _, gone := range []string{"<b>", "<i>", "<sub>", "<div>", "<br"} {
		if strings.Contains(out, gone) {
			t.Errorf("raw HTML %s was passed through: %s", gone, out)
		}
	}
	if !strings.Contains(out, "bold") || !strings.Contains(out, "block") {
		// the words inside raw inline tags are kept; a raw block's text is dropped with it
		if !strings.Contains(out, "bold") {
			t.Errorf("text between raw inline tags was lost: %s", out)
		}
	}
}

// kanbanClass reports whether val is a class a kanban board puts on element tag.
func kanbanClass(tag, val string) bool {
	switch tag {
	case "div":
		return val == "kanban"
	case "section":
		return val == "kanban-column" || val == "kanban-column kanban-column-complete"
	case "ul":
		return val == "kanban-cards"
	case "li":
		return val == "kanban-card" || val == "kanban-card kanban-card-done"
	}
	return false
}

func FuzzRenderIsAlwaysSafe(f *testing.F) {
	for _, a := range attacks {
		f.Add(a)
	}
	f.Add("# Title\n\n![x](shot.png) [l](a/b)\n")
	f.Add("[ref]: <a b> \"t\"\n\n![r][ref]")
	f.Add("---\ntitle: <img src=x onerror=alert(1)>\nk: \"[x](javascript:alert(1))\"\n---\n{{page.title}} {{page.k}} ![{{page.k}}]({{page.k}})\n")
	f.Add("---\nkanban: true\n---\n## A\n- [ ] {{page.title}} <script>alert(1)</script>\n## B\n- [x] y\n")
	f.Add("<!-- kanban -->\n## A\n- [ ] <img src=x onerror=alert(1)>\n<!-- /kanban -->")
	f.Fuzz(func(t *testing.T, src string) {
		out, err := Render(src, Options{
			Page: Page{Title: "<b>t</b>", Path: "a/b", Slug: "b", Space: "main"},
			Images: func(name string, _ Size) (Image, bool) {
				return Image{Src: "/m/" + name, Link: "/m/full/" + name, Width: 3, Height: 2}, name != ""
			},
			Links: func(d string) (bool, bool) { return true, len(d)%2 == 0 },
		})
		if err != nil {
			t.Fatalf("render failed: %v", err)
		}
		if bad := unsafeHTML(out); bad != "" {
			t.Fatalf("unsafe output %s\n for %q\n -> %s", bad, src, out)
		}
		// Extracting references from any text must not fail either.
		ImageRefs(src)
		ImageDests(src)
		Links(src)
		Title(src)
	})
}

func FuzzRewriteImageRefKeepsTheTextSafe(f *testing.F) {
	f.Add("![a](shot.png) `![b](shot.png)`\n\n```\n![c](shot.png)\n```", "shot.png", "diagram.png")
	f.Add("![a][r]\n\n[r]: shot.png \"T\"", "shot.png", "x.png")
	f.Fuzz(func(t *testing.T, src, from, to string) {
		for _, fn := range []func(string, string, string) (string, bool){RewriteImageRef, RewriteImageDest} {
			out, ok := fn(src, from, to)
			if !ok && out != src {
				t.Fatalf("a refused rewrite changed the text: %q -> %q", src, out)
			}
		}
	})
}
