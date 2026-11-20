package clusterauth

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// CommandSpec describes one console command the ladder runs. Name is resolved
// through PATH unless it contains a path separator.
type CommandSpec struct {
	Name string
	Args []string
	// Dir is the child's working directory; empty inherits the caller's.
	Dir string
	// Env holds extra KEY=VALUE entries appended to os.Environ().
	Env []string
	// ClearEnv lists environment names removed from the inherited environment
	// before Env is appended, so a spec can reliably override ambient values.
	ClearEnv []string
}

// CommandRunner runs console commands for the authentication ladder.
// Implementations must stop the command when ctx is done.
type CommandRunner interface {
	// LookPath resolves name against PATH.
	LookPath(name string) (string, error)
	// Run executes spec with the console attached and returns its failure.
	Run(ctx context.Context, spec CommandSpec) error
	// Output executes spec without the console and returns its combined output.
	Output(ctx context.Context, spec CommandSpec) (string, error)
	// Stdout executes spec without the console and returns only its standard output.
	Stdout(ctx context.Context, spec CommandSpec) (string, error)
}

// command builds the exec.Cmd for spec with its working directory and environment.
func (spec CommandSpec) command(ctx context.Context) *exec.Cmd {
	command := exec.CommandContext(ctx, spec.Name, spec.Args...)
	command.Dir = spec.Dir
	if len(spec.Env) > 0 || len(spec.ClearEnv) > 0 {
		environment := os.Environ()
		if len(spec.ClearEnv) > 0 {
			environment = withoutEnvironment(environment, spec.ClearEnv)
		}
		// Extra environment entries ride on top of the remaining process environment.
		command.Env = append(environment, spec.Env...)
	}
	return command
}

// withoutEnvironment removes the named entries from an environment slice.
func withoutEnvironment(environment, names []string) []string {
	removed := make(map[string]struct{}, len(names))
	for _, name := range names {
		removed[name] = struct{}{}
	}
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if _, drop := removed[name]; !drop {
			filtered = append(filtered, entry)
		}
	}
	return filtered
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
	// Log optionally receives copies of a command's stdout and stderr. Run
	// serializes writes from the two streams onto it.
	Log io.Writer
}

// LookPath resolves name against PATH.
func (r ExecRunner) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// Run executes spec with inherited stdin and stdout/stderr optionally tee'd to the
// log, and waits for it to finish. Canceling ctx kills the process.
func (r ExecRunner) Run(ctx context.Context, spec CommandSpec) error {
	command := spec.command(ctx)
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

// Output executes spec without the console and returns its combined stdout and
// stderr. The captured output is returned even when the command fails.
func (r ExecRunner) Output(ctx context.Context, spec CommandSpec) (string, error) {
	output, err := spec.command(ctx).CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%s %s: %w", spec.Name, strings.Join(spec.Args, " "), err)
	}
	return string(output), nil
}

// Stdout executes spec without the console and returns only its standard output.
// Stderr is folded into the returned error text.
func (r ExecRunner) Stdout(ctx context.Context, spec CommandSpec) (string, error) {
	var stderr bytes.Buffer
	command := spec.command(ctx)
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return string(output), fmt.Errorf("%s %s: %w: %s", spec.Name, strings.Join(spec.Args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(output), nil
}
