package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/redhat-appstudio/lkmabt/internal/evidence"
)

type aggregateRun struct {
	RunID     string
	Name      string
	Directory string
	StartedAt time.Time
	Log       *runLog
}

// newAggregateRun creates the aggregate run directory, its run log, the recorded
// start-time marker, and the latest pointer. Marker and pointer failures are
// logged warnings; only directory or run-log creation failures fail the run.
func newAggregateRun(stateDir string) (*aggregateRun, error) {
	startedAt := time.Now().UTC()
	runID := "run-" + uuid.NewString()
	name := evidence.ArtifactDirectoryName("multi", startedAt, runID)
	directory := filepath.Join(stateDir, name)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, fmt.Errorf("create aggregate run directory %s: %w", directory, err)
	}
	log, err := newAttachedRunLog(filepath.Join(directory, "run.log"))
	if err != nil {
		return nil, err
	}
	store := evidence.NewManifestStore(stateDir)
	if err := store.RecordAggregateStart(name, startedAt); err != nil {
		fmt.Fprintf(log, "warning: could not record aggregate start time: %v\n", err)
	}
	// A stale latest pointer never blocks the run; it is only a convenience alias.
	if err := store.PointLatestAt(name); err != nil {
		fmt.Fprintf(log, "warning: could not update the latest pointer: %v\n", err)
	}
	return &aggregateRun{RunID: runID, Name: name, Directory: directory, StartedAt: startedAt, Log: log}, nil
}

// reportPaths returns the Markdown and HTML report paths for the aggregate run.
func (a *aggregateRun) reportPaths() (string, string) {
	return filepath.Join(a.Directory, "report.md"), filepath.Join(a.Directory, "report.html")
}
