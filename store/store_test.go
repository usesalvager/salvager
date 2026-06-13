package store

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

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
	os.Remove(filepath.Join(root, "a.txt"))
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
