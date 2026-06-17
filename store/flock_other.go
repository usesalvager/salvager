//go:build !unix

// Salvager releases for darwin and linux only (the CI/release matrix). On any
// other platform cross-process locking is unimplemented, and the store refuses
// to operate rather than silently running WITHOUT the lock — running unlocked is
// the exact corruption this package exists to prevent. A no-op stub would be a
// trap; a loud error is honest.
package store

import (
	"errors"
	"time"
)

type flockHandle struct{}

func (h *flockHandle) release() {}

func acquireFlock(_ string, _ bool, _ time.Duration) (*flockHandle, error) {
	return nil, errors.New("salvager: cross-process store locking is unsupported on this platform (darwin/linux only)")
}
