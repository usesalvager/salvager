package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// salvager init is an idempotent reconciler with two independent pieces:
//
//  1. the MCP server registration in Claude Code (scope local), driven entirely
//     through the `claude` CLI — we never touch ~/.claude.json by hand, because
//     that file is critical (auth, history, settings) and a hand-edit has cost a
//     user their config before.
//  2. a delimited block in the USER CLAUDE.md (~/.claude/CLAUDE.md) that teaches
//     the agent which Salvager tools exist and when to reach for them.
//
// Each piece detects its own state and completes only what is missing; running
// init twice changes nothing, and running it after drift repairs only the drifted
// piece. The final report tells the truth about each piece separately.

// CLAUDE.md block markers. The region between them is owned by salvager and is the
// ONLY thing init rewrites — anything the user put around it is preserved.
const (
	mdStartMarker = "<!-- salvager:start -->"
	mdEndMarker   = "<!-- salvager:end -->"
)

// mdBlockBody is the content between the markers. Imperative instruction to an
// agent, deliberately short — not marketing prose.
const mdBlockBody = `## Salvager — local file history (recovery)

You have Salvager MCP tools: a filesystem watcher saves per-file revisions
automatically, independent of git. When you (or the user) break, lose, or clobber
UNCOMMITTED work, recover it yourself instead of giving up:

- ` + "`salvager_list_versions`" + ` — list a file's saved revisions
- ` + "`salvager_get_version`" + ` — read one revision's content (inspect before restoring)
- ` + "`salvager_restore`" + ` — restore a revision (reversible: returns a pre-restore timestamp)

Reach for them when a file was overwritten, corrupted, or deleted and git has
nothing staged to recover from.`

// desiredBlock is the exact start..end region init writes.
func desiredBlock() string {
	return mdStartMarker + "\n" + mdBlockBody + "\n" + mdEndMarker
}

// initEnv carries everything init touches, so tests can inject a temp HOME, a
// fake executable path, and a mock `claude` instead of depending on the real CLI.
type initEnv struct {
	root    string // project root (cwd)
	home    string // user home dir
	exePath string // os.Executable(), symlink left unresolved on purpose:
	// a Homebrew install puts a stable symlink on PATH that points into a
	// versioned Cellar dir; resolving it would pin the registration to a path
	// that breaks on the next upgrade.
	claudePath string                                      // "" when `claude` is not on PATH
	runClaude  func(args ...string) (out string, code int) // invoke the claude CLI
	stdout     io.Writer
	stderr     io.Writer
}

// pieceResult is the verified outcome of reconciling one piece.
type pieceResult struct {
	label  string // "MCP server" / "CLAUDE.md"
	ok     bool   // ✓ vs ✗
	state  string // short status, e.g. "connected", "already current"
	detail string // extra guidance (manual command, reason) — may be multi-line
}

func cmdInit(root string, args []string) {
	noClaudeMD := false
	undo := false
	for _, a := range args {
		switch a {
		case "--no-claude-md":
			noClaudeMD = true
		case "--undo":
			undo = true
		default:
			fatalf("usage: salvager init [--no-claude-md] [--undo]")
		}
	}

	exe, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}

	claudePath, _ := exec.LookPath("claude")
	env := &initEnv{
		root:       root,
		home:       home,
		exePath:    exe,
		claudePath: claudePath,
		runClaude:  defaultRunClaude,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
	}

	if undo {
		runUndo(env, noClaudeMD)
		return
	}
	runInit(env, noClaudeMD)
}

// defaultRunClaude invokes the real `claude` CLI and returns its stdout and exit
// code. A non-zero exit is a normal signal (e.g. `mcp get` for an absent server),
// not a fatal error, so it never returns an error — only the code.
func defaultRunClaude(args ...string) (string, int) {
	cmd := exec.Command("claude", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out // claude prints some status to stderr; fold it in for parsing
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return out.String(), code
}

// runInit reconciles both pieces and prints the report.
func runInit(env *initEnv, noClaudeMD bool) {
	if !looksLikeProjectRoot(env.root) {
		fmt.Fprintf(env.stderr,
			"salvager: warning: %s does not look like a project root (no .git, go.mod, package.json, …)\n"+
				"  continuing anyway — salvager works in any directory, but make sure this is where you want it.\n\n",
			env.root)
	}

	var results []pieceResult
	results = append(results, reconcileMCP(env))
	if !noClaudeMD {
		results = append(results, reconcileClaudeMD(env))
	}
	report(env, env.root, results, false)
}

// runUndo removes both pieces (each independently) and reports.
func runUndo(env *initEnv, noClaudeMD bool) {
	var results []pieceResult
	results = append(results, undoMCP(env))
	if !noClaudeMD {
		results = append(results, undoClaudeMD(env))
	}
	report(env, env.root, results, true)
}

// looksLikeProjectRoot is a best-effort heuristic; a false result only warns.
func looksLikeProjectRoot(root string) bool {
	for _, m := range []string{".git", "go.mod", "package.json", "Cargo.toml", "pyproject.toml", ".hg"} {
		if _, err := os.Stat(filepath.Join(root, m)); err == nil {
			return true
		}
	}
	return false
}

// --- MCP piece -------------------------------------------------------------

// reconcileMCP registers the salvager MCP server for this project (scope local)
// through the claude CLI. Idempotent: already-correct → left alone; absent →
// added; pointing at the wrong binary → remove + re-add. On a parse failure of
// `mcp get` (a future CLI may change its output) it degrades safe: it leaves an
// existing registration untouched rather than risk a destructive remove.
func reconcileMCP(env *initEnv) pieceResult {
	r := pieceResult{label: "MCP server"}
	if env.claudePath == "" {
		r.ok = false
		r.state = "skipped — claude CLI not found on PATH"
		r.detail = "install Claude Code, then register the server yourself:\n      " + manualAddCmd(env.exePath)
		return r
	}

	out, code := env.runClaude("mcp", "get", "salvager")
	switch {
	case code != 0:
		// Absent → add.
		return verifyMCP(env, addMCP(env))
	default:
		// Present → check it points at this binary.
		cmd, argsLine, parsed := parseMCPGet(out)
		if !parsed {
			// Degrade safe: don't remove something we can't read.
			r.ok = true
			r.state = "registered (target could not be auto-verified)"
			r.detail = "the claude CLI output an unexpected format; verify with: claude mcp get salvager"
			return r
		}
		if cmd == env.exePath && argsLine == "mcp" {
			r.ok = true
			r.state = "already registered for this project"
			return r
		}
		// Points at the wrong binary/args → correct it.
		env.runClaude("mcp", "remove", "salvager", "--scope", "local")
		return verifyMCP(env, addMCP(env))
	}
}

// addMCP runs `claude mcp add`; returns the CLI exit code.
func addMCP(env *initEnv) int {
	_, code := env.runClaude("mcp", "add", "salvager", "--scope", "local", "--", env.exePath, "mcp")
	return code
}

// verifyMCP confirms the registration with a fresh `mcp get` so the report never
// claims success it did not observe.
func verifyMCP(env *initEnv, addCode int) pieceResult {
	r := pieceResult{label: "MCP server"}
	if addCode != 0 {
		r.ok = false
		r.state = "could not register"
		r.detail = "run it yourself: " + manualAddCmd(env.exePath)
		return r
	}
	out, code := env.runClaude("mcp", "get", "salvager")
	if code != 0 {
		r.ok = false
		r.state = "registered but not verifiable"
		r.detail = "check with: claude mcp get salvager"
		return r
	}
	r.ok = true
	if strings.Contains(out, "Connected") {
		r.state = "connected"
	} else {
		r.state = "registered"
	}
	return r
}

// undoMCP removes the registration if present.
func undoMCP(env *initEnv) pieceResult {
	r := pieceResult{label: "MCP server"}
	if env.claudePath == "" {
		r.ok = true
		r.state = "skipped — claude CLI not found on PATH"
		return r
	}
	_, code := env.runClaude("mcp", "get", "salvager")
	if code != 0 {
		r.ok = true
		r.state = "not registered (nothing to remove)"
		return r
	}
	_, rmCode := env.runClaude("mcp", "remove", "salvager", "--scope", "local")
	if rmCode != 0 {
		r.ok = false
		r.state = "could not remove"
		r.detail = "run it yourself: claude mcp remove salvager --scope local"
		return r
	}
	r.ok = true
	r.state = "removed"
	return r
}

func manualAddCmd(exe string) string {
	return fmt.Sprintf("claude mcp add salvager --scope local -- %s mcp", exe)
}

// parseMCPGet extracts the Command and joined Args from `claude mcp get` output.
// Returns parsed=false when the expected lines are absent (caller degrades safe).
func parseMCPGet(out string) (cmd, argsLine string, parsed bool) {
	haveCmd := false
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(t, "Command:"); ok {
			cmd = strings.TrimSpace(v)
			haveCmd = true
		} else if v, ok := strings.CutPrefix(t, "Args:"); ok {
			argsLine = strings.TrimSpace(v)
		}
	}
	return cmd, argsLine, haveCmd
}

// --- CLAUDE.md piece -------------------------------------------------------

func claudeMDPath(home string) string {
	return filepath.Join(home, ".claude", "CLAUDE.md")
}

// reconcileClaudeMD creates ~/.claude/CLAUDE.md if absent, or rewrites only the
// region between the markers if present, never regenerating the whole file (it
// may hold the user's own instructions). Idempotent: an already-current block is
// left untouched.
func reconcileClaudeMD(env *initEnv) pieceResult {
	r := pieceResult{label: "CLAUDE.md"}
	path := claudeMDPath(env.home)

	data, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		r.ok = false
		r.state = "could not read"
		r.detail = err.Error()
		return r
	}
	content := string(data)

	newContent, action := mergeBlock(content, exists)
	if action == "already current" {
		r.ok = true
		r.state = "already current"
		return r
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.ok = false
		r.state = "could not create ~/.claude"
		r.detail = err.Error()
		return r
	}
	if err := atomicWrite(path, []byte(newContent)); err != nil {
		r.ok = false
		r.state = "could not write"
		r.detail = err.Error()
		return r
	}
	r.ok = true
	r.state = action
	return r
}

// mergeBlock returns the new file content and the action taken: "created",
// "updated", or "already current". Content outside the markers is preserved.
func mergeBlock(content string, exists bool) (string, string) {
	desired := desiredBlock()

	if !exists || strings.TrimSpace(content) == "" {
		return desired + "\n", "created"
	}

	if i := strings.Index(content, mdStartMarker); i >= 0 {
		j := strings.Index(content, mdEndMarker)
		if j >= i {
			j += len(mdEndMarker)
			replaced := content[:i] + desired + content[j:]
			if replaced == content {
				return content, "already current"
			}
			return replaced, "updated"
		}
		// Start marker without a matching end — treat as no block and append.
	}

	// No (usable) block present → append, preserving everything above.
	base := content
	if !strings.HasSuffix(base, "\n") {
		base += "\n"
	}
	return base + "\n" + desired + "\n", "updated"
}

// undoClaudeMD removes the salvager block, restoring the file to roughly its
// pre-init state (collapses the blank run left behind so an append/undo round
// trip is clean). Content outside the markers is preserved.
func undoClaudeMD(env *initEnv) pieceResult {
	r := pieceResult{label: "CLAUDE.md"}
	path := claudeMDPath(env.home)

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		r.ok = true
		r.state = "no file (nothing to remove)"
		return r
	}
	if err != nil {
		r.ok = false
		r.state = "could not read"
		r.detail = err.Error()
		return r
	}
	content := string(data)

	i := strings.Index(content, mdStartMarker)
	j := strings.Index(content, mdEndMarker)
	if i < 0 || j < i {
		r.ok = true
		r.state = "no block present (nothing to remove)"
		return r
	}
	j += len(mdEndMarker)

	result := content[:i] + content[j:]
	result = collapseBlankRun(result)
	result = strings.TrimRight(result, "\n")
	if result != "" {
		result += "\n"
	}

	if err := atomicWrite(path, []byte(result)); err != nil {
		r.ok = false
		r.state = "could not write"
		r.detail = err.Error()
		return r
	}
	r.ok = true
	r.state = "block removed"
	return r
}

// collapseBlankRun squeezes runs of 3+ newlines down to 2, tidying the seam left
// where the block was removed.
func collapseBlankRun(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return s
}

// atomicWrite writes via a temp file in the same dir + rename, so the original is
// replaced atomically and never left half-written. The original bytes survive on
// disk until the rename succeeds — that is the temporary backup; no permanent
// .bak file is left behind.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".salvager-claudemd-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// --- report ----------------------------------------------------------------

func report(env *initEnv, root string, results []pieceResult, undo bool) {
	w := env.stdout
	verb := "init"
	if undo {
		verb = "init --undo"
	}
	fmt.Fprintf(w, "salvager %s — %s\n\n", verb, root)

	var details []pieceResult
	for _, r := range results {
		mark := "✓" // ✓
		if !r.ok {
			mark = "✗" // ✗
		}
		fmt.Fprintf(w, "  %-12s %s %s\n", r.label, mark, r.state)
		if r.detail != "" {
			details = append(details, r)
		}
	}

	if len(details) > 0 {
		fmt.Fprintln(w)
		for _, r := range details {
			fmt.Fprintf(w, "  %s: %s\n", r.label, r.detail)
		}
	}

	if !undo {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Next: start the watcher to begin saving history:")
		fmt.Fprintln(w, "  salvager watch")
		fmt.Fprintln(w, "(the watcher runs until killed; a persistent service comes later.)")
	}
}
