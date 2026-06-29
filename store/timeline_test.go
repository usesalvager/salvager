package store

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func mustRecord(t *testing.T, s *FS, rel string) {
	t.Helper()
	if err := s.Record(rel); err != nil {
		t.Fatal(err)
	}
}

// ev is a terse constructor for synthetic timeline events in DetectBursts tests.
func ev(ts int64, path string, label Label, delta int) RevisionEvent {
	return RevisionEvent{
		Path: path,
		Revision: Revision{
			Timestamp:  ts,
			Label:      label,
			HasSignal:  true,
			Delta:      delta,
			DeltaKnown: true,
		},
	}
}

func TestDetectBursts(t *testing.T) {
	// Three deletes within the gap → one burst; RestoreAt is the instant before.
	t.Run("delete burst flagged", func(t *testing.T) {
		bursts := DetectBursts([]RevisionEvent{
			ev(1000, "a.txt", LabelDelete, 0),
			ev(1001, "b.txt", LabelDelete, 0),
			ev(1002, "c.txt", LabelDelete, 0),
		})
		if len(bursts) != 1 {
			t.Fatalf("got %d bursts, want 1", len(bursts))
		}
		b := bursts[0]
		if b.Start != 1000 || b.End != 1002 || b.Deletes != 3 || b.RestoreAt != 999 {
			t.Errorf("burst = %+v", b)
		}
		if !reflect.DeepEqual(b.Files, []string{"a.txt", "b.txt", "c.txt"}) {
			t.Errorf("files = %v", b.Files)
		}
	})

	// A gap larger than burstGapMs splits one stream into two clusters; each must
	// independently clear the file threshold to count.
	t.Run("gap splits clusters", func(t *testing.T) {
		bursts := DetectBursts([]RevisionEvent{
			ev(1000, "a.txt", LabelDelete, 0),
			ev(1001, "b.txt", LabelDelete, 0),
			ev(1002, "c.txt", LabelDelete, 0),
			// > burstGapMs later: a separate, sub-threshold pair → not a burst.
			ev(1000+burstGapMs+1+10, "d.txt", LabelDelete, 0),
			ev(1000+burstGapMs+1+11, "e.txt", LabelDelete, 0),
		})
		if len(bursts) != 1 {
			t.Fatalf("got %d bursts, want 1 (second cluster is sub-threshold)", len(bursts))
		}
		if bursts[0].End != 1002 {
			t.Errorf("first cluster End = %d, want 1002", bursts[0].End)
		}
	})

	// Below the distinct-file threshold → not a burst, even if many events.
	t.Run("sub-threshold not flagged", func(t *testing.T) {
		bursts := DetectBursts([]RevisionEvent{
			ev(1000, "a.txt", LabelDelete, 0),
			ev(1001, "b.txt", LabelDelete, 0),
		})
		if len(bursts) != 0 {
			t.Fatalf("got %d bursts, want 0", len(bursts))
		}
	})

	// Repeated deletes of the SAME file are one distinct file → not a burst.
	t.Run("same file repeated not flagged", func(t *testing.T) {
		bursts := DetectBursts([]RevisionEvent{
			ev(1000, "a.txt", LabelDelete, 0),
			ev(1001, "a.txt", LabelDelete, 0),
			ev(1002, "a.txt", LabelDelete, 0),
		})
		if len(bursts) != 0 {
			t.Fatalf("got %d bursts, want 0", len(bursts))
		}
	})

	// Large-negative-delta modifies count as destructive (reset --hard shrinking
	// many files); small edits and salvager's own restore bookkeeping do not.
	t.Run("large shrink counts, noise ignored", func(t *testing.T) {
		bursts := DetectBursts([]RevisionEvent{
			ev(1000, "a.txt", LabelModify, burstShrink),     // exactly at threshold → destructive
			ev(1001, "b.txt", LabelModify, burstShrink-100), // well past → destructive
			ev(1002, "c.txt", LabelModify, -1),              // tiny edit → ignored
			ev(1003, "d.txt", LabelModify, burstShrink),     // destructive
			ev(1004, "e.txt", LabelRestore, -9999),          // restore bookkeeping → ignored
			ev(1005, "f.txt", LabelPreRestore, -9999),       // pre-restore safeguard → ignored
		})
		if len(bursts) != 1 {
			t.Fatalf("got %d bursts, want 1", len(bursts))
		}
		if !reflect.DeepEqual(bursts[0].Files, []string{"a.txt", "b.txt", "d.txt"}) {
			t.Errorf("files = %v, want the three large-shrink modifies only", bursts[0].Files)
		}
		if bursts[0].Deletes != 0 {
			t.Errorf("Deletes = %d, want 0", bursts[0].Deletes)
		}
	})
}

func TestScanRevisions(t *testing.T) {
	fakeClock(t)
	root := t.TempDir()
	s := New(root)

	// Two files; one is later deleted (so its latest revision is a deletion — it
	// must still appear, unlike in TrackedUnder).
	write(t, root, "a.txt", "a1")
	mustRecord(t, s, "a.txt")
	write(t, root, "sub/b.txt", "b1")
	mustRecord(t, s, "sub/b.txt")
	write(t, root, "a.txt", "a2")
	mustRecord(t, s, "a.txt")
	if err := os.Remove(filepath.Join(root, "sub/b.txt")); err != nil {
		t.Fatal(err)
	}
	mustRecord(t, s, "sub/b.txt")

	events, err := s.ScanRevisions("")
	if err != nil {
		t.Fatalf("ScanRevisions: %v", err)
	}
	// a.txt: initial + modify; sub/b.txt: initial + delete → 4 events.
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(events), events)
	}
	// Strictly timestamp-ordered.
	for i := 1; i < len(events); i++ {
		if events[i].Timestamp < events[i-1].Timestamp {
			t.Fatalf("not ordered: %+v", events)
		}
	}
	// The deletion is present.
	sawDelete := false
	for _, e := range events {
		if e.Path == "sub/b.txt" && e.Label == LabelDelete {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Error("deletion of sub/b.txt missing from scan")
	}

	// relDir scopes the scan to a subtree.
	scoped, err := s.ScanRevisions("sub")
	if err != nil {
		t.Fatalf("ScanRevisions(sub): %v", err)
	}
	for _, e := range scoped {
		if e.Path != "sub/b.txt" {
			t.Errorf("scoped scan leaked %s", e.Path)
		}
	}
	if len(scoped) != 2 {
		t.Errorf("scoped scan = %d events, want 2", len(scoped))
	}
}
