// Package filelock provides non-blocking, advisory, exclusive file locks shared
// across processes. TryAcquire never waits: it reports ErrLocked when another
// holder has the lock. Advisory means programs that ignore the lock can still
// touch the file.
package filelock

import "errors"

// ErrLocked reports that another process holds the lock. Errors returned by
// TryAcquire wrap it, so callers match with errors.Is.
var ErrLocked = errors.New("lock is held by another process")
