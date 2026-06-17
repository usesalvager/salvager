//go:build unix

package store

import (
	"testing"
	"time"
)

// TestFlock_ExclusionTimeoutAndShared proves the three properties the cross-
// process design rests on, using two store.New(root) over one root (= two
// processes; in one test process they are two independent fds, which flock(2)
// contends across — Linux/BSD both):
//
//  1. an exclusive holder blocks another exclusive acquirer;
//  2. a blocked acquirer ABORTS LOUDLY after wait (returns an error, never hangs,
//     never silently "succeeds" unlocked) — the property gating the whole design;
//  3. shared holders coexist.
func TestFlock_ExclusionTimeoutAndShared(t *testing.T) {
	root := t.TempDir()
	a := New(root)
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}
	b := New(root)

	// 1+2. A holds EX; B's EX must time out with an error after ~the wait.
	hA, err := acquireFlock(a.lockPath(), true, time.Second)
	if err != nil {
		t.Fatalf("A acquire EX: %v", err)
	}
	start := time.Now()
	const wait = 150 * time.Millisecond
	if hB, err := acquireFlock(b.lockPath(), true, wait); err == nil {
		hB.release()
		t.Fatal("B acquired EX while A held it — exclusion broken")
	}
	if waited := time.Since(start); waited < wait {
		t.Fatalf("B returned after %v, before the %v deadline — did not actually wait", waited, wait)
	}

	// 3. After A releases, B's EX succeeds promptly.
	hA.release()
	hB, err := acquireFlock(b.lockPath(), true, time.Second)
	if err != nil {
		t.Fatalf("B acquire EX after release: %v", err)
	}
	hB.release()

	// 4. Two shared holders coexist (readers do not block readers).
	r1, err := acquireFlock(a.lockPath(), false, time.Second)
	if err != nil {
		t.Fatalf("first SH: %v", err)
	}
	r2, err := acquireFlock(b.lockPath(), false, time.Second)
	if err != nil {
		t.Fatalf("second SH should coexist with the first: %v", err)
	}
	r1.release()
	r2.release()
}
