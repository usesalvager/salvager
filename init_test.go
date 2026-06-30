package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClaude records calls and returns canned (out, code) responses keyed by the
// subcommand verb ("get", "add", "remove"). It models the real CLI's contract:
// `mcp get` exits non-zero when the server is absent, zero when present.
type fakeClaude struct {
	registered   bool   // is "salvager" currently registered?
	command      string // what Command: `get` reports
	calls        []string
	failAdd      bool
	notConnected bool // registered but NOT connected → `get` omits "Connected"
}

func (f *fakeClaude) run(args ...string) (string, int) {
	f.calls = append(f.calls, strings.Join(args, " "))
	// args like: mcp get salvager  /  mcp add salvager --scope local -- <exe> mcp
	if len(args) < 2 || args[0] != "mcp" {
		return "", 1
	}
	switch args[1] {
	case "get":
		if !f.registered {
			return "", 1
		}
		status := "✔ Connected"
		if f.notConnected {
			status = "✘ Failed to connect"
		}
		out := "salvager:\n  Status: " + status + "\n  Type: stdio\n  Command: " +
			f.command + "\n  Args: mcp\n"
		return out, 0
	case "add":
		if f.failAdd {
			return "error adding", 1
		}
		f.registered = true
		// the binary registered is the last-but-one arg (before "mcp")
		if len(args) >= 2 {
			f.command = args[len(args)-2]
		}
		return "Added stdio MCP server salvager", 0
	case "remove":
		f.registered = false
		return "Removed", 0
	}
	return "", 1
}

// newTestEnv builds an initEnv with a temp HOME and an injected fake claude.
func newTestEnv(t *testing.T, fc *fakeClaude, claudeOnPath bool) *initEnv {
	t.Helper()
	home := t.TempDir()
	root := t.TempDir()
	claudePath := ""
	if claudeOnPath {
		claudePath = "claude"
	}
	return &initEnv{
		root:       root,
		home:       home,
		exePath:    "/usr/local/bin/salvager",
		claudePath: claudePath,
		runClaude:  fc.run,
		stdout:     io.Discard,
		stderr:     io.Discard,
	}
}

// newCapturingEnv is newTestEnv with stdout routed to a buffer, so a test can
// assert on init's user-facing report (newTestEnv discards it). The report is
// the only confirmation the user gets of what init actually did, so it is worth
// guarding.
func newCapturingEnv(t *testing.T, fc *fakeClaude, claudeOnPath bool) (*initEnv, *bytes.Buffer) {
	t.Helper()
	env := newTestEnv(t, fc, claudeOnPath)
	var buf bytes.Buffer
	env.stdout = &buf
	return env, &buf
}

func readMD(t *testing.T, env *initEnv) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(claudeMDPath(env.home))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	return string(data), true
}

func mdHasBlock(s string) bool {
	return strings.Contains(s, mdStartMarker) && strings.Contains(s, mdEndMarker)
}

func TestInit_Idempotent_BothPieces(t *testing.T) {
	fc := &fakeClaude{}
	env := newTestEnv(t, fc, true)

	runInit(env, false)
	first, ok := readMD(t, env)
	if !ok || !mdHasBlock(first) {
		t.Fatal("first init did not create the CLAUDE.md block")
	}
	if !fc.registered {
		t.Fatal("first init did not register the MCP server")
	}

	// Second run: same state, no churn.
	r := reconcileMCP(env)
	if r.state != "already registered for this project" {
		t.Fatalf("MCP not idempotent: %q", r.state)
	}
	rmd := reconcileClaudeMD(env)
	if rmd.state != "already current" {
		t.Fatalf("CLAUDE.md not idempotent: %q", rmd.state)
	}
	second, _ := readMD(t, env)
	if first != second {
		t.Fatal("second init changed CLAUDE.md content")
	}
}

func TestInit_CreatesClaudeMDWhenAbsent(t *testing.T) {
	fc := &fakeClaude{}
	env := newTestEnv(t, fc, true)
	if _, ok := readMD(t, env); ok {
		t.Fatal("precondition: CLAUDE.md should not exist yet")
	}
	r := reconcileClaudeMD(env)
	if r.state != "created" {
		t.Fatalf("expected created, got %q", r.state)
	}
	got, ok := readMD(t, env)
	if !ok || !mdHasBlock(got) {
		t.Fatal("CLAUDE.md not created with block")
	}
}

func TestInit_PreservesSurroundingContent(t *testing.T) {
	fc := &fakeClaude{}
	env := newTestEnv(t, fc, true)

	// Plant user content with an existing (stale) block in the MIDDLE.
	before := "# My personal instructions\n\nAlways use tabs.\n\n"
	staleBlock := mdStartMarker + "\nOLD STALE CONTENT\n" + mdEndMarker
	after := "\n\n## More of my notes\n\nDeploy on Fridays.\n"
	original := before + staleBlock + after
	if err := os.MkdirAll(filepath.Dir(claudeMDPath(env.home)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeMDPath(env.home), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	r := reconcileClaudeMD(env)
	if r.state != "updated" {
		t.Fatalf("expected updated, got %q", r.state)
	}
	got, _ := readMD(t, env)
	if !strings.Contains(got, "Always use tabs.") {
		t.Error("content before the block was lost")
	}
	if !strings.Contains(got, "Deploy on Fridays.") {
		t.Error("content after the block was lost")
	}
	if strings.Contains(got, "OLD STALE CONTENT") {
		t.Error("stale block content survived the update")
	}
	if !strings.Contains(got, "salvager_restore") {
		t.Error("fresh block content not written")
	}
}

func TestInit_AppendsWhenNoMarkers(t *testing.T) {
	fc := &fakeClaude{}
	env := newTestEnv(t, fc, true)

	original := "# Personal CLAUDE.md\n\nNo salvager here yet.\n"
	if err := os.MkdirAll(filepath.Dir(claudeMDPath(env.home)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeMDPath(env.home), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	r := reconcileClaudeMD(env)
	if r.state != "updated" {
		t.Fatalf("expected updated, got %q", r.state)
	}
	got, _ := readMD(t, env)
	if !strings.HasPrefix(got, original) {
		t.Error("original content not preserved at the top after append")
	}
	if !mdHasBlock(got) {
		t.Error("block not appended")
	}
}

func TestInit_NoClaudeCLI_SkipsMCPButWritesClaudeMD(t *testing.T) {
	fc := &fakeClaude{}
	env := newTestEnv(t, fc, false) // claude NOT on PATH

	rmcp := reconcileMCP(env)
	if rmcp.ok {
		t.Error("MCP piece should report not-ok when claude is absent")
	}
	if !strings.Contains(rmcp.detail, "claude mcp add salvager") {
		t.Errorf("expected manual command in detail, got %q", rmcp.detail)
	}
	if len(fc.calls) != 0 {
		t.Errorf("claude must not be invoked when absent; calls=%v", fc.calls)
	}

	// CLAUDE.md proceeds independently.
	rmd := reconcileClaudeMD(env)
	if !rmd.ok || rmd.state != "created" {
		t.Fatalf("CLAUDE.md should still be created, got ok=%v state=%q", rmd.ok, rmd.state)
	}
}

func TestInit_PartialState_MCPPresentClaudeMDMissing(t *testing.T) {
	fc := &fakeClaude{registered: true, command: "/usr/local/bin/salvager"}
	env := newTestEnv(t, fc, true)

	rmcp := reconcileMCP(env)
	if rmcp.state != "already registered for this project" {
		t.Fatalf("MCP should be left alone, got %q", rmcp.state)
	}
	// no add call beyond the get/verify path
	for _, c := range fc.calls {
		if strings.HasPrefix(c, "mcp add") {
			t.Error("should not re-add an already-correct server")
		}
	}
	rmd := reconcileClaudeMD(env)
	if rmd.state != "created" {
		t.Fatalf("CLAUDE.md should be created, got %q", rmd.state)
	}
}

func TestInit_MCPWrongTarget_GetsCorrected(t *testing.T) {
	fc := &fakeClaude{registered: true, command: "/some/old/path/salvager"}
	env := newTestEnv(t, fc, true)

	r := reconcileMCP(env)
	if !r.ok {
		t.Fatalf("expected corrected MCP to be ok, got state=%q", r.state)
	}
	sawRemove, sawAdd := false, false
	for _, c := range fc.calls {
		if strings.HasPrefix(c, "mcp remove") {
			sawRemove = true
		}
		if strings.HasPrefix(c, "mcp add") {
			sawAdd = true
		}
	}
	if !sawRemove || !sawAdd {
		t.Errorf("wrong target must trigger remove+add; calls=%v", fc.calls)
	}
	if fc.command != env.exePath {
		t.Errorf("re-add must point at this binary, got %q", fc.command)
	}
}

func TestInit_MCPParseFailure_DegradesSafe(t *testing.T) {
	// registered=true but command empty → `get` output has no Command: line.
	fc := &fakeClaude{registered: true, command: ""}
	// Override run to emit output with NO Command: line.
	env := newTestEnv(t, fc, true)
	env.runClaude = func(args ...string) (string, int) {
		fc.calls = append(fc.calls, strings.Join(args, " "))
		if len(args) >= 2 && args[1] == "get" {
			return "salvager:\n  Status: ✔ Connected\n  (unexpected format)\n", 0
		}
		return "", 0
	}

	r := reconcileMCP(env)
	if !r.ok {
		t.Errorf("parse failure should degrade safe (ok), got not-ok: %q", r.state)
	}
	for _, c := range fc.calls {
		if strings.HasPrefix(c, "mcp remove") {
			t.Error("parse failure must NOT trigger a destructive remove")
		}
	}
}

func TestInit_AddFailure_ReportsManualCommand(t *testing.T) {
	fc := &fakeClaude{failAdd: true}
	env := newTestEnv(t, fc, true)
	r := reconcileMCP(env)
	if r.ok {
		t.Error("add failure should report not-ok")
	}
	if !strings.Contains(r.detail, "claude mcp add salvager") {
		t.Errorf("expected manual command on failure, got %q", r.detail)
	}
}

func TestUndo_RestoresClaudeMD(t *testing.T) {
	fc := &fakeClaude{}
	env := newTestEnv(t, fc, true)

	// Start with personal content, no block.
	original := "# Personal CLAUDE.md\n\nMy own notes.\n"
	if err := os.MkdirAll(filepath.Dir(claudeMDPath(env.home)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeMDPath(env.home), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	// init appends the block...
	if r := reconcileClaudeMD(env); r.state != "updated" {
		t.Fatalf("setup: expected updated, got %q", r.state)
	}
	// ...undo removes it, leaving the original.
	if r := undoClaudeMD(env); r.state != "block removed" {
		t.Fatalf("expected block removed, got %q", r.state)
	}
	got, _ := readMD(t, env)
	if got != original {
		t.Errorf("undo did not restore original:\n--- got ---\n%q\n--- want ---\n%q", got, original)
	}
}

func TestUndo_MCPRemoves(t *testing.T) {
	fc := &fakeClaude{registered: true, command: "/usr/local/bin/salvager"}
	env := newTestEnv(t, fc, true)
	r := undoMCP(env)
	if r.state != "removed" {
		t.Fatalf("expected removed, got %q", r.state)
	}
	if fc.registered {
		t.Error("server still registered after undo")
	}
}

func TestUndo_MCPNotPresent(t *testing.T) {
	fc := &fakeClaude{registered: false}
	env := newTestEnv(t, fc, true)
	r := undoMCP(env)
	if !r.ok || !strings.Contains(r.state, "nothing to remove") {
		t.Fatalf("expected graceful no-op, got ok=%v state=%q", r.ok, r.state)
	}
}

func TestNoClaudeMD_FlagSkipsFile(t *testing.T) {
	fc := &fakeClaude{}
	env := newTestEnv(t, fc, true)
	runInit(env, true) // noClaudeMD = true
	if _, ok := readMD(t, env); ok {
		t.Error("--no-claude-md should not create CLAUDE.md")
	}
	if !fc.registered {
		t.Error("--no-claude-md should still register MCP")
	}
}

// --- report output --------------------------------------------------------
//
// init's report is its only confirmation to the user of what happened: which MCP
// server got registered, whether CLAUDE.md was written, and what to do next. The
// rest of the suite routes stdout to io.Discard, so nothing guards these promises
// — if the report silently drifts or a line is dropped, no test notices. These
// capture stdout and assert the REAL strings report()/printPieceReport() print.

func TestInit_Report_DefaultRun(t *testing.T) {
	fc := &fakeClaude{}
	env, buf := newCapturingEnv(t, fc, true)

	runInit(env, false)
	out := buf.String()

	// Promise 1: the MCP server was registered for this project (✓, connected —
	// the fake claude reports "Connected", which report() renders as "connected").
	if !strings.Contains(out, "MCP server") || !strings.Contains(out, "✓ connected") {
		t.Errorf("report omits the MCP-registered confirmation:\n%s", out)
	}
	// Promise 2: the CLAUDE.md block was written (fresh HOME → "created").
	if !strings.Contains(out, "CLAUDE.md") || !strings.Contains(out, "✓ created") {
		t.Errorf("report omits the CLAUDE.md confirmation:\n%s", out)
	}
	// Promise 3: the next-step footer points the user at the watcher.
	if !strings.Contains(out, "Next: start the watcher") {
		t.Errorf("report omits the start-watcher footer:\n%s", out)
	}
}

func TestInit_Report_NoMDFlag(t *testing.T) {
	fc := &fakeClaude{}
	env, buf := newCapturingEnv(t, fc, true)

	runInit(env, true) // --no-claude-md
	out := buf.String()

	// MCP is still registered and confirmed.
	if !strings.Contains(out, "MCP server") || !strings.Contains(out, "✓ connected") {
		t.Errorf("--no-claude-md must still confirm MCP registration:\n%s", out)
	}
	// No CLAUDE.md piece is reconciled, so no CLAUDE.md line appears at all.
	if strings.Contains(out, "CLAUDE.md") {
		t.Errorf("--no-claude-md must not emit a CLAUDE.md confirmation:\n%s", out)
	}
	// The footer still guides the user to the watcher.
	if !strings.Contains(out, "Next: start the watcher") {
		t.Errorf("--no-claude-md report omits the start-watcher footer:\n%s", out)
	}
}

func TestInit_Report_Undo(t *testing.T) {
	fc := &fakeClaude{registered: true, command: "/usr/local/bin/salvager"}
	env, buf := newCapturingEnv(t, fc, true)

	// Seed a CLAUDE.md block so undo has both pieces to remove.
	if r := reconcileClaudeMD(env); r.state != "created" {
		t.Fatalf("setup: expected created, got %q", r.state)
	}

	runUndo(env, false)
	out := buf.String()

	// Undo identifies itself as such in the header.
	if !strings.Contains(out, "init --undo") {
		t.Errorf("undo report omits the --undo verb:\n%s", out)
	}
	// Both pieces report their removal ("✓ removed" is the MCP line; the
	// CLAUDE.md line is the distinct "✓ block removed").
	if !strings.Contains(out, "MCP server") || !strings.Contains(out, "✓ removed") {
		t.Errorf("undo report omits the MCP removal:\n%s", out)
	}
	if !strings.Contains(out, "CLAUDE.md") || !strings.Contains(out, "✓ block removed") {
		t.Errorf("undo report omits the CLAUDE.md removal:\n%s", out)
	}
	// Undo must NOT tell the user to start the watcher.
	if strings.Contains(out, "Next: start the watcher") {
		t.Errorf("undo report should not print the start-watcher footer:\n%s", out)
	}
}

// registered-but-not-connected is plausibly the MOST common first-run state:
// init adds the MCP server, but it isn't "Connected" yet (the watcher hasn't
// started / claude hasn't dialed it). The report must say "registered" there,
// not "connected" — that wording is exactly what a first-run user sees. The
// other three report tests only ever hit the connected path.
func TestInit_Report_RegisteredNotConnected(t *testing.T) {
	fc := &fakeClaude{notConnected: true}
	env, buf := newCapturingEnv(t, fc, true)

	runInit(env, false)
	out := buf.String()

	// Promise: the MCP server is registered (verifyMCP → "registered" when the
	// `get` output lacks "Connected").
	if !strings.Contains(out, "MCP server") || !strings.Contains(out, "✓ registered") {
		t.Errorf("report omits the registered-not-connected MCP confirmation:\n%s", out)
	}
	// Isolation: this state must NOT render with the connected-path wording.
	if strings.Contains(out, "✓ connected") {
		t.Errorf("registered-not-connected must not render as connected:\n%s", out)
	}
	// Footer is not conditional on connection state, so it still prints.
	if !strings.Contains(out, "Next: start the watcher") {
		t.Errorf("report omits the start-watcher footer:\n%s", out)
	}
}

// #24 — atomicWrite must NOT widen the permissions of an existing file. This is
// a failure-mode the happy-path suite can't catch: a normal init run stays green
// whether perms are preserved or clobbered, because nothing else asserts them.
func TestAtomicWrite_PreservesExistingPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil { // defeat umask, pin the mode
		t.Fatal(err)
	}
	if err := atomicWrite(path, []byte("new content")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("atomicWrite widened perms: got %o, want 0600", fi.Mode().Perm())
	}
	if got, _ := os.ReadFile(path); string(got) != "new content" {
		t.Errorf("content not written: %q", got)
	}
}

// A brand-new file gets the 0o644 default (subject to umask, so assert it is not
// MORE permissive than 0o644).
func TestAtomicWrite_NewFileDefaultPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	if err := atomicWrite(path, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&^0o644 != 0 {
		t.Errorf("new file more permissive than 0644: got %o", fi.Mode().Perm())
	}
}

// #35/#36 — writeFileAtomic writes the exact bytes at the requested mode and
// leaves no temp file behind. (Crash-atomicity itself needs fault injection to
// prove; this guards the observable contract: content, mode, and a clean dir.)
func TestWriteFileAtomic_ContentModeAndNoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unit.service")
	if err := writeFileAtomic(path, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "[Unit]\n" {
		t.Errorf("content = %q", got)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm()&^0o644 != 0 {
		t.Errorf("mode too permissive: %o", fi.Mode().Perm())
	}
	// No .salvager-tmp-* left in the directory.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".salvager-tmp-") {
			t.Errorf("atomic write left a temp file behind: %s", e.Name())
		}
	}
}

// --- PreToolUse hook piece (#P1) -------------------------------------------

func readSettingsFile(t *testing.T, env *initEnv) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(settingsLocalPath(env.root))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read settings.local.json: %v", err)
	}
	return string(data), true
}

func writeSettingsFile(t *testing.T, env *initEnv, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(settingsLocalPath(env.root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsLocalPath(env.root), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hookAdded when the settings file is absent: a fresh hook with the full file-editor
// matcher, pointing at this binary, is written.
func TestHook_AddedWhenAbsent(t *testing.T) {
	env := newTestEnv(t, &fakeClaude{}, true)

	r := reconcileHook(env)
	if !r.ok || r.state != "registered" {
		t.Fatalf("expected registered, got ok=%v state=%q", r.ok, r.state)
	}
	content, ok := readSettingsFile(t, env)
	if !ok {
		t.Fatal("settings.local.json was not created")
	}
	if !strings.Contains(content, env.exePath+" hook") || !strings.Contains(content, `"`+hookMatcher+`"`) {
		t.Errorf("settings missing the salvager %s hook:\n%s", hookMatcher, content)
	}
}

// An older install (matcher "Bash", or the "Bash|Edit|Write" that predates
// MultiEdit/NotebookEdit) is widened to the current matcher on re-run: the old
// matcher is treated as drift and replaced, not duplicated, so protected-path
// guarding covers every file-editor tool after an upgrade without a manual edit.
func TestHook_WidensOldMatcherOnUpgrade(t *testing.T) {
	for _, old := range []string{"Bash", "Bash|Edit|Write"} {
		t.Run(old, func(t *testing.T) {
			env := newTestEnv(t, &fakeClaude{}, true)
			writeSettingsFile(t, env, fmt.Sprintf(`{
  "hooks": {"PreToolUse": [
    {"matcher": %q, "hooks": [{"type": "command", "command": %q, "timeout": 5}]}
  ]}
}`, old, env.exePath+" hook"))

			if r := reconcileHook(env); !r.ok || r.state != "registered" {
				t.Fatalf("expected the %q matcher to be widened (registered), got %+v", old, r)
			}
			content, _ := readSettingsFile(t, env)
			if !strings.Contains(content, `"`+hookMatcher+`"`) {
				t.Errorf("matcher was not widened to %s:\n%s", hookMatcher, content)
			}
			if strings.Count(content, "salvager hook") != 1 {
				t.Errorf("expected exactly one salvager hook after widening:\n%s", content)
			}
			// And it is now idempotent at the widened matcher.
			if r := reconcileHook(env); !r.ok || r.state != "already registered for this project" {
				t.Errorf("widened hook not idempotent on the next run: %+v", r)
			}
		})
	}
}

// Running reconcileHook twice changes nothing: second run reports already
// registered and the file bytes are byte-for-byte identical.
func TestHook_Idempotent(t *testing.T) {
	env := newTestEnv(t, &fakeClaude{}, true)

	reconcileHook(env)
	first, _ := readSettingsFile(t, env)

	r := reconcileHook(env)
	if !r.ok || r.state != "already registered for this project" {
		t.Fatalf("second run not idempotent: ok=%v state=%q", r.ok, r.state)
	}
	second, _ := readSettingsFile(t, env)
	if first != second {
		t.Errorf("second run rewrote the file:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// Undo removes ONLY Salvager's entry, leaving a co-existing unrelated hook (and
// other keys) intact.
func TestHook_UndoRemovesOnlyOurs(t *testing.T) {
	env := newTestEnv(t, &fakeClaude{}, true)

	// Seed an unrelated PreToolUse hook plus an unrelated top-level key.
	writeSettingsFile(t, env, `{
  "model": "opus",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Write", "hooks": [{"type": "command", "command": "other-tool guard", "timeout": 10}]}
    ]
  }
}`)
	if r := reconcileHook(env); !r.ok || r.state != "registered" {
		t.Fatalf("add failed: %+v", r)
	}
	withBoth, _ := readSettingsFile(t, env)
	if !strings.Contains(withBoth, "other-tool guard") || !strings.Contains(withBoth, env.exePath+" hook") {
		t.Fatalf("expected both hooks present after add:\n%s", withBoth)
	}

	r := undoHook(env)
	if !r.ok || r.state != "removed" {
		t.Fatalf("undo failed: %+v", r)
	}
	after, _ := readSettingsFile(t, env)
	if strings.Contains(after, env.exePath+" hook") {
		t.Errorf("undo left Salvager's hook behind:\n%s", after)
	}
	if !strings.Contains(after, "other-tool guard") {
		t.Errorf("undo removed the unrelated hook:\n%s", after)
	}
	if !strings.Contains(after, `"model": "opus"`) && !strings.Contains(after, `"model":"opus"`) {
		t.Errorf("undo dropped an unrelated top-level key:\n%s", after)
	}
}

// A malformed settings file degrades safe: reported as a failure, left untouched.
func TestHook_MalformedDegradesSafe(t *testing.T) {
	env := newTestEnv(t, &fakeClaude{}, true)
	const bad = "{ this is not valid json"
	writeSettingsFile(t, env, bad)

	r := reconcileHook(env)
	if r.ok || !strings.Contains(r.state, "not valid JSON") {
		t.Fatalf("expected a safe-degrade failure, got %+v", r)
	}
	got, _ := readSettingsFile(t, env)
	if got != bad {
		t.Errorf("malformed file must be left untouched:\ngot:  %q\nwant: %q", got, bad)
	}
}

// The atomic merge preserves every other key in the settings file.
func TestHook_PreservesOtherKeys(t *testing.T) {
	env := newTestEnv(t, &fakeClaude{}, true)
	writeSettingsFile(t, env, `{"model": "opus", "permissions": {"allow": ["Bash(ls:*)"]}}`)

	if r := reconcileHook(env); !r.ok {
		t.Fatalf("reconcile failed: %+v", r)
	}
	var parsed map[string]any
	content, _ := readSettingsFile(t, env)
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, content)
	}
	if parsed["model"] != "opus" {
		t.Errorf("lost the model key:\n%s", content)
	}
	if _, ok := parsed["permissions"]; !ok {
		t.Errorf("lost the permissions key:\n%s", content)
	}
	if parsed["hooks"] == nil {
		t.Errorf("hook was not added:\n%s", content)
	}
}

// Drift repair: a stale salvager hook (a moved binary) is replaced, not duplicated.
func TestHook_RepairsDrift(t *testing.T) {
	env := newTestEnv(t, &fakeClaude{}, true)
	writeSettingsFile(t, env, `{
  "hooks": {"PreToolUse": [
    {"matcher": "Bash", "hooks": [{"type": "command", "command": "/old/path/salvager hook", "timeout": 5}]}
  ]}
}`)
	if r := reconcileHook(env); !r.ok || r.state != "registered" {
		t.Fatalf("drift repair failed: %+v", r)
	}
	content, _ := readSettingsFile(t, env)
	if strings.Contains(content, "/old/path/salvager hook") {
		t.Errorf("stale hook not removed:\n%s", content)
	}
	if strings.Count(content, "salvager hook") != 1 {
		t.Errorf("expected exactly one salvager hook after repair:\n%s", content)
	}
}
