package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"lochis/store"
)

// --- test helpers (uniquely prefixed to avoid collisions in package mcp) ---

// mcpClientFor seeds nothing itself; it wires server b over the SDK's in-memory
// transport and returns a connected client session for a real round trip.
// The server must be connected before the client (the client initializes the
// session during connection).
func mcpClientFor(t *testing.T, b Backend) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	srv := NewServer(b)
	serverT, clientT := mcp.NewInMemoryTransports()

	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	return cs
}

// mcpWrite writes content into root/rel (byte-exact, no normalization).
func mcpWrite(t *testing.T, root, rel string, content []byte) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// mcpResultJSON returns the JSON body the server emitted for a tool call. The
// SDK serializes a ToolHandlerFor's typed output into a TextContent block whose
// text is exactly the structured-content JSON, so this is the canonical wire
// payload regardless of how StructuredContent (an any) decodes on the client.
func mcpResultJSON(t *testing.T, res *mcp.CallToolResult) []byte {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatalf("tool result has no content blocks")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("first content block is %T, want *mcp.TextContent", res.Content[0])
	}
	return []byte(tc.Text)
}

// mcp seed: a real FS store under tmp, recording revisions through the store so
// the MCP server reads the very same .lochis/ (A8.4).
func mcpSeedStore(t *testing.T) (*store.FS, string) {
	t.Helper()
	root := t.TempDir()
	return store.New(root), root
}

// A8.1 — lochis_list_versions returns revisions newest-first, each with raw
// timestamp, timestamp_human, hash_short and label.
func TestMCP_ListVersions(t *testing.T) {
	ctx := context.Background()
	s, root := mcpSeedStore(t)

	mcpWrite(t, root, "a.txt", []byte("one"))
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	mcpWrite(t, root, "a.txt", []byte("two"))
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	mcpWrite(t, root, "a.txt", []byte("three"))
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}

	cs := mcpClientFor(t, s)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "lochis_list_versions",
		Arguments: map[string]any{"file": "a.txt"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", mcpResultJSON(t, res))
	}

	var out listOutput
	if err := json.Unmarshal(mcpResultJSON(t, res), &out); err != nil {
		t.Fatalf("decode list output: %v\n%s", err, mcpResultJSON(t, res))
	}

	if len(out.Versions) != 3 {
		t.Fatalf("want 3 versions, got %d: %+v", len(out.Versions), out.Versions)
	}

	// Newest first: strictly descending timestamps.
	for i := 1; i < len(out.Versions); i++ {
		if out.Versions[i-1].Timestamp <= out.Versions[i].Timestamp {
			t.Errorf("not newest-first: ts[%d]=%d <= ts[%d]=%d",
				i-1, out.Versions[i-1].Timestamp, i, out.Versions[i].Timestamp)
		}
	}

	// Newest is the "three" modify; oldest is "one" initial.
	if got := out.Versions[0].Label; got != string(store.LabelModify) {
		t.Errorf("newest label = %q, want modify", got)
	}
	if got := out.Versions[2].Label; got != string(store.LabelInitial) {
		t.Errorf("oldest label = %q, want initial", got)
	}

	// Every entry must carry raw ts, human ts, and a short hash.
	for i, v := range out.Versions {
		if v.Timestamp == 0 {
			t.Errorf("version %d has zero raw timestamp", i)
		}
		if v.TimestampHuman == "" {
			t.Errorf("version %d has empty timestamp_human", i)
		}
		// human form must correspond to the raw ts.
		if want := human(v.Timestamp); v.TimestampHuman != want {
			t.Errorf("version %d human ts = %q, want %q", i, v.TimestampHuman, want)
		}
		if v.HashShort == "" {
			t.Errorf("version %d has empty hash_short", i)
		}
		if len(v.HashShort) > 8 {
			t.Errorf("version %d hash_short = %q, longer than 8", i, v.HashShort)
		}
	}

	// Cross-check hash_short against what the store actually recorded.
	revs, _ := s.List("a.txt")
	if len(revs) != 3 {
		t.Fatalf("store sanity: want 3 revs, got %d", len(revs))
	}
	if out.Versions[0].HashShort != short(revs[0].Hash) {
		t.Errorf("newest hash_short = %q, want %q", out.Versions[0].HashShort, short(revs[0].Hash))
	}
}

// A8.2 — lochis_get_version returns exactly the content for that revision.
func TestMCP_GetVersion(t *testing.T) {
	ctx := context.Background()
	s, root := mcpSeedStore(t)

	mcpWrite(t, root, "a.txt", []byte("XCONTENT"))
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	mcpWrite(t, root, "a.txt", []byte("YCONTENT_newer"))
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}

	revs, _ := s.List("a.txt") // newest first: Y, X
	xTs := revs[1].Timestamp

	cs := mcpClientFor(t, s)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "lochis_get_version",
		Arguments: map[string]any{"file": "a.txt", "timestamp": xTs},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", mcpResultJSON(t, res))
	}

	var out getOutput
	if err := json.Unmarshal(mcpResultJSON(t, res), &out); err != nil {
		t.Fatalf("decode get output: %v\n%s", err, mcpResultJSON(t, res))
	}
	if out.Content != "XCONTENT" {
		t.Errorf("get content = %q, want %q", out.Content, "XCONTENT")
	}
}

// A8.3 — lochis_restore restores the on-disk file AND reports a non-zero
// pre_restore_timestamp that can undo. Verify the undo round-trips.
func TestMCP_Restore_ReportsPreRestoreAndIsReversible(t *testing.T) {
	ctx := context.Background()
	s, root := mcpSeedStore(t)

	good := []byte("GOOD work")
	broken := []byte("BROKEN garbage")

	mcpWrite(t, root, "a.txt", good)
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	mcpWrite(t, root, "a.txt", broken)
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}

	revs, _ := s.List("a.txt") // newest first: broken, good
	goodTs := revs[1].Timestamp

	cs := mcpClientFor(t, s)

	// Restore to the good revision.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "lochis_restore",
		Arguments: map[string]any{"file": "a.txt", "timestamp": goodTs},
	})
	if err != nil {
		t.Fatalf("CallTool restore: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", mcpResultJSON(t, res))
	}

	var out restoreOutput
	if err := json.Unmarshal(mcpResultJSON(t, res), &out); err != nil {
		t.Fatalf("decode restore output: %v\n%s", err, mcpResultJSON(t, res))
	}
	if !out.OK {
		t.Errorf("restore ok = false, want true")
	}
	if out.PreRestoreTimestamp == 0 {
		t.Fatalf("pre_restore_timestamp = 0, want non-zero (undo point)")
	}

	// On-disk file must now be the good content, byte-for-byte.
	onDisk, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(good) {
		t.Errorf("after restore, file = %q, want %q", onDisk, good)
	}

	// Undo: a second restore to pre_restore_timestamp brings back the broken state.
	res2, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "lochis_restore",
		Arguments: map[string]any{"file": "a.txt", "timestamp": out.PreRestoreTimestamp},
	})
	if err != nil {
		t.Fatalf("CallTool undo restore: %v", err)
	}
	if res2.IsError {
		t.Fatalf("unexpected error on undo: %s", mcpResultJSON(t, res2))
	}

	onDisk2, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk2) != string(broken) {
		t.Errorf("after undo, file = %q, want %q (the pre-restore/broken state)", onDisk2, broken)
	}
}

// A8.4 — revisions written via store.Record are visible through the MCP backend
// (same .lochis/). Asserted explicitly: write via store, read via MCP tool, and
// confirm the revision count/timestamps match what the store reports.
func TestMCP_SharedStore_StoreWritesVisibleViaMCP(t *testing.T) {
	ctx := context.Background()
	s, root := mcpSeedStore(t)

	for _, c := range []string{"v1", "v2", "v3", "v4"} {
		mcpWrite(t, root, "shared.txt", []byte(c))
		if err := s.Record("shared.txt"); err != nil {
			t.Fatal(err)
		}
	}

	// Confirm it really lives under root/.lochis/ (the shared store dir).
	if _, err := os.Stat(filepath.Join(root, store.Dir, "index", "shared.txt.log")); err != nil {
		t.Fatalf("expected shared store log on disk: %v", err)
	}

	cs := mcpClientFor(t, s)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "lochis_list_versions",
		Arguments: map[string]any{"file": "shared.txt"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", mcpResultJSON(t, res))
	}

	var out listOutput
	if err := json.Unmarshal(mcpResultJSON(t, res), &out); err != nil {
		t.Fatalf("decode list output: %v", err)
	}

	storeRevs, _ := s.List("shared.txt")
	if len(out.Versions) != len(storeRevs) {
		t.Fatalf("MCP saw %d versions, store has %d", len(out.Versions), len(storeRevs))
	}
	for i := range storeRevs {
		if out.Versions[i].Timestamp != storeRevs[i].Timestamp {
			t.Errorf("version %d ts via MCP = %d, store = %d",
				i, out.Versions[i].Timestamp, storeRevs[i].Timestamp)
		}
	}

	// And the newest content read via MCP equals the latest store write.
	getRes, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "lochis_get_version",
		Arguments: map[string]any{"file": "shared.txt", "timestamp": out.Versions[0].Timestamp},
	})
	if err != nil {
		t.Fatalf("CallTool get: %v", err)
	}
	var got getOutput
	if err := json.Unmarshal(mcpResultJSON(t, getRes), &got); err != nil {
		t.Fatalf("decode get output: %v", err)
	}
	if got.Content != "v4" {
		t.Errorf("newest content via MCP = %q, want v4", got.Content)
	}
}

// A8.5 — EXACTLY 3 tools, none destructive. Security invariant: the agent that
// might break things must not be able to delete/purge/gc its own safety net.
func TestMCP_ExactlyThreeReadOrRestoreTools(t *testing.T) {
	ctx := context.Background()
	s, _ := mcpSeedStore(t)

	cs := mcpClientFor(t, s)
	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	if len(lt.Tools) != 3 {
		var names []string
		for _, tool := range lt.Tools {
			names = append(names, tool.Name)
		}
		t.Fatalf("want exactly 3 tools, got %d: %v", len(lt.Tools), names)
	}

	got := map[string]bool{}
	for _, tool := range lt.Tools {
		got[tool.Name] = true
	}
	want := []string{"lochis_list_versions", "lochis_get_version", "lochis_restore"}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing expected tool %q (have %v)", w, keysOf(got))
		}
	}

	// No tool name may imply a destructive operation.
	destructive := []string{"delete", "purge", "gc", "rm", "remove", "destroy", "wipe", "clear", "drop", "prune"}
	for name := range got {
		lower := strings.ToLower(name)
		for _, bad := range destructive {
			if strings.Contains(lower, bad) {
				t.Errorf("tool %q implies a destructive operation (%q) — must not be exposed", name, bad)
			}
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A8.6 — lochis_get_version with a non-existent timestamp surfaces a clear,
// structured error (IsError result), not a panic and not an ambiguous empty
// success.
func TestMCP_GetVersion_NonexistentTimestamp_ClearError(t *testing.T) {
	ctx := context.Background()
	s, root := mcpSeedStore(t)

	mcpWrite(t, root, "a.txt", []byte("only"))
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}

	cs := mcpClientFor(t, s)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "lochis_get_version",
		Arguments: map[string]any{"file": "a.txt", "timestamp": int64(999999999999)},
	})
	// A tool-level error is reported as IsError on the result, not as a
	// transport/protocol error, so err should be nil here.
	if err != nil {
		t.Fatalf("transport error (want a tool IsError result instead): %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError result for a non-existent timestamp, got success: %s", mcpResultJSON(t, res))
	}

	// The error must be comprehensible: non-empty text mentioning the file or
	// the missing revision.
	if len(res.Content) == 0 {
		t.Fatalf("error result has no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("error content is %T, want *mcp.TextContent", res.Content[0])
	}
	msg := strings.ToLower(tc.Text)
	if msg == "" {
		t.Fatalf("error message is empty (ambiguous)")
	}
	if !strings.Contains(msg, "revision") && !strings.Contains(msg, "timestamp") && !strings.Contains(msg, "a.txt") {
		t.Errorf("error message not comprehensible: %q", tc.Text)
	}
}
