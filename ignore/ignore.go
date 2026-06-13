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

	for _, comp := range strings.Split(rel, "/") {
		if _, ok := m.defaults[comp]; ok {
			return true
		}
	}

	if m.gi != nil && m.gi.MatchesPath(rel) {
		return true
	}
	return false
}
