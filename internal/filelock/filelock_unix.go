//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package filelock

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// TryAcquire takes an exclusive, non-blocking advisory lock on path and returns
// an idempotent release function. It creates the file with mode 0600 when
// absent, refuses symlinks and non-regular files, and never waits. Contention,
// including a second acquisition in the same process, wraps ErrLocked; other
// failures describe the open, stat, or flock error. Callers must invoke release
// to unlock early; the lock also drops when the process exits.
//
// The O_NOFOLLOW and regular-file checks are deliberate: a lock file that
// points somewhere else, or is a device, is not a lock—it is a suggestion.
func TryAcquire(path string) (func(), error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	lockFile := os.NewFile(uintptr(fd), path)
	if lockFile == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open lock file %s: invalid file descriptor", path)
	}
	info, err := lockFile.Stat()
	if err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("inspect lock file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		_ = lockFile.Close()
		return nil, fmt.Errorf("lock path %s is not a regular file", path)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lockFile.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("acquire lock %s: %w", path, err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = unix.Flock(fd, unix.LOCK_UN)
			_ = lockFile.Close()
		})
	}, nil
}
