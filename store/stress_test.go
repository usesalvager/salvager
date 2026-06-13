package store

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// B4.2 — two concurrent restores on the same file. After both return, the store
// is consistent: every .log line parses, the working-tree file equals one of the
// restored contents, and List is strictly monotonic in timestamp.
// -----------------------------------------------------------------------------

func TestStress_B4_2_ConcurrentRestoresSameFile(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	// Two good historical contents to restore to.
	write(t, root, "a.txt", "ALPHA")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "a.txt", "BETA")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	// Current broken state.
	write(t, root, "a.txt", "BROKEN")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}

	revs, _ := s.List("a.txt") // newest first: BROKEN, BETA, ALPHA
	var alphaTs, betaTs int64
	for _, r := range revs {
		switch r.Hash {
		case sha256hex([]byte("ALPHA")):
			alphaTs = r.Timestamp
		case sha256hex([]byte("BETA")):
			betaTs = r.Timestamp
		}
	}
	if alphaTs == 0 || betaTs == 0 {
		t.Fatalf("could not locate ALPHA/BETA timestamps: %+v", revs)
	}

	var wg sync.WaitGroup
	targets := []int64{alphaTs, betaTs}
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Restore("a.txt", targets[i])
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("concurrent restore %d failed: %v", i, e)
		}
	}

	// 1. Every .log line parses (no interleaved/corrupt lines).
	raw, err := os.ReadFile(filepath.Join(root, Dir, "index", "a.txt.log"))
	if err != nil {
		t.Fatal(err)
	}
	var prevTs int64 = -1
	for ln, line := range bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n")) {
		rev, ok := parseLine(string(line))
		if !ok {
			t.Fatalf("line %d does not parse: %q", ln, line)
		}
		// 3. Timestamps strictly increasing oldest->newest in the file.
		if rev.Timestamp <= prevTs {
			t.Errorf("line %d timestamp %d not > previous %d", ln, rev.Timestamp, prevTs)
		}
		prevTs = rev.Timestamp
	}

	// 2. The working-tree file equals one of the restored contents.
	onDisk, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, []byte("ALPHA")) && !bytes.Equal(onDisk, []byte("BETA")) {
		t.Errorf("working tree = %q, want ALPHA or BETA", onDisk)
	}

	// List is monotonic (strictly decreasing newest-first == strictly
	// increasing oldest-first) and every entry retrievable.
	list, _ := s.List("a.txt")
	for i := 1; i < len(list); i++ {
		if list[i-1].Timestamp <= list[i].Timestamp {
			t.Errorf("List not strictly monotonic at %d: %d then %d",
				i, list[i-1].Timestamp, list[i].Timestamp)
		}
	}
}

// -----------------------------------------------------------------------------
// B4.3 — crash mid-write / atomicity. A leftover ".tmp-xxxx" object and a
// truncated final .log line must be ignored by List/Get; GC must not delete
// valid objects nor choke on the temp file.
// -----------------------------------------------------------------------------

func TestStress_B4_3_LeftoverTempObjectAndTruncatedLine(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "valid-one")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "a.txt", "valid-two")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}

	validHashOne := sha256hex([]byte("valid-one"))
	validHashTwo := sha256hex([]byte("valid-two"))

	// Drop a leftover temp object simulating a crash mid writeObject.
	if err := os.WriteFile(
		filepath.Join(root, Dir, "objects", ".tmp-deadbeef"),
		[]byte("partial garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Truncate the final .log line (crash mid appendLog): no label, no newline.
	lp := filepath.Join(root, Dir, "index", "a.txt.log")
	f, err := os.OpenFile(lp, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("123456\tcafebabe"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// List/Get must ignore the corrupt line and not crash.
	revs, err := s.List("a.txt")
	if err != nil {
		t.Fatalf("List should tolerate corruption, got: %v", err)
	}
	if len(revs) != 2 {
		t.Fatalf("want 2 valid revisions (corrupt line ignored), got %d", len(revs))
	}
	for _, r := range revs {
		if _, err := s.Get("a.txt", r.Timestamp); err != nil {
			t.Errorf("Get(%d) failed: %v", r.Timestamp, err)
		}
	}

	// GC with a huge window (purge nothing): must keep both valid objects and
	// must not choke on the .tmp- object. It must NOT delete the temp file
	// either (GC explicitly skips .tmp- entries) nor any referenced object.
	if err := s.GC(365 * 24 * time.Hour); err != nil {
		t.Fatalf("GC crashed on leftover temp object / truncated line: %v", err)
	}
	if !accObjectExists(t, root, validHashOne) {
		t.Errorf("GC removed valid object %s", validHashOne)
	}
	if !accObjectExists(t, root, validHashTwo) {
		t.Errorf("GC removed valid object %s", validHashTwo)
	}
	// The temp object is skipped by GC (not treated as garbage to collect).
	if _, err := os.Stat(filepath.Join(root, Dir, "objects", ".tmp-deadbeef")); err != nil {
		t.Errorf("GC unexpectedly removed/altered the .tmp- object: %v", err)
	}

	// Records still work after the corruption is in place.
	write(t, root, "a.txt", "valid-three")
	if err := s.Record("a.txt"); err != nil {
		t.Fatalf("Record after corruption failed: %v", err)
	}
	revs, _ = s.List("a.txt")
	if len(revs) != 3 {
		t.Errorf("after new record want 3 revs, got %d", len(revs))
	}
}

// B4.3 (GC-with-corruption angle): a corrupted/truncated final line in one
// file's log must not cause GC to incorrectly purge an object referenced only by
// a valid line, nor to crash.
func TestStress_B4_3_GCWithCorruptLine(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "keep-me")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	keepHash := sha256hex([]byte("keep-me"))

	// Append a garbage final line referencing a hash that has NO object.
	lp := filepath.Join(root, Dir, "index", "a.txt.log")
	f, _ := os.OpenFile(lp, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("not-a-timestamp\tffffffff\tmodify\n") // unparseable ts
	f.Close()

	if err := s.GC(365 * 24 * time.Hour); err != nil {
		t.Fatalf("GC crashed on corrupt line: %v", err)
	}
	// The valid object stays.
	if !accObjectExists(t, root, keepHash) {
		t.Errorf("GC removed the object referenced by the only valid line")
	}
	// The valid revision is still retrievable.
	revs, _ := s.List("a.txt")
	if len(revs) != 1 {
		t.Fatalf("want 1 valid rev, got %d", len(revs))
	}
	got, err := s.Get("a.txt", revs[0].Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("keep-me")) {
		t.Errorf("content = %q, want keep-me", got)
	}
}

// -----------------------------------------------------------------------------
// B6.2 — store growth and gc under load. After many days of churn across many
// files, gc must recover space (drop old revisions + their now-unreferenced
// objects), keep the recent ones, leave the store consistent, and never
// over-delete (every surviving revision's content stays recoverable).
// -----------------------------------------------------------------------------

func TestStress_B6_2_GCUnderLoad(t *testing.T) {
	clock := fakeClock(t)
	root := t.TempDir()
	s := New(root)

	const nFiles = 40
	base := int64(1_000_000)
	day := func(d int) int64 { return base + int64(d)*int64(24*time.Hour/time.Millisecond) }
	churn := func(d int, prefix string) {
		atomic.StoreInt64(clock, day(d))
		for f := 0; f < nFiles; f++ {
			rel := fmt.Sprintf("f%02d.txt", f)
			write(t, root, rel, fmt.Sprintf("%s-day%d-f%d", prefix, d, f))
			if err := s.Record(rel); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Old churn: days 0..6 (distinct content each day => distinct objects).
	for d := 0; d <= 6; d++ {
		churn(d, "old")
	}
	// Recent churn: days 9 and 10.
	churn(9, "RECENT")
	churn(10, "RECENT")

	atomic.StoreInt64(clock, day(10))
	objsBefore := accObjectCount(t, root)

	// Keep the last 2 days (cutoff = day 8). Days 0..6 purge; 9,10 survive.
	if err := s.GC(48 * time.Hour); err != nil {
		t.Fatal(err)
	}

	objsAfter := accObjectCount(t, root)
	if objsAfter >= objsBefore {
		t.Errorf("gc recovered no space: objects before=%d after=%d", objsBefore, objsAfter)
	}

	for f := 0; f < nFiles; f++ {
		rel := fmt.Sprintf("f%02d.txt", f)
		revs, err := s.List(rel)
		if err != nil {
			t.Fatalf("List(%s): %v", rel, err)
		}
		if len(revs) == 0 {
			t.Errorf("%s lost all history after gc (over-deletion)", rel)
			continue
		}
		// Newest must be the day-10 content and recoverable (no over-deletion of
		// still-referenced objects).
		got, err := s.Get(rel, revs[0].Timestamp)
		if err != nil {
			t.Errorf("Get newest %s after gc: %v", rel, err)
			continue
		}
		want := fmt.Sprintf("RECENT-day10-f%d", f)
		if string(got) != want {
			t.Errorf("%s newest = %q, want %q", rel, got, want)
		}
		// All surviving revisions are within the retention window.
		for _, r := range revs {
			if r.Timestamp < day(8) {
				t.Errorf("%s kept a revision older than the cutoff: ts=%d", rel, r.Timestamp)
			}
		}
	}
}
