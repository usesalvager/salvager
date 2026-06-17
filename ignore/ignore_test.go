package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultExcludes(t *testing.T) {
	root := t.TempDir()
	m := New(root)

	ignored := []string{
		"node_modules/react/index.js",
		".git/config",
		".salvager/objects/abc",
		"vendor/foo.php",
		"src/__pycache__/x.pyc",
		"target/debug/app",
	}
	for _, p := range ignored {
		if !m.Match(filepath.Join(root, p)) {
			t.Errorf("expected %q ignored", p)
		}
	}

	tracked := []string{"src/main.go", "README.md", "lib/util.py"}
	for _, p := range tracked {
		if m.Match(filepath.Join(root, p)) {
			t.Errorf("expected %q tracked, got ignored", p)
		}
	}
}

func TestGitignoreRespected(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.log\nsecret/\n"), 0o644)
	m := New(root)

	if !m.Match(filepath.Join(root, "debug.log")) {
		t.Error("*.log should be ignored")
	}
	if !m.Match(filepath.Join(root, "secret/key.txt")) {
		t.Error("secret/ should be ignored")
	}
	if m.Match(filepath.Join(root, "app.go")) {
		t.Error("app.go should be tracked")
	}
}

// A directory-only pattern ("secret/") must match the directory ITSELF via
// MatchDir, so the walker prunes the whole subtree instead of descending into a
// gitignored directory. Match (file semantics) must NOT match a bare file named
// "secret", or it would wrongly stop tracking that file.
func TestGitignoreDirectoryPattern(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("secret/\n"), 0o644)
	m := New(root)

	if !m.MatchDir(filepath.Join(root, "secret")) {
		t.Error("MatchDir(secret) should be true so the dir subtree is pruned")
	}
	if m.Match(filepath.Join(root, "secret")) {
		t.Error("Match(secret) as a file must be false (a file named secret is not a dir)")
	}
	// Contents are excluded either way.
	if !m.Match(filepath.Join(root, "secret/key.txt")) {
		t.Error("secret/key.txt should be ignored")
	}
}

func TestRootItself(t *testing.T) {
	root := t.TempDir()
	m := New(root)
	if m.Match(root) {
		t.Error("root dir should not be ignored")
	}
}

func TestEditorTempIgnored(t *testing.T) {
	root := t.TempDir()
	m := New(root)

	ignored := []string{
		"a.txt.swp", "a.txt.swo", ".a.txt.swp", // vim swap (hidden or not)
		"notes~", "src/main.go~", // backups, incl. nested
		".#main.go",             // emacs lock
		"#main.go#",             // emacs autosave
		"4913",                  // vim probe
		".goutputstream-AB12CD", // gnome/gedit
		".~lock.report.odt#",    // libreoffice
	}
	for _, p := range ignored {
		if !m.Match(filepath.Join(root, p)) {
			t.Errorf("expected editor-temp %q ignored", p)
		}
	}

	// Real files that merely resemble the patterns must still be tracked.
	tracked := []string{"main.go", "a.swift", "swap.go", "issue4913.md", "lock.json"}
	for _, p := range tracked {
		if m.Match(filepath.Join(root, p)) {
			t.Errorf("expected %q tracked, got ignored", p)
		}
	}
}
