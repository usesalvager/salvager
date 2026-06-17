package main

import "os"

// selfExe returns the path of the running salvager binary WITHOUT resolving
// symlinks. A Homebrew install puts a stable symlink on PATH that points into a
// versioned Cellar directory; resolving it (filepath.EvalSymlinks) would pin
// whatever we write — an MCP registration, a service unit — to that versioned
// path, which breaks on the next `brew upgrade`. The unresolved path follows the
// symlink and survives upgrades. Same reasoning as the MCP registration in init.
func selfExe() (string, error) {
	return os.Executable()
}
