//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package evidence

import "errors"

// acquireLatestUpdateLock reports that cross-process aggregate latest locking is
// unsupported on this platform. LockLatest and PrepareLatest therefore fail
// here, while reading a run by ID remains available.
func acquireLatestUpdateLock(string) (func(), error) {
	return nil, errors.New("cross-process aggregate latest locking is unsupported on this platform")
}
