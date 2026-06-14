package watch

import (
	"errors"
	"strings"
	"syscall"
)

// A backend is the OS-specific source of real-time file-change notifications.
// It is deliberately small and injectable: production wires the fsnotify
// backend (inotify on Linux, kqueue on macOS/BSD), while tests inject a fake
// that reports the descriptor-limit error on demand — the one condition that
// silently broke coverage and which a real machine will not reproduce until it
// is already drowning in open files.
//
// The watcher does not depend on the backend being complete. AddDir may fail
// for a directory once the kernel watch cap is reached; the watcher's contract
// is that any directory it could not place under a live watch is handed to the
// polling sweep instead, so coverage is "part real-time, part polling" and
// always whole.
type backend interface {
	// AddDir registers dir for real-time notification. It may return a
	// descriptor-limit error (see isDescriptorLimit) when the kernel cap is hit.
	AddDir(dir string) error
	// Events delivers normalized change notifications until Close.
	Events() <-chan Event
	// Errors delivers non-fatal backend errors (logged, never fatal).
	Errors() <-chan error
	// Close releases the backend's OS resources.
	Close() error
}

// Op is a normalized change kind, mirroring the fsnotify operations the watcher
// actually distinguishes.
type Op uint8

const (
	Create Op = 1 << iota // file or directory created
	Write                 // file written
	Remove                // file or directory removed
	Rename                // file or directory renamed away
	Chmod                 // metadata change
)

// Event is one normalized notification. Path is absolute.
type Event struct {
	Path string
	Op   Op
}

// isDescriptorLimit reports whether err is the kernel refusing another watch
// because a resource cap was hit. This is the exact condition that used to
// freeze large trees at t0: the directory's AddDir failed, but store.Record
// (a transient fd) still succeeded, so files got an initial snapshot and then
// never another event.
//
//   - kqueue (macOS/BSD): EMFILE / ENFILE — one fd per watched entry, capped by
//     kern.maxfilesperproc.
//   - inotify (Linux): ENOSPC — one watch per directory, capped by
//     fs.inotify.max_user_watches (inotify reuses ENOSPC, not a disk-full error).
//
// Matched by errno first (errors.Is unwraps fsnotify's wrapping), with a text
// fallback for backends that return an unwrapped message.
func isDescriptorLimit(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOSPC) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "too many open files") ||
		strings.Contains(msg, "no space left") ||
		strings.Contains(msg, "user limit")
}
