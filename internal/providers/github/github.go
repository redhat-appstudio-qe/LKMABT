package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	gh "github.com/google/go-github/v66/github"
	"github.com/redhat-appstudio/lkmabt/internal/providers"
)

const (
	CanonicalFixtureURL = "https://github.com/redhat-appstudio-qe/dr_test_mathwizz"
	canonicalOwner      = "redhat-appstudio-qe"
	canonicalName       = "dr_test_mathwizz"
)

type client interface {
	// GetRepository looks up a repository by owner and name.
	GetRepository(string, string) (*gh.Repository, error)
	// GetFile reads a repository path at the requested branch.
	GetFile(string, string, string, string) (*gh.RepositoryContent, error)
	// UpdateFile commits replacement content using the current file SHA.
	UpdateFile(string, string, string, string, string, string) (*gh.RepositoryContentResponse, error)
	// ListOrganizationInstallations lists the GitHub App installations for an organization.
	ListOrganizationInstallations(context.Context, string) ([]*gh.Installation, error)
	// ListUserInstallationRepositories lists the repositories the token can see for an installation.
	ListUserInstallationRepositories(context.Context, int64) ([]*gh.Repository, error)
}

type apiClient struct{ client *gh.Client }

// GetRepository fetches a repository from the GitHub API.
func (c apiClient) GetRepository(owner, name string) (*gh.Repository, error) {
	repository, _, err := c.client.Repositories.Get(context.Background(), owner, name)
	return repository, err
}

// GetFile fetches a file from the requested GitHub repository branch.
func (c apiClient) GetFile(owner, name, path, branch string) (*gh.RepositoryContent, error) {
	file, _, _, err := c.client.Repositories.GetContents(context.Background(), owner, name, path, &gh.RepositoryContentGetOptions{Ref: branch})
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, fmt.Errorf("github path %s is a directory", path)
	}
	return file, nil
}

// UpdateFile commits content to a GitHub repository using the supplied file SHA.
func (c apiClient) UpdateFile(owner, name, path, content, branch, sha string) (*gh.RepositoryContentResponse, error) {
	message := "lkmabt trigger"
	response, _, err := c.client.Repositories.UpdateFile(context.Background(), owner, name, path, &gh.RepositoryContentFileOptions{Message: &message, Content: []byte(content), Branch: &branch, SHA: &sha})
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

type Adapter struct {
	client client
	retry  providers.RetryPolicy
}

// New creates a GitHub adapter with the default retry policy.
func New(client client) *Adapter {
	return NewWithRetry(client, providers.DefaultRetryPolicy())
}

// NewWithRetry creates a GitHub adapter with the supplied retry policy.
func NewWithRetry(client client, retry providers.RetryPolicy) *Adapter {
	return &Adapter{client: client, retry: retry}
}

// NewFromEnv creates a GitHub adapter using GITHUB_TOKEN.
func NewFromEnv() (*Adapter, error) { return NewFromCredentials(os.Getenv("GITHUB_TOKEN")) }

// NewFromCredentials validates a token and creates an authenticated GitHub adapter.
func NewFromCredentials(token string) (*Adapter, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN is required")
	}
	return New(apiClient{client: gh.NewClient(nil).WithAuthToken(token)}), nil
}

// ValidateFixture confirms the repository is the canonical GitHub test fixture.
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
		repository, err = a.client.GetRepository(canonicalOwner, canonicalName)
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

// CheckOrganizationAccess confirms the token can list App installations for the fixture organization.
func (a *Adapter) CheckOrganizationAccess(ctx context.Context) error {
	if a == nil || a.client == nil {
		return fmt.Errorf("github client is required")
	}
	if _, err := a.client.ListOrganizationInstallations(ctx, canonicalOwner); err != nil {
		return fmt.Errorf("list GitHub App installations for %s: %w", canonicalOwner, err)
	}
	return nil
}

// VerifyGitHubAppInstallation confirms the cluster GitHub App can onboard the canonical fixture repository.
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
		return fmt.Errorf("GitHub App %s is installed for selected repositories in %s but not for %s/%s; add the repository from %s or select all repositories", appSlug, canonicalOwner, canonicalOwner, canonicalName, installationSettingsURL(installation))
	default:
		return fmt.Errorf("GitHub App %s installation in %s reports unknown repository selection %q", appSlug, canonicalOwner, installation.GetRepositorySelection())
	}
}

// ReadFile returns decoded file content and version metadata from GitHub.
func (a *Adapter) ReadFile(ctx context.Context, fixture providers.FixtureRepository, path, branch string) (providers.FileVersion, error) {
	var file *gh.RepositoryContent
	err := providers.RetryOn5xx(ctx, a.retry, func() error {
		var err error
		file, err = a.client.GetFile(fixture.Owner, fixture.Name, path, branch)
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

// UpdateFile checks the current SHA, commits replacement content, and returns commit metadata.
func (a *Adapter) UpdateFile(ctx context.Context, fixture providers.FixtureRepository, path, branch, content, expectedSHA string) (providers.Commit, error) {
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
		updated, err = a.client.UpdateFile(fixture.Owner, fixture.Name, path, content, branch, expectedSHA)
		return err
	}, isGitHub5xx)
	if err != nil {
		return providers.Commit{}, err
	}
	return providers.Commit{SHA: updated.Commit.GetSHA(), URL: updated.Commit.GetHTMLURL(), Message: "lkmabt trigger", CreatedAt: time.Now().UTC()}, nil
}

// isGitHub5xx reports whether err contains an HTTP 5xx GitHub response.
func isGitHub5xx(err error) bool {
	var response *gh.ErrorResponse
	return errors.As(err, &response) && response.Response != nil && response.Response.StatusCode >= 500 && response.Response.StatusCode <= 599
}
