package main

import (
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
	registered bool   // is "salvager" currently registered?
	command    string // what Command: `get` reports
	calls      []string
	failAdd    bool
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
		out := "salvager:\n  Status: ✔ Connected\n  Type: stdio\n  Command: " +
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
