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

func runHookStr(event, fallback string) string {
	var out bytes.Buffer
	runHook(strings.NewReader(event), &out, fallback)
	return out.String()
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
