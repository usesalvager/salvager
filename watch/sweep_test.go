package watch

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lochis/ignore"
	"lochis/store"
)

// TestSweepGateZeroReadsWhenUnchanged guards the load-bearing I/O invariant in
// CI (unlike TestSweepIOCost, which is env-gated and only measures): a warm pass
// over unchanged files reads ZERO content, and a change re-reads only the
// changed file. If the stat gate in walkRecord regressed, the warm pass would
// re-hash everything and this test would fail.
func TestSweepGateZeroReadsWhenUnchanged(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	const n = 50
	for i := 0; i < n; i++ {
		d := filepath.Join(tree, fmt.Sprintf("d%d", i%5))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, fmt.Sprintf("f%02d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := store.New(root)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	sw := newSweeper(root, s, ignore.New(root), time.Second)
	sw.addRoot(tree)

	b0 := sw.snapshotStats()
	sw.sweepRoots()
	cold := sw.snapshotStats()
	if got := cold.reads - b0.reads; got != n {
		t.Fatalf("cold pass content reads = %d, want %d (every file new)", got, n)
	}

	b1 := sw.snapshotStats()
	sw.sweepRoots()
	warm := sw.snapshotStats()
	if r, by := warm.reads-b1.reads, warm.bytes-b1.bytes; r != 0 || by != 0 {
		t.Fatalf("warm unchanged pass read content (reads=%d bytes=%d); the stat gate is not short-circuiting", r, by)
	}

	// One file changes; only it is re-read.
	if err := os.WriteFile(filepath.Join(tree, "d0", "f00.txt"), []byte("xxxxxx"), 0o644); err != nil {
		t.Fatal(err)
	}
	b2 := sw.snapshotStats()
	sw.sweepRoots()
	chg := sw.snapshotStats()
	if got := chg.reads - b2.reads; got != 1 {
		t.Fatalf("after one change, content reads = %d, want 1 (reads must scale with churn, not tree size)", got)
	}
}

// TestSweepCapturesMtimePreservedEdit is the regression guard for the stat-gate
// blind spot: an in-place rewrite of identical byte length that also RESTORES
// the old mtime would fool a naive mtime+size gate and freeze the file at stale
// content. ctime (which the write advances and Chtimes cannot rewind) must catch
// it, keeping the polling path at parity with the real-time path.
func TestSweepCapturesMtimePreservedEdit(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(tree, "f.txt")
	if err := os.WriteFile(p, []byte("AAAA"), 0o644); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if ctimeNano(fi) == 0 {
		t.Skip("no ctime on this platform; the mtime+size gate has a documented residual blind spot here")
	}

	s := store.New(root)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	sw := newSweeper(root, s, ignore.New(root), time.Second)
	sw.addRoot(tree)
	sw.sweepRoots() // initial capture

	rel := filepath.Join("tree", "f.txt")
	if n, _ := s.List(rel); len(n) != 1 {
		t.Fatalf("want 1 initial revision, got %d", len(n))
	}

	// Rewrite SAME-SIZE content, then restore the original atime+mtime so the
	// mtime+size signal is byte-identical to before.
	orig := fi.ModTime()
	if err := os.WriteFile(p, []byte("BBBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, orig, orig); err != nil {
		t.Fatal(err)
	}
	fi2, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !fi2.ModTime().Equal(orig) {
		t.Skipf("filesystem did not honor the mtime restore (%v != %v); cannot exercise the blind spot", fi2.ModTime(), orig)
	}
	if fi2.Size() != 4 {
		t.Fatalf("size changed unexpectedly to %d; test precondition broken", fi2.Size())
	}

	sw.sweepRoots()

	revs, _ := s.List(rel)
	if len(revs) < 2 {
		t.Fatalf("mtime-preserved in-place edit was NOT captured by the sweep: %d revisions (ctime gate failed)", len(revs))
	}
	if got, _ := s.Get(rel, revs[0].Timestamp); !bytes.Equal(got, []byte("BBBB")) {
		t.Errorf("captured content = %q, want BBBB", got)
	}
}

// TestSweeperAddRootDedup proves the poll set stays minimal and non-overlapping:
// a directory already covered by an ancestor is dropped, and a new ancestor
// prunes descendants it now covers.
func TestSweeperAddRootDedup(t *testing.T) {
	root := t.TempDir()
	s := store.New(root)
	sw := newSweeper(root, s, ignore.New(root), time.Second)

	sw.addRoot(filepath.Join(root, "a", "b"))
	sw.addRoot(filepath.Join(root, "a", "b", "c")) // under a/b: dropped
	if got := len(sw.roots); got != 1 {
		t.Fatalf("descendant should not add a root: have %d roots %v", got, sw.roots)
	}
	sw.addRoot(filepath.Join(root, "a")) // ancestor of a/b: prunes it
	if len(sw.roots) != 1 {
		t.Fatalf("ancestor should collapse to one root, have %v", sw.roots)
	}
	if _, ok := sw.roots["a"]; !ok {
		t.Fatalf("expected the ancestor root 'a', have %v", sw.roots)
	}
}

// TestSweepIOCost measures the I/O cost of a reconciliation pass across ~88k
// files — the worst case from the scaling sweep where the descriptor cap is
// exhausted. It is gated behind LOCHIS_SWEEP_BENCH=1 because building 88k files
// is far too heavy for the normal suite. The numbers it prints are what the
// sweep.go header comment documents. Run:
//
//	LOCHIS_SWEEP_BENCH=1 go test ./watch -run TestSweepIOCost -v -timeout 20m
func TestSweepIOCost(t *testing.T) {
	if os.Getenv("LOCHIS_SWEEP_BENCH") == "" {
		t.Skip("set LOCHIS_SWEEP_BENCH=1 to run the 88k-file I/O measurement")
	}

	const (
		dirs     = 8800
		perDir   = 10 // 88,000 files total
		fileSize = 64
	)
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	content := make([]byte, fileSize)
	t.Logf("building %d files across %d dirs ...", dirs*perDir, dirs)
	build := time.Now()
	for d := 0; d < dirs; d++ {
		sub := filepath.Join(tree, fmt.Sprintf("d%04d", d))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		for f := 0; f < perDir; f++ {
			if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("f%02d.txt", f)), content, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Logf("built in %s", time.Since(build).Round(time.Millisecond))

	s := store.New(root)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	sw := newSweeper(root, s, ignore.New(root), time.Second)
	sw.addRoot(tree)

	report := func(label string, before sweepStats, wall time.Duration) {
		now := sw.snapshotStats()
		t.Logf("%-18s wall=%-9s dir_reads=%-6d file_stats=%-6d content_reads=%-6d bytes_read=%d",
			label, wall.Round(time.Millisecond),
			now.dirs-before.dirs, now.files-before.files,
			now.reads-before.reads, now.bytes-before.bytes)
	}

	// Cold: first pass, every file is new -> all read and recorded.
	b0 := sw.snapshotStats()
	t0 := time.Now()
	sw.sweepRoots()
	report("cold (all new)", b0, time.Since(t0))
	if got := sw.snapshotStats().reads - b0.reads; got != int64(dirs*perDir) {
		t.Fatalf("cold reads = %d, want %d", got, dirs*perDir)
	}

	// Warm, nothing changed: pure metadata, zero content reads.
	b1 := sw.snapshotStats()
	t1 := time.Now()
	sw.sweepRoots()
	report("warm 0%% changed", b1, time.Since(t1))
	if a := sw.snapshotStats(); a.reads-b1.reads != 0 || a.bytes-b1.bytes != 0 {
		t.Fatalf("warm pass read content: reads=%d bytes=%d, want 0/0", a.reads-b1.reads, a.bytes-b1.bytes)
	}

	// Warm, ~1% changed: content reads scale with churn, not tree size.
	changed := 0
	for d := 0; d < dirs; d += 100 { // 1 dir in 100, all its files
		for f := 0; f < perDir; f++ {
			p := filepath.Join(tree, fmt.Sprintf("d%04d", d), fmt.Sprintf("f%02d.txt", f))
			if err := os.WriteFile(p, []byte("changed-content"), 0o644); err != nil {
				t.Fatal(err)
			}
			changed++
		}
	}
	t.Logf("modified %d files (~%.1f%%)", changed, 100*float64(changed)/float64(dirs*perDir))
	b2 := sw.snapshotStats()
	t2 := time.Now()
	sw.sweepRoots()
	report("warm 1%% changed", b2, time.Since(t2))
	if got := sw.snapshotStats().reads - b2.reads; got != int64(changed) {
		t.Fatalf("1%% pass reads = %d, want %d (reads must scale with churn)", got, changed)
	}
}
