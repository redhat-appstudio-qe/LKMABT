// Package github implements the providers.Provider contract against the GitHub
// REST API, including GitHub App installation checks and the credential
// capability probes used before a run.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	gh "github.com/google/go-github/v66/github"
	"github.com/redhat-appstudio/lkmabt/internal/providers"
)

const (
	// CanonicalFixtureURL is the repository ValidateFixture accepts; every other
	// fixture is rejected.
	CanonicalFixtureURL = "https://github.com/redhat-appstudio-qe/dr_test_mathwizz"
	canonicalOwner      = "redhat-appstudio-qe"
	canonicalName       = "dr_test_mathwizz"
)

type client interface {
	// GetRepository looks up a repository by owner and name.
	GetRepository(context.Context, string, string) (*gh.Repository, error)
	// GetFile reads a repository path at the requested branch.
	GetFile(context.Context, string, string, string, string) (*gh.RepositoryContent, error)
	// UpdateFile commits replacement content using the current file SHA.
	UpdateFile(context.Context, string, string, string, string, string, string, string) (*gh.RepositoryContentResponse, error)
	// ListOrganizationInstallations lists the GitHub App installations for an organization.
	ListOrganizationInstallations(context.Context, string) ([]*gh.Installation, error)
	// ListUserInstallationRepositories lists the repositories the token can see for an installation.
	ListUserInstallationRepositories(context.Context, int64) ([]*gh.Repository, error)
}

type apiClient struct{ client *gh.Client }

// GetRepository fetches a repository from the GitHub API.
func (c apiClient) GetRepository(ctx context.Context, owner, name string) (*gh.Repository, error) {
	repository, _, err := c.client.Repositories.Get(ctx, owner, name)
	return repository, err
}

// GetFile fetches a file from the requested GitHub repository branch. It returns
// an error when path resolves to a directory.
func (c apiClient) GetFile(ctx context.Context, owner, name, path, branch string) (*gh.RepositoryContent, error) {
	file, _, _, err := c.client.Repositories.GetContents(ctx, owner, name, path, &gh.RepositoryContentGetOptions{Ref: branch})
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, fmt.Errorf("github path %s is a directory", path)
	}
	return file, nil
}

// UpdateFile commits content to a GitHub repository using the supplied file SHA.
// GitHub rejects the commit when the SHA no longer matches the file's current
// version.
func (c apiClient) UpdateFile(ctx context.Context, owner, name, path, content, branch, sha, message string) (*gh.RepositoryContentResponse, error) {
	response, _, err := c.client.Repositories.UpdateFile(ctx, owner, name, path, &gh.RepositoryContentFileOptions{Message: &message, Content: []byte(content), Branch: &branch, SHA: &sha})
	return response, err
}

// ListOrganizationInstallations fetches every installation page for an organization.
func (c apiClient) ListOrganizationInstallations(ctx context.Context, org string) ([]*gh.Installation, error) {
	var installations []*gh.Installation
	options := &gh.ListOptions{PerPage: 100}
	for {
		page, response, err := c.client.Organizations.ListInstallations(ctx, org, options)
		if err != nil {
			return nil, err
		}
		if page != nil {
			installations = append(installations, page.Installations...)
		}
		if response == nil || response.NextPage == 0 {
			return installations, nil
		}
		options.Page = response.NextPage
	}
}

// ListUserInstallationRepositories fetches every repository page visible for an installation.
func (c apiClient) ListUserInstallationRepositories(ctx context.Context, id int64) ([]*gh.Repository, error) {
	var repositories []*gh.Repository
	options := &gh.ListOptions{PerPage: 100}
	for {
		page, response, err := c.client.Apps.ListUserRepos(ctx, id, options)
		if err != nil {
			return nil, err
		}
		if page != nil {
			repositories = append(repositories, page.Repositories...)
		}
		if response == nil || response.NextPage == 0 {
			return repositories, nil
		}
		options.Page = response.NextPage
	}
}

// Adapter implements the fixture repository contract against GitHub using the
// supplied API client and retry policy.
type Adapter struct {
	client client
	retry  providers.RetryPolicy
	// oauthToken is true for gh OAuth user tokens (gho_*). The installation
	// repository endpoint rejects them, so that capability is not required.
	oauthToken bool
}

// New creates a GitHub adapter with the default retry policy.
func New(client client) *Adapter {
	return NewWithRetry(client, providers.DefaultRetryPolicy())
}

// NewWithRetry creates a GitHub adapter with the supplied retry policy. A zero
// policy retries nothing.
func NewWithRetry(client client, retry providers.RetryPolicy) *Adapter {
	return &Adapter{client: client, retry: retry}
}

// NewFromEnv creates a GitHub adapter using GITHUB_TOKEN.
func NewFromEnv() (*Adapter, error) { return NewFromCredentials(os.Getenv("GITHUB_TOKEN")) }

// NewFromCredentials checks that token is non-empty and creates a GitHub adapter
// for it. It makes no API request, so credential capability is established by
// CheckOrganizationAccess, CheckFixtureWriteAccess, and
// CheckInstallationRepositoryAccess. Tokens with the gho_ prefix are recorded as
// gh OAuth user tokens, for which installation repository listing is skipped.
func NewFromCredentials(token string) (*Adapter, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN is required")
	}
	adapter := New(apiClient{client: gh.NewClient(nil).WithAuthToken(token)})
	adapter.oauthToken = strings.HasPrefix(token, "gho_")
	return adapter, nil
}

// ValidateFixture checks that fixture names the canonical GitHub fixture, fetches
// it, and returns the canonical identity. fixture.URL may be empty, but when set
// it must equal CanonicalFixtureURL. 5xx responses are retried; other read
// errors are returned wrapped.
func (a *Adapter) ValidateFixture(ctx context.Context, fixture providers.FixtureRepository) (providers.FixtureRepository, error) {
	if a == nil || a.client == nil {
		return providers.FixtureRepository{}, fmt.Errorf("github client is required")
	}
	if fixture.Owner != canonicalOwner || fixture.Name != canonicalName || (fixture.URL != "" && fixture.URL != CanonicalFixtureURL) {
		return providers.FixtureRepository{}, fmt.Errorf("github fixture must be %s", CanonicalFixtureURL)
	}
	var repository *gh.Repository
	err := providers.RetryOn5xx(ctx, a.retry, func() error {
		var err error
		repository, err = a.client.GetRepository(ctx, canonicalOwner, canonicalName)
		return err
	}, isGitHub5xx)
	if err != nil {
		return providers.FixtureRepository{}, fmt.Errorf("validate github fixture: %w", err)
	}
	if repository.GetOwner().GetLogin() != canonicalOwner || repository.GetName() != canonicalName {
		return providers.FixtureRepository{}, fmt.Errorf("github API returned unexpected fixture identity")
	}
	return providers.FixtureRepository{Owner: canonicalOwner, Name: canonicalName, URL: CanonicalFixtureURL}, nil
}

// listInstallations fetches the App installations for the fixture organization,
// marking credential rejections as insufficient access.
func (a *Adapter) listInstallations(ctx context.Context) ([]*gh.Installation, error) {
	installations, err := a.client.ListOrganizationInstallations(ctx, canonicalOwner)
	if err != nil {
		if isAccessRejection(err) {
			return nil, fmt.Errorf("%w: list GitHub App installations for %s: %v (the GitHub token needs the read:org scope and an owner who administers the organization)", providers.ErrInsufficientGitHubAccess, canonicalOwner, err)
		}
		return nil, fmt.Errorf("list GitHub App installations for %s: %w", canonicalOwner, err)
	}
	return installations, nil
}

// CheckOrganizationAccess confirms the token can list App installations for the
// fixture organization. A rejected credential is reported as
// ErrInsufficientGitHubAccess; other failures are returned wrapped.
func (a *Adapter) CheckOrganizationAccess(ctx context.Context) error {
	if a == nil || a.client == nil {
		return fmt.Errorf("github client is required")
	}
	_, err := a.listInstallations(ctx)
	return err
}

// CheckInstallationRepositoryAccess confirms the token can list the repositories
// of every selected-repository App installation in the fixture organization. The
// cluster app slug is unknown at acquisition, so it checks every such
// installation without requiring the fixture in any of them. Suspended
// installations and installations selecting all repositories are skipped, as is
// the whole check for gh OAuth user tokens. An installation the token cannot see
// (404) is ignored; any other rejection is reported as
// ErrInsufficientGitHubAccess.
func (a *Adapter) CheckInstallationRepositoryAccess(ctx context.Context) error {
	if a == nil || a.client == nil {
		return fmt.Errorf("github client is required")
	}
	// GitHub rejects gh OAuth user tokens (gho_*) for the installation
	// repositories endpoint even with read:user, requiring a GitHub App user
	// token, personal access token, or basic auth. Keep the gh login usable and
	// leave fixture-selection verification to preflight's warning path.
	if a.oauthToken {
		return nil
	}
	installations, err := a.listInstallations(ctx)
	if err != nil {
		return err
	}
	for _, installation := range installations {
		if installation.SuspendedAt != nil || installation.GetRepositorySelection() != "selected" {
			continue
		}
		if _, err := a.client.ListUserInstallationRepositories(ctx, installation.GetID()); err != nil {
			if isAccessRejection(err) {
				var response *gh.ErrorResponse
				if errors.As(err, &response) && response.Response != nil && response.Response.StatusCode == http.StatusNotFound {
					// GitHub hides installations the token cannot see. The cluster App
					// is verified against the fixture at preflight, so one invisible
					// installation must not reject an otherwise usable token.
					continue
				}
				return fmt.Errorf("%w: list repositories for GitHub App installation %d in %s: %v (the GitHub token needs the read:user scope to list installation repositories)", providers.ErrInsufficientGitHubAccess, installation.GetID(), canonicalOwner, err)
			}
			return fmt.Errorf("list repositories for GitHub App installation %d in %s: %w", installation.GetID(), canonicalOwner, err)
		}
	}
	return nil
}

// CheckFixtureWriteAccess confirms the token can update the canonical fixture
// repository, which the run requires before it commits build triggers. It reads
// the repository permissions map and requires push. A rejected credential or a
// missing push permission is reported as ErrInsufficientGitHubAccess; 5xx
// responses are retried.
func (a *Adapter) CheckFixtureWriteAccess(ctx context.Context) error {
	if a == nil || a.client == nil {
		return fmt.Errorf("github client is required")
	}
	var repository *gh.Repository
	err := providers.RetryOn5xx(ctx, a.retry, func() error {
		var err error
		repository, err = a.client.GetRepository(ctx, canonicalOwner, canonicalName)
		return err
	}, isGitHub5xx)
	if err != nil {
		if isAccessRejection(err) {
			return fmt.Errorf("%w: read %s: %v", providers.ErrInsufficientGitHubAccess, CanonicalFixtureURL, err)
		}
		return fmt.Errorf("read %s: %w", CanonicalFixtureURL, err)
	}
	if !repository.GetPermissions()["push"] {
		return fmt.Errorf("%w: the GitHub token cannot push to %s, which is required to trigger builds; use a token with the repo scope or sign in with gh", providers.ErrInsufficientGitHubAccess, CanonicalFixtureURL)
	}
	return nil
}

// isAccessRejection reports whether err establishes that GitHub rejected the
// credential itself, meaning an HTTP 401, 403, or 404 response. Rate limits, 5xx
// responses, and context errors are transient and never justify replacing the
// token.
func isAccessRejection(err error) bool {
	var rateLimit *gh.RateLimitError
	if errors.As(err, &rateLimit) {
		return false
	}
	var abuse *gh.AbuseRateLimitError
	if errors.As(err, &abuse) {
		return false
	}
	var response *gh.ErrorResponse
	if errors.As(err, &response) && response.Response != nil {
		switch response.Response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return true
		}
	}
	return false
}

// VerifyGitHubAppInstallation confirms the cluster GitHub App can onboard the
// canonical fixture repository. It returns AppInstallationRequiredError when the
// App is not installed for the organization, AppRepositorySelectionError when a
// selected-repository installation excludes the fixture, an error for a
// suspended installation, ErrUnverifiable when selected repositories cannot be
// listed, and a plain error for API failures. Unlike the fixture reads, this
// check does not retry 5xx responses.
func (a *Adapter) VerifyGitHubAppInstallation(ctx context.Context, appSlug string) error {
	if a == nil || a.client == nil {
		return fmt.Errorf("github client is required")
	}
	appSlug = strings.TrimSpace(appSlug)
	if appSlug == "" {
		return fmt.Errorf("GitHub App slug is required")
	}
	installations, err := a.client.ListOrganizationInstallations(ctx, canonicalOwner)
	if err != nil {
		return fmt.Errorf("list GitHub App installations for %s: %w (the GitHub token needs the read:org scope and an owner who administers the organization)", canonicalOwner, err)
	}
	for _, installation := range installations {
		if installation.GetAppSlug() == appSlug {
			return verifyGitHubAppRepositories(ctx, a.client, installation, appSlug)
		}
	}
	return &providers.AppInstallationRequiredError{
		AppSlug:    appSlug,
		Owner:      canonicalOwner,
		Repository: canonicalOwner + "/" + canonicalName,
		InstallURL: fmt.Sprintf("https://github.com/apps/%s/installations/new", url.PathEscape(appSlug)),
	}
}

// installationSettingsURL returns the organization page that manages an installation's repositories.
func installationSettingsURL(installation *gh.Installation) string {
	return fmt.Sprintf("https://github.com/organizations/%s/settings/installations/%d", canonicalOwner, installation.GetID())
}

// verifyGitHubAppRepositories rejects installations that cannot reach the fixture repository.
func verifyGitHubAppRepositories(ctx context.Context, client client, installation *gh.Installation, appSlug string) error {
	if installation.SuspendedAt != nil {
		return fmt.Errorf("GitHub App %s installation in %s is suspended", appSlug, canonicalOwner)
	}
	switch installation.GetRepositorySelection() {
	case "all":
		return nil
	case "selected":
		repositories, err := client.ListUserInstallationRepositories(ctx, installation.GetID())
		if err != nil {
			return fmt.Errorf("%w: GitHub App %s is installed for selected repositories in %s, so its access to %s/%s cannot be verified: %v; add the repository to the installation and use a token that can list installation repositories, or select all repositories", providers.ErrUnverifiable, appSlug, canonicalOwner, canonicalOwner, canonicalName, err)
		}
		for _, repository := range repositories {
			if repository.GetOwner().GetLogin() == canonicalOwner && repository.GetName() == canonicalName {
				return nil
			}
		}
		return &providers.AppRepositorySelectionError{
			AppSlug:     appSlug,
			Owner:       canonicalOwner,
			Repository:  canonicalOwner + "/" + canonicalName,
			SettingsURL: installationSettingsURL(installation),
		}
	default:
		return fmt.Errorf("GitHub App %s installation in %s reports unknown repository selection %q", appSlug, canonicalOwner, installation.GetRepositorySelection())
	}
}

// ReadFile returns decoded file content and version metadata from GitHub. 5xx
// responses are retried. A failed read is returned unwrapped; a content decoding
// failure is wrapped.
func (a *Adapter) ReadFile(ctx context.Context, fixture providers.FixtureRepository, path, branch string) (providers.FileVersion, error) {
	var file *gh.RepositoryContent
	err := providers.RetryOn5xx(ctx, a.retry, func() error {
		var err error
		file, err = a.client.GetFile(ctx, fixture.Owner, fixture.Name, path, branch)
		return err
	}, isGitHub5xx)
	if err != nil {
		return providers.FileVersion{}, err
	}
	content, err := file.GetContent()
	if err != nil {
		return providers.FileVersion{}, fmt.Errorf("decode github file %s: %w", path, err)
	}
	return providers.FileVersion{Path: path, Branch: branch, Content: content, SHA: file.GetSHA(), URL: file.GetHTMLURL()}, nil
}

// UpdateFile reads the current file version, rejects a blank or stale
// expectedSHA, commits content with message, and returns the created commit. The
// commit call retries 5xx responses with the adapter policy. CreatedAt is local
// time and URL is the commit's HTML URL.
func (a *Adapter) UpdateFile(ctx context.Context, fixture providers.FixtureRepository, path, branch, content, expectedSHA, message string) (providers.Commit, error) {
	current, err := a.ReadFile(ctx, fixture, path, branch)
	if err != nil {
		return providers.Commit{}, err
	}
	if err := providers.ValidateExpectedSHA(expectedSHA, current.SHA); err != nil {
		return providers.Commit{}, err
	}
	var updated *gh.RepositoryContentResponse
	err = providers.RetryOn5xx(ctx, a.retry, func() error {
		var err error
		updated, err = a.client.UpdateFile(ctx, fixture.Owner, fixture.Name, path, content, branch, expectedSHA, message)
		return err
	}, isGitHub5xx)
	if err != nil {
		return providers.Commit{}, err
	}
	return providers.Commit{SHA: updated.Commit.GetSHA(), URL: updated.Commit.GetHTMLURL(), Message: message, CreatedAt: time.Now().UTC()}, nil
}

// isGitHub5xx reports whether err contains an HTTP 5xx GitHub response.
func isGitHub5xx(err error) bool {
	var response *gh.ErrorResponse
	return errors.As(err, &response) && response.Response != nil && response.Response.StatusCode >= 500 && response.Response.StatusCode <= 599
}
