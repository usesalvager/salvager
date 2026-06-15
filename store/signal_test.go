package store

import (
	"os"
	"path/filepath"
	"testing"
)

// newest returns the most-recent revision of rel, failing the test if there is
// none. List is newest-first, so that is element 0.
func newest(t *testing.T, s *FS, rel string) Revision {
	t.Helper()
	revs, err := s.List(rel)
	if err != nil {
		t.Fatalf("List(%s): %v", rel, err)
	}
	if len(revs) == 0 {
		t.Fatalf("List(%s): no revisions", rel)
	}
	return revs[0]
}

// TestSignalComputedAtCapture proves the content signal (lines, bytes, delta,
// start signature) is computed at capture for the cases that matter: a brand-new
// file, a modification that adds lines, one that removes lines, and a binary
// file whose signature must degrade gracefully instead of corrupting the line.
func TestSignalComputedAtCapture(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	// New file: 3 lines. Delta counts every line as added.
	const created = "line1\nline2\nline3\n"
	write(t, root, "stats.py", created)
	if err := s.Record("stats.py"); err != nil {
		t.Fatal(err)
	}
	r := newest(t, s, "stats.py")
	if !r.HasSignal {
		t.Fatal("new file: HasSignal = false, want true")
	}
	if r.Lines != 3 || r.Bytes != len(created) {
		t.Errorf("new file: lines=%d bytes=%d, want lines=3 bytes=%d", r.Lines, r.Bytes, len(created))
	}
	if !r.DeltaKnown || r.Delta != 3 || r.DeltaString() != "+3" {
		t.Errorf("new file: delta=%d known=%v str=%q, want +3", r.Delta, r.DeltaKnown, r.DeltaString())
	}
	if r.Sig != "line1\nline2\nline3" {
		t.Errorf("new file: signature = %q, want the first three lines", r.Sig)
	}

	// Modification that ADDS lines: 3 -> 6, delta +3.
	write(t, root, "stats.py", "line1\nline2\nline3\nline4\nline5\nline6\n")
	if err := s.Record("stats.py"); err != nil {
		t.Fatal(err)
	}
	r = newest(t, s, "stats.py")
	if r.Lines != 6 || !r.DeltaKnown || r.Delta != 3 || r.DeltaString() != "+3" {
		t.Errorf("add: lines=%d delta=%s, want lines=6 delta=+3", r.Lines, r.DeltaString())
	}

	// Modification that REMOVES lines: 6 -> 2, delta -4. This is the signal that
	// instantly disproves a "nothing was really lost" conclusion.
	write(t, root, "stats.py", "line1\nline2\n")
	if err := s.Record("stats.py"); err != nil {
		t.Fatal(err)
	}
	r = newest(t, s, "stats.py")
	if r.Lines != 2 || !r.DeltaKnown || r.Delta != -4 || r.DeltaString() != "-4" {
		t.Errorf("remove: lines=%d delta=%s, want lines=2 delta=-4", r.Lines, r.DeltaString())
	}

	// Binary content: signature degrades to empty (NUL in the head), but the
	// revision is still captured with an exact byte count and never panics.
	binary := []byte{'M', 'Z', 0x00, 0x01, 0x02, 'x', 'y'}
	if err := os.WriteFile(filepath.Join(root, "stats.py"), binary, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("stats.py"); err != nil {
		t.Fatal(err)
	}
	r = newest(t, s, "stats.py")
	if !r.HasSignal {
		t.Fatal("binary: HasSignal = false, want true")
	}
	if r.Bytes != len(binary) {
		t.Errorf("binary: bytes=%d, want %d", r.Bytes, len(binary))
	}
	if r.Sig != "" {
		t.Errorf("binary: signature = %q, want empty (graceful degrade)", r.Sig)
	}
}

// TestSignalBackwardCompatLegacyLine proves a .log written before the signal
// existed still parses: the legacy revision is surfaced as "signal unavailable"
// (HasSignal false, empty delta string) rather than rejected, and a new revision
// appended after it reports its delta as unknowable because there is no prior
// line count to diff against.
func TestSignalBackwardCompatLegacyLine(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	// Hand-write a legacy three-column line (the old format, no signal).
	lp := s.logPath("legacy.py")
	if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyHash := sha256hex([]byte("legacy body"))
	if err := os.WriteFile(lp, []byte("1000000\t"+legacyHash+"\tmodify\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	revs, err := s.List("legacy.py")
	if err != nil {
		t.Fatalf("legacy line must parse without error, got %v", err)
	}
	if len(revs) != 1 {
		t.Fatalf("want 1 legacy revision, got %d", len(revs))
	}
	if revs[0].HasSignal {
		t.Error("legacy revision: HasSignal = true, want false (signal unavailable)")
	}
	if revs[0].DeltaString() != "" {
		t.Errorf("legacy revision: delta string = %q, want empty", revs[0].DeltaString())
	}

	// Appending a new revision after a legacy one: the new line gets a signal,
	// but its delta is unknowable (the predecessor has no line count).
	write(t, root, "legacy.py", "fresh\nbody\nhere\n")
	if err := s.Record("legacy.py"); err != nil {
		t.Fatal(err)
	}
	r := newest(t, s, "legacy.py")
	if !r.HasSignal || r.Lines != 3 {
		t.Errorf("new-after-legacy: HasSignal=%v lines=%d, want true/3", r.HasSignal, r.Lines)
	}
	if r.DeltaKnown || r.DeltaString() != "?" {
		t.Errorf("new-after-legacy: delta string = %q (known=%v), want \"?\"", r.DeltaString(), r.DeltaKnown)
	}
}

// TestListDoesNotReadObjects is the store-level zero-content-read guard,
// complementing the watcher's TestSweepGateZeroReadsWhenUnchanged: the signal is
// computed once at capture, so List must answer entirely from the index/ logs
// and never open an object. Wiping the objects/ dir makes any object read fail,
// so a List that still returns the full signal proves it touched no object.
func TestListDoesNotReadObjects(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "alpha\nbeta\n")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "a.txt", "alpha\nbeta\ngamma\n")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}

	// Wipe every stored object: only the index/ logs remain.
	if err := os.RemoveAll(s.objectsDir()); err != nil {
		t.Fatal(err)
	}

	revs, err := s.List("a.txt")
	if err != nil {
		t.Fatalf("List read an object (it must not): %v", err)
	}
	if len(revs) != 2 {
		t.Fatalf("want 2 revisions from the log alone, got %d", len(revs))
	}
	for i, r := range revs {
		if !r.HasSignal || r.Lines == 0 {
			t.Errorf("revision %d lost its signal after objects were wiped (lines=%d) — List must read it from the log", i, r.Lines)
		}
	}

	// Sanity: the objects really are gone, so Get (which DOES read content) fails.
	if _, err := s.Get("a.txt", revs[0].Timestamp); err == nil {
		t.Error("Get succeeded after objects were wiped; the guard is not actually exercising a missing object")
	}
}
