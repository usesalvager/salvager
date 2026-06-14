package store

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// These benchmarks isolate the capture hot path from the filesystem-event
// machinery, so they produce low-noise, machine-comparable numbers
// (ns/op, B/op, allocs/op, and MB/s via SetBytes). Run:
//
//	go test ./store -bench=. -benchmem -run=^$
//
// The watcher's end-to-end latency and CPU/watch footprint live in
// bench/run.sh; this file measures only the store's per-revision cost.

const benchFileSize = 4096

// BenchmarkRecordUnique measures first-capture of a file: read + sha256 +
// write a new object + append the initial log line. This is the initial-scan
// hot path — each iteration captures a distinct file against an empty log, so
// the figure is the true per-file cost and is not inflated by re-reading a
// growing history. Unique bytes per file mean the content-addressed store
// cannot deduplicate the object (worst case).
func BenchmarkRecordUnique(b *testing.B) {
	root := b.TempDir()
	s := New(root)
	if err := s.Init(); err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, benchFileSize)

	b.SetBytes(benchFileSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		rel := "f" + strconv.Itoa(i) + ".txt"
		binary.LittleEndian.PutUint64(buf, uint64(i)) // unique content -> unique hash
		if err := os.WriteFile(filepath.Join(root, rel), buf, 0o644); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := s.Record(rel); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRecordUnchanged measures the path the watcher hits most often: a
// change event fires but the file's content is identical to the last
// revision, so Record reads + hashes + compares and writes nothing. This is
// what keeps a debounced burst or a touched-but-unmodified file cheap.
func BenchmarkRecordUnchanged(b *testing.B) {
	root := b.TempDir()
	s := New(root)
	if err := s.Init(); err != nil {
		b.Fatal(err)
	}
	rel := "f.txt"
	abs := filepath.Join(root, rel)
	buf := make([]byte, benchFileSize)
	if err := os.WriteFile(abs, buf, 0o644); err != nil {
		b.Fatal(err)
	}
	if err := s.Record(rel); err != nil { // establish the baseline revision
		b.Fatal(err)
	}

	b.SetBytes(benchFileSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Record(rel); err != nil { // dedup: no object, no log line
			b.Fatal(err)
		}
	}
}
