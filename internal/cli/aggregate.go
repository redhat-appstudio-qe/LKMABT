package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/redhat-appstudio/lkmabt/internal/evidence"
)

// aggregateRun holds one multi-cluster run: its identity, the directory and log
// shared by every cluster, and the state-directory lock that Close releases.
type aggregateRun struct {
	RunID     string
	Name      string
	Directory string
	StartedAt time.Time
	Log       *runLog
	release   func()
}

// newAggregateRun rotates the previous latest aggregate directory in place,
// creates a fresh real latest directory under stateDir, and opens its run log.
// On success Directory is <stateDir>/latest and the returned run holds the
// state-directory lock until Close. If latest preparation fails for a reason
// other than another live run, the run falls back to a permanent
// <stateDir>/<name> directory without the lock and records a warning in the run
// log.
func newAggregateRun(stateDir string) (*aggregateRun, error) {
	startedAt := time.Now().UTC()
	runID := "run-" + uuid.NewString()
	name := evidence.ArtifactDirectoryName("multi", startedAt, runID)
	store := evidence.NewManifestStore(stateDir)
	directory, release, prepareErr := store.PrepareLatest(runID, startedAt)
	if prepareErr != nil {
		if errors.Is(prepareErr, evidence.ErrLatestLocked) {
			return nil, fmt.Errorf("another LKMABT run is using state directory %s; wait for it to finish or pass a different --state-dir", stateDir)
		}
		directory = filepath.Join(stateDir, name)
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return nil, fmt.Errorf("create aggregate run directory %s: %w", directory, err)
		}
	}
	log, err := newAttachedRunLog(filepath.Join(directory, aggregateFileName(name, "run.log")))
	if err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	if prepareErr != nil {
		fmt.Fprintf(log, "warning: could not prepare the latest aggregate directory: %v\n", prepareErr)
	}
	return &aggregateRun{RunID: runID, Name: name, Directory: directory, StartedAt: startedAt, Log: log, release: release}, nil
}

// aggregateFileName prefixes a top-level aggregate file with the run name.
func aggregateFileName(runName, base string) string {
	return runName + "-" + base
}

// Close closes the run log and releases the state-directory lock. It is safe to
// call more than once; later calls are no-ops.
func (a *aggregateRun) Close() error {
	err := a.Log.Close()
	if a.release != nil {
		a.release()
		a.release = nil
	}
	return err
}

// reportPaths returns the run-named Markdown and HTML report paths for the aggregate run.
func (a *aggregateRun) reportPaths() (string, string) {
	return filepath.Join(a.Directory, aggregateFileName(a.Name, "report.md")), filepath.Join(a.Directory, aggregateFileName(a.Name, "report.html"))
}

// runLogName returns the file name of the aggregate run log, without a directory.
func (a *aggregateRun) runLogName() string {
	return aggregateFileName(a.Name, "run.log")
}
