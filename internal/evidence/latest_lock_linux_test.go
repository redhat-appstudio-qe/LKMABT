//go:build linux

package evidence

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	latestLockRootEnv = "LKMABT_LATEST_LOCK_ROOT"
	latestLockRunEnv  = "LKMABT_LATEST_LOCK_RUN"
)

var latestLockCandidateStartedAt = time.Date(2026, 10, 8, 12, 5, 0, 0, time.UTC)

// TestPrepareLatestFailsWhileCrossProcessLockHeld verifies that a second
// process is refused instead of writing into a live run's latest directory.
func TestPrepareLatestFailsWhileCrossProcessLockHeld(t *testing.T) {
	if root := os.Getenv(latestLockRootEnv); root != "" {
		store := NewManifestStore(root)
		_, release, err := store.PrepareLatest(os.Getenv(latestLockRunEnv), latestLockCandidateStartedAt)
		if release != nil {
			release()
		}
		if !errors.Is(err, ErrLatestLocked) {
			t.Fatalf("PrepareLatest error = %v, want ErrLatestLocked", err)
		}
		if err := os.WriteFile(filepath.Join(root, "child-refused"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}

	root := t.TempDir()
	store := NewManifestStore(root)
	baselineStartedAt := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	_, release, err := store.PrepareLatest("baseline", baselineStartedAt)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	command := exec.Command(os.Args[0], "-test.run=^TestPrepareLatestFailsWhileCrossProcessLockHeld$", "-test.v")
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, latestLockRootEnv+"=") || strings.HasPrefix(value, latestLockRunEnv+"=") {
			continue
		}
		command.Env = append(command.Env, value)
	}
	command.Env = append(command.Env, latestLockRootEnv+"="+root, latestLockRunEnv+"=candidate")
	var childOutput bytes.Buffer
	command.Stdout = &childOutput
	command.Stderr = &childOutput
	finished := make(chan error, 1)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { finished <- command.Wait() }()

	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("child failed: %v; output: %s", err, childOutput.String())
		}
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("child blocked instead of failing fast on the held latest lock")
	}
	if _, err := os.Stat(filepath.Join(root, "child-refused")); err != nil {
		t.Fatalf("child did not confirm the held lock: %v; output: %s", err, childOutput.String())
	}
	metadata, err := readAggregateMetadata(filepath.Join(root, "latest"))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.RunID != "baseline" {
		t.Fatalf("latest run = %q, want baseline", metadata.RunID)
	}
	if _, err := os.Stat(filepath.Join(root, ArtifactDirectoryName("multi", latestLockCandidateStartedAt, "candidate"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused child still created a candidate directory: %v", err)
	}

	release()
	released = true
	candidate, candidateRelease, err := store.PrepareLatest("candidate", latestLockCandidateStartedAt)
	if err != nil {
		t.Fatal(err)
	}
	defer candidateRelease()
	if candidate != filepath.Join(root, "latest") {
		t.Fatalf("candidate directory = %q, want latest", candidate)
	}
	metadata, err = readAggregateMetadata(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.RunID != "candidate" {
		t.Fatalf("latest run = %q, want candidate", metadata.RunID)
	}
	rotated := filepath.Join(root, ArtifactDirectoryName("multi", baselineStartedAt, "baseline"))
	if info, err := os.Stat(rotated); err != nil || !info.IsDir() {
		t.Fatalf("baseline was not rotated to %s: %v", rotated, err)
	}
}
