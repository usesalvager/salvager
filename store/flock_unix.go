//go:build unix

// Cross-process advisory locking via flock(2). The store's targets are darwin
// and linux (the CI/release matrix), both POSIX, so the stdlib syscall.Flock is
// enough — no new dependency. flock is chosen over an O_EXCL pidfile or POSIX
// fcntl locks because it is CRASH-SAFE: the kernel drops the lock when the
// holding fd is closed or the process dies, so a SIGKILL'd CLI mid-operation
// cannot wedge the long-lived watcher service forever. The lock is associated
// with the open file description (the fd), so two store.New(root) — in the same
// process or different ones — hold independent fds and therefore genuinely
// contend, which is exactly the cross-process exclusion the in-process
// sync.Mutex cannot provide. (Linux flock(2): file descriptors from separate
// open() calls are treated independently and contend; BSD/macOS likewise.)
package store

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// flockHandle owns a locked fd; release drops the lock and closes it.
type flockHandle struct{ f *os.File }

func (h *flockHandle) release() {
	if h == nil || h.f == nil {
		return
	}
	_ = syscall.Flock(int(h.f.Fd()), syscall.LOCK_UN)
	_ = h.f.Close()
}

// acquireFlock takes an advisory lock on path — exclusive (writers) or shared
// (Get). It creates the lock file if absent (0o600, dir 0o700: the file is part
// of the secret-bearing store and must not be world-readable). It blocks up to
// wait, then returns a clear error rather than hanging: the kernel flock is
// blocking-only, so the timeout is built from a non-blocking (LOCK_NB) retry
// loop with capped backoff. EINTR is retried without counting against the
// deadline. The loop uses real wall-clock time, not nowFunc (the revision clock,
// which tests may freeze).
func acquireFlock(path string, exclusive bool, wait time.Duration) (*flockHandle, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}

	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}

	start := time.Now()
	backoff := time.Millisecond
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			return &flockHandle{f: f}, nil
		}
		if err == syscall.EINTR {
			continue // interrupted; retry immediately, do not penalize the deadline
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if time.Since(start) >= wait {
			_ = f.Close()
			return nil, fmt.Errorf("another salvager process holds the store lock (waited %s): %s", wait, path)
		}
		time.Sleep(backoff)
		if backoff < 50*time.Millisecond {
			backoff *= 2
		}
	}
}
