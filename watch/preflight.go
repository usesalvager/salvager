package watch

import "github.com/usesalvager/salvager/ignore"

// Preflight verifies the watcher can construct on root without running or
// touching the tree: it builds the OS backend (the same newOSBackend the live
// watcher uses) and tears it straight back down. A nil return means a real
// `salvager watch` on this root will not die during startup for a reason the
// backend can see now — chiefly the kernel refusing a new fsnotify instance
// (Linux ENOMEM / fs.inotify.max_user_instances exhausted), which is the one
// failure that would otherwise crash-loop a freshly installed service.
//
// It deliberately does NOT run initialScan: it places no per-directory watches,
// records no initial revisions, and has no side effects on the store or tree.
// Per-directory AddDir overflow is not a startup failure anyway — the live
// watcher covers it with the polling sweep — so it is out of scope here. The
// caller is responsible for having initialised the store separately (store.Init
// is the other thing that can fail startup, and it is the caller's to own).
func Preflight(root string, s Recorder, ign *ignore.Matcher) error {
	w, err := New(root, s, ign)
	if err != nil {
		return err
	}
	return w.Close()
}
