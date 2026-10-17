package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauro/boaky/internal/ingest"
	bmcp "github.com/mauro/boaky/internal/mcp"
)

// TestMCPToolSurface verifies every exposed tool over a real MCP session
// (in-memory transport), that responses are structured, and that no tool
// modifies the library.
func TestMCPToolSurface(t *testing.T) {
	e := newEnv(t, map[string]string{"b.txt": book1, "c.txt": book2})
	e.index(t, ingest.Options{})

	deps := bmcp.Deps{
		DB: e.db, Cfg: e.cfg, Log: nil,
		Search: e.srch, Pipe: e.pipe, Eval: e.ev,
	}
	srv := bmcp.NewServer(deps)

	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st); err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	// tools/list must show exactly the five read-only tools.
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"list_books": false, "search_books": false, "get_passages": false,
		"get_book_metadata": false, "check_evidence": false,
	}
	for _, tl := range list.Tools {
		if _, ok := want[tl.Name]; !ok {
			t.Errorf("unexpected tool exposed: %s", tl.Name)
			continue
		}
		want[tl.Name] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing tool: %s", k)
		}
	}

	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s returned tool error: %+v", name, res.Content)
		}
		var out map[string]any
		if res.StructuredContent != nil {
			b, _ := json.Marshal(res.StructuredContent)
			json.Unmarshal(b, &out)
		} else if len(res.Content) > 0 {
			if tc, ok := res.Content[0].(*sdk.TextContent); ok {
				json.Unmarshal([]byte(tc.Text), &out)
			}
		}
		return out
	}

	// list_books
	out := call("list_books", map[string]any{"limit": 50})
	arr, _ := out["books"].([]any)
	if len(arr) != 2 {
		t.Errorf("list_books: %v", out)
	}

	// search_books
	out = call("search_books", map[string]any{"query": "narrator forgets name"})
	res2, _ := out["results"].([]any)
	if len(res2) == 0 {
		t.Fatalf("search_books empty: %v", out)
	}

	// get_passages with exact IDs
	var first map[string]any
	for _, r := range res2 {
		first, _ = r.(map[string]any)
		break
	}
	pid, _ := first["passage_id"].(string)
	if pid == "" {
		t.Fatal("no passage_id in result")
	}
	out = call("get_passages", map[string]any{"passage_ids": []string{pid}})
	ps, _ := out["passages"].([]any)
	if len(ps) != 1 {
		t.Errorf("get_passages: %v", out)
	}
	out = call("get_passages", map[string]any{"passage_ids": []string{"bk-nope:1"}})
	if _, ok := out["missing"]; !ok {
		t.Error("missing passage should be reported")
	}

	// get_book_metadata
	out = call("get_book_metadata", map[string]any{"book_id": first["book_id"]})
	if out["title"] == nil || out["title"] == "" {
		t.Errorf("metadata title missing: %v", out)
	}

	// check_evidence: sufficient
	out = call("check_evidence", map[string]any{"question": "Does the narrator forget his name?"})
	if !strings.Contains(mustJSON(out), "sufficient") {
		t.Errorf("expected sufficient: %v", out)
	}
	// check_evidence: insufficient
	out = call("check_evidence", map[string]any{"question": "what is the population of the moon?"})
	if !strings.Contains(mustJSON(out), "insufficient") {
		t.Errorf("expected insufficient: %v", out)
	}

	// search with filters + limit
	call("search_books", map[string]any{"query": "memory", "title": "Identity", "limit": 3})

	// no state changed by read-only tools
	var books int
	e.db.QueryRow(`SELECT count(*) FROM books`).Scan(&books)
	if books != 2 {
		t.Errorf("read-only tools changed the library: %d books", books)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
