package store

import (
	"bytes"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// accObjectCount returns the number of real object files (excludes .tmp-*).
func accObjectCount(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, Dir, "objects"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if len(e.Name()) >= 5 && e.Name()[:5] == ".tmp-" {
			continue
		}
		n++
	}
	return n
}

// accObjectExists reports whether an object with the given hash is on disk.
func accObjectExists(t *testing.T, root, hash string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(root, Dir, "objects", hash))
	return err == nil
}

// -----------------------------------------------------------------------------
// A2.3 — delete records a 'delete' rev, no new object, prior content recoverable
// -----------------------------------------------------------------------------

func TestAcc_A2_3_DeleteNoNewObjectPriorRecoverable(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "alive content")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	objsBefore := accObjectCount(t, root)
	if objsBefore != 1 {
		t.Fatalf("want 1 object after initial record, got %d", objsBefore)
	}

	revsBefore, _ := s.List("a.txt")
	preDeleteTs := revsBefore[0].Timestamp // the 'initial' rev timestamp

	// Remove the file from disk and Record the deletion.
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}

	revs, _ := s.List("a.txt")
	if len(revs) != 2 {
		t.Fatalf("want 2 revisions (initial + delete), got %d", len(revs))
	}
	if revs[0].Label != LabelDelete {
		t.Errorf("newest label = %q, want delete", revs[0].Label)
	}

	// No new object created by the delete.
	if got := accObjectCount(t, root); got != objsBefore {
		t.Errorf("delete created %d new object(s); objects went %d -> %d", got-objsBefore, objsBefore, got)
	}

	// Prior content is still retrievable via Get at the pre-delete timestamp.
	got, err := s.Get("a.txt", preDeleteTs)
	if err != nil {
		t.Fatalf("Get pre-delete content failed: %v", err)
	}
	if !bytes.Equal(got, []byte("alive content")) {
		t.Errorf("pre-delete Get = %q, want %q", got, "alive content")
	}

	// The delete revision references the last known content hash (not empty,
	// not a fresh object): it points back at the existing object.
	if revs[0].Hash != revs[1].Hash {
		t.Errorf("delete rev hash %q != last content hash %q", revs[0].Hash, revs[1].Hash)
	}
}

// -----------------------------------------------------------------------------
// A3.2 — reverting to an earlier content reuses the existing object
// -----------------------------------------------------------------------------

func TestAcc_A3_2_RevertReusesObject(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "X")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "a.txt", "Y")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	if got := accObjectCount(t, root); got != 2 {
		t.Fatalf("want 2 objects after X,Y, got %d", got)
	}

	// Revert to X.
	write(t, root, "a.txt", "X")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}

	revs, _ := s.List("a.txt") // newest first: X(modify), Y(modify), X(initial)
	if len(revs) != 3 {
		t.Fatalf("want 3 revisions, got %d", len(revs))
	}

	// No new object was created: still exactly 2 (X and Y).
	if got := accObjectCount(t, root); got != 2 {
		t.Errorf("revert to X created a new object; objects = %d, want 2", got)
	}

	// The newest (reverted) rev's hash equals the first (original X) rev's hash.
	newest := revs[0]
	oldest := revs[2]
	if newest.Hash != oldest.Hash {
		t.Errorf("reverted hash %q != original X hash %q", newest.Hash, oldest.Hash)
	}
	// And it differs from Y's hash.
	if newest.Hash == revs[1].Hash {
		t.Errorf("reverted hash equals Y hash %q; expected to point at X", revs[1].Hash)
	}

	// Sanity: the reverted content is byte-equal to X.
	got, err := s.Get("a.txt", newest.Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("X")) {
		t.Errorf("reverted Get = %q, want X", got)
	}
}

// -----------------------------------------------------------------------------
// A6.1 — history newest-first, strictly decreasing ts, non-empty hash + label
// -----------------------------------------------------------------------------

func TestAcc_A6_1_HistoryNewestFirst(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	contents := []string{"one", "two", "three"}
	for _, c := range contents {
		write(t, root, "a.txt", c)
		if err := s.Record("a.txt"); err != nil {
			t.Fatal(err)
		}
	}

	revs, err := s.List("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 3 {
		t.Fatalf("want 3 revisions, got %d", len(revs))
	}

	for i := 1; i < len(revs); i++ {
		if revs[i-1].Timestamp <= revs[i].Timestamp {
			t.Errorf("timestamps not strictly decreasing at %d: %d then %d",
				i, revs[i-1].Timestamp, revs[i].Timestamp)
		}
	}
	for i, r := range revs {
		if r.Hash == "" {
			t.Errorf("rev %d has empty hash", i)
		}
		if r.Label == "" {
			t.Errorf("rev %d has empty label", i)
		}
	}
	// Newest first means revs[0] is "three", revs[2] is "one".
	if revs[0].Label != LabelModify || revs[2].Label != LabelInitial {
		t.Errorf("labels = %q..%q, want modify..initial", revs[0].Label, revs[2].Label)
	}
}

// -----------------------------------------------------------------------------
// A7.4 — restore aborts if the pre-restore safeguard cannot be written; the
// working-tree file is left UNCHANGED. This is the critical safety property.
// -----------------------------------------------------------------------------

func TestAcc_A7_4_RestoreAbortsWhenSafeguardFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod-based permission denial is ineffective")
	}
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "good")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "a.txt", "BROKEN")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}

	revs, _ := s.List("a.txt") // newest first: BROKEN, good
	goodTs := revs[1].Timestamp

	// Make the pre-restore safeguard impossible: the .log file itself
	// read-only so appendLog's O_APPEND open fails. (A read-only directory
	// is not enough on macOS: appending to an existing file is governed by
	// the file's own permissions, not the dir's.)
	lp := filepath.Join(root, Dir, "index", "a.txt.log")
	if err := os.Chmod(lp, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(lp, 0o644) })

	// Record the current on-disk bytes before the attempted restore.
	beforeBytes, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, []byte("BROKEN")) {
		t.Fatalf("precondition: working tree = %q, want BROKEN", beforeBytes)
	}

	_, rErr := s.Restore("a.txt", goodTs)
	if rErr == nil {
		t.Fatalf("Restore should have failed because the safeguard could not be written")
	}

	// CRITICAL: the working-tree file must be UNCHANGED (still BROKEN).
	afterBytes, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterBytes, beforeBytes) {
		t.Errorf("BLOCKER: Restore overwrote the working tree despite safeguard failure: file = %q, want %q (unchanged)", afterBytes, beforeBytes)
	}
}

// -----------------------------------------------------------------------------
// A7.5 — restoring to the pre-delete revision brings the file back; restoring
// to the 'delete' revision removes the file (documented behavior).
// -----------------------------------------------------------------------------

func TestAcc_A7_5_RestoreThePreDeleteRevision(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "X content")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	revs, _ := s.List("a.txt")
	xTs := revs[0].Timestamp

	// Delete the file and record the deletion.
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	revs, _ = s.List("a.txt")
	if revs[0].Label != LabelDelete {
		t.Fatalf("precondition: newest label = %q, want delete", revs[0].Label)
	}
	// File is gone from disk now.
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("precondition: file should be absent, stat err = %v", err)
	}

	// Restore to the X revision (the one BEFORE the delete).
	if _, err := s.Restore("a.txt", xTs); err != nil {
		t.Fatalf("restore to pre-delete rev failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatalf("file should reappear after restore: %v", err)
	}
	if !bytes.Equal(got, []byte("X content")) {
		t.Errorf("restored content = %q, want %q", got, "X content")
	}
}

func TestAcc_A7_5_RestoreToDeleteRevisionRemovesFile(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "X content")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	revs, _ := s.List("a.txt") // newest first: delete, initial
	deleteTs := revs[0].Timestamp

	// Bring the file back so we can prove restoring-to-delete removes it.
	if _, err := s.Restore("a.txt", revs[1].Timestamp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("precondition: file should exist again, got %v", err)
	}

	// Restoring to the delete revision must remove the file from disk
	// (documented store.go behavior).
	if _, err := s.Restore("a.txt", deleteTs); err != nil {
		t.Fatalf("restore-to-delete failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Errorf("restore-to-delete did not remove the file; stat err = %v", err)
	}
}

// -----------------------------------------------------------------------------
// A9.3 — gc collects an object referenced only by evicted (old, non-newest)
// revs, while keeping objects still referenced — including each file's pinned
// newest revision, which age GC never evicts (README: "always keeps each file's
// latest revision").
// -----------------------------------------------------------------------------

func TestAcc_A9_3_GCKeepsStillReferencedObject(t *testing.T) {
	clock := fakeClock(t)
	root := t.TempDir()
	s := New(root)

	// Old content "shared" recorded into a.txt while the clock is "old". a.txt's
	// only rev is its newest, so it is pinned and survives regardless of age.
	write(t, root, "a.txt", "shared")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	sharedHash := sha256hex([]byte("shared"))

	// b.txt gets TWO old revs: "lonely" then "lonelyV2". After GC the older
	// "lonely" rev is evicted (non-newest, past cutoff) and its object — which
	// nothing else references — is collected; "lonelyV2" is b.txt's pinned newest.
	write(t, root, "b.txt", "lonely")
	if err := s.Record("b.txt"); err != nil {
		t.Fatal(err)
	}
	lonelyHash := sha256hex([]byte("lonely"))
	atomic.AddInt64(clock, 1000)
	write(t, root, "b.txt", "lonelyV2")
	if err := s.Record("b.txt"); err != nil {
		t.Fatal(err)
	}

	if !accObjectExists(t, root, sharedHash) || !accObjectExists(t, root, lonelyHash) {
		t.Fatalf("precondition: both objects should exist")
	}

	// Jump the clock far forward; record "shared" again into c.txt so the
	// 'shared' object is now ALSO referenced by a recent revision.
	atomic.StoreInt64(clock, 1_000_000+int64(48*time.Hour/time.Millisecond))
	write(t, root, "c.txt", "shared")
	if err := s.Record("c.txt"); err != nil {
		t.Fatal(err)
	}

	// GC with a 24h window: every revision recorded before the jump is past the
	// cutoff, but each file's newest is pinned. Only b.txt's older "lonely" rev
	// is actually evicted.
	if err := s.GC(24 * time.Hour); err != nil {
		t.Fatal(err)
	}

	// shared object survives (referenced by a.txt's pinned rev and c.txt's recent rev).
	if !accObjectExists(t, root, sharedHash) {
		t.Errorf("gc removed 'shared' object that is still referenced")
	}
	// lonely object is gone (its only referencing rev was evicted).
	if accObjectExists(t, root, lonelyHash) {
		t.Errorf("gc kept 'lonely' object that no surviving rev references")
	}

	// a.txt keeps its pinned newest (the latest-revision floor), content intact.
	aRevs, _ := s.List("a.txt")
	if len(aRevs) != 1 {
		t.Fatalf("a.txt should keep its pinned newest rev, got %d", len(aRevs))
	}
	if got, _ := s.Get("a.txt", aRevs[0].Timestamp); !bytes.Equal(got, []byte("shared")) {
		t.Errorf("a.txt surviving content = %q, want shared", got)
	}
	// b.txt keeps only its pinned newest "lonelyV2".
	bRevs, _ := s.List("b.txt")
	if len(bRevs) != 1 {
		t.Fatalf("b.txt should keep 1 (pinned newest) rev, got %d", len(bRevs))
	}
	if got, _ := s.Get("b.txt", bRevs[0].Timestamp); !bytes.Equal(got, []byte("lonelyV2")) {
		t.Errorf("b.txt surviving content = %q, want lonelyV2", got)
	}
	// c.txt's recent rev survives and its content is still retrievable.
	cRevs, _ := s.List("c.txt")
	if len(cRevs) != 1 {
		t.Fatalf("c.txt should have 1 surviving rev, got %d", len(cRevs))
	}
	got, err := s.Get("c.txt", cRevs[0].Timestamp)
	if err != nil {
		t.Fatalf("get surviving content failed: %v", err)
	}
	if !bytes.Equal(got, []byte("shared")) {
		t.Errorf("surviving content = %q, want shared", got)
	}
}

// -----------------------------------------------------------------------------
// B5.1 — binary content (nulls + full 0x00..0xFF) round-trips byte-identical.
// -----------------------------------------------------------------------------

func TestAcc_B5_1_BinaryContentByteIdentical(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	// 0x00..0xFF repeated a few times plus extra nulls.
	var buf []byte
	for rep := 0; rep < 4; rep++ {
		for b := 0; b < 256; b++ {
			buf = append(buf, byte(b))
		}
	}
	buf = append(buf, 0x00, 0x00, 0x00)
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), buf, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("bin.dat"); err != nil {
		t.Fatal(err)
	}

	revs, _ := s.List("bin.dat")
	if len(revs) != 1 {
		t.Fatalf("want 1 rev, got %d", len(revs))
	}
	got, err := s.Get("bin.dat", revs[0].Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, buf) {
		t.Errorf("binary Get differs from stored bytes (len got=%d want=%d)", len(got), len(buf))
	}

	// Restore round-trips byte-identical too. First clobber the file.
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte("clobbered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("bin.dat"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore("bin.dat", revs[0].Timestamp); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "bin.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, buf) {
		t.Errorf("restored binary differs from original (len got=%d want=%d)", len(onDisk), len(buf))
	}
}

// -----------------------------------------------------------------------------
// B5.2 — special / unicode relpaths (spaces, accents, emoji, subdirs).
// -----------------------------------------------------------------------------

func TestAcc_B5_2_SpecialUnicodeRelPaths(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	rels := []string{
		"a file with spaces.txt",
		"café.txt",
		"emoji_\U0001F600_\U0001F4A9.txt",
		filepath.Join("sub dir", "nested café", "ünïcödé.txt"),
		filepath.Join("deep", "deeper", "deepest", "résumé.md"),
	}

	for i, rel := range rels {
		content := []byte("content for #" + string(rune('A'+i)) + ": " + rel)
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), content, 0o644); err != nil {
			t.Fatalf("write %q: %v", rel, err)
		}
		if err := s.Record(rel); err != nil {
			t.Fatalf("record %q: %v", rel, err)
		}

		revs, err := s.List(rel)
		if err != nil {
			t.Fatalf("list %q: %v", rel, err)
		}
		if len(revs) != 1 {
			t.Fatalf("%q: want 1 rev, got %d", rel, len(revs))
		}

		got, err := s.Get(rel, revs[0].Timestamp)
		if err != nil {
			t.Fatalf("get %q: %v", rel, err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("%q: Get = %q, want %q", rel, got, content)
		}

		// path -> log mapping round-trips: the log file exists at the expected
		// location and Restore works.
		if err := os.WriteFile(filepath.Join(root, rel), []byte("broken"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := s.Record(rel); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Restore(rel, revs[0].Timestamp); err != nil {
			t.Fatalf("restore %q: %v", rel, err)
		}
		onDisk, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(onDisk, content) {
			t.Errorf("%q: restored = %q, want %q", rel, onDisk, content)
		}
	}
}

// -----------------------------------------------------------------------------
// B5.3 — empty (0-byte) file: hash of empty content, Get returns 0 bytes,
// Restore works.
// -----------------------------------------------------------------------------

func TestAcc_B5_3_EmptyFile(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	if err := os.WriteFile(filepath.Join(root, "empty.txt"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("empty.txt"); err != nil {
		t.Fatal(err)
	}

	revs, _ := s.List("empty.txt")
	if len(revs) != 1 {
		t.Fatalf("want 1 rev, got %d", len(revs))
	}
	// The recorded hash must be the sha256 of empty content.
	emptyHash := sha256hex([]byte{})
	if revs[0].Hash != emptyHash {
		t.Errorf("empty-file hash = %q, want %q", revs[0].Hash, emptyHash)
	}

	got, err := s.Get("empty.txt", revs[0].Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("Get returned %d bytes, want 0", len(got))
	}

	// Restore over a non-empty working tree yields a 0-byte file again.
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), []byte("not empty"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("empty.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore("empty.txt", revs[0].Timestamp); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(root, "empty.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Errorf("restored empty file size = %d, want 0", fi.Size())
	}
}

// -----------------------------------------------------------------------------
// B5.4 — CRLF / arbitrary bytes restore byte-identical (no newline normalize).
// -----------------------------------------------------------------------------

func TestAcc_B5_4_CRLFNoNormalization(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	// Mixed line endings + trailing CR + a lone CR + no final newline.
	content := []byte("line1\r\nline2\r\nmixed\nlone\rcr\r\n\r\ntail-no-newline")
	if err := os.WriteFile(filepath.Join(root, "crlf.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("crlf.txt"); err != nil {
		t.Fatal(err)
	}

	revs, _ := s.List("crlf.txt")
	got, err := s.Get("crlf.txt", revs[0].Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("Get normalized line endings: got %q want %q", got, content)
	}

	// Restore must also preserve bytes exactly.
	if err := os.WriteFile(filepath.Join(root, "crlf.txt"), []byte("LF only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("crlf.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore("crlf.txt", revs[0].Timestamp); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "crlf.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, content) {
		t.Errorf("Restore normalized line endings: got %q want %q", onDisk, content)
	}
}
