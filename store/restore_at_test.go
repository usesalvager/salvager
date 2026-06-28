package store

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resultsByPath indexes a batch's results for per-file assertions.
func resultsByPath(results []RestoreResult) map[string]RestoreResult {
	m := make(map[string]RestoreResult, len(results))
	for _, r := range results {
		m[r.Path] = r
	}
	return m
}

func readDisk(t *testing.T, root, rel string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false
		}
		t.Fatal(err)
	}
	return string(b), true
}

// -----------------------------------------------------------------------------
// Multi-file tree: each file returns to its latest revision <= atMs.
// -----------------------------------------------------------------------------

func TestRestoreAt_MultiFileTree(t *testing.T) {
	clock := fakeClock(t)
	root := t.TempDir()
	s := New(root)

	files := map[string]string{
		"a.txt":         "a-old",
		"sub/b.txt":     "b-old",
		"sub/deep/c.md": "c-old",
	}
	for rel, content := range files {
		write(t, root, rel, content)
		if err := s.Record(rel); err != nil {
			t.Fatal(err)
		}
	}
	atMs := atomic.LoadInt64(clock) // covers every initial revision

	// Clobber each file after atMs.
	for rel := range files {
		write(t, root, rel, "CLOBBERED "+rel)
		if err := s.Record(rel); err != nil {
			t.Fatal(err)
		}
	}

	_, _, results, err := s.RestoreAt("", atMs)
	if err != nil {
		t.Fatalf("RestoreAt: %v", err)
	}
	byPath := resultsByPath(results)
	for rel, want := range files {
		if got, _ := readDisk(t, root, rel); got != want {
			t.Errorf("%s disk = %q, want %q", rel, got, want)
		}
		if byPath[rel].Action != ActionRestored {
			t.Errorf("%s action = %q, want %q", rel, byPath[rel].Action, ActionRestored)
		}
		if byPath[rel].RestoredToTs == 0 || byPath[rel].RestoredToTs > atMs {
			t.Errorf("%s restored-to %d, want a ts in (0, %d]", rel, byPath[rel].RestoredToTs, atMs)
		}
	}
}

// -----------------------------------------------------------------------------
// git-checkout fixture: untracked-but-watched files deleted from disk at T+,
// one RestoreAt(T) brings them all back byte-equal to their <=T revision.
// -----------------------------------------------------------------------------

func TestRestoreAt_GitCheckoutDeletedFiles(t *testing.T) {
	clock := fakeClock(t)
	root := t.TempDir()
	s := New(root)

	files := map[string]string{
		"pricing.go":     "package main\n// real pricing\n",
		"seed_data.json": "{\"rows\": 204}\n",
		"notes/todo.md":  "- ship restore-at\n",
	}
	for rel, content := range files {
		write(t, root, rel, content)
		if err := s.Record(rel); err != nil {
			t.Fatal(err)
		}
	}
	atMs := atomic.LoadInt64(clock)

	// Simulate `git clean -fd` wiping the working tree, with the watcher noticing.
	for rel := range files {
		if err := os.Remove(filepath.Join(root, rel)); err != nil {
			t.Fatal(err)
		}
		if err := s.Record(rel); err != nil { // records a delete
			t.Fatal(err)
		}
		if _, ok := readDisk(t, root, rel); ok {
			t.Fatalf("precondition: %s should be gone from disk", rel)
		}
	}

	_, _, results, err := s.RestoreAt("", atMs)
	if err != nil {
		t.Fatalf("RestoreAt: %v", err)
	}
	byPath := resultsByPath(results)
	for rel, want := range files {
		got, ok := readDisk(t, root, rel)
		if !ok {
			t.Errorf("%s was not brought back", rel)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
		if byPath[rel].Action != ActionRestored {
			t.Errorf("%s action = %q, want restored", rel, byPath[rel].Action)
		}
	}
}

// -----------------------------------------------------------------------------
// Non-destructive: a file created after atMs is left untouched.
// -----------------------------------------------------------------------------

func TestRestoreAt_NonDestructive_CreatedAfter(t *testing.T) {
	clock := fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "old.txt", "old-v1")
	if err := s.Record("old.txt"); err != nil {
		t.Fatal(err)
	}
	atMs := atomic.LoadInt64(clock)

	// A file that did not exist at atMs.
	write(t, root, "new.txt", "born later")
	if err := s.Record("new.txt"); err != nil {
		t.Fatal(err)
	}
	// And a real change to old.txt so the batch genuinely does work.
	write(t, root, "old.txt", "old-v2")
	if err := s.Record("old.txt"); err != nil {
		t.Fatal(err)
	}

	_, _, results, err := s.RestoreAt("", atMs)
	if err != nil {
		t.Fatalf("RestoreAt: %v", err)
	}
	byPath := resultsByPath(results)

	if got, _ := readDisk(t, root, "old.txt"); got != "old-v1" {
		t.Errorf("old.txt = %q, want old-v1 (should have been rewound)", got)
	}
	if got, ok := readDisk(t, root, "new.txt"); !ok || got != "born later" {
		t.Errorf("new.txt = %q (ok=%v), want it untouched", got, ok)
	}
	if byPath["new.txt"].Action != ActionSkippedNoRevision {
		t.Errorf("new.txt action = %q, want %q", byPath["new.txt"].Action, ActionSkippedNoRevision)
	}
}

// -----------------------------------------------------------------------------
// --undo round-trips to pre-batch bytes AND touches only the batch's files
// (a file with an unrelated, older pre-restore is left alone).
// -----------------------------------------------------------------------------

func TestRestoreAt_UndoRoundTripAndIsolation(t *testing.T) {
	clock := fakeClock(t)
	root := t.TempDir()
	s := New(root)

	// p and q: simple two-revision files.
	write(t, root, "p.txt", "p1")
	if err := s.Record("p.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "q.txt", "q1")
	if err := s.Record("q.txt"); err != nil {
		t.Fatal(err)
	}

	// r: gets an UNRELATED pre-restore (from a manual restore) BEFORE the batch.
	write(t, root, "r.txt", "r1")
	if err := s.Record("r.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "r.txt", "r2")
	if err := s.Record("r.txt"); err != nil {
		t.Fatal(err)
	}
	rRevs, _ := s.List("r.txt") // newest-first: r2(modify), r1(initial)
	if _, err := s.Restore("r.txt", rRevs[1].Timestamp); err != nil {
		t.Fatal(err) // r.txt now "r1" on disk, with a pre-restore("r2") recorded
	}

	atMs := atomic.LoadInt64(clock) // r.txt's restore-to-r1 is <= atMs

	// Pre-batch on-disk state for p and q.
	write(t, root, "p.txt", "p2")
	if err := s.Record("p.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "q.txt", "q2")
	if err := s.Record("q.txt"); err != nil {
		t.Fatal(err)
	}

	bs, be, results, err := s.RestoreAt("", atMs)
	if err != nil {
		t.Fatalf("RestoreAt: %v", err)
	}
	byPath := resultsByPath(results)
	if byPath["p.txt"].Action != ActionRestored || byPath["q.txt"].Action != ActionRestored {
		t.Fatalf("p/q should be restored, got %q/%q", byPath["p.txt"].Action, byPath["q.txt"].Action)
	}
	if byPath["r.txt"].Action != ActionUnchanged {
		t.Fatalf("r.txt should be unchanged (disk matched its <=atMs state), got %q", byPath["r.txt"].Action)
	}
	if g, _ := readDisk(t, root, "p.txt"); g != "p1" {
		t.Fatalf("after batch p.txt = %q, want p1", g)
	}
	if g, _ := readDisk(t, root, "q.txt"); g != "q1" {
		t.Fatalf("after batch q.txt = %q, want q1", g)
	}

	// Undo: p, q return to their pre-batch bytes; r is NOT disturbed (its only
	// pre-restore predates the batch window).
	undo, err := s.UndoRestoreAt("", bs, be)
	if err != nil {
		t.Fatalf("UndoRestoreAt: %v", err)
	}
	if len(undo) != 2 {
		t.Errorf("undo touched %d files, want exactly 2 (p, q)", len(undo))
	}
	if g, _ := readDisk(t, root, "p.txt"); g != "p2" {
		t.Errorf("after undo p.txt = %q, want p2", g)
	}
	if g, _ := readDisk(t, root, "q.txt"); g != "q2" {
		t.Errorf("after undo q.txt = %q, want q2", g)
	}
	if g, _ := readDisk(t, root, "r.txt"); g != "r1" {
		t.Errorf("after undo r.txt = %q, want r1 (its old pre-restore must not be used)", g)
	}
}

// -----------------------------------------------------------------------------
// Mid-batch failure stops loud; the file processed before it stays restored and
// reversible, the failing file's working tree is untouched.
// -----------------------------------------------------------------------------

func TestRestoreAt_MidBatchFailureStopsLoud(t *testing.T) {
	clock := fakeClock(t)
	root := t.TempDir()
	s := New(root)

	// Path order is "a.txt" < "b.txt": a restores first, then b fails.
	write(t, root, "a.txt", "a-old")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "b.txt", "b-old")
	if err := s.Record("b.txt"); err != nil {
		t.Fatal(err)
	}
	atMs := atomic.LoadInt64(clock)

	write(t, root, "a.txt", "a-new")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "b.txt", "b-new")
	if err := s.Record("b.txt"); err != nil {
		t.Fatal(err)
	}

	// Make b's target (b-old) object unreadable by removing it.
	if err := os.Remove(s.objectPath(sha256hex([]byte("b-old")))); err != nil {
		t.Fatal(err)
	}

	_, _, results, err := s.RestoreAt("", atMs)
	if err == nil {
		t.Fatalf("RestoreAt should fail when b's object is missing")
	}

	// a.txt was restored before the failure...
	if g, _ := readDisk(t, root, "a.txt"); g != "a-old" {
		t.Errorf("a.txt = %q, want a-old (restored before the failure)", g)
	}
	// ...and is reversible via its recorded pre-restore.
	byPath := resultsByPath(results)
	aPre := byPath["a.txt"].PreRestoreTs
	if aPre == 0 {
		t.Fatalf("a.txt has no pre-restore timestamp; not reversible")
	}
	if _, err := s.Restore("a.txt", aPre); err != nil {
		t.Fatalf("undo a.txt via pre-restore failed: %v", err)
	}
	if g, _ := readDisk(t, root, "a.txt"); g != "a-new" {
		t.Errorf("after undo a.txt = %q, want a-new", g)
	}

	// b.txt's working tree is untouched (the read failed before any overwrite).
	if g, _ := readDisk(t, root, "b.txt"); g != "b-new" {
		t.Errorf("b.txt = %q, want b-new (untouched)", g)
	}
}

// -----------------------------------------------------------------------------
// Containment: a relDir escaping the root is refused before any effect.
// -----------------------------------------------------------------------------

func TestRestoreAt_ContainmentRefused(t *testing.T) {
	clock := fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "v1")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	atMs := atomic.LoadInt64(clock)
	write(t, root, "a.txt", "v2")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"../escape", "../../etc", "a/../../x"} {
		_, _, results, err := s.RestoreAt(bad, atMs)
		if !errors.Is(err, ErrUnsafePath) {
			t.Errorf("RestoreAt(%q) err = %v, want ErrUnsafePath", bad, err)
		}
		if results != nil {
			t.Errorf("RestoreAt(%q) returned results despite escape: %v", bad, results)
		}
	}
	// No effect: a.txt is still its post-atMs content, nothing rewound.
	if g, _ := readDisk(t, root, "a.txt"); g != "v2" {
		t.Errorf("a.txt = %q, want v2 (containment must be a pure no-op)", g)
	}
	if undo, err := s.UndoRestoreAt("../escape", atMs, atMs+1); !errors.Is(err, ErrUnsafePath) || undo != nil {
		t.Errorf("UndoRestoreAt escape: err=%v results=%v, want ErrUnsafePath/nil", err, undo)
	}
}

// -----------------------------------------------------------------------------
// Concurrency: restore-at vs a concurrent GC and writer (separate *FS = separate
// processes) never tears a batch — one write lock serializes all three. Reuses
// the cross-process dangling/timestamp scanners.
// -----------------------------------------------------------------------------

func TestCrossProcess_RestoreAtVsGCAndWriter(t *testing.T) {
	root := t.TempDir()
	writer := New(root)
	if err := writer.Init(); err != nil {
		t.Fatal(err)
	}
	restorer := New(root) // a separate process running restore-at
	gc := New(root)       // a separate process running gc

	const n = 6
	for i := 0; i < n; i++ {
		rel := filepath.Join("dir", fileName(i))
		write(t, root, rel, "init "+fileName(i))
		if err := writer.Record(rel); err != nil {
			t.Fatal(err)
		}
	}
	atMs := time.Now().UnixMilli() // rewinds toward the initial revisions

	var stop atomic.Bool
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			rel := filepath.Join("dir", fileName(i%n))
			content := []byte(fileName(i%n) + " rev " + fileName(i))
			if err := os.WriteFile(filepath.Join(root, rel), content, 0o644); err != nil {
				return
			}
			_ = writer.Record(rel)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_, _, _, _ = restorer.RestoreAt("dir", atMs)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_, _ = gc.GCBySize(1) // aggressive: forces rewrite + sweep every pass
		}
	}()

	time.Sleep(raceWindow)
	stop.Store(true)
	wg.Wait()

	// Quiescent: no concurrent writers, so every hit is real corruption.
	if d, err := scanDangling(writer); err != nil {
		t.Fatalf("final dangling scan: %v", err)
	} else if len(d) > 0 {
		t.Fatalf("STORE CORRUPTED: %d dangling revision(s) after restore-at/gc/writer race:\n  %v", len(d), d)
	}
	if bad, err := scanTimestampOrder(writer); err != nil {
		t.Fatalf("final ts scan: %v", err)
	} else if len(bad) > 0 {
		t.Fatalf("STORE CORRUPTED: %d timestamp-order violation(s):\n  %v", len(bad), bad)
	}
}

// fileName makes a small stable name for index i without importing strconv here.
func fileName(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "f0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{digits[i%10]}, b...)
		i /= 10
	}
	return "f" + string(b)
}
