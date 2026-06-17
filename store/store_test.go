package store

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// objectNames lists the real objects on disk (skipping in-flight temps).
func objectNames(t *testing.T, root string) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(root, Dir, "objects"))
	var names []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		names = append(names, e.Name())
	}
	return names
}

// assertNoOrphanRestore checks the P2 invariant: every surviving restore still
// has its pre-restore immediately before it, so no recorded restore was made
// irreversible by the GC.
func assertNoOrphanRestore(t *testing.T, s *FS, relPath string) {
	t.Helper()
	revs, err := s.readLog(relPath) // oldest-first
	if err != nil {
		t.Fatal(err)
	}
	for j, r := range revs {
		if r.Label != LabelRestore {
			continue
		}
		if j == 0 || revs[j-1].Label != LabelPreRestore {
			t.Fatalf("orphan restore at %s ts=%d: missing immediate pre-restore", relPath, r.Timestamp)
		}
	}
}

// fakeClock makes timestamps deterministic and strictly increasing.
func fakeClock(t *testing.T) *int64 {
	var n int64 = 1_000_000
	orig := nowFunc
	nowFunc = func() int64 { return atomic.AddInt64(&n, 1) }
	t.Cleanup(func() { nowFunc = orig })
	return &n
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRecordInitialAndModify(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "one")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "a.txt", "two")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}

	revs, err := s.List("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 {
		t.Fatalf("want 2 revisions, got %d", len(revs))
	}
	// Most recent first.
	if revs[0].Label != LabelModify {
		t.Errorf("newest label = %q, want modify", revs[0].Label)
	}
	if revs[1].Label != LabelInitial {
		t.Errorf("oldest label = %q, want initial", revs[1].Label)
	}
	if revs[0].Timestamp <= revs[1].Timestamp {
		t.Errorf("timestamps not increasing: %d then %d", revs[1].Timestamp, revs[0].Timestamp)
	}
}

func TestRecordDedup(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "same")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	// Identical content again -> no new revision.
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}

	revs, _ := s.List("a.txt")
	if len(revs) != 1 {
		t.Fatalf("dedup failed: want 1 revision, got %d", len(revs))
	}
}

func TestObjectDedupAcrossFiles(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "shared")
	write(t, root, "b.txt", "shared")
	s.Record("a.txt")
	s.Record("b.txt")

	entries, err := os.ReadDir(filepath.Join(root, Dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 shared object, got %d", len(entries))
	}
}

func TestGetReturnsHistoricContent(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "v1")
	s.Record("a.txt")
	write(t, root, "a.txt", "v2")
	s.Record("a.txt")

	revs, _ := s.List("a.txt") // newest first: v2, v1
	old, err := s.Get("a.txt", revs[1].Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != "v1" {
		t.Errorf("Get oldest = %q, want v1", old)
	}
}

func TestRestoreSafeguard(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "good")
	s.Record("a.txt")
	write(t, root, "a.txt", "BROKEN")
	s.Record("a.txt")

	revs, _ := s.List("a.txt") // newest first: BROKEN, good
	goodTs := revs[1].Timestamp

	preTs, err := s.Restore("a.txt", goodTs)
	if err != nil {
		t.Fatal(err)
	}

	// Working tree recovered.
	got, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(got) != "good" {
		t.Errorf("after restore file = %q, want good", got)
	}

	// Safeguard captured the broken state and restore is reversible.
	revs, _ = s.List("a.txt") // newest first: restore, pre-restore, BROKEN, good
	if revs[0].Label != LabelRestore {
		t.Errorf("newest label = %q, want restore", revs[0].Label)
	}
	if revs[1].Label != LabelPreRestore {
		t.Errorf("second label = %q, want pre-restore", revs[1].Label)
	}
	if revs[1].Timestamp != preTs {
		t.Errorf("returned preTs %d != logged pre-restore ts %d", preTs, revs[1].Timestamp)
	}

	// The pre-restore content is the broken state -> restore is reversible.
	broken, err := s.Get("a.txt", preTs)
	if err != nil {
		t.Fatal(err)
	}
	if string(broken) != "BROKEN" {
		t.Errorf("pre-restore content = %q, want BROKEN", broken)
	}
}

func TestRecordDeletion(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "alive")
	s.Record("a.txt")
	_ = os.Remove(filepath.Join(root, "a.txt")) // simulate a deletion; a failure surfaces in the assertion below
	s.Record("a.txt")

	revs, _ := s.List("a.txt")
	if revs[0].Label != LabelDelete {
		t.Fatalf("newest label = %q, want delete", revs[0].Label)
	}
	// Recording deletion again is a no-op.
	s.Record("a.txt")
	revs2, _ := s.List("a.txt")
	if len(revs2) != len(revs) {
		t.Errorf("double-delete recorded extra revision")
	}
}

func TestCorruptLastLineTolerated(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "one")
	s.Record("a.txt")

	// Append a truncated/garbage line simulating a crash mid-write.
	lp := filepath.Join(root, Dir, "index", "a.txt.log")
	f, _ := os.OpenFile(lp, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("99999\tdeadbeef") // no label, no newline
	f.Close()

	revs, err := s.List("a.txt")
	if err != nil {
		t.Fatalf("corrupt line should be tolerated, got err: %v", err)
	}
	if len(revs) != 1 {
		t.Errorf("want 1 valid revision, got %d", len(revs))
	}
}

func TestGC(t *testing.T) {
	clock := fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "old")
	s.Record("a.txt")
	// Jump the clock far forward so the first revision is "old".
	atomic.StoreInt64(clock, 1_000_000+int64(48*time.Hour/time.Millisecond))
	write(t, root, "a.txt", "new")
	s.Record("a.txt")

	if err := s.GC(24 * time.Hour); err != nil {
		t.Fatal(err)
	}

	revs, _ := s.List("a.txt")
	if len(revs) != 1 {
		t.Fatalf("GC: want 1 surviving revision, got %d", len(revs))
	}
	if revs[0].Label != LabelModify {
		t.Errorf("survivor label = %q, want modify (the new one)", revs[0].Label)
	}

	// The old object must be collected; only the surviving one remains.
	objs, _ := os.ReadDir(filepath.Join(root, Dir, "objects"))
	if len(objs) != 1 {
		t.Errorf("GC: want 1 object after collection, got %d", len(objs))
	}
}

// Regression: when EVERY revision of a file predates the age cutoff, GC must
// still keep the newest one (P1) rather than erasing the file's whole history.
// Before the fix, age GC pruned blind and left the file with zero recoverable
// revisions — the exact data loss the store exists to prevent.
func TestGCKeepsNewestWhenAllOlderThanCutoff(t *testing.T) {
	clock := fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "v1")
	s.Record("a.txt")
	atomic.AddInt64(clock, 1000)
	write(t, root, "a.txt", "v2")
	s.Record("a.txt")

	// Jump far past both revisions so a 24h cutoff leaves NONE by age alone.
	atomic.AddInt64(clock, int64(48*time.Hour/time.Millisecond))
	if err := s.GC(24 * time.Hour); err != nil {
		t.Fatal(err)
	}

	revs, _ := s.List("a.txt")
	if len(revs) != 1 {
		t.Fatalf("GC erased a file's whole history: want 1 pinned survivor, got %d", len(revs))
	}
	got, err := s.Get("a.txt", revs[0].Timestamp)
	if err != nil || string(got) != "v2" {
		t.Fatalf("newest revision unrecoverable after GC: content=%q err=%v", got, err)
	}
	objs, _ := os.ReadDir(filepath.Join(root, Dir, "objects"))
	if len(objs) != 1 {
		t.Errorf("want 1 object retained for the pinned revision, got %d", len(objs))
	}
}

// Regression: a restore pins both itself (P1) and its pre-restore (P2) even when
// both predate the age cutoff, so a recorded restore stays reversible.
func TestGCKeepsPreRestorePairWhenOlderThanCutoff(t *testing.T) {
	clock := fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "first")
	s.Record("a.txt")
	atomic.AddInt64(clock, 1000)
	write(t, root, "a.txt", "second")
	s.Record("a.txt")
	atomic.AddInt64(clock, 1000)
	// Restore the first revision: writes a pre-restore (of "second") then a
	// restore back-to-back as the two newest revisions.
	first, _ := s.List("a.txt")
	if _, err := s.Restore("a.txt", first[0].Timestamp); err != nil {
		t.Fatal(err)
	}

	atomic.AddInt64(clock, int64(48*time.Hour/time.Millisecond))
	if err := s.GC(24 * time.Hour); err != nil {
		t.Fatal(err)
	}

	revs, _ := s.List("a.txt")
	var nPre, nRestore int
	for _, r := range revs {
		switch r.Label {
		case LabelPreRestore:
			nPre++
		case LabelRestore:
			nRestore++
		}
	}
	if nPre != 1 || nRestore != 1 {
		t.Fatalf("pre-restore/restore pair not preserved: pre=%d restore=%d (revs=%d)", nPre, nRestore, len(revs))
	}
}

// Test 1: exceeding the budget evicts the oldest revisions until the store fits;
// the newest is pinned (P1). Object size == content length (content-addressed).
func TestGCBySizeEvictsOldest(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", strings.Repeat("a", 100))
	s.Record("a.txt")
	write(t, root, "a.txt", strings.Repeat("b", 100))
	s.Record("a.txt")
	write(t, root, "a.txt", strings.Repeat("c", 100))
	s.Record("a.txt")

	// total 300; budget 250 -> evict exactly the oldest, 200 fits.
	final, err := s.GCBySize(250)
	if err != nil {
		t.Fatal(err)
	}
	if final != 200 {
		t.Fatalf("final=%d, want 200", final)
	}
	revs, _ := s.List("a.txt")
	if len(revs) != 2 {
		t.Fatalf("want 2 surviving revisions, got %d", len(revs))
	}
	if got := len(objectNames(t, root)); got != 2 {
		t.Fatalf("want 2 objects, got %d", got)
	}
}

// Test 1 (dedup): an object shared by two revisions is freed only when its LAST
// reference is evicted. Here b.txt's only (newest, pinned) revision shares the
// blob, so evicting a.txt's old reference frees nothing — the P1 floor is the
// shared blob plus a.txt's newest, and GCBySize reports it could not go lower.
func TestGCBySizeSharedObjectNotFreedUntilAllRefsGone(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	shared := strings.Repeat("s", 100)
	write(t, root, "a.txt", shared)
	s.Record("a.txt")
	write(t, root, "a.txt", strings.Repeat("A", 50))
	s.Record("a.txt")
	write(t, root, "b.txt", shared) // b's single (newest) revision pins the shared blob
	s.Record("b.txt")

	// objects: shared(100) + A(50) = 150. budget 50 is unreachable: shared is
	// pinned by b's newest, and a's old reference alone cannot free it.
	final, err := s.GCBySize(50)
	if err != nil {
		t.Fatal(err)
	}
	if final != 150 {
		t.Fatalf("final=%d, want 150 (shared blob pinned by b.txt's newest)", final)
	}
	if got := len(objectNames(t, root)); got != 2 {
		t.Fatalf("want 2 objects (shared survives), got %d", got)
	}
	// a.txt lost its old (shared) revision — evicted, though it freed nothing.
	if revs, _ := s.List("a.txt"); len(revs) != 1 {
		t.Fatalf("a.txt: want 1 revision, got %d", len(revs))
	}
	if revs, _ := s.List("b.txt"); len(revs) != 1 {
		t.Fatalf("b.txt: want 1 revision, got %d", len(revs))
	}
}

// Test 1 (dedup, positive): when BOTH references of a shared blob are evicted it
// is finally freed, dropping under budget.
func TestGCBySizeSharedObjectFreedWhenAllRefsEvicted(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	shared := strings.Repeat("s", 100)
	write(t, root, "a.txt", shared)
	s.Record("a.txt")
	write(t, root, "a.txt", strings.Repeat("A", 10))
	s.Record("a.txt")
	write(t, root, "b.txt", shared)
	s.Record("b.txt")
	write(t, root, "b.txt", strings.Repeat("B", 10))
	s.Record("b.txt")

	// objects: shared(100)+A(10)+B(10)=120. floor (newest of each)=20.
	// budget 20: evict a-old (refcount 2->1, no free), evict b-old (1->0, free 100).
	final, err := s.GCBySize(20)
	if err != nil {
		t.Fatal(err)
	}
	if final != 20 {
		t.Fatalf("final=%d, want 20", final)
	}
	if got := len(objectNames(t, root)); got != 2 {
		t.Fatalf("want 2 objects (A,B) after freeing shared, got %d", got)
	}
}

// Test 2 (P1): an impossible budget keeps exactly each file's newest revision;
// the log is never emptied.
func TestGCBySizeP1KeepsLastRevision(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", strings.Repeat("a", 100))
	s.Record("a.txt")
	write(t, root, "a.txt", strings.Repeat("b", 100))
	s.Record("a.txt")
	write(t, root, "a.txt", strings.Repeat("c", 100))
	s.Record("a.txt")

	final, err := s.GCBySize(1) // impossible
	if err != nil {
		t.Fatal(err)
	}
	if final != 100 {
		t.Fatalf("final=%d, want 100 (newest object is the floor)", final)
	}
	revs, _ := s.List("a.txt")
	if len(revs) != 1 {
		t.Fatalf("P1: want exactly 1 surviving revision, got %d", len(revs))
	}
	if revs[0].Label != LabelModify {
		t.Errorf("survivor label = %q, want the newest (modify)", revs[0].Label)
	}
	if _, err := os.Stat(s.logPath("a.txt")); err != nil {
		t.Fatalf("P1: log must not be removed: %v", err)
	}
	if got := len(objectNames(t, root)); got != 1 {
		t.Fatalf("want 1 object, got %d", got)
	}
}

// makePair drives the store into a pre-restore/restore pair on relPath. The
// dirty (unrecorded) on-disk state gives the pre-restore a UNIQUE object, so the
// pair's reversibility blob is distinguishable from every other revision's.
func makePair(t *testing.T, s *FS, root, relPath, dirty string, restoreToTs int64) {
	t.Helper()
	write(t, root, relPath, dirty) // dirty working-tree state, not recorded
	if _, err := s.Restore(relPath, restoreToTs); err != nil {
		t.Fatal(err)
	}
}

// Test 3 (P2): a budget met BEFORE the walk reaches the pair leaves the pair
// fully intact — both the pre-restore and the restore survive, so the restore
// stays reversible.
func TestGCBySizeP2PairStaysIntact(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", strings.Repeat("v", 60)) // V1, ref only by v1
	s.Record("a.txt")
	v2ts := func() int64 { r, _ := s.List("a.txt"); return r[0].Timestamp }
	write(t, root, "a.txt", strings.Repeat("w", 20)) // V2
	s.Record("a.txt")
	makePair(t, s, root, "a.txt", strings.Repeat("d", 40), v2ts()) // preR=D(40), restore->V2
	write(t, root, "a.txt", strings.Repeat("z", 30))               // V3 newest
	s.Record("a.txt")

	// objects: V1(60)+V2(20)+D(40)+V3(30)=150. budget 100: evict v1 (free 60 ->90),
	// 90<=100 stop. The pair (preR+restore) is never reached.
	final, err := s.GCBySize(100)
	if err != nil {
		t.Fatal(err)
	}
	if final != 90 {
		t.Fatalf("final=%d, want 90", final)
	}
	assertNoOrphanRestore(t, s, "a.txt")
	revs, _ := s.readLog("a.txt")
	var pre, res bool
	for _, r := range revs {
		if r.Label == LabelPreRestore {
			pre = true
		}
		if r.Label == LabelRestore {
			res = true
		}
	}
	if !pre || !res {
		t.Fatalf("pair must stay intact: preRestore=%v restore=%v", pre, res)
	}
}

// Test 3 (P2 x P1, the matiz): when the restore IS the file's newest revision it
// is pinned by P1, which pins the pre-restore by extension. Even an impossible
// budget cannot break the pair, and GCBySize reports the floor rather than
// orphaning the restore.
func TestGCBySizeP2RestorePinnedByP1(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", strings.Repeat("v", 10)) // V1
	s.Record("a.txt")
	v1ts := func() int64 { r, _ := s.List("a.txt"); return r[0].Timestamp }()
	write(t, root, "a.txt", strings.Repeat("w", 20)) // V2
	s.Record("a.txt")
	makePair(t, s, root, "a.txt", strings.Repeat("d", 40), v1ts) // preR=D(40), restore->V1 (newest)

	// objects: V1(10)+V2(20)+D(40)=70. Evictable: only v1 (frees nothing, V1 also
	// held by restore) and v2 (frees 20). floor=50. budget 1 unreachable.
	final, err := s.GCBySize(1)
	if err != nil {
		t.Fatal(err)
	}
	if final != 50 {
		t.Fatalf("final=%d, want 50 (pair pinned: V1+D survive)", final)
	}
	assertNoOrphanRestore(t, s, "a.txt")
	revs, _ := s.readLog("a.txt")
	if len(revs) != 2 || revs[0].Label != LabelPreRestore || revs[1].Label != LabelRestore {
		t.Fatalf("pair must stay intact, got %+v", revs)
	}
	if got := len(objectNames(t, root)); got != 2 {
		t.Fatalf("want 2 objects (V1,D), got %d", got)
	}
}

// Test 4: identical store + identical budget + identical clock -> identical
// eviction (object set and surviving timestamps).
func TestGCBySizeDeterministic(t *testing.T) {
	orig := nowFunc
	t.Cleanup(func() { nowFunc = orig })

	run := func() ([]string, []int64) {
		var n int64 = 1_000_000
		nowFunc = func() int64 { return atomic.AddInt64(&n, 1) }
		root := t.TempDir()
		s := New(root)
		write(t, root, "a.txt", strings.Repeat("a", 100))
		s.Record("a.txt")
		write(t, root, "a.txt", strings.Repeat("b", 100))
		s.Record("a.txt")
		write(t, root, "a.txt", strings.Repeat("c", 100))
		s.Record("a.txt")
		write(t, root, "b.txt", strings.Repeat("d", 100))
		s.Record("b.txt")
		write(t, root, "b.txt", strings.Repeat("e", 100))
		s.Record("b.txt")
		if _, err := s.GCBySize(250); err != nil {
			t.Fatal(err)
		}
		objs := objectNames(t, root)
		sort.Strings(objs)
		ar, _ := s.List("a.txt")
		var ats []int64
		for _, r := range ar {
			ats = append(ats, r.Timestamp)
		}
		return objs, ats
	}

	o1, a1 := run()
	o2, a2 := run()
	if !reflect.DeepEqual(o1, o2) {
		t.Errorf("objects differ across runs: %v vs %v", o1, o2)
	}
	if !reflect.DeepEqual(a1, a2) {
		t.Errorf("surviving timestamps differ across runs: %v vs %v", a1, a2)
	}
}

// Test 5: --max-age then --max-bytes compose — age prunes first, size caps what
// remains.
func TestGCBySizeComposesWithAge(t *testing.T) {
	clock := fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", strings.Repeat("a", 100)) // old
	s.Record("a.txt")
	atomic.StoreInt64(clock, 1_000_000+int64(48*time.Hour/time.Millisecond))
	write(t, root, "a.txt", strings.Repeat("b", 100)) // recent
	s.Record("a.txt")
	write(t, root, "a.txt", strings.Repeat("c", 100)) // recent, newest
	s.Record("a.txt")

	// Age GC drops the >24h-old revision and collects its object.
	if err := s.GC(24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := len(objectNames(t, root)); got != 2 {
		t.Fatalf("after age GC: want 2 objects, got %d", got)
	}
	// Size GC over the survivors (200): budget 100 evicts the older of the two.
	final, err := s.GCBySize(100)
	if err != nil {
		t.Fatal(err)
	}
	if final != 100 {
		t.Fatalf("final=%d, want 100", final)
	}
	if revs, _ := s.List("a.txt"); len(revs) != 1 {
		t.Fatalf("after age+size: want 1 revision, got %d", len(revs))
	}
	if got := len(objectNames(t, root)); got != 1 {
		t.Fatalf("after age+size: want 1 object, got %d", got)
	}
}
