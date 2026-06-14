package watch

import "github.com/fsnotify/fsnotify"

// fsnotifyBackend is the real backend on every platform: inotify on Linux,
// kqueue on macOS/BSD, ReadDirectoryChangesW on Windows. It is a thin translator
// from fsnotify's per-file events to the watcher's normalized Op set. AddDir is
// a real per-directory registration, so it surfaces the descriptor-limit error
// that the watcher converts into a polling fallback.
type fsnotifyBackend struct {
	fsw    *fsnotify.Watcher
	events chan Event
	done   chan struct{}
}

func newOSBackend(root string) (backend, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	b := &fsnotifyBackend{
		fsw:    fsw,
		events: make(chan Event),
		done:   make(chan struct{}),
	}
	go b.translate()
	return b, nil
}

func (b *fsnotifyBackend) translate() {
	for {
		select {
		case <-b.done:
			return
		case ev, ok := <-b.fsw.Events:
			if !ok {
				return
			}
			out := Event{Path: ev.Name}
			if ev.Op&fsnotify.Create != 0 {
				out.Op |= Create
			}
			if ev.Op&fsnotify.Write != 0 {
				out.Op |= Write
			}
			if ev.Op&fsnotify.Remove != 0 {
				out.Op |= Remove
			}
			if ev.Op&fsnotify.Rename != 0 {
				out.Op |= Rename
			}
			if ev.Op&fsnotify.Chmod != 0 {
				out.Op |= Chmod
			}
			select {
			case b.events <- out:
			case <-b.done:
				return
			}
		}
	}
}

func (b *fsnotifyBackend) AddDir(dir string) error { return b.fsw.Add(dir) }
func (b *fsnotifyBackend) Events() <-chan Event    { return b.events }
func (b *fsnotifyBackend) Errors() <-chan error    { return b.fsw.Errors }
func (b *fsnotifyBackend) Close() error {
	close(b.done)
	return b.fsw.Close()
}
