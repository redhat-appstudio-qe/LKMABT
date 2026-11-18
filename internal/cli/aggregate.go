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

type aggregateRun struct {
	RunID     string
	Directory string
	StartedAt time.Time
	Log       *runLog
	release   func()
}

// newAggregateRun rotates the previous latest aggregate directory in place,
// creates a fresh real latest directory, and opens its run log. The returned
// run holds the state-directory lock until Close. If latest preparation fails
// for a reason other than another live run, the run falls back to a permanent
// directory and records a warning in the run log.
func newAggregateRun(stateDir string) (*aggregateRun, error) {
	startedAt := time.Now().UTC()
	runID := "run-" + uuid.NewString()
	store := evidence.NewManifestStore(stateDir)
	directory, release, prepareErr := store.PrepareLatest(runID, startedAt)
	if prepareErr != nil {
		if errors.Is(prepareErr, evidence.ErrLatestLocked) {
			return nil, fmt.Errorf("another LKMABT run is using state directory %s; wait for it to finish or pass a different --state-dir", stateDir)
		}
		name := evidence.ArtifactDirectoryName("multi", startedAt, runID)
		directory = filepath.Join(stateDir, name)
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return nil, fmt.Errorf("create aggregate run directory %s: %w", directory, err)
		}
	}
	log, err := newAttachedRunLog(filepath.Join(directory, "run.log"))
	if err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	if prepareErr != nil {
		fmt.Fprintf(log, "warning: could not prepare the latest aggregate directory: %v\n", prepareErr)
	}
	return &aggregateRun{RunID: runID, Directory: directory, StartedAt: startedAt, Log: log, release: release}, nil
}

// Close closes the run log and releases the state-directory lock.
func (a *aggregateRun) Close() error {
	err := a.Log.Close()
	if a.release != nil {
		a.release()
		a.release = nil
	}
	return err
}

// reportPaths returns the Markdown and HTML report paths for the aggregate run.
func (a *aggregateRun) reportPaths() (string, string) {
	return filepath.Join(a.Directory, "report.md"), filepath.Join(a.Directory, "report.html")
}
