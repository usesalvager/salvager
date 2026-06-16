// Package watch observes a working tree and feeds changes to the store. It is
// passive: it never modifies the files it watches. On startup it records an
// initial revision of every tracked file, guaranteeing a "good" version exists
// before any later change.
//
// Coverage is the invariant. Real-time watches are placed where the kernel
// allows; any directory that cannot be watched in real time (the descriptor
// cap) is handed to the polling sweep instead, automatically and silently, so
// the union of "watched" and "polled" is always the whole tree. A user or agent
// can only ever run with partial coverage by asking for it explicitly
// (--allow-partial); it is never the default and never silent.
package watch

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/usesalvager/salvager/ignore"
)

// Recorder is the slice of the store the watcher needs.
type Recorder interface {
	Record(relPath string) error
}

// Default debounce parameters (see spec §6.3).
const (
	DefaultDebounce = 300 * time.Millisecond
	DefaultTick     = 100 * time.Millisecond

	// DefaultSweepInterval is how often overflow subtrees are polled. It trades
	// detection latency for I/O: at 2s an idle tree costs one metadata pass
	// (readdir + lstat, no content reads) every 2s. See sweep.go for the
	// measured cost.
	DefaultSweepInterval = 2 * time.Second

	// DefaultRacySlop is the width of the real-time gate's racy-clean window (see
	// shouldCapture). A file whose recorded mtime sits within this much of when it
	// was captured is treated as racily clean and re-streamed rather than skipped,
	// because a same-size in-place rewrite within one timestamp tick would be
	// invisible to a stat compare. 3s clears the known coarse filesystems with
	// margin: FAT rounds mtime to 2s by design, and ≥1s NFS mounts exist, so 3s
	// keeps the edge outside the window. On nanosecond-resolution filesystems the
	// only cost is re-streaming the rare same-size rewrite within 3s of a capture.
	DefaultRacySlop = 3 * time.Second
)

// ErrPartialCoverage is returned by Run when real-time watches fell short and
// polling is unavailable to cover the gap, and the operator did not pass
// --allow-partial. Refusing here is the safety rule: Salvager must not silently
// run without full coverage.
var ErrPartialCoverage = errors.New(
	"salvager: real-time watch limit reached and polling is unavailable; " +
		"refusing to run with partial coverage (pass --allow-partial to override)")

// captureState is the stat the real-time gate observed when it last let a path
// through to a capture. It is the gate's own record — not read from the store or
// the .log, and distinct from the sweep's seen set (which is a presence tracker,
// not a clean tracker). capturedAt is the gate's wall clock at that capture; it
// anchors the racy-clean window in shouldCapture.
type captureState struct {
	size       int64
	mtime      int64 // unix ns
	ctime      int64 // unix ns; 0 where the OS exposes no ctime
	capturedAt int64 // unix ns, gate wall clock at capture
}

// Watcher couples the OS backend, the ignore matcher, the store and the polling
// sweep that backs up the parts the backend could not watch.
type Watcher struct {
	root         string
	store        Recorder
	ign          *ignore.Matcher
	backend      backend
	sweeper      *sweeper
	debounce     time.Duration
	tick         time.Duration
	allowPartial bool
	pollDisabled bool // test seam: simulate "polling unavailable"

	// Real-time capture gate: a pure performance shortcut in front of the
	// real-time Record calls, so an unchanged file does not pay a full stream on
	// every event. The store remains the authority and never skips on stat; this
	// gate may only ever avoid work, never a capture. See shouldCapture.
	gateMu sync.Mutex
	gate   map[string]captureState
	slop   time.Duration
}

// New creates a Watcher rooted at root using the platform's real-time backend.
func New(root string, s Recorder, ign *ignore.Matcher) (*Watcher, error) {
	b, err := newOSBackend(root)
	if err != nil {
		return nil, err
	}
	return newWithBackend(root, s, ign, b), nil
}

// newWithBackend builds a Watcher over a supplied backend. Used by tests to
// inject a backend that reports the descriptor limit on demand.
func newWithBackend(root string, s Recorder, ign *ignore.Matcher, b backend) *Watcher {
	return &Watcher{
		root:     root,
		store:    s,
		ign:      ign,
		backend:  b,
		sweeper:  newSweeper(root, s, ign, DefaultSweepInterval),
		debounce: DefaultDebounce,
		tick:     DefaultTick,
		gate:     map[string]captureState{},
		slop:     DefaultRacySlop,
	}
}

// SetAllowPartial records that the operator knowingly accepts partial coverage
// if polling cannot cover a real-time shortfall. It is a conscious opt-out, not
// a default.
func (w *Watcher) SetAllowPartial(v bool) { w.allowPartial = v }

// SetRacySlop overrides the racy-clean window width (see shouldCapture). A
// non-positive value is ignored, so the conservative default stands. Set after
// construction, mirroring SetAllowPartial; store.New stays zero-arg.
func (w *Watcher) SetRacySlop(d time.Duration) {
	if d > 0 {
		w.slop = d
	}
}

// ProbeRacySlop measures the working tree's actual mtime granularity and widens
// the racy window if the filesystem is coarser than the conservative default.
// It is best-effort and empirical (it measures the filesystem rather than
// guessing from a mount type that needs statfs to read): any failure leaves the
// default untouched, so it is always safe to skip. No CLI flag, no new
// dependency.
func (w *Watcher) ProbeRacySlop() {
	if g := probeMtimeGranularity(w.root); 2*g > w.slop {
		w.slop = 2 * g
	}
}

// probeMtimeGranularity returns the smallest mtime change the filesystem under
// dir will record, by repeatedly rewriting a temp file until its mtime advances
// and timing how far it jumped. Bounded so a pathological filesystem cannot hang
// startup; returns 0 on any error (caller keeps its default).
func probeMtimeGranularity(dir string) time.Duration {
	f, err := os.CreateTemp(dir, ".salvager-probe-*")
	if err != nil {
		return 0
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()

	mtime := func() (time.Time, bool) {
		fi, err := os.Stat(name)
		if err != nil {
			return time.Time{}, false
		}
		return fi.ModTime(), true
	}
	bump := func() bool { _, err := f.WriteAt([]byte{0}, 0); return err == nil && f.Sync() == nil }

	if !bump() {
		return 0
	}
	first, ok := mtime()
	if !ok {
		return 0
	}
	const maxProbe = 3 * time.Second
	deadline := time.Now().Add(maxProbe)
	for time.Now().Before(deadline) {
		if !bump() {
			return 0
		}
		m, ok := mtime()
		if !ok {
			return 0
		}
		if m.After(first) {
			return m.Sub(first)
		}
		time.Sleep(time.Millisecond) // gentle: don't peg a core while waiting for a tick
	}
	return maxProbe // mtime never advanced within the cap: treat as very coarse
}

// shouldCapture is the real-time gate: a cheap stat check in front of a
// streaming capture, so an unchanged file does not re-stream on every event.
//
// The store is the authority on whether content changed; it always streams and
// content-hashes when Record is called and NEVER skips on stat. This gate is a
// pure performance shortcut that must fail TOWARD capture: every branch that is
// unsure returns true. Branch 1 (any stat field moved) and the racy-window
// branch exist so a same-size in-place rewrite is never skipped. ctime is
// load-bearing — it closes the mtime-restore hole (utimes / cp --preserve / an
// mtime-preserving formatter rewrite a file in place at the same length and
// restore the old mtime; ctime still advances and cannot be rewound). Do NOT
// "simplify" this into a size+mtime check: that reintroduces that hole one layer
// up from where the sweep already closes it. A wrong skip here is a lost edit; a
// wrong capture here is one redundant stream. Only the second is acceptable.
func (w *Watcher) shouldCapture(rel, abs string) bool {
	fi, err := os.Lstat(abs)
	if err != nil {
		return true // vanished/unreadable: let the store record the deletion
	}
	cur := captureState{size: fi.Size(), mtime: fi.ModTime().UnixNano(), ctime: ctimeNano(fi)}

	w.gateMu.Lock()
	prev, ok := w.gate[rel]
	w.gateMu.Unlock()
	if !ok {
		return true // never captured here: capture (the store dedups if identical)
	}
	return captureDecision(cur, prev, w.slop)
}

// captureDecision is the pure three-branch racy-clean logic: given the current
// stat, the stat at last capture and the racy window, it returns true to capture
// and false only when the file is provably unchanged. It is kept pure so every
// branch — including the ctime-unavailable degrade — is unit-testable without
// touching the filesystem. Every branch that is unsure returns true.
func captureDecision(cur, prev captureState, slop time.Duration) bool {
	// Branch 1: any stat field moved -> changed. ctime is load-bearing: it closes
	// the mtime-restore hole (utimes / cp --preserve / an mtime-preserving
	// formatter rewrite a file in place at the same length and restore the old
	// mtime; ctime still advances and cannot be rewound). Do NOT "simplify" this
	// into a size+mtime check: that reintroduces that hole one layer up from where
	// the sweep already closes it.
	if cur.size != prev.size || cur.mtime != prev.mtime || cur.ctime != prev.ctime {
		return true
	}
	// Fail toward capture where ctime is unavailable (==0, non-unix platforms):
	// without it Branch 1 loses its hole-closing field, so a clean stat there
	// cannot be trusted. None of the shipped release targets (linux/darwin) hit
	// this; the cost is that such a build's gate degrades to always-capture.
	if cur.ctime == 0 || prev.ctime == 0 {
		return true
	}
	// Racy-clean: the capture raced the file's own mtime tick (recorded mtime is
	// within slop of when we captured), so a same-tick same-size rewrite would be
	// invisible to the stat compare. Don't trust equality -> capture.
	if prev.mtime >= prev.capturedAt-int64(slop) {
		return true
	}
	// Stat identical and the capture happened well after the file's mtime tick:
	// any later rewrite would have moved mtime/ctime -> truly unchanged -> skip.
	return false
}

// markCaptured records the file's post-capture stat so the next event can be
// gated against it. Called only after a successful real-time Record. A vanished
// file drops its entry (its deletion was just recorded).
func (w *Watcher) markCaptured(rel, abs string) {
	fi, err := os.Lstat(abs)
	w.gateMu.Lock()
	defer w.gateMu.Unlock()
	if err != nil {
		delete(w.gate, rel)
		return
	}
	w.gate[rel] = captureState{
		size:       fi.Size(),
		mtime:      fi.ModTime().UnixNano(),
		ctime:      ctimeNano(fi),
		capturedAt: nowFunc().UnixNano(),
	}
}

// recordRealtime runs the gate, captures through the store when warranted, and
// updates the gate on success. It is the single real-time capture entry point so
// the gate is populated identically from the initial scan, runtime directory
// adds and live events — which is what stops the first live event after startup
// from redundantly re-streaming a file the initial scan just captured.
func (w *Watcher) recordRealtime(rel, abs string) {
	if !w.shouldCapture(rel, abs) {
		return
	}
	if err := w.store.Record(rel); err != nil {
		log.Printf("salvager: record %s: %v", rel, err)
		return
	}
	w.markCaptured(rel, abs)
}

// Close releases the backend.
func (w *Watcher) Close() error { return w.backend.Close() }

// rel converts an absolute path to one relative to the project root.
func (w *Watcher) rel(abs string) string {
	r, err := filepath.Rel(w.root, abs)
	if err != nil {
		return abs
	}
	return r
}

// pollingAvailable reports whether the sweep can cover a real-time shortfall.
// In production it is always true; the pollDisabled seam exists only so tests
// can exercise the refuse-to-run policy.
func (w *Watcher) pollingAvailable() bool {
	return !w.pollDisabled && w.sweeper.interval > 0
}

// initialScan walks the tree, records every tracked file, and registers every
// tracked directory with the backend. A directory the backend refuses for the
// descriptor limit is handed to the polling sweep and not descended into — the
// sweep re-enumerates it (recording the initial revisions on its first pass),
// so nothing in that subtree is lost, it is merely covered by polling instead.
func (w *Watcher) initialScan() error {
	return filepath.WalkDir(w.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries, keep going
		}
		if w.ign.Match(path) {
			if d.IsDir() {
				return filepath.SkipDir // don't descend into ignored dirs
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // never follow symlinks (may point outside the project)
		}
		if d.IsDir() {
			if err := w.backend.AddDir(path); err != nil {
				// ANY directory we could not place under a live watch is handed
				// to the polling sweep — not only the classified descriptor
				// limit — so coverage stays whole regardless of why the backend
				// refused. The descriptor limit is the expected, silent case;
				// anything else (an unclassified overflow errno, a transient
				// backend error, EACCES) is unexpected and worth one log line,
				// but it is covered all the same rather than silently dropped.
				if !isDescriptorLimit(err) {
					log.Printf("salvager: watch add %s: %v (covering by polling)", path, err)
				}
				w.sweeper.addRoot(path)
				return filepath.SkipDir // the sweep owns this subtree now
			}
			return nil
		}
		w.recordRealtime(w.rel(path), path)
		return nil
	})
}

// addTree registers a newly created directory and all its (tracked) contents,
// recording any files found. Used when a directory appears at runtime. If the
// backend refuses a directory for the descriptor limit, that subtree is handed
// to the polling sweep instead of being silently lost.
func (w *Watcher) addTree(dir string) {
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if w.ign.Match(path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // never follow symlinks (may point outside the project)
		}
		if d.IsDir() {
			if err := w.backend.AddDir(path); err != nil {
				// Any AddDir failure -> polling, so a directory appearing at
				// runtime that the backend refuses is covered, not dropped.
				if !isDescriptorLimit(err) {
					log.Printf("salvager: watch add %s: %v (covering by polling)", path, err)
				}
				w.sweeper.addRoot(path)
				return filepath.SkipDir
			}
			return nil
		}
		w.recordRealtime(w.rel(path), path)
		return nil
	})
}

// Run starts watching and blocks until done is closed or an unrecoverable error
// occurs. Changes are debounced per file: a Record fires after the file has
// been quiet for w.debounce. Overflow subtrees are covered by the polling sweep
// running concurrently.
func (w *Watcher) Run(done <-chan struct{}) error {
	if err := w.initialScan(); err != nil {
		return err
	}

	// Degradation policy. If the real-time backend fell short (overflow) and
	// polling cannot cover it, refuse — unless the operator accepted partial
	// coverage. This is the only place coverage can be less than whole, and it
	// is never reached silently.
	if w.sweeper.active() && !w.pollingAvailable() && !w.allowPartial {
		return ErrPartialCoverage
	}

	// Start the sweep unconditionally when polling is available: it is cheap
	// with no roots and immediately covers any overflow that appears at runtime.
	if w.pollingAvailable() {
		go w.sweeper.run(done)
	}

	pending := map[string]time.Time{}
	ticker := time.NewTicker(w.tick)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return nil

		case ev, ok := <-w.backend.Events():
			if !ok {
				return nil
			}
			if w.ign.Match(ev.Path) {
				continue
			}
			// Never follow symlinks: a link can point outside the project or
			// form a loop. Lstat inspects the link itself, not its target. A
			// missing path (Lstat error) falls through so deletions still record.
			if fi, err := os.Lstat(ev.Path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
				continue
			}
			// A newly created directory must be registered (and scanned), since
			// per-directory backends won't report events inside it otherwise.
			if ev.Op&Create != 0 {
				if fi, err := os.Stat(ev.Path); err == nil && fi.IsDir() {
					w.addTree(ev.Path)
					continue
				}
			}
			pending[ev.Path] = nowFunc()

		case err, ok := <-w.backend.Errors():
			if !ok {
				return nil
			}
			log.Printf("salvager: watch error: %v", err)

		case <-ticker.C:
			now := nowFunc()
			for path, last := range pending {
				if now.Sub(last) > w.debounce {
					w.recordRealtime(w.rel(path), path)
					delete(pending, path)
				}
			}
		}
	}
}

// nowFunc is the clock; overridable in tests.
var nowFunc = time.Now
