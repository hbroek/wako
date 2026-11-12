package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// fakeWiki answers every request with a small canned reply, and records the
// paths it was asked for.
type fakeWiki struct{ paths []string }

func (f *fakeWiki) Do(ctx context.Context, method, path string, h http.Header, body []byte) (int, http.Header, []byte, error) {
	f.paths = append(f.paths, method+" "+path)
	hdr := http.Header{"Etag": {`"v3"`}, "Location": {"/main/x"}}
	switch {
	case strings.HasPrefix(path, "/_api/spaces/") && strings.HasSuffix(path, "/tree"):
		return 200, hdr, []byte(`[{"slug":"a","title":"A","path":"a","children":[{"slug":"b","title":"B","path":"a/b"}]}]`), nil
	case method == "PUT":
		return 202, hdr, []byte(`{"id":7}`), nil
	case strings.Contains(path, "/search"):
		return 200, hdr, []byte(`[{"space":"main","path":"a","title":"A","snippet_html":"x <mark>y</mark> &amp; z"}]`), nil
	case strings.HasSuffix(path, ".md"):
		return 200, hdr, []byte("# A\n\ntext\n"), nil
	}
	return 200, hdr, []byte(`[]`), nil
}

// dotSegment reports a "." or ".." path segment, which would climb out of the
// page tree. A name that merely contains dots ("..0", "v1..2") is an ordinary page name.
func dotSegment(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// Whatever bytes arrive, the handler answers with valid JSON-RPC (or nothing),
// and never asks the wiki for anything outside page addresses.
func FuzzHandle(f *testing.F) {
	for _, s := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_page","arguments":{"space":"main","path":"a/b"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"edit_page","arguments":{"space":"main","path":"a","old_text":"text","new_text":"x"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"write_page","arguments":{"space":"m","path":"../../etc","markdown":"x","expected_version":-1}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"search","arguments":{"query":"a&b=c#d","limit":1e99}}}`,
		`[{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":8,"method":"ping"}]`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"get_page","arguments":{"space":["x"],"path":{"a":1}}}}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"write_page","arguments":{"space":"0","path":"..0","markdown":"0"}}}`, // a name with dots in it is not a traversal
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"get_page","arguments":{"space":"main","path":"a/../b"}}}`,
		`{`, `[]`, `null`, `"x"`, `{"id":1}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, msg string) {
		fw := &fakeWiki{}
		s := &Server{Doer: fw, Version: "t"}
		reply := s.Handle(context.Background(), []byte(msg))
		if reply != nil {
			var v any
			if err := json.Unmarshal(reply, &v); err != nil {
				t.Fatalf("reply is not JSON: %q (%v)", reply, err)
			}
		}
		for _, p := range fw.paths {
			path := p[strings.IndexByte(p, ' ')+1:]
			if !strings.HasPrefix(path, "/") || dotSegment(path) || strings.Contains(path, "//") || strings.ContainsAny(path, "\x00\r\n ") {
				t.Fatalf("a tool made a strange request %q from %q", p, msg)
			}
			if strings.Contains(path, "/_") && !strings.HasPrefix(path, "/_api/") && !strings.Contains(path, "/_proposals") {
				t.Fatalf("a tool reached a reserved path %q from %q", p, msg)
			}
		}
	})
}

func FuzzSnippetText(f *testing.F) {
	f.Add("a <mark>b</mark> &lt;script&gt; <b")
	f.Fuzz(func(t *testing.T, s string) {
		out := SnippetText(s)
		// A search excerpt arrives escaped, so a "<" in the output can only come from an entity.
		if strings.Contains(out, "<") && !strings.Contains(s, "&") {
			t.Fatalf("markup left in %q -> %q", s, out)
		}
	})
}
