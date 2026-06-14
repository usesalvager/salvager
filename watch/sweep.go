package watch

import (
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lochis/ignore"
)

// The sweep engine is the portable coverage guarantee. When the real-time
// backend cannot place a directory under a live watch (the kernel descriptor
// cap — kqueue EMFILE on macOS, inotify ENOSPC on Linux), that subtree is
// handed here and polled instead. Real-time where it fits, polling where it
// does not, and the union is always whole. No banner, no flag, no human in the
// loop: addRoot is called silently from the scan and the next tick covers it.
//
// It reconciles, it does not merely restate: every pass re-enumerates the
// overflow subtree with filepath.WalkDir, so files created while unwatched are
// discovered, not just changes to files we already knew. A stat (mtime+size)
// gates the expensive step: only when a file's stat moved do we call
// store.Record, which reads and content-hashes it; identical content is
// deduplicated away, so a touched-but-unchanged file costs one stat and zero
// writes. False positives from the coarse stat signal are absorbed for free.
//
// I/O cost of a sweep (measured — Apple M2 Pro, APFS, go1.25, 88,000 files
// across 8,800 directories; reproduce with `LOCHIS_SWEEP_BENCH=1 go test
// ./watch -run TestSweepIOCost -v`, which prints the live counters):
//
//	                 wall     dir reads   file stats   content reads   bytes read
//	cold (1st pass)  22.1 s   8,801       88,000       88,000          5.6 MB
//	warm, 0 changed  1.73 s   8,801       88,000       0               0 B
//	warm, 1% changed 2.00 s   8,801       88,000       880             13 KB
//
// Two facts decide the design:
//
//   - Disk I/O is trivial. The steady-state pass (nothing changed) reads ZERO
//     content bytes: the stat gate short-circuits before store.Record. It is
//     pure metadata — one readdir per directory, one lstat per file — and those
//     are overwhelmingly page-cache hits. Content reads scale with churn, not
//     tree size (1% changed -> 880 reads, 13 KB). FSEvents would save almost no
//     disk I/O, so disk cost does not justify a cgo dependency or losing the
//     static binary. The cold pass's 22 s is store.Record writing 88k objects
//     and logs — a one-time initial capture, not a recurring sweep cost.
//
//   - CPU/syscall is not trivial at extreme scale. 88k lstat is ~1.7 s/pass on
//     macOS (its syscalls are several times slower than Linux's). A fixed tight
//     interval would peg a core on a 200k-file overflow tree. So run() backs off
//     adaptively (wait proportional to the last pass) to hold the background
//     duty cycle near ~10% regardless of scale. This matters only in overflow:
//     a project small enough to fit under the watch cap registers no poll roots
//     and the sweep idles. FSEvents (which avoids the scan entirely) is the
//     answer if this CPU cost ever proves painful — shelved as future work,
//     gated on evidence, not bought speculatively.
//
// Note on the cheaper-looking shortcut we did NOT take: gating on a directory's
// mtime to skip whole directories is unsafe. A POSIX directory's mtime moves
// only when an entry is added, removed or renamed — NOT when a file inside it is
// rewritten in place (os.WriteFile opens O_TRUNC on the same inode). Skipping a
// directory whose mtime did not move would miss exactly the common case, an
// in-place edit of an existing file. So the per-file stat is irreducible if
// edits are to be caught; that is what this engine does.
type sweeper struct {
	root     string
	store    Recorder
	tracker  tracker // optional: store-index delete detection for one-shot rescans
	ign      *ignore.Matcher
	interval time.Duration

	mu    sync.Mutex
	roots map[string]struct{}    // overflow subtree roots, relative to root
	seen  map[string]fileState   // last-observed stat per file, relative to root

	stats sweepStats
}

// fileState is the cheap change signal: a file whose mtime and size both match
// the last sweep is assumed unchanged and is not read.
type fileState struct {
	mtime int64 // unix nanoseconds
	size  int64
}

// tracker is the slice of the store the one-shot rescan needs to detect files
// that vanished while a directory was unwatched (a forced/drop rescan cannot
// rely on the in-memory seen set being warm).
type tracker interface {
	TrackedUnder(relDir string, recursive bool) ([]string, error)
}

// sweepStats are I/O counters, updated atomically, so the cost of a pass can be
// observed and documented rather than guessed.
type sweepStats struct {
	dirs   int64 // directories enumerated (readdir)
	files  int64 // files stat'd (lstat)
	reads  int64 // files whose content was read (stat gate opened)
	bytes  int64 // content bytes read
	sweeps int64 // full passes completed
}

func newSweeper(root string, s Recorder, ign *ignore.Matcher, interval time.Duration) *sweeper {
	sw := &sweeper{
		root:     root,
		store:    s,
		ign:      ign,
		interval: interval,
		roots:    map[string]struct{}{},
		seen:     map[string]fileState{},
	}
	if tr, ok := s.(tracker); ok {
		sw.tracker = tr
	}
	return sw
}

// rel converts an absolute path to one relative to the project root.
func (sw *sweeper) rel(abs string) string {
	r, err := filepath.Rel(sw.root, abs)
	if err != nil {
		return abs
	}
	return r
}

// addRoot registers an overflow subtree for polling. It is silent and
// idempotent: a directory already covered by an ancestor root is dropped, and
// descendants made redundant by a new ancestor are pruned, so the poll set
// stays minimal and non-overlapping.
func (sw *sweeper) addRoot(absDir string) {
	r := sw.rel(absDir)
	sw.mu.Lock()
	defer sw.mu.Unlock()
	for existing := range sw.roots {
		if existing == r || isUnder(r, existing) {
			return // already covered
		}
	}
	for existing := range sw.roots {
		if isUnder(existing, r) {
			delete(sw.roots, existing) // r covers it now
		}
	}
	sw.roots[r] = struct{}{}
}

// active reports whether any subtree is under polling.
func (sw *sweeper) active() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return len(sw.roots) > 0
}

// run polls until done is closed. It is cheap when no roots are registered, so
// it can be started unconditionally. The wait between passes backs off with the
// last pass's duration (nextWait) so the background duty cycle stays bounded
// even when a huge overflow region makes each pass expensive; sw.interval is the
// floor (and the value tests pin small).
func (sw *sweeper) run(done <-chan struct{}) {
	wait := sw.interval
	for {
		select {
		case <-done:
			return
		case <-time.After(wait):
		}
		start := time.Now()
		sw.sweepRoots()
		wait = sw.nextWait(time.Since(start))
	}
}

// nextWait targets a ~10% background duty cycle: idle past the floor, but a
// 1.7s pass over 88k files is followed by ~15s of quiet rather than hammered
// every 2s. Capped so detection latency stays bounded even for enormous trees.
func (sw *sweeper) nextWait(last time.Duration) time.Duration {
	const dutyMultiplier = 9 // wait = 9*pass -> pass/(pass+wait) = 10%
	const maxWait = 60 * time.Second
	w := last * dutyMultiplier
	if w < sw.interval {
		w = sw.interval
	}
	if w > maxWait {
		w = maxWait
	}
	return w
}

// sweepRoots reconciles every registered overflow subtree once. Modifications
// and new files are captured by walkRecord; deletions are found by diffing the
// in-memory seen set (authoritative here, since the poll enumerates the whole
// region every pass) against what this pass observed.
func (sw *sweeper) sweepRoots() {
	sw.mu.Lock()
	roots := make([]string, 0, len(sw.roots))
	for r := range sw.roots {
		roots = append(roots, r)
	}
	sw.mu.Unlock()
	if len(roots) == 0 {
		return
	}

	present := map[string]bool{}
	for _, r := range roots {
		sw.walkRecord(filepath.Join(sw.root, r), true, present)
	}

	// Deletions: a file we have seen, under a swept root, absent this pass.
	sw.mu.Lock()
	var gone []string
	for rel := range sw.seen {
		if present[rel] {
			continue
		}
		if relUnderAny(rel, roots) {
			gone = append(gone, rel)
		}
	}
	sw.mu.Unlock()
	for _, rel := range gone {
		_ = sw.store.Record(rel) // missing file -> delete revision
		sw.mu.Lock()
		delete(sw.seen, rel)
		sw.mu.Unlock()
	}

	atomic.AddInt64(&sw.stats.sweeps, 1)
}

// ReconcileTree rescans one subtree once and returns. It is the reusable
// reconciliation primitive: the polling loop is built on the same walkRecord,
// and a forced rescan (e.g. recovering from a dropped batch of real-time
// events) is exactly this call. Because a forced rescan cannot trust the
// in-memory seen set to be warm for the subtree, deletions are detected against
// the store index via tracker, not against seen.
func (sw *sweeper) ReconcileTree(absDir string) {
	present := map[string]bool{}
	sw.walkRecord(absDir, true, present)

	if sw.tracker == nil {
		return
	}
	tracked, err := sw.tracker.TrackedUnder(sw.rel(absDir), true)
	if err != nil {
		return
	}
	for _, rel := range tracked {
		if present[rel] {
			continue
		}
		_ = sw.store.Record(rel) // store knows it, disk does not -> delete
		sw.mu.Lock()
		delete(sw.seen, rel)
		sw.mu.Unlock()
	}
}

// walkRecord enumerates dir (recursively, or just its direct children) and
// records any file whose stat moved. It fills present with every file relpath
// it observed, so the caller can compute deletions. Ignored paths and symlinks
// are skipped exactly as the real-time path skips them, so polling and watching
// agree on what is in scope.
func (sw *sweeper) walkRecord(dir string, recursive bool, present map[string]bool) {
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable or vanished mid-walk: skip, keep going
		}
		if sw.ign.Match(path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // never follow symlinks (parity with the real-time path)
		}
		if d.IsDir() {
			if !recursive && path != dir {
				return filepath.SkipDir
			}
			atomic.AddInt64(&sw.stats.dirs, 1)
			return nil
		}

		rel := sw.rel(path)
		present[rel] = true
		info, err := d.Info()
		if err != nil {
			return nil
		}
		atomic.AddInt64(&sw.stats.files, 1)
		st := fileState{mtime: info.ModTime().UnixNano(), size: info.Size()}

		sw.mu.Lock()
		cached, ok := sw.seen[rel]
		sw.mu.Unlock()
		if ok && cached == st {
			return nil // stat unchanged: do not read content
		}

		atomic.AddInt64(&sw.stats.reads, 1)
		atomic.AddInt64(&sw.stats.bytes, info.Size())
		if err := sw.store.Record(rel); err == nil {
			sw.mu.Lock()
			sw.seen[rel] = st
			sw.mu.Unlock()
		}
		return nil
	})
}

// snapshotStats returns a copy of the live I/O counters.
func (sw *sweeper) snapshotStats() sweepStats {
	return sweepStats{
		dirs:   atomic.LoadInt64(&sw.stats.dirs),
		files:  atomic.LoadInt64(&sw.stats.files),
		reads:  atomic.LoadInt64(&sw.stats.reads),
		bytes:  atomic.LoadInt64(&sw.stats.bytes),
		sweeps: atomic.LoadInt64(&sw.stats.sweeps),
	}
}

// isUnder reports whether child is the same path as parent or nested beneath it.
func isUnder(child, parent string) bool {
	if parent == "." {
		return true
	}
	return child == parent || strings.HasPrefix(child, parent+string(filepath.Separator))
}

// relUnderAny reports whether the file relpath lives under any of roots.
func relUnderAny(rel string, roots []string) bool {
	for _, r := range roots {
		if r == "." || strings.HasPrefix(rel, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
