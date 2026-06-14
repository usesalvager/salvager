package watch

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"lochis/ignore"
	"lochis/store"
)

// These tests make the discovered failure mode a permanent regression surface.
// They prove Lochis fails CORRECTLY, not just that it captures correctly: when
// the kernel watch cap is hit, polling takes over automatically and coverage
// stays whole; and when polling is taken away, Lochis refuses to run partial
// rather than degrade silently.
//
// The descriptor limit is injected through a fake backend, because a real
// machine only reproduces it once it is already exhausting file descriptors —
// far too costly and flaky for a unit test. The fake returns the exact errno
// (EMFILE) the kqueue/inotify backends return, so isDescriptorLimit and the
// whole fallback path are exercised for real.

// fakeBackend is an injectable backend whose AddDir fails with the descriptor
// limit for any directory the fail predicate selects. It delivers no real-time
// events: the point is precisely that overflow directories are covered by
// polling, not by the backend.
type fakeBackend struct {
	events  chan Event
	errs    chan error
	mu      sync.Mutex
	added   []string
	fail    func(dir string) bool
	failErr error // returned when fail() is true; defaults to EMFILE
}

func newFakeBackend(fail func(dir string) bool) *fakeBackend {
	return &fakeBackend{
		events: make(chan Event),
		errs:   make(chan error),
		fail:   fail,
	}
}

func (f *fakeBackend) AddDir(dir string) error {
	if f.fail != nil && f.fail(dir) {
		if f.failErr != nil {
			return f.failErr
		}
		return syscall.EMFILE // exactly what kqueue returns at kern.maxfilesperproc
	}
	f.mu.Lock()
	f.added = append(f.added, dir)
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) Events() <-chan Event { return f.events }
func (f *fakeBackend) Errors() <-chan error { return f.errs }
func (f *fakeBackend) Close() error         { return nil }

func underPath(p, base string) bool {
	return p == base || strings.HasPrefix(p, base+string(filepath.Separator))
}

func dgRevCount(s *store.FS, rel string) int {
	r, _ := s.List(rel)
	return len(r)
}

// startOverflow wires a watcher whose backend refuses every directory under
// overflowDir, so that subtree is forced onto the polling sweep. The sweep runs
// fast so tests stay quick.
func startOverflow(t *testing.T, root, overflowDir string) (*store.FS, *Watcher, func()) {
	return startOverflowErr(t, root, overflowDir, nil)
}

// startOverflowErr is startOverflow with a chosen AddDir failure error, so tests
// can prove that a NON-descriptor-limit backend error is still covered by
// polling, not silently dropped.
func startOverflowErr(t *testing.T, root, overflowDir string, failErr error) (*store.FS, *Watcher, func()) {
	t.Helper()
	s := store.New(root)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	fake := newFakeBackend(func(dir string) bool { return underPath(dir, overflowDir) })
	fake.failErr = failErr
	w := newWithBackend(root, s, ignore.New(root), fake)
	w.debounce = 40 * time.Millisecond
	w.sweeper.interval = 25 * time.Millisecond
	done := make(chan struct{})
	go w.Run(done)
	stop := func() { close(done); w.Close() }
	return s, w, stop
}

// D1 — a file in an overflow zone accumulates REAL history via polling. It must
// not stay frozen at its first snapshot: successive edits become successive
// revisions, captured by the sweep, not the (failed) real-time watch.
func TestDowngradeOverflowFileAccumulatesHistory(t *testing.T) {
	root := t.TempDir()
	big := filepath.Join(root, "big")
	if err := os.MkdirAll(big, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(big, "f.txt")
	if err := os.WriteFile(path, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, _, stop := startOverflow(t, root, big)
	defer stop()

	rel := filepath.Join("big", "f.txt")
	// The initial revision comes from the sweep's first pass (the overflow
	// subtree was skipped by the real-time initial scan).
	if !waitFor(t, 3*time.Second, func() bool { return dgRevCount(s, rel) >= 1 }) {
		t.Fatal("overflow file never got an initial revision via polling")
	}

	// Two edits, each separated past the poll interval so each is its own pass.
	// Contents differ in length so the change is detected regardless of
	// filesystem timestamp resolution.
	for _, v := range []string{"v1-edit", "v2-edited"} {
		time.Sleep(120 * time.Millisecond)
		if err := os.WriteFile(path, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
		want := v
		if !waitFor(t, 3*time.Second, func() bool {
			revs, _ := s.List(rel)
			if len(revs) < 2 {
				return false
			}
			got, _ := s.Get(rel, revs[0].Timestamp)
			return bytes.Equal(got, []byte(want))
		}) {
			t.Fatalf("overflow file did not accumulate edit %q via polling", v)
		}
	}

	revs, _ := s.List(rel)
	if len(revs) < 3 {
		t.Fatalf("want >=3 revisions (initial + 2 edits) accumulated by polling, got %d", len(revs))
	}
	if revs[len(revs)-1].Label != store.LabelInitial {
		t.Errorf("oldest label = %q, want initial (proves it did not stay at t0)", revs[len(revs)-1].Label)
	}
}

// D2 — a file CREATED in an overflow directory AFTER startup is captured. This
// is the reconcile-not-restate property: each sweep re-enumerates the subtree,
// so files that did not exist at startup are still found.
func TestDowngradeNewFileInOverflowCaptured(t *testing.T) {
	root := t.TempDir()
	big := filepath.Join(root, "big")
	if err := os.MkdirAll(big, 0o755); err != nil {
		t.Fatal(err)
	}

	s, _, stop := startOverflow(t, root, big)
	defer stop()

	// Let the watcher start and register big/ as an overflow root.
	time.Sleep(150 * time.Millisecond)

	created := filepath.Join(big, "appeared.txt")
	if err := os.WriteFile(created, []byte("born after startup"), 0o644); err != nil {
		t.Fatal(err)
	}

	rel := filepath.Join("big", "appeared.txt")
	if !waitFor(t, 3*time.Second, func() bool { return dgRevCount(s, rel) >= 1 }) {
		t.Fatal("file created in an overflow directory after startup was never captured")
	}
	revs, _ := s.List(rel)
	got, _ := s.Get(rel, revs[0].Timestamp)
	if !bytes.Equal(got, []byte("born after startup")) {
		t.Errorf("captured content = %q, want %q", got, "born after startup")
	}
}

// D3 — when the descriptor limit is exceeded, polling is activated automatically
// and effective coverage is 100%: every file across many overflow directories
// ends with its latest content recorded, none frozen.
func TestDowngradeFullCoverageUnderOverflow(t *testing.T) {
	root := t.TempDir()
	big := filepath.Join(root, "big")
	const dirs, perDir = 6, 8
	for d := 0; d < dirs; d++ {
		sub := filepath.Join(big, fmt.Sprintf("d%02d", d))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		for f := 0; f < perDir; f++ {
			p := filepath.Join(sub, fmt.Sprintf("f%02d.txt", f))
			if err := os.WriteFile(p, []byte("orig"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	s, w, stop := startOverflow(t, root, big)
	defer stop()

	// Polling must have been activated automatically — no flag, no intervention.
	if !waitFor(t, 2*time.Second, func() bool { return w.sweeper.active() }) {
		t.Fatal("polling was not auto-activated on descriptor-limit overflow")
	}

	relOf := func(d, f int) string {
		return filepath.Join("big", fmt.Sprintf("d%02d", d), fmt.Sprintf("f%02d.txt", f))
	}

	// Every file recorded at least once (full coverage of the overflow region).
	if !waitFor(t, 5*time.Second, func() bool {
		for d := 0; d < dirs; d++ {
			for f := 0; f < perDir; f++ {
				if dgRevCount(s, relOf(d, f)) < 1 {
					return false
				}
			}
		}
		return true
	}) {
		t.Fatal("not all overflow files were covered by polling")
	}

	// Edit every file; all must reach their new content (none frozen).
	time.Sleep(120 * time.Millisecond)
	for d := 0; d < dirs; d++ {
		for f := 0; f < perDir; f++ {
			p := filepath.Join(root, relOf(d, f))
			if err := os.WriteFile(p, []byte(fmt.Sprintf("edited-%d-%d", d, f)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !waitFor(t, 6*time.Second, func() bool {
		for d := 0; d < dirs; d++ {
			for f := 0; f < perDir; f++ {
				revs, _ := s.List(relOf(d, f))
				if len(revs) < 2 {
					return false
				}
				got, _ := s.Get(relOf(d, f), revs[0].Timestamp)
				if !bytes.Equal(got, []byte(fmt.Sprintf("edited-%d-%d", d, f))) {
					return false
				}
			}
		}
		return true
	}) {
		t.Fatal("100%% coverage failed: some overflow file did not reach its edited content")
	}

	// No spurious re-records: each file is exactly initial + one edit. A
	// regression that re-recorded unchanged files every pass would blow past 2.
	for _, d := range []int{0, dirs / 2, dirs - 1} {
		for _, f := range []int{0, perDir - 1} {
			if n := dgRevCount(s, relOf(d, f)); n != 2 {
				t.Errorf("%s has %d revisions, want exactly 2 (initial+edit); spurious re-record?", relOf(d, f), n)
			}
		}
	}
}

// D4 — the degradation policy. With polling unavailable and overflow present,
// Lochis REFUSES to run rather than silently lose coverage; with --allow-partial
// it runs, having been told to. The invariant: partial coverage is reachable
// only by explicit choice, never by default and never silent.
func TestDowngradePartialCoveragePolicy(t *testing.T) {
	makeWatcher := func(t *testing.T) (*Watcher, *store.FS, string, func()) {
		t.Helper()
		root := t.TempDir()
		big := filepath.Join(root, "big")
		if err := os.MkdirAll(big, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(big, "f.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		s := store.New(root)
		if err := s.Init(); err != nil {
			t.Fatal(err)
		}
		fake := newFakeBackend(func(dir string) bool { return underPath(dir, big) })
		w := newWithBackend(root, s, ignore.New(root), fake)
		w.pollDisabled = true // simulate "polling unavailable for some reason"
		return w, s, root, func() { w.Close() }
	}

	// Without --allow-partial: refuse.
	t.Run("refuses without flag", func(t *testing.T) {
		w, _, _, closeW := makeWatcher(t)
		defer closeW()
		errc := make(chan error, 1)
		done := make(chan struct{})
		defer close(done)
		go func() { errc <- w.Run(done) }()
		select {
		case err := <-errc:
			if !errors.Is(err, ErrPartialCoverage) {
				t.Fatalf("want ErrPartialCoverage, got %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("watcher silently ran in partial mode instead of refusing")
		}
	})

	// With --allow-partial: run (do not refuse) AND coverage is genuinely
	// partial — the overflow file is neither watched nor polled, so it gets no
	// revision. This proves the flag means what it says rather than quietly
	// re-enabling full coverage.
	t.Run("runs with flag", func(t *testing.T) {
		w, s, _, closeW := makeWatcher(t)
		defer closeW()
		w.SetAllowPartial(true)
		errc := make(chan error, 1)
		done := make(chan struct{})
		go func() { errc <- w.Run(done) }()
		select {
		case err := <-errc:
			t.Fatalf("--allow-partial should run, not refuse; got %v", err)
		case <-time.After(400 * time.Millisecond):
			// Still running in (accepted) partial mode: correct.
		}
		if n := dgRevCount(s, filepath.Join("big", "f.txt")); n != 0 {
			t.Errorf("under --allow-partial the overflow file should be uncovered (0 revisions), got %d", n)
		}
		close(done)
	})
}

// D5 — drop recovery. After a batch of real-time events is LOST (the FSEvents
// MustScanSubDirs condition, or any missed change), the reconciliation sweep
// recovers the lost edits, creations and deletions. This exercises the exact
// engine an FSEvents drop handler reuses, so the recovery path is proven even
// though v1 ships the portable polling backend.
func TestDowngradeSimulatedDropRecovery(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	mod := filepath.Join(sub, "a.txt")
	del := filepath.Join(sub, "c.txt")
	if err := os.WriteFile(mod, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(del, []byte("doomed"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := store.New(root)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	// The watcher had these under a live watch and recorded their t0.
	if err := s.Record(filepath.Join("sub", "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(filepath.Join("sub", "c.txt")); err != nil {
		t.Fatal(err)
	}

	// Now real-time delivery DROPS while the tree changes underneath us:
	//   a.txt edited, b.txt created, c.txt deleted — all missed.
	if err := os.WriteFile(mod, []byte("v1-was-lost"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.txt"), []byte("created-while-dropped"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(del); err != nil {
		t.Fatal(err)
	}

	// The drop-recovery primitive: rescan the affected subtree. This is what an
	// FSEvents MustScanSubDirs event maps to.
	sw := newSweeper(root, s, ignore.New(root), time.Second)
	sw.ReconcileTree(sub)

	// Lost edit recovered.
	aRevs, _ := s.List(filepath.Join("sub", "a.txt"))
	if got, _ := s.Get(filepath.Join("sub", "a.txt"), aRevs[0].Timestamp); !bytes.Equal(got, []byte("v1-was-lost")) {
		t.Errorf("lost edit not recovered: a.txt newest = %q, want v1-was-lost", got)
	}
	// Lost creation recovered.
	if n := dgRevCount(s, filepath.Join("sub", "b.txt")); n < 1 {
		t.Errorf("lost creation not recovered: b.txt has %d revisions", n)
	}
	// Lost deletion recovered.
	cRevs, _ := s.List(filepath.Join("sub", "c.txt"))
	if len(cRevs) < 2 || cRevs[0].Label != store.LabelDelete {
		t.Errorf("lost deletion not recovered: c.txt latest label = %v", cRevs[0].Label)
	}
}

// D6 — a backend error that is NOT the classified descriptor limit must still be
// covered by polling, not silently dropped. The original failure mode is wider
// than EMFILE: any AddDir failure (a transient backend error, an unclassified or
// localized overflow errno) left the directory both unwatched and unpolled. The
// contract is that ANY directory the backend refuses goes to the sweep.
func TestDowngradeNonLimitErrorCoveredByPolling(t *testing.T) {
	root := t.TempDir()
	big := filepath.Join(root, "big")
	if err := os.MkdirAll(big, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(big, "f.txt"), []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, _, stop := startOverflowErr(t, root, big, errors.New("transient backend failure"))
	defer stop()

	rel := filepath.Join("big", "f.txt")
	if !waitFor(t, 3*time.Second, func() bool { return dgRevCount(s, rel) >= 1 }) {
		t.Fatal("dir refused with a non-descriptor-limit error was not covered by polling (silent partial coverage)")
	}
	// And it keeps tracking edits, proving genuine polling coverage, not a fluke.
	time.Sleep(120 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(root, rel), []byte("v1-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		revs, _ := s.List(rel)
		if len(revs) < 2 {
			return false
		}
		got, _ := s.Get(rel, revs[0].Timestamp)
		return bytes.Equal(got, []byte("v1-longer"))
	}) {
		t.Fatal("non-limit-error directory was not tracked by polling after initial coverage")
	}
}

// D7 — deletion through the WIRED polling path. sweepRoots detects deletes by
// diffing its in-memory seen set (a different mechanism from ReconcileTree's
// store-index diff exercised by D5). This drives a real Watcher so the
// production seen-diff delete arm is guarded, not just the one-shot primitive.
func TestDowngradeOverflowDeleteViaPolling(t *testing.T) {
	root := t.TempDir()
	big := filepath.Join(root, "big")
	if err := os.MkdirAll(big, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(big, "doomed.txt")
	if err := os.WriteFile(p, []byte("here"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, _, stop := startOverflow(t, root, big)
	defer stop()

	rel := filepath.Join("big", "doomed.txt")
	// First the sweep must record it (so its stat is in the seen set).
	if !waitFor(t, 3*time.Second, func() bool { return dgRevCount(s, rel) >= 1 }) {
		t.Fatal("overflow file never got an initial revision via polling")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		revs, _ := s.List(rel)
		return len(revs) >= 2 && revs[0].Label == store.LabelDelete
	}) {
		revs, _ := s.List(rel)
		var labels []string
		for _, r := range revs {
			labels = append(labels, string(r.Label))
		}
		t.Fatalf("deletion in an overflow dir not recovered via the polling seen-diff; labels=%v", labels)
	}
}
