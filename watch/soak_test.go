package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSoakNoLeak runs the watcher under a sustained rewrite load and asserts that
// goroutine and open-fd counts return to baseline once the load stops — i.e. the
// streaming capture path (os.Open + temp + rename, per event) does not leak fds
// or goroutines over prolonged execution. It is opt-in: set SALVAGER_SOAK to a
// duration (e.g. SALVAGER_SOAK=2h) to run the multi-hour validation. Unset, it
// skips, so CI is unaffected.
//
// The load deliberately stresses the new path: a large file rewritten in a loop
// (repeated multi-buffer streaming + temp + rename) and a same-size in-place
// rewriter that lands inside the racy window (Branch 3 of the gate — the path
// that most often opens-streams-then-discards a temp because the racy guard
// forces a capture that the hash then dedups away).
func TestSoakNoLeak(t *testing.T) {
	raw := os.Getenv("SALVAGER_SOAK")
	if raw == "" {
		t.Skip("set SALVAGER_SOAK=<duration> (e.g. 2h) to run the soak")
	}
	dur, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("SALVAGER_SOAK=%q: %v", raw, err)
	}

	root := t.TempDir()
	_, stop := stressStart(t, root, 50*time.Millisecond)
	defer stop()

	// Let the initial scan and one sweep settle so the baseline reflects the
	// steady-state watcher, not startup transients.
	time.Sleep(2 * time.Second)
	runtime.GC()
	baseG := runtime.NumGoroutine()
	baseFD := openFDs()
	t.Logf("baseline: goroutines=%d fds=%d (fd sampling %s)", baseG, baseFD, fdMode())

	stopWriters := make(chan struct{})
	var wg sync.WaitGroup

	// Writer 1: large file rewritten in a loop — exercises repeated multi-buffer
	// streaming + temp + rename with genuinely changing content.
	wg.Add(1)
	go func() {
		defer wg.Done()
		big := filepath.Join(root, "big.dat")
		block := []byte(strings.Repeat("salvager soak payload block\n", 4096)) // ~112 KiB
		for i := 0; ; i++ {
			select {
			case <-stopWriters:
				return
			default:
			}
			// Vary the leading byte so content (and hash) actually changes.
			block[0] = byte('A' + i%26)
			_ = os.WriteFile(big, block, 0o644)
			time.Sleep(5 * time.Millisecond)
		}
	}()

	// Writer 2: same-size in-place rewriter — content changes but byte length is
	// constant, and rewrites land within the racy window, hammering Branch 3 and
	// the open-stream-discard temp path.
	wg.Add(1)
	go func() {
		defer wg.Done()
		racy := filepath.Join(root, "racy.csv")
		buf := []byte("0000000000000000000000000000000\n") // fixed length
		for i := 0; ; i++ {
			select {
			case <-stopWriters:
				return
			default:
			}
			for j := range buf[:len(buf)-1] {
				buf[j] = byte('0' + (i+j)%10) // same length, different content
			}
			_ = os.WriteFile(racy, buf, 0o644)
			time.Sleep(2 * time.Millisecond)
		}
	}()

	// Run the load, sampling for monotonic growth (a leak shows as an fd/goroutine
	// count that keeps climbing rather than oscillating around a steady state).
	deadline := time.Now().Add(dur)
	var peakG, peakFD int
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		g, fd := runtime.NumGoroutine(), openFDs()
		if g > peakG {
			peakG = g
		}
		if fd > peakFD {
			peakFD = fd
		}
		t.Logf("load: goroutines=%d fds=%d", g, fd)
	}

	// Stop the load and let debounce, sweep and GC drain.
	close(stopWriters)
	wg.Wait()
	time.Sleep(3 * time.Second)
	runtime.GC()
	runtime.GC()
	finalG := runtime.NumGoroutine()
	finalFD := openFDs()
	t.Logf("final: goroutines=%d fds=%d (peak during load: goroutines=%d fds=%d)", finalG, finalFD, peakG, peakFD)

	// Tolerances: steady state may differ from baseline by a few runtime/GC
	// goroutines and a couple of fds; a leak is monotonic growth far beyond this.
	const gTol, fdTol = 5, 8
	if finalG > baseG+gTol {
		t.Errorf("goroutines did not return to baseline: base=%d final=%d (tol %d) — possible leak", baseG, finalG, gTol)
	}
	if baseFD >= 0 && finalFD > baseFD+fdTol {
		t.Errorf("open fds did not return to baseline: base=%d final=%d (tol %d) — possible fd leak in capture path", baseFD, finalFD, fdTol)
	}
}

// openFDs returns the number of file descriptors open to this process, or -1 on
// platforms where it cannot be sampled. /dev/fd lists the live fds on both Linux
// (symlink to /proc/self/fd) and macOS (fdescfs), so a single path covers the
// shipped targets without cgo. It uses Readdirnames, not os.ReadDir: on macOS
// fdescfs the per-fd entries are ephemeral and lstat'ing them (which ReadDir
// does to build DirEntry) fails, while listing names alone succeeds. The one fd
// opened to read the directory is subtracted, and it is opened on every sample,
// so baseline and final are directly comparable.
func openFDs() int {
	f, err := os.Open("/dev/fd")
	if err != nil {
		return -1
	}
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		return -1
	}
	return len(names) - 1
}

func fdMode() string {
	if _, err := os.Stat("/dev/fd"); err != nil {
		return fmt.Sprintf("unavailable: %v", err)
	}
	return "/dev/fd"
}
