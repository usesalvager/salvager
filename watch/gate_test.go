package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// captureDecision is the pure three-branch racy-clean gate. These tests pin every
// branch deterministically, including the ctime-unavailable degrade that cannot
// be produced from a real lstat on the shipped (linux/darwin) platforms.
func TestCaptureDecision(t *testing.T) {
	const slop = 3 * time.Second
	// base: stat identical to a capture taken well after the file's mtime tick,
	// so by default the file is provably clean (skip).
	mtime := int64(1_000_000_000)
	base := captureState{size: 10, mtime: mtime, ctime: mtime, capturedAt: mtime + int64(2*slop)}

	cases := []struct {
		name string
		cur  captureState
		prev captureState
		want bool // true = must capture
	}{
		{
			name: "clean: stat identical, capture well after mtime tick -> skip",
			cur:  captureState{size: 10, mtime: mtime, ctime: mtime},
			prev: base,
			want: false,
		},
		{
			name: "branch1 size moved -> capture",
			cur:  captureState{size: 11, mtime: mtime, ctime: mtime},
			prev: base,
			want: true,
		},
		{
			name: "branch1 mtime moved -> capture",
			cur:  captureState{size: 10, mtime: mtime + 5_000_000_000, ctime: mtime},
			prev: base,
			want: true,
		},
		{
			// The hole the gate must not have: an in-place same-size rewrite that
			// restores the old mtime. ctime advanced; the gate must still capture.
			name: "branch1 ctime moved with mtime+size restored -> capture (mtime-restore hole closed)",
			cur:  captureState{size: 10, mtime: mtime, ctime: mtime + 5_000_000_000},
			prev: base,
			want: true,
		},
		{
			// Racy: capture raced the file's mtime tick. Stat identical, but the
			// recorded mtime sits inside the racy window, so a same-tick same-size
			// rewrite would be invisible -> capture.
			name: "racy: mtime within slop of capturedAt -> capture",
			cur:  captureState{size: 10, mtime: mtime, ctime: mtime},
			prev: captureState{size: 10, mtime: mtime, ctime: mtime, capturedAt: mtime},
			want: true,
		},
		{
			// ctime unavailable on either side: never trust a clean stat.
			name: "ctime==0 current -> capture",
			cur:  captureState{size: 10, mtime: mtime, ctime: 0},
			prev: captureState{size: 10, mtime: mtime, ctime: 0, capturedAt: mtime + int64(2*slop)},
			want: true,
		},
		{
			name: "ctime==0 stored -> capture",
			cur:  captureState{size: 10, mtime: mtime, ctime: mtime},
			prev: captureState{size: 10, mtime: mtime, ctime: 0, capturedAt: mtime + int64(2*slop)},
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := captureDecision(c.cur, c.prev, slop); got != c.want {
				t.Errorf("captureDecision = %v, want %v", got, c.want)
			}
		})
	}
}

// TestGateBranch3SameSizeRewriteCaptured is the no-silent-hole test through the
// real filesystem path: a file rewritten in place at the SAME byte length while
// its recorded mtime sits in the racy window is captured, not skipped. This is
// the case a naive size+mtime gate would lose.
func TestGateBranch3SameSizeRewriteCaptured(t *testing.T) {
	w := &Watcher{gate: map[string]captureState{}, slop: DefaultRacySlop}
	dir := t.TempDir()
	abs := filepath.Join(dir, "data.csv")
	if err := os.WriteFile(abs, []byte("AAAA"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Prime the gate as if we captured "AAAA" at the same instant as the file's
	// mtime (the racy collision a coarse-resolution filesystem produces).
	fi, err := os.Lstat(abs)
	if err != nil {
		t.Fatal(err)
	}
	w.gate["data.csv"] = captureState{
		size:       fi.Size(),
		mtime:      fi.ModTime().UnixNano(),
		ctime:      ctimeNano(fi),
		capturedAt: fi.ModTime().UnixNano(), // captured == file mtime -> racy
	}

	// Same-size in-place rewrite.
	if err := os.WriteFile(abs, []byte("BBBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Restore the original mtime to emulate a coarse-FS same-tick collision, so
	// the test does not lean on mtime advancing.
	old := fi.ModTime()
	if err := os.Chtimes(abs, old, old); err != nil {
		t.Fatal(err)
	}

	if !w.shouldCapture("data.csv", abs) {
		t.Fatal("same-size in-place rewrite in the racy window was skipped: silent content hole")
	}
}

// TestGateBranch2NoReStream proves an untouched file outside the racy window is
// skipped: no redundant re-stream on a replayed event.
func TestGateBranch2NoReStream(t *testing.T) {
	w := &Watcher{gate: map[string]captureState{}, slop: DefaultRacySlop}
	dir := t.TempDir()
	abs := filepath.Join(dir, "stable.txt")
	if err := os.WriteFile(abs, []byte("unchanging"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Age the file well past the racy window so a clean stat is trustworthy.
	old := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(abs, old, old); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(abs)
	if err != nil {
		t.Fatal(err)
	}
	w.gate["stable.txt"] = captureState{
		size:       fi.Size(),
		mtime:      fi.ModTime().UnixNano(),
		ctime:      ctimeNano(fi),
		capturedAt: time.Now().UnixNano(), // captured now, file mtime is an hour old
	}

	if w.shouldCapture("stable.txt", abs) {
		t.Fatal("untouched file outside the racy window was captured: redundant re-stream")
	}
}

// TestGateFirstSightCaptures confirms a path the gate has never seen is always
// captured (the store remains the dedup backstop).
func TestGateFirstSightCaptures(t *testing.T) {
	w := &Watcher{gate: map[string]captureState{}, slop: DefaultRacySlop}
	dir := t.TempDir()
	abs := filepath.Join(dir, "new.txt")
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !w.shouldCapture("new.txt", abs) {
		t.Fatal("never-seen path was skipped")
	}
}

// TestProbeRacySlopWidensOnly confirms the probe never narrows the conservative
// default on a normal (fine-resolution) filesystem.
func TestProbeRacySlopWidensOnly(t *testing.T) {
	w := &Watcher{gate: map[string]captureState{}, slop: DefaultRacySlop, root: t.TempDir()}
	w.ProbeRacySlop()
	if w.slop < DefaultRacySlop {
		t.Fatalf("probe narrowed slop to %v, below default %v", w.slop, DefaultRacySlop)
	}
}
