package store

// Cross-process corruption repro.
//
// The store's only mutual exclusion is FS.mu — an in-process sync.Mutex. But a
// persistent `salvager watch` service, an ad-hoc CLI invocation, and an MCP
// server are SEPARATE PROCESSES, each constructing its own store.New(root) over
// the SAME .salvager directory (confirmed: main.go, service.go:174, mcp). Two
// store.New(root) instances therefore have two independent mutexes and serialize
// nothing relative to each other.
//
// These tests model that exactly: two *FS over one root = two processes. They
// must FAIL on today's code (corruption detected). After the flock fix they
// become the regression guard and must PASS.
//
// Detected corruption invariants:
//
//	DANGLING: a surviving log line names content hash H, but objects/H is gone —
//	          Get(ts)/Restore(ts) for that revision is now unrecoverable.
//	TS ORDER: per file, timestamps must be strictly increasing (the appendLog
//	          contract: "every revision uniquely addressable by timestamp").

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// raceWindow is how long each repro churns. Long enough that the OS interleaves
// the two instances' syscalls many times; short enough to stay a fast test.
const raceWindow = 3 * time.Second

// danglingHit is one log line that references a content object absent from
// objects/ at the moment it was scanned.
type danglingHit struct {
	rel  string
	ts   int64
	hash string
}

func (h danglingHit) String() string {
	short := h.hash
	if len(short) > 12 {
		short = short[:12]
	}
	return fmt.Sprintf("%s@%d -> objects/%s MISSING", h.rel, h.ts, short)
}

// scanDangling returns every (relPath, ts, hash) whose log line references a
// content object absent from objects/ — the "history says it exists, disk lost
// it" signal. It is safe to call from a non-test goroutine: it returns transient
// read errors (a log removed mid-rename, etc.) instead of failing the test.
//
// Lock-free, so a single scan can produce PHANTOMS: it may read a stale log that
// still names a hash a concurrent legitimate GC has since evicted AND swept.
// Phantoms are not corruption — confirmDangling filters them.
func scanDangling(s *FS) ([]danglingHit, error) {
	logs, err := s.allLogs()
	if err != nil {
		return nil, err
	}
	var dangling []danglingHit
	for _, rel := range logs {
		revs, err := s.readLog(rel)
		if err != nil {
			return nil, err
		}
		for _, r := range revs {
			if r.Hash == "" { // deletion: references no object
				continue
			}
			if _, err := os.Stat(s.objectPath(r.Hash)); err != nil {
				dangling = append(dangling, danglingHit{rel: rel, ts: r.Timestamp, hash: r.Hash})
			}
		}
	}
	return dangling, nil
}

// confirmDangling re-checks a suspected hit against the CURRENT store and reports
// whether it is real corruption rather than a stale-read phantom.
//
// ORDER IS LOAD-BEARING: stat the object FIRST, then re-read the log. Combined
// with a structural invariant in store.go this makes a lock-free check correct.
// The invariant: gcLocked and gcBySizeLocked each perform ALL of their
// rewriteLog calls BEFORE the single sweepUnreferenced (verified — in both, the
// rewrite loop precedes the one post-loop sweep). So "object gone" implies "the
// log that referenced it was already rewritten to drop it". Hence:
//   - object present                          -> not dangling.
//   - object gone, current log STILL refs it  -> REAL: a committed revision whose
//     object was swept, only possible if a writer and GC raced unlocked (the bug).
//   - object gone, current log no longer refs  -> phantom: GC legitimately evicted
//     that revision, so sweeping its object was correct.
//
// Reading the log first instead would let one call pair a stale "still refs H"
// with a post-sweep "gone" and cry wolf.
//
// WARNING: if a future GC ever interleaves rewriteLog and sweepUnreferenced, this
// gate can report a transient (object swept before its log line was dropped) as
// real — a false positive no test will catch. Keep rewrite strictly before sweep,
// or rewrite this gate. The dependency is the price of a lock-free detector.
func confirmDangling(s *FS, h danglingHit) bool {
	if _, err := os.Stat(s.objectPath(h.hash)); err == nil {
		return false // object present: not dangling
	}
	revs, err := s.readLog(h.rel)
	if err != nil {
		return false
	}
	for _, r := range revs {
		if r.Hash == h.hash {
			return true // object gone AND current log still references it => real
		}
	}
	return false // no longer referenced: legitimate eviction, a phantom
}

// scanTimestampOrder returns per-file timestamp-order violations (a ts <= its
// predecessor), which break unique addressability by timestamp.
func scanTimestampOrder(s *FS) ([]string, error) {
	logs, err := s.allLogs()
	if err != nil {
		return nil, err
	}
	var bad []string
	for _, rel := range logs {
		revs, err := s.readLog(rel)
		if err != nil {
			return nil, err
		}
		for i := 1; i < len(revs); i++ {
			if revs[i].Timestamp <= revs[i-1].Timestamp {
				bad = append(bad, fmt.Sprintf("%s: ts %d <= prev %d (idx %d)",
					rel, revs[i].Timestamp, revs[i-1].Timestamp, i))
			}
		}
	}
	return bad, nil
}

// TestCrossProcess_RecordVsGC_DanglingObject models a watcher recording
// revisions while a second process runs GC. The writer places objects/<hash>
// then appends the referencing log line; GC builds its referenced set from the
// logs then sweeps unreferenced objects. With no shared lock, GC's sweep can
// delete an object whose log line the writer has already (or concurrently)
// written — a dangling, unrecoverable revision.
//
// Detection is continuous, not just end-of-run: aggressive GC evicts old
// revisions, so a dangling line is often pruned from the log before the test
// ends, hiding evidence of a corruption that genuinely happened. A checker
// goroutine therefore samples throughout. A sighting is a TRUE positive by
// construction: only the writer creates objects (placing the object BEFORE
// appending its line) and only GC deletes them, so "log line present AND its
// object absent" can only mean GC swept an object the writer had already
// committed to history — exactly the dangling bug. A re-stat rules out a
// momentary FS read ordering.
func TestCrossProcess_RecordVsGC_DanglingObject(t *testing.T) {
	root := t.TempDir()
	writer := New(root) // "salvager watch" process
	if err := writer.Init(); err != nil {
		t.Fatal(err)
	}
	gc := New(root) // a separate GC/CLI process over the same .salvager

	const file = "main.go"
	abs := filepath.Join(root, file)

	var stop atomic.Bool
	var wg sync.WaitGroup

	// Writer: every iteration is fresh content => a brand-new object + a new log
	// line. Distinct content means each object is unique (never re-created by
	// dedup), so any object deleted out from under its log line stays dangling.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			content := []byte(fmt.Sprintf("package main\n// revision %d\nfunc main() {}\n", i))
			if err := os.WriteFile(abs, content, 0o644); err != nil {
				return
			}
			_ = writer.Record(file)
		}
	}()

	// GC: aggressive size cap forces eviction (rewriteLog) + sweepUnreferenced on
	// every pass, maximizing the sweep window against the writer's appends.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_, _ = gc.GCBySize(1)
		}
	}()

	// Checker: samples continuously and captures the first dangling revision it can
	// CONFIRM against the current store, so a transient (later evicted) corruption
	// is not missed while stale-read phantoms are filtered.
	var firstHit atomic.Pointer[string]
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			hits, err := scanDangling(writer)
			if err != nil {
				continue // transient read race against a concurrent rewrite/sweep
			}
			for _, hit := range hits {
				if !confirmDangling(writer, hit) {
					continue // stale-read phantom, not corruption
				}
				s := hit.String()
				if firstHit.CompareAndSwap(nil, &s) {
					stop.Store(true)
				}
			}
		}
	}()

	time.Sleep(raceWindow)
	stop.Store(true)
	wg.Wait()

	if h := firstHit.Load(); h != nil {
		t.Fatalf("STORE CORRUPTED: dangling revision observed — log references a swept object: %s", *h)
	}
	// Quiescent final scan: no writer/GC running, so every hit is real.
	d, err := scanDangling(writer)
	if err != nil {
		t.Fatalf("final scan: %v", err)
	}
	if len(d) > 0 {
		t.Fatalf("STORE CORRUPTED: %d dangling revision(s) survived to end of run:\n  %v", len(d), d)
	}
}

// TestCrossProcess_TwoWriters_TimestampCollision models two processes recording
// the SAME file (e.g. the watcher and an MCP-triggered restore safeguard). Each
// Record reads the last revision then appends with a ts derived from it; with no
// shared lock both can read the same predecessor and append the same (or
// out-of-order) timestamp, breaking unique addressability.
func TestCrossProcess_TwoWriters_TimestampCollision(t *testing.T) {
	root := t.TempDir()
	a := New(root)
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}
	b := New(root)

	const file = "shared.txt"
	abs := filepath.Join(root, file)

	var stop atomic.Bool
	var wg sync.WaitGroup
	writer := func(s *FS, tag string) {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			content := []byte(fmt.Sprintf("%s line %d\n", tag, i))
			if err := os.WriteFile(abs, content, 0o644); err != nil {
				return
			}
			_ = s.Record(file)
		}
	}
	wg.Add(2)
	go writer(a, "a")
	go writer(b, "b")

	time.Sleep(raceWindow)
	stop.Store(true)
	wg.Wait()

	bad, err := scanTimestampOrder(a)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(bad) > 0 {
		t.Fatalf("STORE CORRUPTED: %d timestamp-order violation(s) — revisions no longer uniquely addressable:\n  %v",
			len(bad), bad)
	}
}
