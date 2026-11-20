package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/model"
)

// ManifestStore persists run manifests and status snapshots under Root. Writes
// are redacted and atomic. Save and Transition create the run directory when
// missing; Create refuses an existing one.
type ManifestStore struct {
	Root string
}

// NewManifestStore returns a manifest store rooted at root.
func NewManifestStore(root string) ManifestStore {
	return ManifestStore{Root: root}
}

// RunDir returns the legacy artifact directory path for runID.
func (s ManifestStore) RunDir(runID string) string {
	return filepath.Join(s.Root, runID)
}

// RunDirFor returns the artifact path recorded in manifest, falling back to its run ID.
func (s ManifestStore) RunDirFor(manifest model.RunManifest) string {
	directory := manifest.ArtifactDirectory
	if strings.TrimSpace(directory) == "" {
		directory = manifest.RunID
	}
	return filepath.Join(s.Root, directory)
}

// ArtifactDirectoryName builds a provider-and-time-based directory name for a
// run as <provider>-run-<HH:MM>_<D.M.YYYY>_<runID>. An empty provider becomes
// "run".
func ArtifactDirectoryName(provider string, createdAt time.Time, runID string) string {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "run"
	}
	return fmt.Sprintf("%s-run-%s_%s", provider, createdAt.Format("15:04")+"_"+createdAt.Format("2.1.2006"), runID)
}

// Create initializes and persists a run manifest. A zero CreatedAt defaults to
// the current time and an empty ArtifactDirectory is generated. It creates a new
// run directory, writes manifest.json and status.json, and fails if the
// directory already exists.
func (s ManifestStore) Create(manifest *model.RunManifest) error {
	if manifest == nil || strings.TrimSpace(manifest.RunID) == "" {
		return errors.New("run ID is required")
	}
	if manifest.CreatedAt.IsZero() {
		manifest.CreatedAt = time.Now().UTC()
	}
	if strings.TrimSpace(manifest.ArtifactDirectory) == "" {
		manifest.ArtifactDirectory = ArtifactDirectoryName(manifest.Provider, manifest.CreatedAt, manifest.RunID)
	}
	if err := validateArtifactDirectory(manifest.ArtifactDirectory); err != nil {
		return err
	}
	manifest.UpdatedAt = manifest.CreatedAt
	if err := os.MkdirAll(s.Root, 0o750); err != nil {
		return err
	}
	runDir := s.RunDirFor(*manifest)
	if _, err := os.Lstat(runDir); err == nil {
		return fmt.Errorf("run artifact directory %s already exists", runDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(runDir, 0o750); err != nil {
		return err
	}
	if err := s.writeJSON(filepath.Join(s.RunDirFor(*manifest), "manifest.json"), manifest); err != nil {
		return err
	}
	if err := s.writeStatus(*manifest); err != nil {
		return err
	}
	return nil
}

// Save updates a run manifest and its status file. It fills an empty
// ArtifactDirectory, sets UpdatedAt to the current time, and creates the run
// directory when absent.
func (s ManifestStore) Save(manifest *model.RunManifest) error {
	if manifest == nil || manifest.RunID == "" {
		return errors.New("run ID is required")
	}
	if strings.TrimSpace(manifest.ArtifactDirectory) == "" {
		manifest.ArtifactDirectory = ArtifactDirectoryName(manifest.Provider, manifest.CreatedAt, manifest.RunID)
	}
	if err := validateArtifactDirectory(manifest.ArtifactDirectory); err != nil {
		return err
	}
	manifest.UpdatedAt = time.Now().UTC()
	if err := os.MkdirAll(s.RunDirFor(*manifest), 0o750); err != nil {
		return err
	}
	if err := s.writeJSON(filepath.Join(s.RunDirFor(*manifest), "manifest.json"), manifest); err != nil {
		return err
	}
	return s.writeStatus(*manifest)
}

// Transition validates and persists a run's next phase. The in-memory
// manifest keeps its previous phase unless persistence succeeds.
func (s ManifestStore) Transition(manifest *model.RunManifest, phase model.Phase) error {
	if manifest == nil {
		return errors.New("manifest is nil")
	}
	if err := model.ValidateTransition(manifest.Phase, phase); err != nil {
		return err
	}
	updated := *manifest
	updated.Phase = phase
	if err := s.Save(&updated); err != nil {
		return err
	}
	*manifest = updated
	return nil
}

// Load reads the manifest for runID from its artifact directory. It checks the
// legacy root/<runID> path first, then scans permanent run directories and
// ignores the latest aggregate and dot-prefixed entries.
func (s ManifestStore) Load(runID string) (model.RunManifest, error) {
	directory, err := s.findRunDir(runID)
	if err != nil {
		return model.RunManifest{}, err
	}
	data, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		return model.RunManifest{}, err
	}
	var manifest model.RunManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return model.RunManifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	return manifest, nil
}

// validateArtifactDirectory rejects paths that are not a single directory name.
func validateArtifactDirectory(directory string) error {
	if strings.TrimSpace(directory) == "" || directory == "." || directory == ".." || filepath.IsAbs(directory) || filepath.Base(directory) != directory || strings.ContainsAny(directory, `/\\`) {
		return errors.New("artifact directory must be a single directory name")
	}
	return nil
}

// findRunDir locates the artifact directory containing runID's manifest.
func (s ManifestStore) findRunDir(runID string) (string, error) {
	legacy := s.RunDir(runID)
	if info, err := os.Stat(filepath.Join(legacy, "manifest.json")); err == nil && !info.IsDir() {
		return legacy, nil
	}
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == latestDirectory || strings.HasPrefix(entry.Name(), ".") {
			// A legacy provider latest alias is not a permanent run directory.
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Root, entry.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		var manifest model.RunManifest
		if json.Unmarshal(data, &manifest) == nil && manifest.RunID == runID {
			return filepath.Join(s.Root, entry.Name()), nil
		}
	}
	return "", fmt.Errorf("run %s not found: %w", runID, os.ErrNotExist)
}

// writeStatus writes the compact status snapshot for manifest.
func (s ManifestStore) writeStatus(manifest model.RunManifest) error {
	status := struct {
		RunID     string          `json:"runID"`
		Phase     model.Phase     `json:"phase"`
		UpdatedAt time.Time       `json:"updatedAt"`
		Failure   *model.Failure  `json:"failure,omitempty"`
		Deferred  *model.Deferral `json:"deferred,omitempty"`
	}{manifest.RunID, manifest.Phase, manifest.UpdatedAt, manifest.Failure, manifest.Deferred}
	if err := s.writeJSON(filepath.Join(s.RunDirFor(manifest), "status.json"), status); err != nil {
		return err
	}
	return nil
}

// writeJSON redacts value and atomically writes its indented JSON representation.
func (s ManifestStore) writeJSON(path string, value any) error {
	data, err := RedactedJSON(value)
	if err != nil {
		return err
	}
	return s.atomicWrite(path, append(data, '\n'), 0o640)
}

// atomicWrite replaces path with data through a synced temporary file.
func (s ManifestStore) atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lkmabt-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
