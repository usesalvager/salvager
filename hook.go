package main

import (
	"encoding/json"
	"io"
	"os"

	"github.com/usesalvager/salvager/guard"
)

// hook.go is the Claude Code adapter: the only agent-specific layer in the
// interception feature. It translates Claude Code's PreToolUse protocol to and
// from the agent-agnostic guard core and contains ZERO classification logic — all
// it does is map I/O. A second agent (Cursor, Aider, …) is a sibling adapter over
// the same guard.Classify, not a rewrite.
//
// TODO(adapter): cursor / openclaw / aider adapters reuse guard.Classify — each
// parses its own interception protocol into a guard.Request and maps the
// guard.Decision back, exactly as this one does.

// preToolUseEvent is the subset of Claude Code's PreToolUse stdin payload we read.
// Unknown fields (session_id, tool_use_id, …) are ignored.
type preToolUseEvent struct {
	HookEventName string          `json:"hook_event_name"`
	CWD           string          `json:"cwd"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
}

// bashToolInput is the Bash tool's input; command is the shell string we classify.
type bashToolInput struct {
	Command string `json:"command"`
}

// hookOutput is Claude Code's PreToolUse decision JSON written to stdout.
type hookOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName string `json:"hookEventName"`
	// PermissionDecision is "deny" for Tier A. It is OMITTED for Tier B: emitting
	// "allow" would skip the user's normal permission prompt and over-permit, so a
	// checkpoint adds context only and lets the normal flow proceed.
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
}

// maxStdin caps how much of stdin we read, so a pathological payload cannot make
// the hook allocate without bound in the agent's hot path. A Bash command line is
// tiny; 8 MiB is orders of magnitude of headroom.
const maxStdin = 8 << 20

// cmdHook is the `salvager hook` entry point, invoked by Claude Code (never by a
// human) on every Bash tool call. fallbackRoot (the process cwd) is used only if
// the event omits its own cwd.
func cmdHook(fallbackRoot string) {
	runHook(os.Stdin, os.Stdout, fallbackRoot)
}

// runHook reads one PreToolUse event, classifies it via guard, and writes Claude
// Code's decision JSON. It is split out from cmdHook so a test can drive it with
// in-memory stdin/stdout.
//
// FAIL-OPEN, ALWAYS. If stdin can't be read or parsed, the tool isn't Bash, the
// command is empty, or the core panics, runHook writes nothing and returns —
// Claude Code then proceeds as normal (allow). Salvager must never brick the agent
// by blocking on its own bug: a net that jams the tool it guards is worse than
// none. The only output it ever writes is a Tier A deny or a Tier B context hint.
func runHook(stdin io.Reader, stdout io.Writer, fallbackRoot string) {
	data, err := io.ReadAll(io.LimitReader(stdin, maxStdin))
	if err != nil {
		return // fail open
	}
	var ev preToolUseEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return // unparseable event → allow
	}
	if ev.ToolName != "Bash" {
		return // only the Bash matcher is wired (see init's TODO(hook) for C2)
	}
	var in bashToolInput
	if err := json.Unmarshal(ev.ToolInput, &in); err != nil || in.Command == "" {
		return
	}

	root := ev.CWD
	if root == "" {
		root = fallbackRoot
	}
	req := guard.Request{Tool: "Bash", Command: in.Command, Root: root, Agent: "claude-code"}

	decision := classifySafe(req)
	_ = guard.LogAttempt(req, decision) // seismograph; logging must never affect the verdict

	switch decision.Tier {
	case guard.TierDeny:
		writeDecision(stdout, hookSpecificOutput{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "deny",
			PermissionDecisionReason: decision.Reason,
		})
	case guard.TierCheckpoint:
		writeDecision(stdout, hookSpecificOutput{
			HookEventName:     "PreToolUse",
			AdditionalContext: decision.RecoveryHint,
		})
	default:
		// TierPass: no output, exit 0.
	}
}

// classifySafe wraps the core so a panic becomes an allow (fail-open), preserving
// the invariant even against an unexpected bug in classification.
func classifySafe(req guard.Request) (d guard.Decision) {
	defer func() {
		if recover() != nil {
			d = guard.Decision{Tier: guard.TierPass}
		}
	}()
	return guard.Classify(req)
}

// writeDecision emits the decision JSON. A marshal/write failure is swallowed:
// there is nothing safe to do but stay quiet, which is itself fail-open (allow).
func writeDecision(w io.Writer, out hookSpecificOutput) {
	b, err := json.Marshal(hookOutput{HookSpecificOutput: out})
	if err != nil {
		return
	}
	_, _ = w.Write(b)
}
