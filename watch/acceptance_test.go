package watch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lochis/ignore"
	"lochis/store"
)

// acceptStart wires up a real store + ignore matcher and starts the watcher in
// a goroutine with a small debounce. It returns the store and a stop func that
// closes the done channel and the watcher. Helpers in this file are prefixed
// "accept" to avoid colliding with watch_test.go / stress_test.go helpers.
func acceptStart(t *testing.T, root string, debounce time.Duration) (*store.FS, func()) {
	t.Helper()
	s := store.New(root)
	w, err := New(root, s, ignore.New(root))
	if err != nil {
		t.Fatal(err)
	}
	w.debounce = debounce
	done := make(chan struct{})
	go w.Run(done)
	stop := func() {
		close(done)
		w.Close()
	}
	return s, stop
}

// acceptRevCount returns the number of revisions recorded for rel.
func acceptRevCount(t *testing.T, s *store.FS, rel string) int {
	t.Helper()
	r, _ := s.List(rel)
	return len(r)
}

// A1.1 — three pre-existing files each get exactly one "initial" revision, and
// objects/ holds one object per distinct content.
func TestAcceptA1_1InitialRevisions(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("content-"+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s, stop := acceptStart(t, root, 80*time.Millisecond)
	defer stop()

	ok := waitFor(t, 2*time.Second, func() bool {
		return acceptRevCount(t, s, "a.txt") == 1 &&
			acceptRevCount(t, s, "b.txt") == 1 &&
			acceptRevCount(t, s, "c.txt") == 1
	})
	if !ok {
		t.Fatalf("initial scan did not record all three files exactly once: a=%d b=%d c=%d",
			acceptRevCount(t, s, "a.txt"), acceptRevCount(t, s, "b.txt"), acceptRevCount(t, s, "c.txt"))
	}

	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		revs, _ := s.List(f)
		if len(revs) != 1 {
			t.Fatalf("%s: want exactly 1 revision, got %d", f, len(revs))
		}
		if revs[0].Label != store.LabelInitial {
			t.Errorf("%s: label = %q, want %q", f, revs[0].Label, store.LabelInitial)
		}
	}

	// One object per distinct content (all three contents are distinct).
	objs, err := os.ReadDir(filepath.Join(root, store.Dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range objs {
		if !strings.HasPrefix(e.Name(), ".tmp-") {
			n++
		}
	}
	if n != 3 {
		t.Errorf("want 3 distinct objects, got %d", n)
	}
}

// A1.3 — start the watcher in an EMPTY dir; it must not fail. Then create a
// file: it gets a revision (initial or modify).
func TestAcceptA1_3EmptyProjectThenCreate(t *testing.T) {
	root := t.TempDir()

	s, stop := acceptStart(t, root, 80*time.Millisecond)
	defer stop()

	// Give the watcher a moment to finish the (empty) initial scan and register
	// the root directory before creating content.
	time.Sleep(150 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(root, "nuevo.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	ok := waitFor(t, 3*time.Second, func() bool {
		return acceptRevCount(t, s, "nuevo.txt") >= 1
	})
	if !ok {
		t.Fatal("file created in an initially-empty project was not recorded")
	}
	revs, _ := s.List("nuevo.txt")
	if revs[0].Label != store.LabelInitial && revs[0].Label != store.LabelModify {
		t.Errorf("label = %q, want initial or modify", revs[0].Label)
	}
	got, _ := s.Get("nuevo.txt", revs[0].Timestamp)
	if !bytes.Equal(got, []byte("hello")) {
		t.Errorf("content = %q, want hello", got)
	}
}

// A2.2 — a file created at runtime is captured and its object exists.
func TestAcceptA2_2NewFileCaptured(t *testing.T) {
	root := t.TempDir()

	s, stop := acceptStart(t, root, 80*time.Millisecond)
	defer stop()

	time.Sleep(150 * time.Millisecond)

	content := []byte("brand new content")
	if err := os.WriteFile(filepath.Join(root, "nuevo.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	ok := waitFor(t, 3*time.Second, func() bool {
		return acceptRevCount(t, s, "nuevo.txt") >= 1
	})
	if !ok {
		t.Fatal("runtime-created file was not recorded")
	}

	revs, _ := s.List("nuevo.txt")
	// The object referenced by the revision must exist on disk.
	objPath := filepath.Join(root, store.Dir, "objects", revs[0].Hash)
	if _, err := os.Stat(objPath); err != nil {
		t.Errorf("object for new file missing: %v", err)
	}
	got, _ := s.Get("nuevo.txt", revs[0].Timestamp)
	if !bytes.Equal(got, content) {
		t.Errorf("recorded content mismatch: got %q", got)
	}
}

// A2.3 — deleting a tracked file at runtime produces a "delete" revision.
func TestAcceptA2_3DeleteViaWatcher(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	if err := os.WriteFile(path, []byte("alive"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, stop := acceptStart(t, root, 80*time.Millisecond)
	defer stop()

	// Wait for the initial revision so the file is tracked.
	if !waitFor(t, 2*time.Second, func() bool { return acceptRevCount(t, s, "a.txt") == 1 }) {
		t.Fatal("initial revision never recorded")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	ok := waitFor(t, 3*time.Second, func() bool {
		revs, _ := s.List("a.txt")
		return len(revs) >= 2 && revs[0].Label == store.LabelDelete
	})
	if !ok {
		revs, _ := s.List("a.txt")
		var labels []string
		for _, r := range revs {
			labels = append(labels, string(r.Label))
		}
		t.Fatalf("delete via watcher not recorded; revisions=%v", labels)
	}
}

// A4.2 — two changes separated by more than the debounce window produce two
// distinct new revisions (not collapsed into one).
func TestAcceptA4_2SeparatedChangesTwoRevisions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	if err := os.WriteFile(path, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}

	debounce := 80 * time.Millisecond
	s, stop := acceptStart(t, root, debounce)
	defer stop()

	if !waitFor(t, 2*time.Second, func() bool { return acceptRevCount(t, s, "a.txt") == 1 }) {
		t.Fatal("initial revision never recorded")
	}

	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return acceptRevCount(t, s, "a.txt") == 2 }) {
		t.Fatalf("first modification not recorded; got %d revisions", acceptRevCount(t, s, "a.txt"))
	}

	// Wait clearly longer than the debounce window before the second change.
	time.Sleep(3 * debounce)

	if err := os.WriteFile(path, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return acceptRevCount(t, s, "a.txt") == 3 }) {
		t.Fatalf("second modification not recorded; got %d revisions", acceptRevCount(t, s, "a.txt"))
	}

	revs, _ := s.List("a.txt")
	if len(revs) != 3 {
		t.Fatalf("want exactly 3 revisions (initial + 2 modifies), got %d", len(revs))
	}
	// Newest first: v2, v1, v0.
	if v, _ := s.Get("a.txt", revs[0].Timestamp); !bytes.Equal(v, []byte("v2")) {
		t.Errorf("newest content = %q, want v2", v)
	}
	if v, _ := s.Get("a.txt", revs[1].Timestamp); !bytes.Equal(v, []byte("v1")) {
		t.Errorf("middle content = %q, want v1", v)
	}
}

// A4.3 — per-file debounce: near-simultaneous changes to a.txt, b.txt, c.txt
// each yield their own new revision (debounce is per-file, not global).
func TestAcceptA4_3PerFileDebounce(t *testing.T) {
	root := t.TempDir()
	files := []string{"a.txt", "b.txt", "c.txt"}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(root, f), []byte("v0"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s, stop := acceptStart(t, root, 80*time.Millisecond)
	defer stop()

	if !waitFor(t, 2*time.Second, func() bool {
		return acceptRevCount(t, s, "a.txt") == 1 &&
			acceptRevCount(t, s, "b.txt") == 1 &&
			acceptRevCount(t, s, "c.txt") == 1
	}) {
		t.Fatal("initial revisions never recorded for all three files")
	}

	// Near-simultaneous distinct modifications.
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(root, f), []byte("v1-"+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ok := waitFor(t, 3*time.Second, func() bool {
		return acceptRevCount(t, s, "a.txt") == 2 &&
			acceptRevCount(t, s, "b.txt") == 2 &&
			acceptRevCount(t, s, "c.txt") == 2
	})
	if !ok {
		t.Fatalf("per-file debounce failed: a=%d b=%d c=%d",
			acceptRevCount(t, s, "a.txt"), acceptRevCount(t, s, "b.txt"), acceptRevCount(t, s, "c.txt"))
	}

	for _, f := range files {
		revs, _ := s.List(f)
		if v, _ := s.Get(f, revs[0].Timestamp); !bytes.Equal(v, []byte("v1-"+f)) {
			t.Errorf("%s newest content = %q, want %q", f, v, "v1-"+f)
		}
	}
}

// A5.1 — a .gitignore'd file is never recorded, while a sibling tracked file IS
// (proving the watcher is actually live).
func TestAcceptA5_1GitignoreRespected(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("secret.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, stop := acceptStart(t, root, 80*time.Millisecond)
	defer stop()

	time.Sleep(150 * time.Millisecond)

	// Create the ignored file and a tracked sibling.
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("s0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "public.txt"), []byte("p0"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The tracked sibling proves the watcher is live.
	if !waitFor(t, 3*time.Second, func() bool { return acceptRevCount(t, s, "public.txt") >= 1 }) {
		t.Fatal("sibling tracked file was not recorded (watcher not live?)")
	}

	// Modify the secret a few times.
	for _, v := range []string{"s1", "s2", "s3"} {
		if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	// Allow plenty of time for any (erroneous) recording to flush.
	time.Sleep(400 * time.Millisecond)

	if n := acceptRevCount(t, s, "secret.txt"); n != 0 {
		t.Errorf("ignored secret.txt got %d revisions, want 0", n)
	}
}

// A5.3 — the watcher never records anything under .lochis/ (no auto-capture
// feedback loop). This is critical.
func TestAcceptA5_3NoSelfCaptureLoop(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, stop := acceptStart(t, root, 80*time.Millisecond)
	defer stop()

	if !waitFor(t, 2*time.Second, func() bool { return acceptRevCount(t, s, "a.txt") == 1 }) {
		t.Fatal("initial revision never recorded")
	}

	// Generate normal activity so the store writes into .lochis/ repeatedly.
	for i, v := range []string{"v1", "v2", "v3", "v4", "v5"} {
		if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = i
		time.Sleep(120 * time.Millisecond) // separated > debounce so each flushes a write to .lochis/
	}
	// Let everything settle, including any spurious feedback events.
	time.Sleep(500 * time.Millisecond)

	// 1. A specific known .lochis path must have no history.
	selfPath := filepath.Join(store.Dir, "index", "a.txt.log")
	if n := acceptRevCount(t, s, selfPath); n != 0 {
		t.Errorf("watcher recorded its own log file (%s): %d revisions", selfPath, n)
	}

	// 2. Direct check: no .log exists whose relpath starts with ".lochis".
	var offending []string
	_ = filepath.Walk(filepath.Join(root, store.Dir, "index"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".log") {
			return nil
		}
		rel, _ := filepath.Rel(filepath.Join(root, store.Dir, "index"), p)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, store.Dir+"/") || rel == store.Dir {
			offending = append(offending, rel)
		}
		return nil
	})
	if len(offending) != 0 {
		t.Errorf("watcher created history under %s/: %v", store.Dir, offending)
	}
}

// A5.4 — no .gitignore present: the watcher starts fine and default excludes
// still apply (node_modules ignored).
func TestAcceptA5_4NoGitignoreDefaultsApply(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "dep.js"), []byte("dep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No .gitignore is created.
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("test setup: .gitignore should not exist")
	}

	s, stop := acceptStart(t, root, 80*time.Millisecond)
	defer stop()

	// Watcher must start and record the tracked file without erroring.
	if !waitFor(t, 2*time.Second, func() bool { return acceptRevCount(t, s, "keep.txt") == 1 }) {
		t.Fatal("watcher did not start / record keep.txt without a .gitignore")
	}

	// Modify the node_modules file to ensure runtime events are also ignored.
	if err := os.WriteFile(filepath.Join(root, "node_modules", "dep.js"), []byte("dep2"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	if n := acceptRevCount(t, s, filepath.Join("node_modules", "dep.js")); n != 0 {
		t.Errorf("node_modules file recorded (%d revisions) despite default excludes", n)
	}
}
