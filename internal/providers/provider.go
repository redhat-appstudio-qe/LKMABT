package providers

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ErrUnverifiable marks a cluster integration check that cannot be proven with
// the available credentials.
var ErrUnverifiable = errors.New("cluster integration cannot be verified")

// ErrInsufficientGitHubAccess marks a GitHub credential that GitHub rejected or
// that lacks the fixture repository permissions LKMABT requires.
var ErrInsufficientGitHubAccess = errors.New("GitHub credential cannot access the fixture organization or repository")

// AppRepositorySelectionError reports a GitHub App installation that excludes
// the fixture repository. Repeating the installation check cannot resolve it.
type AppRepositorySelectionError struct {
	AppSlug     string
	Owner       string
	Repository  string
	SettingsURL string
}

// Error names the excluded fixture repository and the page that updates the selection.
func (e *AppRepositorySelectionError) Error() string {
	return fmt.Sprintf("GitHub App %s is installed for selected repositories in %s but not for %s; add the repository from %s or select all repositories", e.AppSlug, e.Owner, e.Repository, e.SettingsURL)
}

// AppInstallationRequiredError reports a cluster GitHub App that is not
// installed for the fixture organization.
type AppInstallationRequiredError struct {
	AppSlug    string
	Owner      string
	Repository string
	InstallURL string
}

// Error names the missing installation and the page that grants it.
func (e *AppInstallationRequiredError) Error() string {
	return fmt.Sprintf("GitHub App %s is not installed for organization %s; install it from %s so it can access %s", e.AppSlug, e.Owner, e.InstallURL, e.Repository)
}

type SourceRepository struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
	URL   string `json:"url"`
}

type FixtureRepository struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
	URL   string `json:"url"`
}

type FileVersion struct {
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Content string `json:"content"`
	SHA     string `json:"sha"`
	URL     string `json:"url,omitempty"`
}

// Commit subjects used by the harness. Configuration commits skip PaC so a
// platform change does not create a throwaway build per component.
const (
	CommitMessageTrigger   = "lkmabt trigger"
	CommitMessageConfigure = "lkmabt configure [skip ci]"
)

type Commit struct {
	SHA       string    `json:"sha"`
	URL       string    `json:"url,omitempty"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
}

type Provider interface {
	// ValidateFixture verifies and returns the canonical test fixture repository.
	ValidateFixture(context.Context, FixtureRepository) (FixtureRepository, error)
	// ReadFile returns the requested file content and its current version metadata.
	ReadFile(context.Context, FixtureRepository, string, string) (FileVersion, error)
	// UpdateFile replaces file content only when its current SHA matches
	// expectedSHA, using message as the commit subject.
	UpdateFile(context.Context, FixtureRepository, string, string, string, string, string) (Commit, error)
}

// ParseRepository extracts owner and name from a repository URL.
func ParseRepository(raw string) (SourceRepository, error) {
	if strings.TrimSpace(raw) == "" {
		return SourceRepository{}, fmt.Errorf("repository is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return SourceRepository{}, fmt.Errorf("invalid repository URL %q", raw)
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 {
		return SourceRepository{}, fmt.Errorf("repository URL %q has no owner/name", raw)
	}
	return SourceRepository{Owner: parts[len(parts)-2], Name: strings.TrimSuffix(parts[len(parts)-1], ".git"), URL: raw}, nil
}

// ValidateExpectedSHA rejects an empty or stale expected file version.
func ValidateExpectedSHA(expected, actual string) error {
	if strings.TrimSpace(expected) == "" {
		return fmt.Errorf("expected file version is required")
	}
	if expected != actual {
		return fmt.Errorf("stale file version: expected %q, got %q", expected, actual)
	}
	return nil
}
