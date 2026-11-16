//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package evidence

import "errors"

// acquireLatestUpdateLock reports that cross-process artifact locking is unsupported on this platform.
func acquireLatestUpdateLock(string) (func(), error) {
	return nil, errors.New("cross-process latest artifact locking is unsupported on this platform")
}
