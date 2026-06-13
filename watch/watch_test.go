package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"lochis/ignore"
	"lochis/store"
)

// waitFor polls cond until true or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func TestInitialScanRecords(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi"), 0o644)
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("yo"), 0o644)

	s := store.New(root)
	w, err := New(root, s, ignore.New(root))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	done := make(chan struct{})
	go w.Run(done)
	defer close(done)

	ok := waitFor(t, 2*time.Second, func() bool {
		ra, _ := s.List("a.txt")
		rb, _ := s.List(filepath.Join("sub", "b.txt"))
		return len(ra) == 1 && len(rb) == 1
	})
	if !ok {
		t.Fatal("initial scan did not record both files")
	}
}

func TestDebounceCollapsesBurst(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	os.WriteFile(path, []byte("v0"), 0o644)

	s := store.New(root)
	w, err := New(root, s, ignore.New(root))
	if err != nil {
		t.Fatal(err)
	}
	w.debounce = 200 * time.Millisecond
	defer w.Close()

	done := make(chan struct{})
	go w.Run(done)
	defer close(done)

	// Wait for the initial revision.
	waitFor(t, 2*time.Second, func() bool {
		r, _ := s.List("a.txt")
		return len(r) == 1
	})

	// Rapid burst of distinct writes, faster than the debounce window.
	for _, v := range []string{"v1", "v2", "v3", "v4"} {
		os.WriteFile(path, []byte(v), 0o644)
		time.Sleep(20 * time.Millisecond)
	}

	// After settling, the burst collapses to exactly one new revision
	// holding the final content.
	ok := waitFor(t, 2*time.Second, func() bool {
		r, _ := s.List("a.txt")
		return len(r) == 2
	})
	if !ok {
		r, _ := s.List("a.txt")
		t.Fatalf("debounce: want 2 revisions after burst, got %d", len(r))
	}
	revs, _ := s.List("a.txt")
	got, _ := s.Get("a.txt", revs[0].Timestamp)
	if string(got) != "v4" {
		t.Errorf("final content = %q, want v4", got)
	}
}

func TestIgnoredFilesNotRecorded(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "node_modules"), 0o755)
	os.WriteFile(filepath.Join(root, "node_modules", "x.js"), []byte("dep"), 0o644)
	os.WriteFile(filepath.Join(root, "keep.txt"), []byte("k"), 0o644)

	s := store.New(root)
	w, err := New(root, s, ignore.New(root))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	done := make(chan struct{})
	go w.Run(done)
	defer close(done)

	waitFor(t, 2*time.Second, func() bool {
		r, _ := s.List("keep.txt")
		return len(r) == 1
	})

	r, _ := s.List(filepath.Join("node_modules", "x.js"))
	if len(r) != 0 {
		t.Errorf("node_modules file was recorded (%d revisions), should be ignored", len(r))
	}
}

func TestNewDirectoryWatchedAtRuntime(t *testing.T) {
	root := t.TempDir()
	s := store.New(root)
	w, err := New(root, s, ignore.New(root))
	if err != nil {
		t.Fatal(err)
	}
	w.debounce = 200 * time.Millisecond
	defer w.Close()

	done := make(chan struct{})
	go w.Run(done)
	defer close(done)

	// Give the watcher a moment to start before creating new content.
	time.Sleep(150 * time.Millisecond)

	os.MkdirAll(filepath.Join(root, "newdir"), 0o755)
	os.WriteFile(filepath.Join(root, "newdir", "c.txt"), []byte("fresh"), 0o644)

	ok := waitFor(t, 3*time.Second, func() bool {
		r, _ := s.List(filepath.Join("newdir", "c.txt"))
		return len(r) >= 1
	})
	if !ok {
		t.Fatal("file in a directory created at runtime was not recorded")
	}
}
