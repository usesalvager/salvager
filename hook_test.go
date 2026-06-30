package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/usesalvager/salvager/guard"
)

// bashEvent builds a PreToolUse stdin payload for the Bash tool.
func bashEvent(cwd, cmd string) string {
	in, _ := json.Marshal(bashToolInput{Command: cmd})
	ev, _ := json.Marshal(preToolUseEvent{
		HookEventName: "PreToolUse",
		CWD:           cwd,
		ToolName:      "Bash",
		ToolInput:     in,
	})
	return string(ev)
}

// fileEvent builds a PreToolUse stdin payload for the file_path tools (Edit/Write/
// MultiEdit).
func fileEvent(cwd, tool, filePath string) string {
	in, _ := json.Marshal(fileToolInput{FilePath: filePath})
	ev, _ := json.Marshal(preToolUseEvent{
		HookEventName: "PreToolUse",
		CWD:           cwd,
		ToolName:      tool,
		ToolInput:     in,
	})
	return string(ev)
}

// notebookEvent builds a PreToolUse stdin payload for NotebookEdit, whose input
// carries notebook_path (not file_path).
func notebookEvent(cwd, notebookPath string) string {
	in, _ := json.Marshal(notebookToolInput{NotebookPath: notebookPath})
	ev, _ := json.Marshal(preToolUseEvent{
		HookEventName: "PreToolUse",
		CWD:           cwd,
		ToolName:      "NotebookEdit",
		ToolInput:     in,
	})
	return string(ev)
}

func runHookStr(event, fallback string) string {
	var out bytes.Buffer
	runHook(strings.NewReader(event), &out, fallback)
	return out.String()
}

// seedProtected writes a .salvager/protected file under root.
func seedProtected(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".salvager"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".salvager", "protected"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestHook_EditWriteProtected — every file-editor tool the matcher fires for is
// mapped to a deny on a protected path, and to silence on an ordinary one. This is
// the no-bypass guarantee: MultiEdit and NotebookEdit must be covered too, or an
// agent skips the whole protection by picking a different editor tool. The adapter
// holds NO policy — the deny reason is exactly what the core produces.
func TestHook_EditWriteProtected(t *testing.T) {
	root := t.TempDir()
	seedProtected(t, root, "config/prod.yaml\n")

	for _, tc := range []struct{ tool, path, event string }{
		{"Write", ".env", fileEvent(root, "Write", ".env")},                       // default set
		{"Edit", "config/prod.yaml", fileEvent(root, "Edit", "config/prod.yaml")}, // user addition
		{"MultiEdit", ".env", fileEvent(root, "MultiEdit", ".env")},               // same file_path field as Edit/Write
		{"NotebookEdit", ".env", notebookEvent(root, ".env")},                     // distinct notebook_path field
	} {
		out := runHookStr(tc.event, root)
		var deny hookOutput
		if err := json.Unmarshal([]byte(out), &deny); err != nil {
			t.Fatalf("%s %s: output not JSON: %v (%q)", tc.tool, tc.path, err, out)
		}
		if deny.HookSpecificOutput.PermissionDecision != "deny" ||
			deny.HookSpecificOutput.PermissionDecisionReason == "" {
			t.Errorf("%s %s: expected a deny, got %q", tc.tool, tc.path, out)
		}
		want := guard.Classify(guard.Request{Tool: tc.tool, FilePath: tc.path, Root: root, Agent: "claude-code"})
		if deny.HookSpecificOutput.PermissionDecisionReason != want.Reason {
			t.Errorf("%s %s: adapter reason diverged from core (policy leaked into adapter)", tc.tool, tc.path)
		}
	}

	// An ordinary path is allowed (no output), for both field shapes.
	if out := runHookStr(fileEvent(root, "MultiEdit", "src/app.go"), root); out != "" {
		t.Errorf("MultiEdit to an ordinary path must be silent, got %q", out)
	}
	if out := runHookStr(notebookEvent(root, "notebooks/run.ipynb"), root); out != "" {
		t.Errorf("NotebookEdit to an ordinary path must be silent, got %q", out)
	}
}

// TestHook_EditWriteFailOpen — malformed/empty Edit/Write input still fails open.
func TestHook_EditWriteFailOpen(t *testing.T) {
	root := t.TempDir()
	cases := map[string]string{
		"empty file_path":        fileEvent(root, "Edit", ""),
		"empty notebook_path":    notebookEvent(root, ""),
		"malformed input":        `{"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":"not-an-object"}`,
		"notebook wrong field":   `{"hook_event_name":"PreToolUse","tool_name":"NotebookEdit","tool_input":{"file_path":".env"}}`,
		"genuinely unknown tool": `{"hook_event_name":"PreToolUse","tool_name":"WebFetch","tool_input":{"file_path":".env"}}`,
		"missing tool_input":     `{"hook_event_name":"PreToolUse","tool_name":"Edit"}`,
	}
	for name, ev := range cases {
		if out := runHookStr(ev, root); out != "" {
			t.Errorf("%s: must fail open (no output), got %q", name, out)
		}
	}
}

// TestHook_DecisionShapePerTier — the adapter maps each guard tier to the right
// Claude Code decision JSON: deny carries a permissionDecision, checkpoint carries
// only additionalContext (no permission decision, which would over-permit), pass
// is silent.
func TestHook_DecisionShapePerTier(t *testing.T) {
	root := t.TempDir()

	// Tier A → deny with a reason.
	out := runHookStr(bashEvent(root, "rm -rf ~"), root)
	var deny hookOutput
	if err := json.Unmarshal([]byte(out), &deny); err != nil {
		t.Fatalf("deny output not JSON: %v (%q)", err, out)
	}
	if deny.HookSpecificOutput.PermissionDecision != "deny" ||
		deny.HookSpecificOutput.PermissionDecisionReason == "" ||
		deny.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Errorf("deny shape wrong: %q", out)
	}

	// Tier B → additionalContext only, NO permission decision.
	out = runHookStr(bashEvent(root, "git reset --hard"), root)
	var cp hookOutput
	if err := json.Unmarshal([]byte(out), &cp); err != nil {
		t.Fatalf("checkpoint output not JSON: %v (%q)", err, out)
	}
	if cp.HookSpecificOutput.PermissionDecision != "" {
		t.Errorf("checkpoint must NOT emit a permissionDecision (would over-permit): %q", out)
	}
	if cp.HookSpecificOutput.AdditionalContext == "" {
		t.Errorf("checkpoint must carry the recovery hint as additionalContext: %q", out)
	}

	// Pass → silence.
	if out := runHookStr(bashEvent(root, "ls -la"), root); out != "" {
		t.Errorf("pass must produce no output, got %q", out)
	}
}

// TestHook_FailOpen — anything that goes wrong must allow (no output), never deny.
func TestHook_FailOpen(t *testing.T) {
	root := t.TempDir()
	cases := map[string]string{
		"malformed json":     "{not valid json",
		"empty stdin":        "",
		"non-Bash tool":      `{"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":"x"}}`,
		"empty command":      bashEvent(root, ""),
		"missing tool_input": `{"hook_event_name":"PreToolUse","tool_name":"Bash"}`,
	}
	for name, ev := range cases {
		if out := runHookStr(ev, root); out != "" {
			t.Errorf("%s: must fail open (no output), got %q", name, out)
		}
	}
}

// TestHook_RecordsAgent — the adapter stamps the seismograph with "claude-code"
// so the log records which adapter saw the attempt.
func TestHook_RecordsAgent(t *testing.T) {
	root := t.TempDir()
	runHookStr(bashEvent(root, "rm -rf ~"), root)

	raw, err := os.ReadFile(filepath.Join(root, ".salvager", "hook-log"))
	if err != nil {
		t.Fatalf("read hook-log: %v", err)
	}
	var e struct {
		Agent string `json:"agent"`
		Tier  string `json:"tier"`
		Tool  string `json:"tool"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &e); err != nil {
		t.Fatalf("hook-log line not JSON: %v", err)
	}
	if e.Agent != "claude-code" || e.Tier != "deny" || e.Tool != "Bash" {
		t.Errorf("seismograph entry wrong: %+v", e)
	}
}

// TestHook_DelegatesToCore — the adapter holds NO tiering logic: the deny reason
// it emits is exactly what guard.Classify produces for the same request. If a
// tier rule ever leaked into the adapter, this divergence would catch it.
func TestHook_DelegatesToCore(t *testing.T) {
	root := t.TempDir()
	cmd := "rm -rf /etc"
	want := guard.Classify(guard.Request{Tool: "Bash", Command: cmd, Root: root, Agent: "claude-code"})

	var got hookOutput
	if err := json.Unmarshal([]byte(runHookStr(bashEvent(root, cmd), root)), &got); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if got.HookSpecificOutput.PermissionDecisionReason != want.Reason {
		t.Errorf("adapter reason diverged from core:\n got %q\nwant %q",
			got.HookSpecificOutput.PermissionDecisionReason, want.Reason)
	}
}

// TestHook_CWDFallback — when the event omits cwd, the process root is used.
func TestHook_CWDFallback(t *testing.T) {
	root := t.TempDir()
	ev := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"git reset --hard"}}`
	out := runHookStr(ev, root)
	if !strings.Contains(out, "additionalContext") {
		t.Errorf("expected a checkpoint using the fallback root, got %q", out)
	}
}
