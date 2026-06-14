//go:build linux

package watch

import (
	"io/fs"
	"syscall"
)

// ctimeNano returns the file's inode change time in unix nanoseconds, or 0 if
// it cannot be read. ctime advances on every content write and cannot be set
// backwards by utimes, so it is the signal that closes the sweep's mtime blind
// spot (see fileState).
func ctimeNano(fi fs.FileInfo) int64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return int64(st.Ctim.Sec)*1_000_000_000 + int64(st.Ctim.Nsec)
}
