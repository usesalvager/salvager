package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// unsafePaths is the set of caller-supplied paths that must never reach disk:
// parent-escapes, inner escapes that climb out, absolute paths, and the empty
// string (a missing/zero-value "file" argument is unsafe, not a default).
var unsafePaths = []string{
	"../escape.txt",
	"../../etc/passwd",
	"a/../../escape.txt",
	"/etc/passwd",
	"",
}

// The store is a project's safety net: it must refuse to read, write, or delete
// outside the watched tree. List rejects every escaping path with ErrUnsafePath
// and performs no read.
func TestStore_List_RejectsTraversal(t *testing.T) {
	s := New(t.TempDir())
	for _, p := range unsafePaths {
		revs, err := s.List(p)
		if !errors.Is(err, ErrUnsafePath) {
			t.Errorf("List(%q) err = %v, want ErrUnsafePath", p, err)
		}
		if revs != nil {
			t.Errorf("List(%q) returned %d revisions, want none", p, len(revs))
		}
	}
}

// Get refuses to read content for an escaping path — it must not be usable to
// exfiltrate a file outside the project root.
func TestStore_Get_RejectsTraversal(t *testing.T) {
	s := New(t.TempDir())
	for _, p := range unsafePaths {
		content, err := s.Get(p, 1)
		if !errors.Is(err, ErrUnsafePath) {
			t.Errorf("Get(%q) err = %v, want ErrUnsafePath", p, err)
		}
		if content != nil {
			t.Errorf("Get(%q) returned content, want none", p)
		}
	}
}

// Restore refuses an escaping path before any effect. Guarantee under test: a
// traversal path can neither overwrite nor delete a host file, AND the
// pre-restore safeguard never runs (which would otherwise pull a foreign file's
// bytes into objects/). We assert the absence of effect, not just the error.
func TestStore_Restore_RejectsTraversal_NoEffect(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	for _, p := range unsafePaths {
		_, err := s.Restore(p, 1)
		if !errors.Is(err, ErrUnsafePath) {
			t.Errorf("Restore(%q) err = %v, want ErrUnsafePath", p, err)
		}
	}
	// The safeguard never ran, so the store recorded nothing: no index/ entries.
	idx := filepath.Join(root, Dir, "index")
	if entries, err := os.ReadDir(idx); err == nil && len(entries) > 0 {
		t.Errorf("Restore on unsafe paths left %d index entries; safeguard must not run", len(entries))
	}
}

// R7 — the most destructive sub-case of the traversal bug. Restore's deletion
// branch does os.Remove(abs); with a path that escapes the root that is an
// arbitrary host-file delete, and unlike the overwrite branch there is no
// pre-restore of the foreign file (it was never in the store) — the deletion is
// unrecoverable. PRIMARY assertion: a sentinel file living OUTSIDE the root is
// still on disk with its exact contents after a restore that targets it via
// traversal. That the sentinel survives proves the guard precedes the effect;
// the ErrUnsafePath return is only the secondary assertion. (Checking the error
// alone would pass even if a mis-ordered os.Remove ran before the guard.)
func TestStore_Restore_DeleteBranch_TraversalLeavesSentinelIntact(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	// Sentinel lives beside the root, outside it. "../sentinel.txt" from root.
	const sentinelBody = "irreplaceable host file — must never be removed"
	sentinel := filepath.Join(parent, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte(sentinelBody), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(root)
	_, err := s.Restore("../sentinel.txt", 1)

	// PRIMARY: the foreign file is untouched, byte-for-byte.
	got, readErr := os.ReadFile(sentinel)
	if readErr != nil {
		t.Fatalf("sentinel gone or unreadable after restore-delete via traversal: %v", readErr)
	}
	if string(got) != sentinelBody {
		t.Fatalf("sentinel contents changed: got %q, want %q", got, sentinelBody)
	}

	// SECONDARY: the call was refused with the containment error.
	if !errors.Is(err, ErrUnsafePath) {
		t.Errorf("Restore err = %v, want ErrUnsafePath", err)
	}
}

// The guard must reject ONLY real escapes — a too-strict containment that
// refused ordinary subdirectory paths would break capture (the watcher feeds
// Record paths like "pkg/sub/file.go"). Legitimate relative paths with
// subdirectories, and inner ".." that stays inside, must round-trip normally.
func TestStore_Guard_AcceptsLegitSubdirPaths(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	for _, rel := range []string{"a.txt", "pkg/sub/file.go", "a/../b.txt"} {
		write(t, root, rel, "content")
		if err := s.Record(rel); err != nil {
			t.Errorf("Record(%q) = %v, want nil (legit path must not be rejected)", rel, err)
		}
		if _, err := s.List(rel); err != nil {
			t.Errorf("List(%q) = %v, want nil", rel, err)
		}
	}
}

// A symlinked intermediate component is a NON-lexical escape: "link/key.txt" is
// a clean relative path (filepath.IsLocal accepts it) yet resolves outside the
// root when "link" is a symlink to a foreign dir. resolveContained must reject
// it so an MCP/CLI caller cannot read, overwrite, or delete host files through
// it. PRIMARY assertion: a secret file outside the root is byte-unchanged.
func TestStore_Guard_RejectsSymlinkedComponentEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(parent, "secret")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	const body = "host secret — out of bounds"
	keyPath := filepath.Join(secret, "key.txt")
	if err := os.WriteFile(keyPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// project/link -> ../secret: an escaping symlinked component.
	if err := os.Symlink(secret, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	s := New(root)
	if err := s.Record("link/key.txt"); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("Record(link/key.txt) err = %v, want ErrUnsafePath", err)
	}
	if _, err := s.Restore("link/key.txt", 1); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("Restore(link/key.txt) err = %v, want ErrUnsafePath", err)
	}
	// PRIMARY: the foreign file was never read into the store nor modified.
	if got, _ := os.ReadFile(keyPath); string(got) != body {
		t.Errorf("host file changed through symlinked component: got %q", got)
	}
}

// Defense in depth: Record is the store's only writer, fed by the watcher with
// already-local paths, but it guards too so no caller can write outside the root.
func TestStore_Record_RejectsTraversal(t *testing.T) {
	s := New(t.TempDir())
	for _, p := range unsafePaths {
		if err := s.Record(p); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("Record(%q) err = %v, want ErrUnsafePath", p, err)
		}
	}
}

// Finding 3 / R4 — store corruption: the log references a revision whose object
// is missing from objects/. Restore reads the object only AFTER the step-1
// safeguard and BEFORE any working-tree write, so a missing object fails the
// call with the working tree byte-unchanged and a pre-restore revision recorded.
// Reversibility holds even when the store is partially corrupt. This invariant
// is already satisfied; the test nails it so a future reordering can't regress it.
func TestStore_Restore_MissingObject_PreservesReversibility(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "v1")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, root, "a.txt", "v2-current")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}

	revs, _ := s.List("a.txt") // newest first: v2, v1
	v1Ts := revs[1].Timestamp
	v1Hash := revs[1].Hash

	// Corrupt the store: delete the object the v1 revision points at.
	if err := os.Remove(s.objectPath(v1Hash)); err != nil {
		t.Fatal(err)
	}

	_, err := s.Restore("a.txt", v1Ts)
	if err == nil {
		t.Fatal("Restore with a missing object must fail, got nil")
	}

	// Working tree is byte-unchanged: the file was NOT half-written or truncated.
	got, readErr := os.ReadFile(filepath.Join(root, "a.txt"))
	if readErr != nil {
		t.Fatalf("working file unreadable after failed restore: %v", readErr)
	}
	if string(got) != "v2-current" {
		t.Fatalf("working file changed by a failed restore: got %q, want %q", got, "v2-current")
	}

	// The pre-restore safeguard ran before the failure, so the prior state is
	// recoverable — the safety net survived the corruption.
	revs2, _ := s.List("a.txt")
	if revs2[0].Label != LabelPreRestore {
		t.Errorf("newest label = %q, want a pre-restore safeguard", revs2[0].Label)
	}
}

// G4 — Get on a log that references a missing object fails visibly rather than
// returning empty/garbage content the agent could mistake for the real version.
func TestStore_Get_MissingObject_Errors(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	write(t, root, "a.txt", "content")
	if err := s.Record("a.txt"); err != nil {
		t.Fatal(err)
	}
	revs, _ := s.List("a.txt")
	if err := os.Remove(s.objectPath(revs[0].Hash)); err != nil {
		t.Fatal(err)
	}

	content, err := s.Get("a.txt", revs[0].Timestamp)
	if err == nil {
		t.Fatalf("Get with a missing object must fail, got content %q", content)
	}
}
