package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lochis/ignore"
	"lochis/store"
)

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

	// Warm, nothing changed: pure metadata, zero content reads.
	b1 := sw.snapshotStats()
	t1 := time.Now()
	sw.sweepRoots()
	report("warm 0%% changed", b1, time.Since(t1))

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
}
