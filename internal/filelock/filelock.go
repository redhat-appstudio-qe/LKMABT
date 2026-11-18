// Package filelock provides non-blocking advisory file locks shared across processes.
package filelock

import "errors"

// ErrLocked reports that another process holds the lock.
var ErrLocked = errors.New("lock is held by another process")
