//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package evidence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// acquireLatestUpdateLock obtains an exclusive cross-process flock on
// <root>/.latest.lock and returns its release function. The lock file is
// created when absent, opened without following symlinks, and must be a regular
// file. It fails with ErrLatestLocked instead of waiting when another process
// holds the lock.
func acquireLatestUpdateLock(root string) (func(), error) {
	lockPath := filepath.Join(root, ".latest.lock")
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	lockFile := os.NewFile(uintptr(fd), lockPath)
	if lockFile == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open lock file: invalid file descriptor")
	}
	info, err := lockFile.Stat()
	if err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("inspect lock file: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = lockFile.Close()
		return nil, fmt.Errorf("lock path is not a regular file")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lockFile.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrLatestLocked
		}
		return nil, fmt.Errorf("acquire file lock: %w", err)
	}
	return func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}
