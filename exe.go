package main

import (
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path via a temp file in the same directory then
// renames over the destination, so a reader never observes a half-written file
// and a crash mid-write leaves the previous contents intact. mode sets the
// permission bits. Shared by everything package main writes to disk (the user
// CLAUDE.md, launchd plists, systemd units).
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".salvager-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// selfExe returns the path of the running salvager binary WITHOUT resolving
// symlinks. A Homebrew install puts a stable symlink on PATH that points into a
// versioned Cellar directory; resolving it (filepath.EvalSymlinks) would pin
// whatever we write — an MCP registration, a service unit — to that versioned
// path, which breaks on the next `brew upgrade`. The unresolved path follows the
// symlink and survives upgrades. Same reasoning as the MCP registration in init.
func selfExe() (string, error) {
	return os.Executable()
}
