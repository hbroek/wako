package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

// Clients validate tools/list strictly: "required" must be an array (or absent),
// never JSON null, or they drop every tool.
func TestToolListIsValidForStrictClients(t *testing.T) {
	s := &Server{Doer: &fakeWiki{}, Version: "t"}
	reply := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	var out struct {
		Result struct {
			Tools []struct {
				Name        string
				InputSchema map[string]any
			}
		}
	}
	if err := json.Unmarshal(reply, &out); err != nil || len(out.Result.Tools) == 0 {
		t.Fatalf("no tools: %v %s", err, reply)
	}
	for _, tool := range out.Result.Tools {
		if req, ok := tool.InputSchema["required"]; ok {
			if _, isArray := req.([]any); !isArray {
				t.Errorf("%s: inputSchema.required is %v, want an array", tool.Name, req)
			}
		}
		if _, ok := tool.InputSchema["properties"].(map[string]any); !ok {
			t.Errorf("%s: inputSchema.properties is not an object", tool.Name)
		}
	}
}
