package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// latestDirectory is the name of the live aggregate run directory under the state root.
const latestDirectory = "latest"

// aggregateMetadataName is the marker holding the identity of the run currently in latest.
const aggregateMetadataName = ".aggregate.json"

// ErrLatestLocked reports that another process is using the state directory's latest run.
var ErrLatestLocked = errors.New("latest aggregate directory is used by another process")

// aggregateMetadata identifies the aggregate run occupying the latest directory.
type aggregateMetadata struct {
	RunID     string    `json:"runID"`
	StartedAt time.Time `json:"startedAt"`
}

// LockLatest acquires the state-directory lock without changing latest. Callers
// use it to keep the latest directory stable while reading a run from it. It
// returns ErrLatestLocked when another process holds the lock.
func (s ManifestStore) LockLatest() (func(), error) {
	if err := os.MkdirAll(s.Root, 0o750); err != nil {
		return nil, err
	}
	unlock, err := acquireLatestUpdateLock(s.Root)
	if err != nil {
		if errors.Is(err, ErrLatestLocked) {
			return nil, ErrLatestLocked
		}
		return nil, fmt.Errorf("lock latest aggregate update: %w", err)
	}
	return unlock, nil
}

// PrepareLatest rotates any existing latest aggregate directory in place to its
// permanent name and creates a fresh real latest directory for runID. The
// returned release function must be called when the run no longer writes into
// latest; it releases the state-directory lock. Rotation preserves the previous
// run's logs, reports, and artifacts: a real directory is renamed according to
// its metadata, a legacy symlink is removed without touching its target, and an
// empty directory is discarded. It returns ErrLatestLocked when another process
// holds the state directory, and an error when runID is empty, startedAt is
// zero, latest is neither a directory nor a symlink, or rotation would
// overwrite an existing run directory.
func (s ManifestStore) PrepareLatest(runID string, startedAt time.Time) (string, func(), error) {
	if strings.TrimSpace(runID) == "" {
		return "", nil, errors.New("run ID is required")
	}
	if startedAt.IsZero() {
		return "", nil, errors.New("aggregate start time is required")
	}
	unlock, err := s.LockLatest()
	if err != nil {
		return "", nil, err
	}
	release := false
	defer func() {
		if !release {
			unlock()
		}
	}()
	if err := s.rotateLatest(); err != nil {
		return "", nil, err
	}
	latestPath := filepath.Join(s.Root, latestDirectory)
	if err := os.Mkdir(latestPath, 0o750); err != nil {
		return "", nil, fmt.Errorf("create latest aggregate directory: %w", err)
	}
	metadata := aggregateMetadata{RunID: runID, StartedAt: startedAt.UTC()}
	if err := s.writeJSON(filepath.Join(latestPath, aggregateMetadataName), metadata); err != nil {
		return "", nil, err
	}
	release = true
	return latestPath, unlock, nil
}

// rotateLatest moves the current latest aggregate directory to its permanent
// name so a fresh latest can be created. Callers hold the latest update lock.
func (s ManifestStore) rotateLatest() error {
	latestPath := filepath.Join(s.Root, latestDirectory)
	info, err := os.Lstat(latestPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// A legacy alias only points at a permanent run directory; remove the link.
		return os.Remove(latestPath)
	}
	if !info.IsDir() {
		return fmt.Errorf("latest aggregate path %s is not a directory", latestPath)
	}
	entries, err := os.ReadDir(latestPath)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return os.Remove(latestPath)
	}
	target := filepath.Join(s.Root, s.rotatedDirectoryName(latestPath, info))
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("rotate latest aggregate directory: %s already exists", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(latestPath, target); err != nil {
		return fmt.Errorf("rotate latest aggregate directory: %w", err)
	}
	return nil
}

// rotatedDirectoryName derives the permanent name for the run in latest.
// Missing or corrupt metadata falls back to the directory modification time
// and a generated run ID so the data is preserved.
func (s ManifestStore) rotatedDirectoryName(latestPath string, info os.FileInfo) string {
	metadata, err := readAggregateMetadata(latestPath)
	if err == nil {
		name := ArtifactDirectoryName("multi", metadata.StartedAt, metadata.RunID)
		if validateArtifactDirectory(name) == nil {
			return name
		}
	}
	mtime := info.ModTime().UTC()
	return ArtifactDirectoryName("multi", mtime, "run-legacy-"+uuid.NewString())
}

// readAggregateMetadata reads the marker identifying the run in directory.
func readAggregateMetadata(directory string) (aggregateMetadata, error) {
	data, err := os.ReadFile(filepath.Join(directory, aggregateMetadataName))
	if err != nil {
		return aggregateMetadata{}, err
	}
	var metadata aggregateMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return aggregateMetadata{}, fmt.Errorf("decode aggregate metadata: %w", err)
	}
	if strings.TrimSpace(metadata.RunID) == "" || metadata.StartedAt.IsZero() {
		return aggregateMetadata{}, errors.New("aggregate metadata is incomplete")
	}
	return metadata, nil
}
