package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// RunLog buffers run output until it can be attached to the run directory's log file.
type RunLog interface {
	// Write appends bytes to the log.
	Write(p []byte) (int, error)
	// Attach redirects buffered and subsequent output to path.
	Attach(path string) error
}

type runLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	file   *os.File
}

// newRunLog returns a run log that buffers output until it is attached to a file.
func newRunLog() *runLog {
	return &runLog{}
}

// newAttachedRunLog returns a run log already writing to path.
func newAttachedRunLog(path string) (*runLog, error) {
	log := newRunLog()
	if err := log.Attach(path); err != nil {
		return nil, err
	}
	return log, nil
}

// Write appends to the attached file, or to the buffer while no file is attached.
func (l *runLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		return l.file.Write(p)
	}
	return l.buffer.Write(p)
}

// flushBuffer writes buffered output to w and drops only the bytes w accepted.
// It reports a write error or a short write, leaving unwritten bytes buffered.
// Callers hold l.mu.
func (l *runLog) flushBuffer(w io.Writer) error {
	if l.buffer.Len() == 0 {
		return nil
	}
	pending := l.buffer.Bytes()
	written, err := w.Write(pending)
	// Guard against a writer reporting a negative count alongside its error.
	if written < 0 {
		written = 0
	}
	if written > len(pending) {
		written = len(pending)
	}
	// Only the accepted bytes leave the buffer, so a retry or Close keeps the rest.
	l.buffer.Next(written)
	if err != nil {
		return err
	}
	if l.buffer.Len() > 0 {
		return fmt.Errorf("wrote %d of %d buffered bytes", written, written+l.buffer.Len())
	}
	return nil
}

// Attach creates path, flushes buffered output into it, and sends later writes there.
func (l *runLog) Attach(path string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create run log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("create run log %s: %w", path, err)
	}
	if err := l.flushBuffer(file); err != nil {
		_ = file.Close()
		return fmt.Errorf("flush run log %s: %w", path, err)
	}
	l.file = file
	return nil
}

// Close flushes and closes the attached file.
func (l *runLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	return file.Close()
}
