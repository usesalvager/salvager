// Package watch observes a working tree and feeds changes to the store. It is
// passive: it never modifies the files it watches. On startup it records an
// initial revision of every tracked file, guaranteeing a "good" version exists
// before any later change.
package watch

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"lochis/ignore"
)

// Recorder is the slice of the store the watcher needs.
type Recorder interface {
	Record(relPath string) error
}

// Default debounce parameters (see spec §6.3).
const (
	DefaultDebounce = 300 * time.Millisecond
	DefaultTick     = 100 * time.Millisecond
)

// Watcher couples fsnotify, the ignore matcher and the store.
type Watcher struct {
	root     string
	store    Recorder
	ign      *ignore.Matcher
	fsw      *fsnotify.Watcher
	debounce time.Duration
	tick     time.Duration
}

// New creates a Watcher rooted at root.
func New(root string, s Recorder, ign *ignore.Matcher) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Watcher{
		root:     root,
		store:    s,
		ign:      ign,
		fsw:      fsw,
		debounce: DefaultDebounce,
		tick:     DefaultTick,
	}, nil
}

// Close releases the underlying fsnotify watcher.
func (w *Watcher) Close() error { return w.fsw.Close() }

// rel converts an absolute path to one relative to the project root.
func (w *Watcher) rel(abs string) string {
	r, err := filepath.Rel(w.root, abs)
	if err != nil {
		return abs
	}
	return r
}

// initialScan walks the tree, records every tracked file, and registers every
// tracked directory with fsnotify. inotify on Linux is not recursive, so each
// subdirectory must be added individually.
func (w *Watcher) initialScan() error {
	return filepath.WalkDir(w.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries, keep going
		}
		if w.ign.Match(path) {
			if d.IsDir() {
				return filepath.SkipDir // don't descend into ignored dirs
			}
			return nil
		}
		if d.IsDir() {
			if err := w.fsw.Add(path); err != nil {
				log.Printf("lochis: watch add %s: %v", path, err)
			}
			return nil
		}
		if err := w.store.Record(w.rel(path)); err != nil {
			log.Printf("lochis: initial record %s: %v", path, err)
		}
		return nil
	})
}

// addTree registers a newly created directory and all its (tracked) contents,
// recording any files found. Used when a directory appears at runtime.
func (w *Watcher) addTree(dir string) {
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if w.ign.Match(path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if err := w.fsw.Add(path); err != nil {
				log.Printf("lochis: watch add %s: %v", path, err)
			}
		} else {
			if err := w.store.Record(w.rel(path)); err != nil {
				log.Printf("lochis: record %s: %v", path, err)
			}
		}
		return nil
	})
}

// Run starts watching and blocks until done is closed or an unrecoverable
// error occurs. Changes are debounced per file: a Record fires after the file
// has been quiet for w.debounce.
func (w *Watcher) Run(done <-chan struct{}) error {
	if err := w.initialScan(); err != nil {
		return err
	}

	pending := map[string]time.Time{}
	ticker := time.NewTicker(w.tick)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return nil

		case ev, ok := <-w.fsw.Events:
			if !ok {
				return nil
			}
			if w.ign.Match(ev.Name) {
				continue
			}
			// A newly created directory must be registered (and scanned),
			// since inotify won't report events inside it otherwise.
			if ev.Op&fsnotify.Create != 0 {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					w.addTree(ev.Name)
					continue
				}
			}
			pending[ev.Name] = nowFunc()

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
			log.Printf("lochis: watch error: %v", err)

		case <-ticker.C:
			now := nowFunc()
			for path, last := range pending {
				if now.Sub(last) > w.debounce {
					if err := w.store.Record(w.rel(path)); err != nil {
						log.Printf("lochis: record %s: %v", w.rel(path), err)
					}
					delete(pending, path)
				}
			}
		}
	}
}

// nowFunc is the clock; overridable in tests.
var nowFunc = time.Now
