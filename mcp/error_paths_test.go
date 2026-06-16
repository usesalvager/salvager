package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/usesalvager/salvager/store"
)

// mcpErrorText returns the text of an error result's first content block. Tool
// errors are surfaced as IsError results carrying a TextContent message (not a
// JSON body), so this is the right accessor for the error-path assertions.
func mcpErrorText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if !res.IsError {
		t.Fatalf("result is not an error result")
	}
	if len(res.Content) == 0 {
		t.Fatalf("error result has no content blocks")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("error content is %T, want *mcp.TextContent", res.Content[0])
	}
	return tc.Text
}

// Security: a path that escapes the project root must be refused by every tool
// with a structured error (IsError result, not a transport error and not a
// silent success). The store guard (ErrUnsafePath) is what fires; here we assert
// it surfaces cleanly through the MCP layer for all three tools.
func TestMCP_AllTools_RejectTraversalPath(t *testing.T) {
	ctx := context.Background()
	s, _ := mcpSeedStore(t)
	cs := mcpClientFor(t, s)

	calls := []struct {
		name string
		args map[string]any
	}{
		{"salvager_list_versions", map[string]any{"file": "../../etc/passwd"}},
		{"salvager_get_version", map[string]any{"file": "../../etc/passwd", "timestamp": int64(1)}},
		{"salvager_restore", map[string]any{"file": "../../etc/passwd", "timestamp": int64(1)}},
		// Explicit empty string passes the SDK "required" check (the property is
		// present) and reaches the handler, where the store guard rejects it.
		{"salvager_list_versions", map[string]any{"file": ""}},
		{"salvager_get_version", map[string]any{"file": "", "timestamp": int64(1)}},
		{"salvager_restore", map[string]any{"file": "", "timestamp": int64(1)}},
	}
	for _, c := range calls {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.name, Arguments: c.args})
		if err != nil {
			t.Fatalf("%s: transport error (want IsError result): %v", c.name, err)
		}
		msg := mcpErrorText(t, res)
		if !strings.Contains(msg, "escapes project root") {
			t.Errorf("%s(%v) error = %q, want containment message", c.name, c.args, msg)
		}
	}
}

// R7 through the real MCP round-trip: restore targeting a host file outside the
// root via traversal. PRIMARY assertion: the sentinel file is intact afterwards
// (the guard precedes the os.Remove in restore's deletion branch). Secondary:
// the call is refused with the containment error.
func TestMCP_Restore_TraversalDelete_LeavesSentinelIntact(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	const sentinelBody = "host file the MCP must never be able to delete"
	sentinel := filepath.Join(parent, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte(sentinelBody), 0o644); err != nil {
		t.Fatal(err)
	}

	cs := mcpClientFor(t, store.New(root))
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "salvager_restore",
		Arguments: map[string]any{"file": "../sentinel.txt", "timestamp": int64(1)},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}

	// PRIMARY: sentinel still present, byte-for-byte.
	got, readErr := os.ReadFile(sentinel)
	if readErr != nil {
		t.Fatalf("sentinel gone after restore-delete via traversal: %v", readErr)
	}
	if string(got) != sentinelBody {
		t.Fatalf("sentinel contents changed: got %q, want %q", got, sentinelBody)
	}

	// SECONDARY: refused with the containment error.
	if msg := mcpErrorText(t, res); !strings.Contains(msg, "escapes project root") {
		t.Errorf("error = %q, want containment message", msg)
	}
}

// SDK-contract test — observed behavior of go-sdk v1.6.1: required arguments and
// argument types are validated at the JSON-schema layer BEFORE the tool handler
// runs. A missing or wrong-typed argument yields an IsError result carrying a
// schema validation message; the store is never reached. This pins the SDK
// contract we rely on (missing "file" never reaches the store as ""). If an SDK
// upgrade changes this, a failure here reads as "the SDK changed", not as a
// regression in salvager's own code.
func TestMCP_SDKContract_RejectsMissingAndWrongTypeArgs(t *testing.T) {
	ctx := context.Background()
	s, root := mcpSeedStore(t)
	mcpWrite(t, root, "a.txt", []byte("x"))
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	cs := mcpClientFor(t, s)

	cases := []struct {
		name      string
		args      map[string]any
		wantInMsg string
	}{
		{"salvager_list_versions", map[string]any{}, "missing properties"},
		{"salvager_get_version", map[string]any{"timestamp": int64(1)}, "missing properties"},
		{"salvager_get_version", map[string]any{"file": "a.txt"}, "missing properties"},
		{"salvager_get_version", map[string]any{"file": "a.txt", "timestamp": "notanint"}, "integer"},
		{"salvager_restore", map[string]any{"timestamp": int64(1)}, "missing properties"},
		{"salvager_restore", map[string]any{"file": "a.txt"}, "missing properties"},
		{"salvager_restore", map[string]any{"file": "a.txt", "timestamp": "notanint"}, "integer"},
	}
	for _, c := range cases {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.name, Arguments: c.args})
		if err != nil {
			t.Fatalf("%s: transport error (want IsError result): %v", c.name, err)
		}
		msg := mcpErrorText(t, res)
		if !strings.Contains(msg, c.wantInMsg) {
			t.Errorf("%s(%v) error = %q, want substring %q", c.name, c.args, msg, c.wantInMsg)
		}
	}
}

// Finding 2 — list_versions on an untracked file is a success (empty history is
// not an error), but the output is self-descriptive: file echoes the query and
// tracked is false, so an agent can tell "no history" from a failed call without
// inferring it from an empty array.
func TestMCP_ListVersions_UntrackedIsSelfDescribing(t *testing.T) {
	ctx := context.Background()
	s, root := mcpSeedStore(t)
	cs := mcpClientFor(t, s)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "salvager_list_versions",
		Arguments: map[string]any{"file": "never-seen.txt"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("untracked file must list as success, got error: %s", mcpResultJSON(t, res))
	}
	var out listOutput
	if err := json.Unmarshal(mcpResultJSON(t, res), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.File != "never-seen.txt" {
		t.Errorf("file echo = %q, want never-seen.txt", out.File)
	}
	if out.Tracked {
		t.Errorf("tracked = true for an untracked file, want false")
	}
	if len(out.Versions) != 0 {
		t.Errorf("want 0 versions, got %d", len(out.Versions))
	}

	// And a tracked file reports tracked=true.
	mcpWrite(t, root, "real.txt", []byte("v1"))
	if err := s.Record("real.txt"); err != nil {
		t.Fatal(err)
	}
	res2, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "salvager_list_versions",
		Arguments: map[string]any{"file": "real.txt"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var out2 listOutput
	if err := json.Unmarshal(mcpResultJSON(t, res2), &out2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out2.Tracked || out2.File != "real.txt" {
		t.Errorf("tracked file: got file=%q tracked=%v, want real.txt/true", out2.File, out2.Tracked)
	}
}
