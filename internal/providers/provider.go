// Package providers defines the fixture repository contract implemented by the
// GitHub and GitLab adapters, the shared retry policy for provider 5xx
// responses, and the commit subjects the harness writes.
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

// ErrTokenCannotListInstallationRepositories marks the expected limitation of
// gh OAuth user tokens, which GitHub rejects for installation repository listing.
// It is wrapped with ErrUnverifiable and is not a user-facing warning.
var ErrTokenCannotListInstallationRepositories = errors.New("GitHub token cannot list installation repositories")

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

// SourceRepository identifies the repository a component is sourced from. URL
// retains the address as supplied.
type SourceRepository struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
	URL   string `json:"url"`
}

// FixtureRepository identifies the test fixture repository a provider operates
// on. URL may be empty; ValidateFixture accepts an omitted URL and returns the
// canonical one.
type FixtureRepository struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
	URL   string `json:"url"`
}

// FileVersion is file content paired with the version identifier of the commit
// that last changed it. SHA is provider-specific: GitHub returns the blob SHA
// from the contents API, GitLab the last commit touching the file and falls
// back to the commit ID. Callers pass SHA as expectedSHA to UpdateFile. URL is
// the GitHub HTML URL; GitLab reports the project-relative file path.
type FileVersion struct {
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Content string `json:"content"`
	SHA     string `json:"sha"`
	URL     string `json:"url,omitempty"`
}

const (
	// CommitMessageTrigger is the subject of the commit that triggers a build.
	CommitMessageTrigger = "lkmabt trigger"
	// CommitMessageConfigure is the subject of configuration commits. The
	// [skip ci] directive keeps a platform change from creating a throwaway
	// build per component.
	CommitMessageConfigure = "lkmabt configure [skip ci]"
)

// Commit describes a commit created by UpdateFile. GitLab leaves URL empty.
// CreatedAt is captured locally when the adapter observes the commit, not the
// timestamp reported by the provider.
type Commit struct {
	SHA       string    `json:"sha"`
	URL       string    `json:"url,omitempty"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
}

// Provider is the write path into a fixture repository. UpdateFile uses
// optimistic concurrency: it refuses to overwrite a file whose version changed
// after it was read. Every call uses ctx for its HTTP requests and for any
// internal retry wait.
type Provider interface {
	// ValidateFixture confirms the fixture is the provider's canonical test
	// repository and returns its canonical owner, name, and URL. An empty
	// fixture URL is accepted.
	ValidateFixture(context.Context, FixtureRepository) (FixtureRepository, error)
	// ReadFile returns the content of the path on the branch and the SHA
	// identifying its current version. Callers pass that SHA as expectedSHA to
	// UpdateFile.
	ReadFile(context.Context, FixtureRepository, string, string) (FileVersion, error)
	// UpdateFile replaces the content of the path on the branch only when the
	// file's current SHA equals expectedSHA, committing with message as the
	// subject. The arguments are fixture, path, branch, content, expectedSHA,
	// then message. A blank or stale expectedSHA returns an error and no commit
	// is created.
	UpdateFile(context.Context, FixtureRepository, string, string, string, string, string) (Commit, error)
}

// ParseRepository extracts owner and name from the last two path segments of a
// repository URL and trims a trailing .git from the name. It rejects a blank
// URL, a URL without a host, and one with fewer than two path segments.
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

// ValidateExpectedSHA returns an error when expected is blank or differs from
// actual, preventing an overwrite of a file that changed after it was read.
func ValidateExpectedSHA(expected, actual string) error {
	if strings.TrimSpace(expected) == "" {
		return fmt.Errorf("expected file version is required")
	}
	if expected != actual {
		return fmt.Errorf("stale file version: expected %q, got %q", expected, actual)
	}
	return nil
}
