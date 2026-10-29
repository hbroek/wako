package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hbroek/wako/internal/store"
)

// rpc sends one JSON-RPC request to /_mcp and returns the decoded result.
func (e *propEnv) rpc(t *testing.T, who reqOpt, method string, params any) (result map[string]any, rpcErr map[string]any, status int) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	opts := []reqOpt{header("Content-Type", "application/json")}
	if who != nil {
		opts = append(opts, who)
	}
	rec := e.do("POST", "/_mcp", string(body), opts...)
	if rec.Code != 200 {
		return nil, nil, rec.Code
	}
	var out struct {
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON-RPC: %s", rec.Body.String())
	}
	return out.Result, out.Error, 200
}

// tool calls a tool and returns its text and whether it reported an error.
func (e *propEnv) tool(t *testing.T, who reqOpt, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, rerr, st := e.rpc(t, who, "tools/call", map[string]any{"name": name, "arguments": args})
	if st != 200 || rerr != nil {
		t.Fatalf("tools/call %s: status %d, error %v", name, st, rerr)
	}
	content, _ := res["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content: %v", res)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

func TestMCPHandshakeAndTools(t *testing.T) {
	e := newPropEnv(t)
	res, rerr, st := e.rpc(t, e.ed, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t"}})
	if st != 200 || rerr != nil || res["protocolVersion"] != "2025-03-26" {
		t.Fatalf("initialize: %d %v %v", st, res, rerr)
	}
	if info, _ := res["serverInfo"].(map[string]any); info["name"] != "wako" {
		t.Errorf("serverInfo: %v", info)
	}
	res, _, _ = e.rpc(t, e.ed, "tools/list", nil)
	names := map[string]bool{}
	for _, x := range res["tools"].([]any) {
		names[x.(map[string]any)["name"].(string)] = true
	}
	for _, n := range []string{"list_spaces", "list_pages", "get_page", "search", "write_page", "edit_page", "append_to_page", "backlinks", "broken_links", "list_suggestions"} {
		if !names[n] {
			t.Errorf("missing tool %s", n)
		}
	}
	// There is no tool to accept or reject: assistants propose, people decide.
	for n := range names {
		if strings.Contains(n, "accept") || strings.Contains(n, "reject") || strings.Contains(n, "delete") {
			t.Errorf("tool %s lets an assistant decide or destroy", n)
		}
	}
	// Notifications get no body; unknown methods are errors; a bad tool is a protocol error.
	rec := e.do("POST", "/_mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, header("Content-Type", "application/json"), e.ed)
	if rec.Code != 202 || rec.Body.Len() != 0 {
		t.Errorf("notification: %d %q", rec.Code, rec.Body.String())
	}
	if _, rerr, _ := e.rpc(t, e.ed, "no/such", nil); rerr == nil || rerr["code"].(float64) != -32601 {
		t.Errorf("unknown method: %v", rerr)
	}
	if _, rerr, _ := e.rpc(t, e.ed, "tools/call", map[string]any{"name": "rm_rf"}); rerr == nil {
		t.Error("unknown tool accepted")
	}
}

func TestMCPNeedsAnAPIToken(t *testing.T) {
	e := newPropEnv(t)
	post := func(opts ...reqOpt) int {
		return e.do("POST", "/_mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, append([]reqOpt{header("Content-Type", "application/json")}, opts...)...).Code
	}
	if c := post(); c != 401 {
		t.Errorf("anonymous: %d", c)
	}
	if c := post(bearer("wako_wrong")); c != 401 {
		t.Errorf("bad token: %d", c)
	}
	j := e.login("ed")
	if c := post(withJar(j), withCSRF(j)); c != 403 {
		t.Errorf("browser session: %d (tokens only)", c)
	}
	if c := post(e.ed); c != 200 {
		t.Errorf("token: %d", c)
	}
	if rec := e.do("GET", "/_mcp", "", e.ed); rec.Code != 405 {
		t.Errorf("GET: %d", rec.Code)
	}
	if rec := e.do("POST", "/_mcp", "{}", e.ed); rec.Code != 415 {
		t.Errorf("wrong content type: %d", rec.Code)
	}
}

func TestMCPReadingAndWriting(t *testing.T) {
	e := newPropEnv(t)
	e.do("PUT", "/main/docs", "# Docs\n\nWelcome to the zebra handbook.\n", e.mdT, e.ed)
	e.do("PUT", "/main/docs/setup", "# Setup\n\nInstall the thing. See [docs](../docs).\n", e.mdT, e.ed)

	if out, bad := e.tool(t, e.ed, "list_spaces", nil); bad || !strings.Contains(out, "main") {
		t.Errorf("list_spaces: %q", out)
	}
	if out, bad := e.tool(t, e.ed, "list_pages", map[string]any{"space": "main"}); bad || !strings.Contains(out, "Setup (docs/setup)") {
		t.Errorf("list_pages: %q", out)
	}
	out, bad := e.tool(t, e.ed, "get_page", map[string]any{"space": "main", "path": "docs"})
	if bad || !strings.HasPrefix(out, "version: 1\n\n# Docs") {
		t.Fatalf("get_page: %q", out)
	}
	if out, bad := e.tool(t, e.ed, "search", map[string]any{"query": "zebra"}); bad || !strings.Contains(out, "main/docs") || !strings.Contains(out, "**zebra**") {
		t.Errorf("search: %q", out)
	}
	if out, _ := e.tool(t, e.ed, "backlinks", map[string]any{"space": "main", "path": "docs"}); !strings.Contains(out, "main/docs/setup") {
		t.Errorf("backlinks: %q", out)
	}

	// edit_page changes one place, and nothing else.
	if out, bad := e.tool(t, e.ed, "edit_page", map[string]any{"space": "main", "path": "docs", "old_text": "zebra handbook", "new_text": "giraffe handbook", "note": "fix"}); bad || !strings.Contains(out, "Saved main/docs (version 2)") {
		t.Errorf("edit_page: %q", out)
	}
	if pg := mustPage(t, e.sp, "docs"); pg.Body != "# Docs\n\nWelcome to the giraffe handbook.\n" {
		t.Errorf("body: %q", pg.Body)
	}
	if _, bad := e.tool(t, e.ed, "edit_page", map[string]any{"space": "main", "path": "docs", "old_text": "no such text", "new_text": "x"}); !bad {
		t.Error("edit_page with a missing text succeeded")
	}
	if out, bad := e.tool(t, e.ed, "edit_page", map[string]any{"space": "main", "path": "docs/setup", "old_text": "e", "new_text": "E"}); !bad || !strings.Contains(out, "occurs") {
		t.Errorf("an ambiguous edit: %q %v", out, bad)
	}
	if out, bad := e.tool(t, e.ed, "append_to_page", map[string]any{"space": "main", "path": "docs", "markdown": "More."}); bad || !strings.Contains(out, "version 3") {
		t.Errorf("append: %q", out)
	}
	if pg := mustPage(t, e.sp, "docs"); !strings.HasSuffix(pg.Body, "handbook.\n\nMore.") {
		t.Errorf("appended body: %q", pg.Body)
	}
	// write_page: creates, refuses to overwrite blindly, replaces with the right version, and refuses a stale one.
	if out, bad := e.tool(t, e.ed, "write_page", map[string]any{"space": "main", "path": "notes", "markdown": "# Notes\n"}); bad || !strings.Contains(out, "version 1") {
		t.Errorf("write_page create: %q", out)
	}
	if out, bad := e.tool(t, e.ed, "write_page", map[string]any{"space": "main", "path": "notes", "markdown": "# Overwrite\n"}); !bad || !strings.Contains(out, "already exists") {
		t.Errorf("a blind overwrite: %q %v", out, bad)
	}
	if _, bad := e.tool(t, e.ed, "write_page", map[string]any{"space": "main", "path": "notes", "markdown": "# N2\n", "expected_version": 1}); bad {
		t.Error("replace with the right version failed")
	}
	if out, bad := e.tool(t, e.ed, "write_page", map[string]any{"space": "main", "path": "notes", "markdown": "# N3\n", "expected_version": 1}); !bad || !strings.Contains(out, "changed") {
		t.Errorf("a stale version: %q", out)
	}
	revs, _ := e.sp.Revisions(mustPage(t, e.sp, "notes").ID)
	if !strings.Contains(revs[0].Note, "API token") {
		t.Errorf("who wrote it: %+v", revs[0])
	}
	// Hostile names never leave the page namespace.
	for _, bad := range []string{"../x", "a/../b", "_assets/x", "a%2Fb", "a?b", ""} {
		if out, isErr := e.tool(t, e.ed, "get_page", map[string]any{"space": "main", "path": bad}); !isErr {
			t.Errorf("path %q accepted: %q", bad, out)
		}
	}
	if _, isErr := e.tool(t, e.ed, "get_page", map[string]any{"space": "main/../other", "path": "x"}); !isErr {
		t.Error("a space name with slashes was accepted")
	}
}

func TestMCPRespectsWhatTheTokenMayDo(t *testing.T) {
	e := newPropEnv(t)
	e.do("PUT", "/main/doc", "# Doc\n\nold line\n", e.mdT, e.ed)

	// A suggester, or a suggest-only token, gets a queued suggestion and nothing goes live.
	for name, who := range map[string]reqOpt{"suggester": e.sug, "suggest-only token": e.editorSuggestOnlyToken} {
		out, bad := e.tool(t, who, "edit_page", map[string]any{"space": "main", "path": "doc", "old_text": "old line", "new_text": "new line", "note": "why not"})
		if bad || !strings.Contains(out, "Queued as suggestion") {
			t.Fatalf("%s: %q %v", name, out, bad)
		}
		if mustPage(t, e.sp, "doc").Body != "# Doc\n\nold line\n" {
			t.Fatalf("%s changed the page", name)
		}
	}
	out, _ := e.tool(t, e.ed, "list_suggestions", map[string]any{"space": "main"})
	if strings.Count(out, "[pending]") != 2 || !strings.Contains(out, "why not") {
		t.Errorf("list_suggestions: %q", out)
	}
	list := e.do("GET", "/main/_proposals", "", e.ed, header("Accept", "application/json"))
	var got []proposalJSON
	json.Unmarshal(list.Body.Bytes(), &got)
	for _, p := range got {
		if p.Source != "mcp" || p.Note != "why not" {
			t.Errorf("source and note: %+v", p)
		}
	}
	// A viewer can read through MCP but every write is refused.
	if out, bad := e.tool(t, e.view, "get_page", map[string]any{"space": "main", "path": "doc"}); bad {
		t.Errorf("viewer read: %q", out)
	}
	if out, bad := e.tool(t, e.view, "append_to_page", map[string]any{"space": "main", "path": "doc", "markdown": "x"}); !bad || !strings.Contains(out, "not allowed") {
		t.Errorf("viewer write: %q %v", out, bad)
	}
}

func TestMCPCannotReadPrivateSpaces(t *testing.T) {
	e := newPropEnv(t)
	secret := e.space("secret", false)
	secret.PutPage("plans", "Plans", "# Plans\n\nlaunch codes\n", store.Refs{}, store.Cond{})
	for name, who := range map[string]reqOpt{"viewer elsewhere": e.view, "suggester elsewhere": e.sug} {
		for _, call := range []struct {
			tool string
			args map[string]any
		}{
			{"get_page", map[string]any{"space": "secret", "path": "plans"}},
			{"list_pages", map[string]any{"space": "secret"}},
			{"search", map[string]any{"query": "launch", "space": "secret"}},
			{"write_page", map[string]any{"space": "secret", "path": "plans", "markdown": "x", "expected_version": 1}},
			{"list_suggestions", map[string]any{"space": "secret"}},
		} {
			out, bad := e.tool(t, who, call.tool, call.args)
			if !bad || strings.Contains(out, "launch") {
				t.Errorf("%s %s: %q (error=%v)", name, call.tool, out, bad)
			}
		}
		if out, _ := e.tool(t, who, "search", map[string]any{"query": "launch"}); strings.Contains(out, "secret") {
			t.Errorf("%s: the global search found the private space: %q", name, out)
		}
		if out, _ := e.tool(t, who, "list_spaces", nil); strings.Contains(out, "secret") {
			t.Errorf("%s: list_spaces shows the private space: %q", name, out)
		}
	}
}
