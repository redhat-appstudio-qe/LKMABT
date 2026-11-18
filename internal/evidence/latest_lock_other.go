//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package evidence

import "errors"

// acquireLatestUpdateLock reports that cross-process aggregate latest locking is unsupported on this platform.
func acquireLatestUpdateLock(string) (func(), error) {
	return nil, errors.New("cross-process aggregate latest locking is unsupported on this platform")
}
