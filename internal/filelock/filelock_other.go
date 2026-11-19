//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package filelock

import "errors"

// TryAcquire always fails with a non-ErrLocked error because cross-process file
// locking is unsupported on this platform; the release function is always nil.
func TryAcquire(string) (func(), error) {
	return nil, errors.New("cross-process file locking is unsupported on this platform")
}
