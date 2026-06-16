package store

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestStreamSignalEqualsByteSlice proves the streaming capture path produces the
// exact same observable result as computing the signal from a full byte slice:
// same hash, same recovered content, same line/byte/delta/start-signature. The
// byte-slice computeSig is the oracle. If the two ever drift, this fails.
func TestStreamSignalEqualsByteSlice(t *testing.T) {
	fakeClock(t)
	corpus := map[string]string{
		"empty":            "",
		"no-trailing-nl":   "alpha\nbeta",
		"trailing-nl":      "alpha\nbeta\n",
		"blank-leading":    "\n\n\n  \nfirst\nsecond\nthird\n",
		"binary-nul":       "head\x00\x01\x02tail",
		"single-long-line": strings.Repeat("x", 200*1024), // > sigScanLimit, one line
		"multi-mb":         strings.Repeat("line of text\n", 200_000),
		"crlf":             "one\r\ntwo\r\nthree\r\n",
	}

	for name, content := range corpus {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			s := New(root)
			write(t, root, "f", content)
			if err := s.Record("f"); err != nil {
				t.Fatal(err)
			}

			revs, err := s.List("f")
			if err != nil {
				t.Fatal(err)
			}
			if len(revs) != 1 {
				t.Fatalf("want 1 revision, got %d", len(revs))
			}
			got := revs[0]

			// Oracle: the signal as computed from the whole slice (no previous rev).
			want := computeSig([]byte(content), Revision{}, false)

			if got.Hash != sha256hex([]byte(content)) {
				t.Errorf("hash = %s, want %s", got.Hash, sha256hex([]byte(content)))
			}
			if got.Lines != want.lines {
				t.Errorf("lines = %d, want %d", got.Lines, want.lines)
			}
			if got.Bytes != want.bytes {
				t.Errorf("bytes = %d, want %d", got.Bytes, want.bytes)
			}
			if got.Delta != want.delta || got.DeltaKnown != want.deltaKnown {
				t.Errorf("delta = (%d,%v), want (%d,%v)", got.Delta, got.DeltaKnown, want.delta, want.deltaKnown)
			}
			if got.Sig != want.sig {
				t.Errorf("sig = %q, want %q", got.Sig, want.sig)
			}

			// Content round-trips byte-for-byte through the streamed object.
			back, err := s.Get("f", got.Timestamp)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(back, []byte(content)) {
				t.Errorf("recovered content differs (%d vs %d bytes)", len(back), len(content))
			}
		})
	}
}

// TestStreamBoundedRAM proves capture memory does not scale with file size. A
// whole-file read (the old os.ReadFile path) would allocate >= the file size; the
// streaming path allocates only its fixed buffer plus the bounded signature
// prefix. TotalAlloc is cumulative, so the delta across one Record is the bytes
// allocated by that capture — which must stay far below the file size.
func TestStreamBoundedRAM(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a large file; skipped under -short")
	}
	const fileSize = 128 << 20 // 128 MiB
	const ceiling = 8 << 20    // capture must allocate << file size

	root := t.TempDir()
	writeLarge(t, root, "big", fileSize)
	s := New(root)

	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	if err := s.Record("big"); err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	delta := m1.TotalAlloc - m0.TotalAlloc
	if delta > ceiling {
		t.Fatalf("capture allocated %d bytes for a %d-byte file; want <= %d (memory must not scale with file size)",
			delta, fileSize, ceiling)
	}
}

// writeLarge creates a file of n bytes without holding it in memory, so the test
// itself stays O(buffer) and only the code under test is measured.
func writeLarge(t *testing.T, root, rel string, n int) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(root, rel), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	chunk := bytes.Repeat([]byte("salvager streaming capture test payload\n"), 1024) // ~40 KiB
	for written := 0; written < n; {
		w := chunk
		if rem := n - written; rem < len(w) {
			w = w[:rem]
		}
		if _, err := f.Write(w); err != nil {
			t.Fatal(err)
		}
		written += len(w)
	}
}
