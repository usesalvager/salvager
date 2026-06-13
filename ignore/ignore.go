// Package ignore decides which paths the watcher should skip. It combines the
// project's .gitignore (parsed with correct semantics) with a fixed set of
// default excludes that apply to every project, regardless of language.
package ignore

import (
	"path/filepath"
	"strings"

	gitignore "github.com/sabhiram/go-gitignore"
	"lochis/store"
)

// Defaults are always excluded. We watch foreign working trees of any
// ecosystem, so this covers several. Excluding store.Dir is mandatory: without
// it the watcher would record its own recordings in a loop.
var Defaults = []string{
	".git",
	store.Dir, // ".lochis"
	"node_modules",
	"vendor",
	".venv",
	"__pycache__",
	"target",
	"dist",
	"build",
}

// EditorTemp are glob patterns (matched against a path's basename) for transient
// editor artifacts — swap, autosave, lock and backup files. They are not project
// content. Without them, an editor temp that outlives the debounce window (e.g.
// vim's .swp, open for the whole session, or emacs's #file# autosave) would get
// its own spurious revision + delete on every edit. The common atomic-save
// pattern (write temp, rename over the target) is already clean because the temp
// is gone before the debounce fires; these patterns cover the long-lived ones.
var EditorTemp = []string{
	"*.swp", "*.swo", "*.swn", // vim swap
	"*~",               // emacs/gedit/joe/nano backup
	".#*",              // emacs lock
	"#*#",              // emacs autosave
	"4913",             // vim write-permission probe file
	".goutputstream-*", // GNOME / gedit
	".~lock.*#",        // LibreOffice
}

// Matcher answers whether a path should be ignored.
type Matcher struct {
	root     string
	defaults map[string]struct{}
	gi       *gitignore.GitIgnore // nil if no .gitignore present
}

// New builds a Matcher for the project rooted at root, loading root/.gitignore
// if it exists.
func New(root string) *Matcher {
	m := &Matcher{
		root:     root,
		defaults: make(map[string]struct{}, len(Defaults)),
	}
	for _, d := range Defaults {
		m.defaults[d] = struct{}{}
	}
	if gi, err := gitignore.CompileIgnoreFile(filepath.Join(root, ".gitignore")); err == nil {
		m.gi = gi
	}
	return m
}

// Match reports whether path (absolute or relative to root) should be ignored.
// A path is ignored if any of its components is a default exclude, or if the
// .gitignore matches it.
func (m *Matcher) Match(path string) bool {
	rel := path
	if filepath.IsAbs(path) {
		if r, err := filepath.Rel(m.root, path); err == nil {
			rel = r
		}
	}
	rel = filepath.ToSlash(rel)

	if rel == "." || rel == "" {
		return false
	}
	// Anything outside the root: be safe and ignore it.
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return true
	}

	parts := strings.Split(rel, "/")
	for _, comp := range parts {
		if _, ok := m.defaults[comp]; ok {
			return true
		}
	}

	// Transient editor artifacts, matched on the basename.
	base := parts[len(parts)-1]
	for _, p := range EditorTemp {
		if ok, _ := filepath.Match(p, base); ok {
			return true
		}
	}

	if m.gi != nil && m.gi.MatchesPath(rel) {
		return true
	}
	return false
}
