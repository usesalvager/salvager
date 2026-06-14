//go:build !linux && !darwin

package watch

import "io/fs"

// ctimeNano returns 0 on platforms where Go does not expose a portable inode
// change time (Windows, Plan 9). The sweep's change gate then degrades to
// mtime+size — correct for the common case; the residual blind spot is the same
// one documented on fileState, and the real-time path is unaffected.
func ctimeNano(fs.FileInfo) int64 { return 0 }
