package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/usesalvager/salvager/store"
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
// the MCP server reads the very same .salvager/ (A8.4).
func mcpSeedStore(t *testing.T) (*store.FS, string) {
	t.Helper()
	root := t.TempDir()
	return store.New(root), root
}

// A8.1 — salvager_list_versions returns revisions newest-first, each with raw
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
		Name:      "salvager_list_versions",
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

// A10 regression — the failure that motivated the content signal. A file is
// captured WITH a function (median), then a later revision loses it. The agent
// in the original trace saw only timestamps + labels, inspected only the broken
// revision, and wrongly concluded the function "was never added". list_versions
// must now let an agent tell which revision holds the work — the line delta and
// signature — WITHOUT calling salvager_get_version on a single revision.
func TestMCP_ListVersions_Signal_DistinguishesByDelta(t *testing.T) {
	ctx := context.Background()
	s, root := mcpSeedStore(t)

	withMedian := "def mean(xs):\n" +
		"    return sum(xs) / len(xs)\n" +
		"\n" +
		"\n" +
		"def median(xs):\n" +
		"    s = sorted(xs)\n" +
		"    n = len(s)\n" +
		"    mid = n // 2\n" +
		"    if n % 2:\n" +
		"        return s[mid]\n" +
		"    return (s[mid - 1] + s[mid]) / 2\n"
	withoutMedian := "def mean(xs):\n" +
		"    return sum(xs) / len(xs)\n"

	mcpWrite(t, root, "stats.py", []byte(withMedian))
	if err := s.Record("stats.py"); err != nil {
		t.Fatal(err)
	}
	mcpWrite(t, root, "stats.py", []byte(withoutMedian))
	if err := s.Record("stats.py"); err != nil {
		t.Fatal(err)
	}

	cs := mcpClientFor(t, s)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "salvager_list_versions",
		Arguments: map[string]any{"file": "stats.py"},
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
	if len(out.Versions) != 2 {
		t.Fatalf("want 2 versions, got %d", len(out.Versions))
	}

	modify := out.Versions[0]    // newest: median removed
	firstSeen := out.Versions[1] // oldest: median present

	// The whole point: every revision exposes its signal up front.
	if !modify.HasSignal || !firstSeen.HasSignal {
		t.Fatalf("a revision is missing its signal: modify=%+v first-seen=%+v", modify, firstSeen)
	}

	// 1. The oldest revision is NOT presented as an empty baseline: its label is
	// the explicit "first-seen", never the misleading "initial".
	if firstSeen.Label != string(store.LabelInitial) {
		t.Errorf("oldest label = %q, want the LabelInitial value", firstSeen.Label)
	}
	if firstSeen.Label != "first-seen" {
		t.Errorf("LabelInitial wire value = %q, want \"first-seen\" (must not read as an empty baseline)", firstSeen.Label)
	}

	// 2. The revision that HOLDS the work is identifiable by line count alone.
	if firstSeen.Lines <= modify.Lines {
		t.Errorf("first-seen lines (%d) should exceed modify lines (%d): the work lives in the larger revision",
			firstSeen.Lines, modify.Lines)
	}

	// 3. The delta on the broken revision is clearly NEGATIVE — lines were
	// removed — which directly refutes a "only formatting changed" reading.
	if !strings.HasPrefix(modify.DeltaLines, "-") {
		t.Errorf("modify delta_lines = %q, want a negative delta (work was removed)", modify.DeltaLines)
	}
	wantDelta := strconv.Itoa(modify.Lines - firstSeen.Lines)
	if got := strings.TrimPrefix(modify.DeltaLines, "+"); got != wantDelta {
		t.Errorf("modify delta_lines = %q, want %q (modify.Lines - firstSeen.Lines)", modify.DeltaLines, wantDelta)
	}

	// 4. The signatures are present so an agent can eyeball the start of each
	// revision without reading the whole object.
	if firstSeen.Signature == "" || modify.Signature == "" {
		t.Errorf("signatures must be present: first-seen=%q modify=%q", firstSeen.Signature, modify.Signature)
	}
}

// list_versions surfaces legacy (pre-signal) revisions as "signal unavailable"
// — has_signal false, no fabricated numbers — rather than failing.
func TestMCP_ListVersions_LegacyRevisionHasNoSignal(t *testing.T) {
	ctx := context.Background()
	s, root := mcpSeedStore(t)

	// Hand-write a legacy three-column log line (the old on-disk format).
	lp := filepath.Join(root, store.Dir, "index", "old.txt.log")
	if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lp, []byte("1700000000000\tdeadbeefdeadbeef\tmodify\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cs := mcpClientFor(t, s)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "salvager_list_versions",
		Arguments: map[string]any{"file": "old.txt"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("legacy log must list without error: %s", mcpResultJSON(t, res))
	}

	var out listOutput
	if err := json.Unmarshal(mcpResultJSON(t, res), &out); err != nil {
		t.Fatalf("decode list output: %v", err)
	}
	if len(out.Versions) != 1 {
		t.Fatalf("want 1 legacy version, got %d", len(out.Versions))
	}
	v := out.Versions[0]
	if v.HasSignal {
		t.Errorf("legacy version: has_signal = true, want false")
	}
	if v.DeltaLines != "" || v.Signature != "" || v.Lines != 0 {
		t.Errorf("legacy version leaked a fabricated signal: %+v", v)
	}
}

// A8.2 — salvager_get_version returns exactly the content for that revision.
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
		Name:      "salvager_get_version",
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

// A8.3 — salvager_restore restores the on-disk file AND reports a non-zero
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
		Name:      "salvager_restore",
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
		Name:      "salvager_restore",
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

// medianFixture is the A10 scenario as two states of one file: a "first-seen"
// revision that HOLDS a median() function and a later "modify" revision that
// lost it. Returns (withWork, withoutWork).
func medianFixture() (string, string) {
	withWork := "def mean(xs):\n" +
		"    return sum(xs) / len(xs)\n" +
		"\n" +
		"\n" +
		"def median(xs):\n" +
		"    s = sorted(xs)\n" +
		"    n = len(s)\n" +
		"    mid = n // 2\n" +
		"    if n % 2:\n" +
		"        return s[mid]\n" +
		"    return (s[mid - 1] + s[mid]) / 2\n"
	withoutWork := "def mean(xs):\n" +
		"    return sum(xs) / len(xs)\n"
	return withWork, withoutWork
}

// seedMedianBaseline records the two-revision A10 baseline (first-seen WITH the
// work, modify WITHOUT it) and leaves disk in the work-lost state. Returns the
// timestamp of the first-seen revision that holds the work.
func seedMedianBaseline(t *testing.T, s *store.FS, root string) (firstSeenTs int64) {
	t.Helper()
	withWork, withoutWork := medianFixture()
	mcpWrite(t, root, "stats.py", []byte(withWork))
	if err := s.Record("stats.py"); err != nil {
		t.Fatal(err)
	}
	mcpWrite(t, root, "stats.py", []byte(withoutWork))
	if err := s.Record("stats.py"); err != nil {
		t.Fatal(err)
	}
	revs, _ := s.List("stats.py") // newest first: modify, first-seen
	return revs[len(revs)-1].Timestamp
}

// Recovering lost work through salvager_restore leaves the safe-path trace in
// history: a pre-restore revision followed by a restore revision, NOT a generic
// modify. The restore tool stays valid after reverting the inspect/restore
// separation, and anyone (human or agent) who uses it must still get the
// reversible pre-restore + restore pair, with the work back on disk byte-for-byte.
func TestMCP_Restore_LeavesPreRestoreThenRestoreTrace(t *testing.T) {
	ctx := context.Background()
	s, root := mcpSeedStore(t)
	firstSeenTs := seedMedianBaseline(t, s, root)
	withWork, _ := medianFixture()

	cs := mcpClientFor(t, s)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "salvager_restore",
		Arguments: map[string]any{"file": "stats.py", "timestamp": firstSeenTs},
	})
	if err != nil {
		t.Fatalf("CallTool restore: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", mcpResultJSON(t, res))
	}

	// The work is back on disk, byte-for-byte.
	onDisk, err := os.ReadFile(filepath.Join(root, "stats.py"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != withWork {
		t.Errorf("after restore, file does not hold the recovered work:\n%s", onDisk)
	}

	// The history's two newest revisions are the safe-path trace: a pre-restore
	// (the saved current state) then a restore — never a bare modify.
	revs, _ := s.List("stats.py") // newest first
	if len(revs) < 4 {
		t.Fatalf("want at least 4 revisions (first-seen, modify, pre-restore, restore), got %d", len(revs))
	}
	if revs[0].Label != store.LabelRestore {
		t.Errorf("newest label = %q, want %q", revs[0].Label, store.LabelRestore)
	}
	if revs[1].Label != store.LabelPreRestore {
		t.Errorf("second-newest label = %q, want %q", revs[1].Label, store.LabelPreRestore)
	}
}

// A8.4 — revisions written via store.Record are visible through the MCP backend
// (same .salvager/). Asserted explicitly: write via store, read via MCP tool, and
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

	// Confirm it really lives under root/.salvager/ (the shared store dir).
	if _, err := os.Stat(filepath.Join(root, store.Dir, "index", "shared.txt.log")); err != nil {
		t.Fatalf("expected shared store log on disk: %v", err)
	}

	cs := mcpClientFor(t, s)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "salvager_list_versions",
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
		Name:      "salvager_get_version",
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
	want := []string{"salvager_list_versions", "salvager_get_version", "salvager_restore"}
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

// A8.6 — salvager_get_version with a non-existent timestamp surfaces a clear,
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
		Name:      "salvager_get_version",
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
