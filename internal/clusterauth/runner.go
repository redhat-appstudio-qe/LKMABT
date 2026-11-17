package clusterauth

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// CommandSpec describes one console command the ladder runs.
type CommandSpec struct {
	Name string
	Args []string
	Dir  string
	Env  []string // extra KEY=VALUE entries appended to os.Environ()
}

// CommandRunner runs console commands for the authentication ladder.
type CommandRunner interface {
	// LookPath resolves name against PATH.
	LookPath(name string) (string, error)
	// Run executes spec with the console attached and returns its failure.
	Run(ctx context.Context, spec CommandSpec) error
}

// lockedWriter serializes concurrent writes from a command's stdout and stderr copies.
type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

// Write appends p under the lock so the wrapped writer is never entered concurrently.
func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

// ExecRunner runs commands through os/exec with the console attached.
type ExecRunner struct {
	Log io.Writer
}

// LookPath resolves name against PATH.
func (r ExecRunner) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// Run executes spec with inherited stdin and stdout/stderr optionally tee'd to the log.
func (r ExecRunner) Run(ctx context.Context, spec CommandSpec) error {
	command := exec.CommandContext(ctx, spec.Name, spec.Args...)
	command.Dir = spec.Dir
	// Extra environment entries ride on top of the process environment.
	if len(spec.Env) > 0 {
		command.Env = append(os.Environ(), spec.Env...)
	}
	out, errOut := io.Writer(os.Stdout), io.Writer(os.Stderr)
	// Tee the console streams into the log so login failures are captured for the report.
	// os/exec copies stdout and stderr from two goroutines, so both wrap one locked sink.
	if r.Log != nil {
		sink := &lockedWriter{writer: r.Log}
		out = io.MultiWriter(os.Stdout, sink)
		errOut = io.MultiWriter(os.Stderr, sink)
	}
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, out, errOut
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", spec.Name, strings.Join(spec.Args, " "), err)
	}
	return nil
}
