// Package gitlab implements the providers.Provider contract against the GitLab
// REST API. Requests authenticate with a Bearer token so both OAuth tokens from
// an interactive login and personal access tokens work.
package gitlab

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/providers"
	gl "github.com/xanzy/go-gitlab"
)

const (
	// CanonicalFixtureURL is the project ValidateFixture accepts; every other
	// fixture is rejected.
	CanonicalFixtureURL = "https://gitlab.com/konflux-qe/dr_test_mathwizz_gl"
	canonicalProject    = "konflux-qe/dr_test_mathwizz_gl"
)

type client interface {
	// GetProject looks up a project by its full namespace and name path.
	GetProject(string) (*gl.Project, error)
	// GetFileMetaData fetches file version metadata for a branch and path.
	GetFileMetaData(string, string, string) (*gl.File, error)
	// GetFile fetches the encoded file content for a branch and path.
	GetFile(string, string, string) (string, error)
	// UpdateFile commits replacement content using the current file SHA.
	UpdateFile(project, path, content, branch, expectedSHA, message string) error
	// GetBranchHead fetches the head commit of a branch.
	GetBranchHead(project, branch string) (*gl.Commit, error)
	// RevokeToken revokes the currently authenticated personal access token.
	RevokeToken(context.Context) error
}

type apiClient struct{ client *gl.Client }

// GetProject fetches a project from the GitLab API.
func (c apiClient) GetProject(project string) (*gl.Project, error) {
	result, _, err := c.client.Projects.GetProject(project, nil)
	return result, err
}

// GetFileMetaData fetches GitLab metadata for a file on the requested branch.
func (c apiClient) GetFileMetaData(project, path, branch string) (*gl.File, error) {
	result, _, err := c.client.RepositoryFiles.GetFileMetaData(project, path, &gl.GetFileMetaDataOptions{Ref: &branch})
	return result, err
}

// GetFile fetches base64-encoded file content from the requested GitLab branch.
func (c apiClient) GetFile(project, path, branch string) (string, error) {
	result, _, err := c.client.RepositoryFiles.GetFile(project, path, &gl.GetFileOptions{Ref: &branch})
	if err != nil {
		return "", err
	}
	return result.Content, nil
}

// UpdateFile commits file content to GitLab using the supplied last commit ID.
// GitLab rejects the update when another commit has touched the file since, which
// is how expectedSHA guards against a stale read.
func (c apiClient) UpdateFile(project, path, content, branch, expectedSHA, message string) error {
	_, _, err := c.client.RepositoryFiles.UpdateFile(project, path, &gl.UpdateFileOptions{Branch: &branch, Content: &content, LastCommitID: &expectedSHA, CommitMessage: &message})
	return err
}

// GetBranchHead fetches the head commit of a branch.
func (c apiClient) GetBranchHead(project, branch string) (*gl.Commit, error) {
	result, _, err := c.client.Branches.GetBranch(project, branch)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Commit == nil {
		return nil, fmt.Errorf("gitlab branch %s has no head commit", branch)
	}
	return result.Commit, nil
}

// RevokeToken revokes the current personal access token.
func (c apiClient) RevokeToken(ctx context.Context) error {
	_, err := c.client.PersonalAccessTokens.RevokePersonalAccessTokenSelf(gl.WithContext(ctx))
	return err
}

// Adapter implements the fixture repository contract against GitLab using the
// supplied API client and retry policy.
type Adapter struct {
	client client
	retry  providers.RetryPolicy
}

// New creates a GitLab adapter with the default retry policy.
func New(client client) *Adapter {
	return NewWithRetry(client, providers.DefaultRetryPolicy())
}

// NewWithRetry creates a GitLab adapter with the supplied retry policy. A zero
// policy retries nothing.
func NewWithRetry(client client, retry providers.RetryPolicy) *Adapter {
	return &Adapter{client: client, retry: retry}
}

// NewFromEnv creates a GitLab adapter using GITLAB_BOT_TOKEN and GITLAB_API_URL.
func NewFromEnv() (*Adapter, error) {
	return NewFromCredentials(os.Getenv("GITLAB_BOT_TOKEN"), os.Getenv("GITLAB_API_URL"))
}

// NewFromCredentials checks that token is non-empty, defaults apiURL to
// https://gitlab.com/api/v4, and creates a GitLab adapter. It makes no API
// request. Authentication uses a Bearer token, which accepts both OAuth tokens
// from an interactive login and GITLAB_BOT_TOKEN personal access tokens; the
// PRIVATE-TOKEN header rejects OAuth tokens.
func NewFromCredentials(token, apiURL string) (*Adapter, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("GITLAB_BOT_TOKEN is required")
	}
	if strings.TrimSpace(apiURL) == "" {
		apiURL = "https://gitlab.com/api/v4"
	}
	gitlabClient, err := gl.NewOAuthClient(token, gl.WithBaseURL(apiURL))
	if err != nil {
		return nil, err
	}
	return New(apiClient{client: gitlabClient}), nil
}

// RevokeToken revokes the run-scoped personal access token. A prompted token is
// always a glpat- value, so a 404 or 400 is surfaced instead of being treated as
// already revoked.
func (a *Adapter) RevokeToken(ctx context.Context) error {
	if a == nil || a.client == nil {
		return fmt.Errorf("gitlab client is required")
	}
	return a.client.RevokeToken(ctx)
}

// ValidateFixture checks that fixture names the canonical GitLab project, fetches
// it, and returns the canonical identity. fixture.URL may be empty, but when set
// it must equal CanonicalFixtureURL. 5xx responses are retried; other read errors
// are returned wrapped.
func (a *Adapter) ValidateFixture(ctx context.Context, fixture providers.FixtureRepository) (providers.FixtureRepository, error) {
	if a == nil || a.client == nil {
		return providers.FixtureRepository{}, fmt.Errorf("gitlab client is required")
	}
	if fixture.Owner+"/"+fixture.Name != canonicalProject || (fixture.URL != "" && fixture.URL != CanonicalFixtureURL) {
		return providers.FixtureRepository{}, fmt.Errorf("gitlab fixture must be %s", CanonicalFixtureURL)
	}
	var project *gl.Project
	err := providers.RetryOn5xx(ctx, a.retry, func() error {
		var err error
		project, err = a.client.GetProject(canonicalProject)
		return err
	}, isGitLab5xx)
	if err != nil {
		return providers.FixtureRepository{}, fmt.Errorf("validate gitlab fixture: %w", err)
	}
	if project.PathWithNamespace != canonicalProject {
		return providers.FixtureRepository{}, fmt.Errorf("gitlab API returned unexpected fixture identity")
	}
	return providers.FixtureRepository{Owner: "konflux-qe", Name: "dr_test_mathwizz_gl", URL: CanonicalFixtureURL}, nil
}

// ReadFile returns decoded file content and version metadata from GitLab. It
// reads metadata and raw content in two requests, each retrying 5xx responses.
// SHA is the last commit touching the file, falling back to the commit ID when
// GitLab omits it, and URL carries the project-relative file path rather than a
// web URL.
func (a *Adapter) ReadFile(ctx context.Context, fixture providers.FixtureRepository, path, branch string) (providers.FileVersion, error) {
	project := fixture.Owner + "/" + fixture.Name
	var metadata *gl.File
	err := providers.RetryOn5xx(ctx, a.retry, func() error {
		var err error
		metadata, err = a.client.GetFileMetaData(project, path, branch)
		return err
	}, isGitLab5xx)
	if err != nil {
		return providers.FileVersion{}, err
	}
	var content string
	err = providers.RetryOn5xx(ctx, a.retry, func() error {
		var err error
		content, err = a.client.GetFile(project, path, branch)
		return err
	}, isGitLab5xx)
	if err != nil {
		return providers.FileVersion{}, err
	}
	decoded, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		return providers.FileVersion{}, fmt.Errorf("decode GitLab file %s: %w", path, err)
	}
	sha := metadata.LastCommitID
	if sha == "" {
		sha = metadata.CommitID
	}
	return providers.FileVersion{Path: path, Branch: branch, Content: string(decoded), SHA: sha, URL: metadata.FilePath}, nil
}

// UpdateFile checks the current SHA, commits replacement content, and returns the
// commit created by the update. The commit SHA is the post-update branch head,
// validated against the pre-update head so another run's commit cannot be
// misattributed to this run's trigger. The update request retries 5xx responses;
// the surrounding branch-head reads do too. CreatedAt is local time and URL is
// left empty.
func (a *Adapter) UpdateFile(ctx context.Context, fixture providers.FixtureRepository, path, branch, content, expectedSHA, message string) (providers.Commit, error) {
	current, err := a.ReadFile(ctx, fixture, path, branch)
	if err != nil {
		return providers.Commit{}, err
	}
	if err := providers.ValidateExpectedSHA(expectedSHA, current.SHA); err != nil {
		return providers.Commit{}, err
	}
	project := fixture.Owner + "/" + fixture.Name
	// The pre-update head is the ancestor the post-update head must descend from;
	// the file's expectedSHA is not necessarily the branch head.
	before, err := a.branchHead(ctx, project, branch)
	if err != nil {
		return providers.Commit{}, err
	}
	if err := providers.RetryOn5xx(ctx, a.retry, func() error {
		return a.client.UpdateFile(project, path, content, branch, expectedSHA, message)
	}, isGitLab5xx); err != nil {
		return providers.Commit{}, err
	}
	after, err := a.branchHead(ctx, project, branch)
	if err != nil {
		return providers.Commit{}, err
	}
	if before.ID == after.ID {
		return providers.Commit{}, fmt.Errorf("gitlab update of %s did not create a new commit on %s", path, branch)
	}
	if !slices.Contains(after.ParentIDs, before.ID) {
		return providers.Commit{}, fmt.Errorf("gitlab branch %s moved during the update of %s: head %s does not descend from %s", branch, path, after.ID, before.ID)
	}
	return providers.Commit{SHA: after.ID, Message: message, CreatedAt: time.Now().UTC()}, nil
}

// branchHead reads the head commit of a branch, retrying GitLab 5xx responses.
func (a *Adapter) branchHead(ctx context.Context, project, branch string) (*gl.Commit, error) {
	var head *gl.Commit
	err := providers.RetryOn5xx(ctx, a.retry, func() error {
		var err error
		head, err = a.client.GetBranchHead(project, branch)
		return err
	}, isGitLab5xx)
	if err != nil {
		return nil, fmt.Errorf("read gitlab branch head: %w", err)
	}
	if head == nil || strings.TrimSpace(head.ID) == "" {
		return nil, fmt.Errorf("gitlab branch %s of %s returned no head commit", branch, project)
	}
	return head, nil
}

// isGitLab5xx reports whether err contains an HTTP 5xx GitLab response.
func isGitLab5xx(err error) bool {
	var response *gl.ErrorResponse
	return errors.As(err, &response) && response.Response != nil && response.Response.StatusCode >= 500 && response.Response.StatusCode <= 599
}
