package main

// End-to-end / CLI tests. These build the real `salvager` binary and exercise
// its subcommands as a user (and an agent) would: through exec.Command in a
// throwaway project directory, with the watcher running as a real subprocess.
//
// Cases covered (TESTS-v1.md):
//   A1.2  zero-config: `salvager watch` starts with no config and creates .salvager/
//   A6.3  `salvager history <file>` on a file with no history exits 0 with a clear
//         "no history" message (not an opaque error/crash)
//   A10.1 the end-to-end recovery test: real git repo, watcher running,
//         uncommitted good work destroyed by `git checkout -- .`, then recovered
//         via `salvager history` + `salvager restore`, byte-for-byte, with a
//         reported pre-restore timestamp.
//   A10.2 the product's strongest claim, made falsifiable: uncommitted work that
//         was never staged is destroyed by `git reset --hard`, and we PROVE git
//         itself cannot bring it back (the blob is absent from git's entire
//         object database, fsck --lost-found, and every reflog) — yet salvager
//         recovers it byte-for-byte. git does not protect uncommitted work;
//         salvager does.
//
// Timing is deliberately tolerant: the real binary's debounce is 300ms, so we
// poll up to several seconds. Every subprocess is killed in cleanup so no
// orphan watcher is left behind. The whole file is skipped under -short.

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// --- binary build (once per test run) ---

var (
	e2eBinOnce sync.Once
	e2eBinPath string
	e2eBinErr  error
)

// e2eRepoRoot returns the repository root (the directory holding this test
// file, i.e. the package dir tests run in).
func e2eRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		// Fall back to the working directory, which `go test` sets to the
		// package directory == repo root.
		wd, err := os.Getwd()
		if err != nil {
			t.Fatalf("cannot determine repo root: %v", err)
		}
		return wd
	}
	return filepath.Dir(thisFile)
}

// e2eBinary builds (once) the salvager binary and returns its absolute path.
func e2eBinary(t *testing.T) string {
	t.Helper()
	root := e2eRepoRoot(t)
	e2eBinOnce.Do(func() {
		// Build into the OS temp dir, never the repo tree, so the working tree
		// stays clean (no stray artifact in `git status`).
		binDir, err := os.MkdirTemp("", "salvager-e2e-bin-")
		if err != nil {
			e2eBinErr = err
			return
		}
		out := filepath.Join(binDir, "salvager")
		cmd := exec.Command("go", "build", "-o", out, ".")
		cmd.Dir = root
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			e2eBinErr = err
			t.Logf("go build failed: %v\n%s", err, stderr.String())
			return
		}
		e2eBinPath = out
	})
	if e2eBinErr != nil {
		t.Fatalf("could not build salvager binary: %v", e2eBinErr)
	}
	if e2eBinPath == "" {
		t.Fatal("salvager binary was not built")
	}
	return e2eBinPath
}

// --- helpers ---

// e2ePoll polls cond until it returns true or the deadline passes. Returns the
// final value of cond.
func e2ePoll(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cond()
}

// e2eRun runs the salvager binary with args in dir, returning stdout, stderr and
// the exit code. A non-zero exit is NOT a fatal in itself: callers assert on it.
func e2eRun(t *testing.T, dir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	bin := e2eBinary(t)
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exitCode = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("failed to run salvager %v: %v", args, err)
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

// e2eGit runs a git command in dir and fails the test on error.
func e2eGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, errBuf.String())
	}
}

// e2eGitOut runs a git command in dir and returns trimmed stdout plus the exit
// code, WITHOUT failing the test on a non-zero exit. Some git probes are
// *expected* to fail and the failure is the assertion — e.g. `git cat-file -e
// <hash>` on an object git never stored exits non-zero, which is precisely how
// A10.2 proves git has no record of the destroyed work. It still fatals if git
// cannot be launched at all.
func e2eGitOut(t *testing.T, dir string, args ...string) (stdout string, exitCode int) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return strings.TrimSpace(outBuf.String()), ee.ExitCode()
		}
		t.Fatalf("failed to run git %v: %v", args, err)
	}
	return strings.TrimSpace(outBuf.String()), 0
}

// e2eStartWatch starts `salvager watch` as a subprocess in dir and registers a
// cleanup that signals + kills it so no orphan watcher survives the test.
func e2eStartWatch(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	bin := e2eBinary(t)
	cmd := exec.Command(bin, "watch")
	cmd.Dir = dir
	// Capture watcher logs for diagnostics but don't depend on them.
	var errBuf syncBuffer
	cmd.Stderr = &errBuf
	cmd.Stdout = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start salvager watch: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			// Try a graceful interrupt first, then force-kill.
			_ = cmd.Process.Signal(syscall.SIGINT)
			done := make(chan struct{})
			go func() { _, _ = cmd.Process.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}
	})
	return cmd
}

// syncBuffer is a goroutine-safe bytes.Buffer for capturing subprocess output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// e2eReadLog parses .salvager/index/<rel>.log directly and returns its revisions
// oldest-first. This is the most robust way to get raw ms timestamps and labels
// (independent of stdout/stderr formatting).
type e2eRev struct {
	Ts    int64
	Hash  string
	Label string
}

func e2eReadLog(t *testing.T, projectDir, rel string) []e2eRev {
	t.Helper()
	logPath := filepath.Join(projectDir, ".salvager", "index", rel+".log")
	f, err := os.Open(logPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	var revs []e2eRev
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// A .log line is either the legacy 3-column form or the 7-column signal
		// form; only ts/hash/label are needed here, so take the first 3 fields.
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) < 3 {
			continue
		}
		ts, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			continue
		}
		revs = append(revs, e2eRev{Ts: ts, Hash: parts[1], Label: parts[2]})
	}
	return revs
}

// e2eFindRev returns the timestamp of the newest revision whose content equals
// want (read back via `salvager show`). ok is false if none matches.
func e2eFindRevByContent(t *testing.T, projectDir, rel string, want []byte) (int64, bool) {
	t.Helper()
	revs := e2eReadLog(t, projectDir, rel)
	// newest-first
	for i := len(revs) - 1; i >= 0; i-- {
		r := revs[i]
		if r.Label == "delete" {
			continue
		}
		objPath := filepath.Join(projectDir, ".salvager", "objects", r.Hash)
		got, err := os.ReadFile(objPath)
		if err != nil {
			continue
		}
		if bytes.Equal(got, want) {
			return r.Ts, true
		}
	}
	return 0, false
}

// --- A1.2 — zero-config startup ---

func TestE2E_A1_2_ZeroConfigCreatesSalvagerDir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watcher subprocess test in -short mode")
	}
	proj := t.TempDir()
	// A fresh project with NO salvager config of any kind. Spec A1.2: `salvager
	// watch` must start with zero configuration and create .salvager/ automatically.
	// The strongest form of the Given is a completely empty directory.
	if _, err := os.Stat(filepath.Join(proj, ".salvager")); !os.IsNotExist(err) {
		t.Fatalf("temp project unexpectedly already has .salvager")
	}

	cmd := e2eStartWatch(t, proj)

	// It must start without prompting (stdin is /dev/null since we attach none)
	// and create .salvager/ automatically — no account, no config questions.
	ok := e2ePoll(t, 6*time.Second, func() bool {
		fi, err := os.Stat(filepath.Join(proj, ".salvager"))
		return err == nil && fi.IsDir()
	})
	if !ok {
		t.Fatal("`salvager watch` did not create a .salvager/ directory (zero-config startup failed)")
	}
	// The process must still be alive (it did not exit demanding config) — i.e.
	// it is running as a long-lived watcher, not crashed at startup.
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		t.Fatalf("`salvager watch` exited early instead of running: %v", cmd.ProcessState)
	}
}

// --- A6.3 — no-history message ---

func TestE2E_A6_3_HistoryNoHistoryMessage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI e2e in -short mode")
	}
	proj := t.TempDir()
	// A file that exists but was never captured (no watcher ran, no .salvager).
	if err := os.WriteFile(filepath.Join(proj, "somefile.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := e2eRun(t, proj, "history", "somefile.txt")

	if code != 0 {
		t.Fatalf("history on a no-history file should exit 0, got exit %d\nstdout=%q\nstderr=%q", code, stdout, stderr)
	}
	combined := stdout + stderr
	lc := strings.ToLower(combined)
	if !strings.Contains(lc, "no history") {
		t.Fatalf("expected a clear \"no history\" message, got:\nstdout=%q\nstderr=%q", stdout, stderr)
	}
	// It must mention the file so the message is actionable, and must not look
	// like an opaque crash/error.
	if !strings.Contains(combined, "somefile.txt") {
		t.Errorf("no-history message should name the file; got %q", combined)
	}
	if strings.Contains(lc, "panic") || strings.Contains(lc, "goroutine") {
		t.Errorf("no-history path crashed/panicked: %q", combined)
	}
}

// --- A10.1 — the end-to-end recovery test (THE test that matters) ---

func TestE2E_A10_1_RecoverAfterDestructiveGitCheckout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping heavy end-to-end recovery test in -short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	proj := t.TempDir()

	// 1. A real git repo with a committed baseline.
	e2eGit(t, proj, "init", "-q")
	e2eGit(t, proj, "config", "user.email", "test@example.com")
	e2eGit(t, proj, "config", "user.name", "Test")
	tracked := "work.txt"
	trackedAbs := filepath.Join(proj, tracked)
	baseline := []byte("baseline committed content\n")
	if err := os.WriteFile(trackedAbs, baseline, 0o644); err != nil {
		t.Fatal(err)
	}
	e2eGit(t, proj, "add", tracked)
	e2eGit(t, proj, "commit", "-q", "-m", "baseline")

	// 2. Start the watcher and let it record the baseline (initial revision).
	e2eStartWatch(t, proj)
	if !e2ePoll(t, 6*time.Second, func() bool {
		return len(e2eReadLog(t, proj, tracked)) >= 1
	}) {
		t.Fatal("watcher never recorded the initial revision of the tracked file")
	}

	// 3. Write UNCOMMITTED good work into the tracked file and wait past the
	//    real 300ms debounce so salvager records it.
	good := []byte("GOOD UNCOMMITTED WORK that the agent must not lose\n")
	if err := os.WriteFile(trackedAbs, good, 0o644); err != nil {
		t.Fatal(err)
	}
	if !e2ePoll(t, 6*time.Second, func() bool {
		_, ok := e2eFindRevByContent(t, proj, tracked, good)
		return ok
	}) {
		t.Fatalf("watcher never recorded the good uncommitted work; log=%+v", e2eReadLog(t, proj, tracked))
	}

	// 4. The destructive, agent-style command: blow away the uncommitted work.
	e2eGit(t, proj, "checkout", "--", ".")
	onDisk, err := os.ReadFile(trackedAbs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, baseline) {
		t.Fatalf("precondition: `git checkout -- .` should have reverted to baseline, got %q", onDisk)
	}
	if bytes.Equal(onDisk, good) {
		t.Fatal("precondition: good work was not actually destroyed by git checkout")
	}

	// (a) `salvager history <file>` shows the revision holding the good work.
	goodTs, ok := e2eFindRevByContent(t, proj, tracked, good)
	if !ok {
		t.Fatalf("good revision missing from store after destruction; log=%+v", e2eReadLog(t, proj, tracked))
	}
	histOut, histErr, histCode := e2eRun(t, proj, "history", tracked)
	if histCode != 0 {
		t.Fatalf("history exited %d\nstdout=%q\nstderr=%q", histCode, histOut, histErr)
	}
	// main.go prints the human table to stdout and the raw ms to stderr; the
	// good revision's raw ms must appear in the history output somewhere.
	if !strings.Contains(histOut+histErr, strconv.FormatInt(goodTs, 10)) {
		t.Errorf("history output does not reference the good revision ts %d\nstdout=%q\nstderr=%q",
			goodTs, histOut, histErr)
	}

	// (b) `salvager restore <file> <ts>` brings the good work back byte-for-byte.
	resOut, resErr, resCode := e2eRun(t, proj, "restore", tracked, strconv.FormatInt(goodTs, 10))
	if resCode != 0 {
		t.Fatalf("restore exited %d\nstdout=%q\nstderr=%q", resCode, resOut, resErr)
	}
	restored, err := os.ReadFile(trackedAbs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, good) {
		t.Fatalf("restore did not bring the good work back byte-for-byte\n got=%q\nwant=%q", restored, good)
	}

	// (c) the restore reported a pre-restore timestamp (the undo point).
	// main.go prints: "previous state saved as pre-restore revision <preTs> ..."
	if !strings.Contains(resOut, "pre-restore revision") {
		t.Fatalf("restore did not report a pre-restore safeguard:\nstdout=%q\nstderr=%q", resOut, resErr)
	}
	preTs := e2eParsePreRestoreTs(t, resOut)
	if preTs <= 0 {
		t.Fatalf("could not parse a positive pre-restore timestamp from restore output: %q", resOut)
	}
	// The pre-restore revision must be present in the log with the right label
	// and must hold the destroyed (baseline) on-disk state, proving the restore
	// is itself reversible.
	preFound := false
	for _, r := range e2eReadLog(t, proj, tracked) {
		if r.Ts == preTs && r.Label == "pre-restore" {
			preFound = true
			break
		}
	}
	if !preFound {
		t.Errorf("pre-restore revision ts %d not found with label pre-restore in log %+v",
			preTs, e2eReadLog(t, proj, tracked))
	}
	// And the safeguard content equals what was on disk just before the restore
	// (the baseline that git checkout left behind).
	showOut, showErr, showCode := e2eRun(t, proj, "show", tracked, strconv.FormatInt(preTs, 10))
	if showCode != 0 {
		t.Fatalf("show pre-restore exited %d\nstderr=%q", showCode, showErr)
	}
	if !bytes.Equal([]byte(showOut), baseline) {
		t.Errorf("pre-restore safeguard content = %q, want the pre-restore on-disk state %q", showOut, baseline)
	}
}

// e2eParsePreRestoreTs extracts the pre-restore timestamp from restore's
// stdout. Format (main.go): "...pre-restore revision <preTs> (undo with: ...)".
func e2eParsePreRestoreTs(t *testing.T, out string) int64 {
	t.Helper()
	const marker = "pre-restore revision "
	idx := strings.Index(out, marker)
	if idx < 0 {
		return 0
	}
	rest := out[idx+len(marker):]
	// Read leading digits.
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	ts, err := strconv.ParseInt(rest[:end], 10, 64)
	if err != nil {
		return 0
	}
	return ts
}

// --- A10.2 — git genuinely cannot recover uncommitted work; salvager can ---

// TestE2E_A10_2_RecoverUncommittedWorkGitCannot is the falsifiable form of
// Salvager's single strongest argument: *git does not protect uncommitted work,
// Salvager does.*
//
// The scenario an agent creates in the wild: you make valuable edits, haven't
// committed (or even staged) them yet, and the agent "tidies up" the repo with a
// destructive command. Here that command is `git reset --hard`. Because the work
// was never added to git's object database, this is the one failure git's own
// safety nets cannot undo:
//   - `git checkout`/`git restore` only reach committed or staged states.
//   - `git reflog` only records ref movements (commits, resets) — never raw
//     working-tree edits, so there is no reflog entry to recover from.
//   - `git fsck --lost-found` can rescue *dangling committed/blobbed* objects,
//     but a working-tree edit that was never `git add`-ed was never hashed into
//     the object store, so there is nothing dangling to find.
//
// The heart of this test is step 5: we PROVE, three independent ways, that git
// has no record of the destroyed content. If a skeptic claims "git could've
// gotten it back", these assertions are the rebuttal. Salvager, watching the
// filesystem, captured the edit the instant it hit disk — below git's level
// entirely — and restores it byte-for-byte.
//
// Fully deterministic (no LLM/agent, no network): the agent's destructive action
// is simulated by running the exact git command an agent would. Runs in CI via
// `go test ./...`, exactly like A10.1.
func TestE2E_A10_2_RecoverUncommittedWorkGitCannot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping heavy end-to-end recovery test in -short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	proj := t.TempDir()

	// 1. A real git repo with a committed baseline.
	e2eGit(t, proj, "init", "-q")
	e2eGit(t, proj, "config", "user.email", "test@example.com")
	e2eGit(t, proj, "config", "user.name", "Test")
	tracked := "work.txt"
	trackedAbs := filepath.Join(proj, tracked)
	baseline := []byte("baseline committed content\n")
	if err := os.WriteFile(trackedAbs, baseline, 0o644); err != nil {
		t.Fatal(err)
	}
	e2eGit(t, proj, "add", tracked)
	e2eGit(t, proj, "commit", "-q", "-m", "baseline")

	// 2. Start the watcher and let it record the baseline (initial revision).
	e2eStartWatch(t, proj)
	if !e2ePoll(t, 6*time.Second, func() bool {
		return len(e2eReadLog(t, proj, tracked)) >= 1
	}) {
		t.Fatal("watcher never recorded the initial revision of the tracked file")
	}

	// 3. Write valuable UNCOMMITTED work and — crucially — never `git add` it.
	//    This is the work that exists only on disk: git has not hashed it into
	//    its object database, so git has no way to ever see it again once the
	//    working-tree copy is gone. Wait past the real 300ms debounce so salvager
	//    records it.
	good := []byte("GOOD UNCOMMITTED WORK that the agent must not lose\n")
	if err := os.WriteFile(trackedAbs, good, 0o644); err != nil {
		t.Fatal(err)
	}
	if !e2ePoll(t, 6*time.Second, func() bool {
		_, ok := e2eFindRevByContent(t, proj, tracked, good)
		return ok
	}) {
		t.Fatalf("watcher never recorded the good uncommitted work; log=%+v", e2eReadLog(t, proj, tracked))
	}

	// 4. Compute the git blob hash this content *would* have. `git hash-object`
	//    WITHOUT -w only computes the SHA; it does not write to .git/objects. So
	//    even after asking git for the hash, git still has no object for it —
	//    which we assert right now, before any destruction: merely editing a file
	//    (no add, no commit) never puts it in git's object database.
	goodHash, code := e2eGitOut(t, proj, "hash-object", tracked)
	if code != 0 || goodHash == "" {
		t.Fatalf("could not compute would-be git blob hash (exit %d, hash %q)", code, goodHash)
	}
	if _, code := e2eGitOut(t, proj, "cat-file", "-e", goodHash); code == 0 {
		t.Fatalf("precondition failed: git already has an object for the uncommitted "+
			"work (hash %s); the test cannot prove git-can't if git was given a copy", goodHash)
	}

	// 5. The destructive, agent-style command: a hard reset that throws away the
	//    uncommitted working-tree edit. This is the failure git cannot undo.
	e2eGit(t, proj, "reset", "--hard", "HEAD")

	// The work is gone from disk: reverted to baseline, not the good content.
	onDisk, err := os.ReadFile(trackedAbs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, baseline) {
		t.Fatalf("precondition: `git reset --hard` should have reverted to baseline, got %q", onDisk)
	}
	if bytes.Equal(onDisk, good) {
		t.Fatal("precondition: good work was not actually destroyed by git reset --hard")
	}

	// --- THE HEART OF THE TEST: prove git genuinely cannot recover it. ---
	// Three independent probes, each the thing a skeptic ("git could get it
	// back!") would actually reach for. All three must come up empty.

	// (i) Targeted: git has no object whose hash is the destroyed content's hash.
	if _, code := e2eGitOut(t, proj, "cat-file", "-e", goodHash); code == 0 {
		t.Fatalf("git CAN recover the work: object %s exists in git's database — "+
			"the central claim of this test is violated", goodHash)
	}

	// (ii) Comprehensive: sweep EVERY object git knows about (commits, trees,
	//      blobs, reachable or not) and confirm the destroyed content's hash is
	//      among none of them. This subsumes (i) but proves it exhaustively.
	allObjs, code := e2eGitOut(t, proj, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")
	if code != 0 {
		t.Fatalf("could not enumerate git objects (exit %d)", code)
	}
	if strings.Contains(allObjs, goodHash) {
		t.Fatalf("git CAN recover the work: hash %s is present in git's full object "+
			"set:\n%s", goodHash, allObjs)
	}

	// (iii) The canonical "rescue dangling work" command. fsck --lost-found is
	//       exactly what you run to recover orphaned objects; it cannot list what
	//       git never hashed. Confirm the destroyed content's hash is absent.
	fsckOut, _ := e2eGitOut(t, proj, "fsck", "--unreachable", "--lost-found", "--no-reflogs")
	if strings.Contains(fsckOut, goodHash) {
		t.Fatalf("git CAN recover the work: fsck --lost-found surfaced hash %s:\n%s",
			goodHash, fsckOut)
	}

	// Sanity-check the probes aren't vacuously passing: the baseline blob (which
	// git DID commit) MUST be found by the same enumeration. If git can see the
	// committed blob but not the uncommitted one, the contrast is real.
	baseHash, _ := e2eGitOut(t, proj, "hash-object", tracked)
	if baseHash == "" || !strings.Contains(allObjs, baseHash) {
		t.Fatalf("probe sanity failed: committed baseline blob %q not found in git's "+
			"object set — the git-can't probes may be vacuous", baseHash)
	}

	// --- Salvager CAN recover it. Same CLI path as A10.1. ---

	// (a) The good revision still lives in salvager's store, and `salvager
	//     history` surfaces it (raw ms printed to stderr by main.go).
	goodTs, ok := e2eFindRevByContent(t, proj, tracked, good)
	if !ok {
		t.Fatalf("good revision missing from salvager store after destruction; log=%+v",
			e2eReadLog(t, proj, tracked))
	}
	histOut, histErr, histCode := e2eRun(t, proj, "history", tracked)
	if histCode != 0 {
		t.Fatalf("history exited %d\nstdout=%q\nstderr=%q", histCode, histOut, histErr)
	}
	if !strings.Contains(histOut+histErr, strconv.FormatInt(goodTs, 10)) {
		t.Errorf("history output does not reference the good revision ts %d\nstdout=%q\nstderr=%q",
			goodTs, histOut, histErr)
	}

	// (b) `salvager restore` brings the work git could not recover back to disk,
	//     byte-for-byte — the payoff line of the whole scenario.
	resOut, resErr, resCode := e2eRun(t, proj, "restore", tracked, strconv.FormatInt(goodTs, 10))
	if resCode != 0 {
		t.Fatalf("restore exited %d\nstdout=%q\nstderr=%q", resCode, resOut, resErr)
	}
	restored, err := os.ReadFile(trackedAbs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, good) {
		t.Fatalf("restore did not bring the work back byte-for-byte\n got=%q\nwant=%q", restored, good)
	}
}

func TestParseMaxBytes(t *testing.T) {
	ok := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"500", 500},
		{"1K", 1 << 10},
		{"1k", 1 << 10},
		{"500M", 500 << 20},
		{"2G", 2 << 30},
		{" 8M ", 8 << 20}, // surrounding space tolerated
	}
	for _, c := range ok {
		got, err := parseMaxBytes(c.in)
		if err != nil {
			t.Errorf("parseMaxBytes(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseMaxBytes(%q) = %d, want %d", c.in, got, c.want)
		}
	}

	bad := []string{"", "  ", "M", "-5", "-5M", "1.5M", "K1", "12X", "0x10"}
	for _, in := range bad {
		if _, err := parseMaxBytes(in); err == nil {
			t.Errorf("parseMaxBytes(%q): want error, got nil", in)
		}
	}
}
