package cli_test

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hbroek/wako/internal/ai"
	"github.com/hbroek/wako/internal/auth"
	"github.com/hbroek/wako/internal/store"
)

type stubModel struct{ reply string }

func (stubModel) Model() string { return "stub-1" }
func (m stubModel) Complete(ctx context.Context, r ai.Request) (*ai.Result, error) {
	return &ai.Result{Text: m.reply, Model: "stub-1"}, nil
}

// A suggester's token: `put` queues, `suggestions` lists, an editor decides.
func TestCLISuggestions(t *testing.T) {
	r := newRig(t)
	main, _ := r.st.GetSpace("main")
	hash, _ := auth.HashPassword("pw-pw-pw-pw")
	u, _ := r.st.CreateUser("sue", hash, false)
	r.st.SetMember(main.ID, u.ID, store.RoleSuggester)
	sue := "wako_" + auth.RandomToken(16)
	r.st.CreateToken(u.ID, "cli", auth.HashToken(sue))

	r.run("# Doc\n\nold\n", "--space", "main", "put", "doc")
	code, out, errOut := r.runAs(sue, "# Doc\n\nnew\n", "--space", "main", "put", "doc")
	if code != 0 || !strings.Contains(out, "suggested /doc as suggestion #1") {
		t.Fatalf("put as a suggester: %d %q %q", code, out, errOut)
	}
	if pg, _ := main.GetPage("doc"); strings.Contains(pg.Body, "new") {
		t.Fatal("a suggester's put changed the page")
	}
	if _, out, _ := r.run("", "--space", "main", "suggestions"); !strings.Contains(out, "#1") || !strings.Contains(out, "/doc") || !strings.Contains(out, "sue") {
		t.Errorf("suggestions: %q", out)
	}
	if _, out, _ := r.run("", "--space", "main", "suggestion", "1"); !strings.Contains(out, "+new") || !strings.Contains(out, "-old") {
		t.Errorf("suggestion diff: %q", out)
	}
	if code, _, errOut := r.runAs(sue, "", "--space", "main", "suggestion", "1", "accept"); code == 0 {
		t.Errorf("a suggester accepted their own: %q", errOut)
	}
	if code, out, errOut := r.run("", "--space", "main", "suggestion", "1", "accept", "--note", "ok"); code != 0 || !strings.Contains(out, "accepted") {
		t.Fatalf("accept: %d %q %q", code, out, errOut)
	}
	if _, out, _ := r.run("", "--space", "main", "get", "doc"); !strings.Contains(out, "new") {
		t.Errorf("accepted text: %q", out)
	}
	if code, _, _ := r.run("", "--space", "main", "suggestion", "x"); code != 2 {
		t.Errorf("a bad number: %d", code)
	}
}

func TestCLIAskAndDraft(t *testing.T) {
	r := newRig(t)
	r.srv.EnableAI(stubModel{reply: "# Stripes\n\nZebras have stripes."}, 100)
	on := true
	r.st.UpdateSpace("main", store.SpaceUpdate{AIEnabled: &on})
	r.run("# Zebras\n\nZebras have stripes.\n", "--space", "main", "put", "zebras")

	r.srv.EnableAI(stubModel{reply: "Zebras have stripes [1]."}, 100)
	code, out, errOut := r.run("", "--space", "main", "ask", "what", "do", "zebras", "have?")
	if code != 0 || !strings.Contains(out, "Zebras have stripes") || !strings.Contains(out, "[1] main/zebras") || strings.Contains(out, "](/main") {
		t.Fatalf("ask: %d %q %q", code, out, errOut)
	}
	r.srv.EnableAI(stubModel{reply: "# Stripes\n\nZebras have stripes."}, 100)
	code, out, errOut = r.run("", "--space", "main", "draft", "--path", "stripes", "write", "about", "stripes")
	if code != 0 || out != "# Stripes\n\nZebras have stripes.\n" {
		t.Fatalf("draft: %d %q %q", code, out, errOut)
	}
	if _, err := mustSpace(r).GetPage("stripes"); err == nil {
		t.Fatal("draft without --save saved")
	}
	code, out, errOut = r.run("", "--space", "main", "draft", "--path", "stripes", "--save", "write", "about", "stripes")
	if code != 0 || !strings.Contains(out, "saved /stripes") || !strings.Contains(out, "stub-1") {
		t.Fatalf("draft --save: %d %q %q", code, out, errOut)
	}
	pg, _ := mustSpace(r).GetPage("stripes")
	revs, _ := mustSpace(r).Revisions(pg.ID)
	if !strings.Contains(revs[0].Note, "drafted by AI (stub-1)") {
		t.Errorf("history note: %q", revs[0].Note)
	}
	if code, _, _ := r.run("", "--space", "main", "draft", "--save", "no", "path"); code != 2 {
		t.Errorf("--save without --path: %d", code)
	}
}

func mustSpace(r *rig) *store.Space {
	sp, err := r.st.GetSpace("main")
	if err != nil {
		r.t.Fatal(err)
	}
	return sp
}

// `wako mcp` speaks line-delimited JSON-RPC on stdin/stdout with the token it was given.
func TestCLIMCPOverStdio(t *testing.T) {
	r := newRig(t)
	r.run("# Doc\n\nhello zebra\n", "--space", "main", "put", "doc")
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_page","arguments":{"space":"main","path":"doc"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"append_to_page","arguments":{"space":"main","path":"doc","markdown":"more"}}}`,
		`not json`,
	}, "\n") + "\n"
	code, out, errOut := r.run(in, "mcp")
	if code != 0 {
		t.Fatalf("mcp: %d %q", code, errOut)
	}
	var replies []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("stdout must hold only JSON-RPC, got %q", sc.Text())
		}
		replies = append(replies, m)
	}
	if len(replies) != 4 { // initialize, get_page, append, and the parse error; the notification gets none
		t.Fatalf("%d replies: %s", len(replies), out)
	}
	if !strings.Contains(out, "hello zebra") || !strings.Contains(out, "Saved main/doc (version 2)") || !strings.Contains(out, "-32700") {
		t.Errorf("replies: %s", out)
	}
	// With a viewer's token every write is refused, and the reads still work.
	_, out, _ = r.runAs(r.viewer, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"append_to_page","arguments":{"space":"main","path":"doc","markdown":"x"}}}`+"\n", "mcp")
	if !strings.Contains(out, `"isError":true`) || !strings.Contains(out, "not allowed") {
		t.Errorf("viewer write: %s", out)
	}
}
