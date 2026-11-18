//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package filelock

import "errors"

// TryAcquire reports that cross-process file locking is unsupported on this platform.
func TryAcquire(string) (func(), error) {
	return nil, errors.New("cross-process file locking is unsupported on this platform")
}
