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
)

// ErrPartialCoverage is returned by Run when real-time watches fell short and
// polling is unavailable to cover the gap, and the operator did not pass
// --allow-partial. Refusing here is the safety rule: Salvager must not silently
// run without full coverage.
var ErrPartialCoverage = errors.New(
	"salvager: real-time watch limit reached and polling is unavailable; " +
		"refusing to run with partial coverage (pass --allow-partial to override)")

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
	}
}

// SetAllowPartial records that the operator knowingly accepts partial coverage
// if polling cannot cover a real-time shortfall. It is a conscious opt-out, not
// a default.
func (w *Watcher) SetAllowPartial(v bool) { w.allowPartial = v }

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
		if err := w.store.Record(w.rel(path)); err != nil {
			log.Printf("salvager: initial record %s: %v", path, err)
		}
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
		if err := w.store.Record(w.rel(path)); err != nil {
			log.Printf("salvager: record %s: %v", path, err)
		}
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
					if err := w.store.Record(w.rel(path)); err != nil {
						log.Printf("salvager: record %s: %v", w.rel(path), err)
					}
					delete(pending, path)
				}
			}
		}
	}
}

// nowFunc is the clock; overridable in tests.
var nowFunc = time.Now
