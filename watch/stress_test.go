package watch

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"lochis/ignore"
	"lochis/store"
)

// stressStart wires a real store + ignore matcher and starts the watcher in a
// goroutine with a small debounce. Returns the store and a stop func. Helpers
// here are prefixed "stress" to avoid collisions with other _test.go files.
func stressStart(t *testing.T, root string, debounce time.Duration) (*store.FS, func()) {
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

func stressRevCount(s *store.FS, rel string) int {
	r, _ := s.List(rel)
	return len(r)
}

// B2.1 — mass refactor (scaled). Create ~300 files, let the initial scan
// settle, then rewrite all of them quickly. After settling, every file must end
// with a revision of its FINAL content; none lost, none with more than
// initial+modify.
func TestStressB2_1MassRefactor(t *testing.T) {
	n := 300
	if testing.Short() {
		n = 60
	}
	root := t.TempDir()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f%04d.txt", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte("orig-"+strconv.Itoa(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Generous debounce window so a single rewrite of each file collapses
	// cleanly, but small enough to keep the test fast.
	s, stop := stressStart(t, root, 120*time.Millisecond)
	defer stop()

	// Wait for the initial scan to record every file exactly once.
	if !waitFor(t, 15*time.Second, func() bool {
		for i := 0; i < n; i++ {
			if stressRevCount(s, fmt.Sprintf("f%04d.txt", i)) < 1 {
				return false
			}
		}
		return true
	}) {
		t.Fatal("initial scan did not record all files")
	}

	// Rewrite all files quickly with new, distinct final content.
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f%04d.txt", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte("final-"+strconv.Itoa(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// After settling, each file should have exactly 2 revisions (initial +
	// modify) and the newest must hold the final content.
	ok := waitFor(t, 20*time.Second, func() bool {
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("f%04d.txt", i)
			revs, _ := s.List(name)
			if len(revs) < 2 {
				return false
			}
			got, _ := s.Get(name, revs[0].Timestamp)
			if !bytes.Equal(got, []byte("final-"+strconv.Itoa(i))) {
				return false
			}
		}
		return true
	})
	if !ok {
		// Report which files are wrong for diagnosis.
		bad := 0
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("f%04d.txt", i)
			revs, _ := s.List(name)
			got, _ := s.Get(name, revs[0].Timestamp)
			if !bytes.Equal(got, []byte("final-"+strconv.Itoa(i))) {
				bad++
			}
		}
		t.Fatalf("mass refactor: %d/%d files did not end with final content", bad, n)
	}

	// None lost, none duplicated beyond initial+modify.
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f%04d.txt", i)
		revs, _ := s.List(name)
		if len(revs) != 2 {
			t.Errorf("%s: want exactly 2 revisions (initial+modify), got %d", name, len(revs))
		}
		if len(revs) >= 2 {
			if revs[len(revs)-1].Label != store.LabelInitial {
				t.Errorf("%s: oldest label = %q, want initial", name, revs[len(revs)-1].Label)
			}
		}
	}
}

// B2.2 — sustained high-frequency writes to ONE file for ~1s with distinct
// contents faster than the debounce window. Revision count must stay small
// (bounded, not one-per-write), and the final content must be captured.
func TestStressB2_2SustainedHighFrequencyWrites(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "hot.txt")
	if err := os.WriteFile(path, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}

	debounce := 100 * time.Millisecond
	s, stop := stressStart(t, root, debounce)
	defer stop()

	if !waitFor(t, 2*time.Second, func() bool { return stressRevCount(s, "hot.txt") == 1 }) {
		t.Fatal("initial revision never recorded")
	}

	// Hammer the file for ~1s, every ~10ms, with distinct contents.
	deadline := time.Now().Add(1 * time.Second)
	i := 0
	var last string
	for time.Now().Before(deadline) {
		last = "w" + strconv.Itoa(i)
		if err := os.WriteFile(path, []byte(last), 0o644); err != nil {
			t.Fatal(err)
		}
		i++
		time.Sleep(10 * time.Millisecond)
	}
	// Let the final state flush.
	time.Sleep(5 * debounce)

	revs, _ := s.List("hot.txt")
	// ~1s of writes at one debounce of 100ms can flush at most a handful of
	// revisions; without debounce we'd see ~100. Bound generously but well
	// below one-per-write.
	if len(revs) > 20 {
		t.Errorf("debounce did not bound revisions: got %d for ~%d writes", len(revs), i)
	}
	if len(revs) < 2 {
		t.Errorf("expected at least the initial + one captured change, got %d", len(revs))
	}

	// Final content captured. The watcher may flush the final write slightly
	// after our last write; allow it to settle and assert byte-exact.
	ok := waitFor(t, 2*time.Second, func() bool {
		r, _ := s.List("hot.txt")
		got, _ := s.Get("hot.txt", r[0].Timestamp)
		return bytes.Equal(got, []byte(last))
	})
	if !ok {
		r, _ := s.List("hot.txt")
		got, _ := s.Get("hot.txt", r[0].Timestamp)
		t.Errorf("final content not captured: got %q, want %q", got, last)
	}
}

// B3.1 — rename a.txt -> b.txt. Documented coherent behavior: a.txt gets a
// 'delete' revision, b.txt gets a new revision, and a.txt's prior history stays
// intact and recoverable.
func TestStressB3_1Rename(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "a.txt")
	dst := filepath.Join(root, "b.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, stop := stressStart(t, root, 80*time.Millisecond)
	defer stop()

	if !waitFor(t, 2*time.Second, func() bool { return stressRevCount(s, "a.txt") == 1 }) {
		t.Fatal("initial revision of a.txt never recorded")
	}
	aInitialTs := func() int64 {
		revs, _ := s.List("a.txt")
		return revs[0].Timestamp
	}()

	if err := os.Rename(src, dst); err != nil {
		t.Fatal(err)
	}

	// a.txt should pick up a 'delete' revision.
	gotDelete := waitFor(t, 3*time.Second, func() bool {
		revs, _ := s.List("a.txt")
		return len(revs) >= 2 && revs[0].Label == store.LabelDelete
	})
	// b.txt should pick up a fresh revision of the moved content.
	gotNew := waitFor(t, 3*time.Second, func() bool {
		return stressRevCount(s, "b.txt") >= 1
	})

	if !gotNew {
		t.Errorf("rename target b.txt was not captured (watcher missed the rename)")
	} else {
		revs, _ := s.List("b.txt")
		got, _ := s.Get("b.txt", revs[0].Timestamp)
		if !bytes.Equal(got, []byte("payload")) {
			t.Errorf("b.txt content = %q, want payload", got)
		}
	}

	if !gotDelete {
		revs, _ := s.List("a.txt")
		var labels []string
		for _, r := range revs {
			labels = append(labels, string(r.Label))
		}
		t.Errorf("rename source a.txt did not get a delete revision; labels=%v", labels)
	}

	// a.txt's prior history must still be intact and recoverable byte-for-byte.
	old, err := s.Get("a.txt", aInitialTs)
	if err != nil {
		t.Fatalf("a.txt prior revision unreadable after rename: %v", err)
	}
	if !bytes.Equal(old, []byte("payload")) {
		t.Errorf("a.txt prior content = %q, want payload", old)
	}
}

// B3.3 — atomic-write-via-rename (editor pattern): write to a temp file then
// os.Rename it over a.txt. A revision of the FINAL content must be captured.
func TestStressB3_3AtomicWriteViaRename(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, stop := stressStart(t, root, 80*time.Millisecond)
	defer stop()

	if !waitFor(t, 2*time.Second, func() bool { return stressRevCount(s, "a.txt") == 1 }) {
		t.Fatal("initial revision never recorded")
	}

	// Editor-style atomic save: write a temp file, then rename over the target.
	tmp := filepath.Join(root, ".a.txt.swp")
	final := []byte("edited via atomic rename")
	if err := os.WriteFile(tmp, final, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, target); err != nil {
		t.Fatal(err)
	}

	ok := waitFor(t, 3*time.Second, func() bool {
		revs, _ := s.List("a.txt")
		if len(revs) < 2 {
			return false
		}
		got, _ := s.Get("a.txt", revs[0].Timestamp)
		return bytes.Equal(got, final)
	})
	if !ok {
		revs, _ := s.List("a.txt")
		var got []byte
		if len(revs) > 0 {
			got, _ = s.Get("a.txt", revs[0].Timestamp)
		}
		t.Fatalf("atomic-write-via-rename: final content not captured (revs=%d, newest=%q)", len(revs), got)
	}
}

// B3.3 (long-lived editor temp): a swap/autosave file that OUTLIVES the debounce
// window (vim keeps .swp open for the whole session, emacs writes #file#) must
// not get its own spurious history, while the real file's edits are still
// captured. Without the EditorTemp ignores this produced an initial+delete pair
// for the swap file on every edit.
func TestStressB3_3LongLivedSwapNoSpuriousHistory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}

	debounce := 80 * time.Millisecond
	s, stop := stressStart(t, root, debounce)
	defer stop()
	if !waitFor(t, 2*time.Second, func() bool { return stressRevCount(s, "a.txt") == 1 }) {
		t.Fatal("initial revision never recorded")
	}

	// Long-lived editor temps, each written and left sitting well past the
	// debounce so they WOULD be captured if not ignored.
	swaps := []string{".a.txt.swp", "a.txt.swp", "#a.txt#", ".#a.txt"}
	for _, name := range swaps {
		if err := os.WriteFile(filepath.Join(root, name), []byte("swap-"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(6 * debounce) // outlive the window; an unignored temp would record now

	// Meanwhile the real file is edited (in place) and must be captured.
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("v1-real"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		revs, _ := s.List("a.txt")
		if len(revs) < 2 {
			return false
		}
		got, _ := s.Get("a.txt", revs[0].Timestamp)
		return bytes.Equal(got, []byte("v1-real"))
	}) {
		t.Fatal("real edit to a.txt was not captured")
	}

	// No swap file produced any history.
	for _, name := range swaps {
		if n := stressRevCount(s, name); n != 0 {
			t.Errorf("editor temp %q produced %d spurious revision(s); it must be ignored", name, n)
		}
	}
}

// B3.4 — symlinks: a symlink inside the project pointing OUTSIDE the root must
// not cause a loop and must not record content from outside the root. Assert
// the documented behavior; report a deviation if the watcher follows the link
// out of root.
func TestStressB3_4Symlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // separate dir, outside the watched root

	// A real file inside the root (control: watcher is live).
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Secret content living outside the root.
	outsideFile := filepath.Join(outside, "secret-outside.txt")
	if err := os.WriteFile(outsideFile, []byte("OUTSIDE-SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A symlink inside the project pointing to the outside file.
	linkToOutside := filepath.Join(root, "link_out.txt")
	if err := os.Symlink(outsideFile, linkToOutside); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	// A symlink inside the project pointing to a sibling inside the project.
	linkInside := filepath.Join(root, "link_in.txt")
	if err := os.Symlink(filepath.Join(root, "real.txt"), linkInside); err != nil {
		t.Fatal(err)
	}

	s, stop := stressStart(t, root, 80*time.Millisecond)
	defer stop()

	// The real file must be captured (proves the watcher is live).
	if !waitFor(t, 3*time.Second, func() bool { return stressRevCount(s, "real.txt") >= 1 }) {
		t.Fatal("real.txt was not captured (watcher not live?)")
	}

	// Give time for any symlink handling / potential loop to manifest.
	time.Sleep(500 * time.Millisecond)

	// Documented behavior (B3.4): the watcher NEVER follows a symlink. Neither
	// the link pointing outside the root nor the one pointing to an in-root
	// sibling may produce history — so a symlink can never leak the content of a
	// path outside the project, and there is no loop. (The in-root *target* of a
	// link is versioned under its own real path, so nothing is lost.)
	if n := stressRevCount(s, "link_out.txt"); n != 0 {
		revs, _ := s.List("link_out.txt")
		got, _ := s.Get("link_out.txt", revs[0].Timestamp)
		t.Errorf("symlink to outside the root was recorded (%d revs, content %q); "+
			"the watcher must not follow symlinks", n, got)
	}
	if n := stressRevCount(s, "link_in.txt"); n != 0 {
		t.Errorf("in-root symlink was recorded (%d revs); the watcher must not follow symlinks", n)
	}

	// Critical safety properties: no loop (the watcher is still responsive) and
	// the store never created any entry whose relpath escapes the root.
	// Verify responsiveness by recording a brand-new in-root file.
	if err := os.WriteFile(filepath.Join(root, "after.txt"), []byte("still alive"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 3*time.Second, func() bool { return stressRevCount(s, "after.txt") >= 1 }) {
		t.Fatal("watcher stopped responding after encountering symlinks (possible loop)")
	}

	// No log file may correspond to a path that escapes the root.
	idx := filepath.Join(root, store.Dir, "index")
	var escaping []string
	_ = filepath.Walk(idx, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || filepath.Ext(p) != ".log" {
			return nil
		}
		rel, _ := filepath.Rel(idx, p)
		rel = filepath.ToSlash(rel)
		if rel == ".." || len(rel) >= 3 && rel[:3] == "../" {
			escaping = append(escaping, rel)
		}
		return nil
	})
	if len(escaping) != 0 {
		t.Errorf("store created history escaping the root: %v", escaping)
	}
}

// B3.5 — unreadable file (chmod 0) changes: the watcher must handle/log it and
// KEEP RUNNING so other files still get recorded afterward. Skipped under root.
func TestStressB3_5UnreadableFileKeepsWatcherAlive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0 would still be readable")
	}
	root := t.TempDir()
	bad := filepath.Join(root, "bad.txt")
	if err := os.WriteFile(bad, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, stop := stressStart(t, root, 80*time.Millisecond)
	defer stop()

	if !waitFor(t, 2*time.Second, func() bool { return stressRevCount(s, "bad.txt") == 1 }) {
		t.Fatal("initial revision of bad.txt never recorded")
	}

	// Make the file unreadable, then change it (touch its mtime/size via a
	// privileged-free path: we can still chmod our own file and rewrite is not
	// possible after chmod 0 without reopening; instead trigger a change event
	// by chmod itself, which fsnotify reports as a Chmod event).
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(bad, 0o644) // restore so TempDir cleanup works
	t.Cleanup(func() { os.Chmod(bad, 0o644) })

	// The chmod itself is a filesystem event; the watcher will try to Record
	// and hit a permission error reading the now-unreadable file. It must not
	// crash. Give it a moment to process.
	time.Sleep(300 * time.Millisecond)

	// Now prove the watcher is still running: a different file gets recorded.
	good := filepath.Join(root, "good.txt")
	if err := os.WriteFile(good, []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 3*time.Second, func() bool { return stressRevCount(s, "good.txt") >= 1 }) {
		t.Fatal("watcher died after an unreadable file: a subsequent file was not recorded")
	}
	revs, _ := s.List("good.txt")
	got, _ := s.Get("good.txt", revs[0].Timestamp)
	if !bytes.Equal(got, []byte("fresh")) {
		t.Errorf("good.txt content = %q, want fresh", got)
	}
}

// B6.3 — watcher restart must not duplicate 'initial' revisions for unchanged
// files. Populate .lochis/ with a first run, stop it, then start a SECOND
// watcher with the files UNCHANGED: no new revisions may be appended. Critical.
func TestStressB6_3RestartNoDuplicate(t *testing.T) {
	root := t.TempDir()
	files := []string{"a.txt", "b.txt", "c.txt"}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(root, f), []byte("stable-"+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// First watcher run: populate .lochis/.
	s1, stop1 := stressStart(t, root, 80*time.Millisecond)
	if !waitFor(t, 3*time.Second, func() bool {
		for _, f := range files {
			if stressRevCount(s1, f) != 1 {
				return false
			}
		}
		return true
	}) {
		stop1()
		t.Fatal("first watcher did not record all files exactly once")
	}
	stop1()

	// Snapshot revision counts and the raw .log contents.
	before := map[string]int{}
	beforeBytes := map[string][]byte{}
	for _, f := range files {
		before[f] = stressRevCount(s1, f)
		b, _ := os.ReadFile(filepath.Join(root, store.Dir, "index", f+".log"))
		beforeBytes[f] = append([]byte(nil), b...)
	}

	// Second watcher run, files UNCHANGED on disk.
	s2, stop2 := stressStart(t, root, 80*time.Millisecond)
	defer stop2()

	// Give the second watcher ample time to perform its initial scan and any
	// debounced flushes.
	time.Sleep(700 * time.Millisecond)

	for _, f := range files {
		after := stressRevCount(s2, f)
		if after != before[f] {
			t.Errorf("%s: restart duplicated revisions: had %d, now %d", f, before[f], after)
		}
		afterB, _ := os.ReadFile(filepath.Join(root, store.Dir, "index", f+".log"))
		if !bytes.Equal(afterB, beforeBytes[f]) {
			t.Errorf("%s: .log changed across an unchanged-file restart\nbefore=%q\nafter =%q",
				f, beforeBytes[f], afterB)
		}
		// Each file must still have exactly its single 'initial' revision.
		revs, _ := s2.List(f)
		if len(revs) != 1 || revs[0].Label != store.LabelInitial {
			var labels []string
			for _, r := range revs {
				labels = append(labels, string(r.Label))
			}
			t.Errorf("%s: after restart want [initial], got %v", f, labels)
		}
	}
}

// -----------------------------------------------------------------------------
// B1.1 — many files. The initial scan must record every file exactly once and
// complete in bounded time without exhausting memory. Scaled down from the
// spec's 50,000 (impractical as a unit test); the same code paths are exercised.
// -----------------------------------------------------------------------------

func TestStressB1_1ManyFiles(t *testing.T) {
	n := 2000
	if testing.Short() {
		n = 200
	}
	root := t.TempDir()
	for i := 0; i < n; i++ {
		p := filepath.Join(root, fmt.Sprintf("f%05d.txt", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("content-%d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s, stop := stressStart(t, root, 80*time.Millisecond)
	defer stop()

	last := fmt.Sprintf("f%05d.txt", n-1)
	if !waitFor(t, 30*time.Second, func() bool {
		return stressRevCount(s, "f00000.txt") >= 1 && stressRevCount(s, last) >= 1
	}) {
		t.Fatalf("initial scan did not complete for %d files", n)
	}
	// No duplication: each sampled file has exactly one revision.
	for _, i := range []int{0, n / 2, n - 1} {
		rel := fmt.Sprintf("f%05d.txt", i)
		if c := stressRevCount(s, rel); c != 1 {
			t.Errorf("%s has %d revisions, want exactly 1", rel, c)
		}
	}
}

// -----------------------------------------------------------------------------
// B2.3 — large file. v1 captures by reading the whole file into memory (no
// streaming hash, no size limit). This proves a multi-MB binary change is
// captured byte-exact. The documented v1 limitation: very large files (hundreds
// of MB) are read whole into RAM per capture — see README "Scope / limits".
// -----------------------------------------------------------------------------

func TestStressB2_3LargeFile(t *testing.T) {
	sizeMB := 8
	if testing.Short() {
		sizeMB = 1
	}
	root := t.TempDir()
	big := bytes.Repeat([]byte("x"), sizeMB*1024*1024)
	for i := range big { // sprinkle binary variation so it is not all-identical
		if i%7 == 0 {
			big[i] = byte(i)
		}
	}
	p := filepath.Join(root, "big.bin")
	if err := os.WriteFile(p, big[:1024], 0o644); err != nil { // small first
		t.Fatal(err)
	}

	s, stop := stressStart(t, root, 80*time.Millisecond)
	defer stop()
	waitFor(t, 3*time.Second, func() bool { return stressRevCount(s, "big.bin") >= 1 })

	if err := os.WriteFile(p, big, 0o644); err != nil { // now the full large content
		t.Fatal(err)
	}
	if !waitFor(t, 15*time.Second, func() bool { return stressRevCount(s, "big.bin") >= 2 }) {
		t.Fatal("large file change was not captured")
	}
	revs, _ := s.List("big.bin")
	got, err := s.Get("big.bin", revs[0].Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, big) {
		t.Errorf("large file not byte-exact: got %d bytes, want %d", len(got), len(big))
	}
}

// -----------------------------------------------------------------------------
// B3.2 — move/rename of a whole directory. The store must not corrupt: files at
// the new path keep being captured, and the old path's history stays intact and
// recoverable.
// -----------------------------------------------------------------------------

func TestStressB3_2DirectoryMove(t *testing.T) {
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "pkg"), 0o755))
	must(os.WriteFile(filepath.Join(root, "pkg", "a.txt"), []byte("A1"), 0o644))
	must(os.WriteFile(filepath.Join(root, "pkg", "b.txt"), []byte("B1"), 0o644))

	s, stop := stressStart(t, root, 80*time.Millisecond)
	defer stop()

	relA := filepath.Join("pkg", "a.txt")
	if !waitFor(t, 3*time.Second, func() bool {
		return stressRevCount(s, relA) >= 1 && stressRevCount(s, filepath.Join("pkg", "b.txt")) >= 1
	}) {
		t.Fatal("initial scan did not record pkg/ files")
	}

	// Move the whole directory, then change a file in the new location.
	must(os.Rename(filepath.Join(root, "pkg"), filepath.Join(root, "pkg2")))
	must(os.WriteFile(filepath.Join(root, "pkg2", "a.txt"), []byte("A2-moved"), 0o644))

	relA2 := filepath.Join("pkg2", "a.txt")
	if !waitFor(t, 5*time.Second, func() bool { return stressRevCount(s, relA2) >= 1 }) {
		t.Fatal("file under the moved directory was not captured at the new path")
	}

	// Old path history intact and recoverable (not corrupted by the move).
	oldRevs, err := s.List(relA)
	if err != nil || len(oldRevs) == 0 {
		t.Fatalf("old path history lost after directory move: %v", err)
	}
	oldGot, err := s.Get(relA, oldRevs[len(oldRevs)-1].Timestamp)
	if err != nil || string(oldGot) != "A1" {
		t.Errorf("old path content not recoverable: %q (%v)", oldGot, err)
	}
	// New path captures the final content byte-exact.
	newRevs, _ := s.List(relA2)
	newGot, _ := s.Get(relA2, newRevs[0].Timestamp)
	if string(newGot) != "A2-moved" {
		t.Errorf("new path newest content = %q, want A2-moved", newGot)
	}
}

// -----------------------------------------------------------------------------
// B4.1 — watcher capturing one file while an MCP client restores a DIFFERENT
// file at the same time. The MCP restore path is exactly store.Restore (what
// mcp.Backend.Restore calls), so driving the same store backend reproduces the
// concurrency. No corruption: every log line parses and timestamps stay
// strictly monotonic. Run under -race to catch data races.
// -----------------------------------------------------------------------------

func TestStressB4_1WatcherAndConcurrentRestore(t *testing.T) {
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(root, "A.txt"), []byte("a0"), 0o644))
	must(os.WriteFile(filepath.Join(root, "B.txt"), []byte("b-good"), 0o644))

	s, stop := stressStart(t, root, 60*time.Millisecond)
	defer stop()
	if !waitFor(t, 3*time.Second, func() bool {
		return stressRevCount(s, "A.txt") >= 1 && stressRevCount(s, "B.txt") >= 1
	}) {
		t.Fatal("initial scan did not record A and B")
	}

	bRevs, _ := s.List("B.txt")
	goodB := bRevs[0].Timestamp
	must(os.WriteFile(filepath.Join(root, "B.txt"), []byte("b-broken"), 0o644))
	waitFor(t, 3*time.Second, func() bool { return stressRevCount(s, "B.txt") >= 2 })

	// Watcher keeps recording A (write loop) while we restore B concurrently.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			os.WriteFile(filepath.Join(root, "A.txt"), []byte(fmt.Sprintf("a%d", i)), 0o644)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if _, err := s.Restore("B.txt", goodB); err != nil {
		t.Fatalf("concurrent restore of B failed: %v", err)
	}
	wg.Wait()

	if got, _ := os.ReadFile(filepath.Join(root, "B.txt")); string(got) != "b-good" {
		t.Errorf("B after restore = %q, want b-good", got)
	}
	// No interleaved/corrupt lines in either log; List is strictly decreasing.
	for _, rel := range []string{"A.txt", "B.txt"} {
		revs, err := s.List(rel)
		if err != nil {
			t.Fatalf("List(%s): %v", rel, err)
		}
		for i := 1; i < len(revs); i++ {
			if revs[i-1].Timestamp <= revs[i].Timestamp {
				t.Errorf("%s timestamps not strictly decreasing (store corruption): %+v", rel, revs)
				break
			}
		}
	}
}
