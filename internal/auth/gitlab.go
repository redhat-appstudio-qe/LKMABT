package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/redhat-appstudio/lkmabt/internal/clusterauth"
)

const (
	gitlabTokenFilename = "gitlab-token"
	defaultGitLabAPIURL = "https://gitlab.com/api/v4"
	glabConfigSubdir    = "glab"
)

// GitLabToken runs the glab web login in an isolated configuration directory and
// stores the resulting token in store. The glab directory stays in the store so
// OAuth refreshes remain available until Store.Destroy.
func GitLabToken(ctx context.Context, runner clusterauth.CommandRunner, store *Store, interactive bool, apiURL string) (string, error) {
	if !interactive {
		return "", fmt.Errorf("GitLab authentication is required: set GITLAB_BOT_TOKEN or run in a terminal to use the glab browser login")
	}
	if runner == nil {
		return "", fmt.Errorf("command runner is required")
	}
	if store == nil || store.Directory() == "" {
		return "", fmt.Errorf("credential store is required")
	}
	if _, err := runner.LookPath("glab"); err != nil {
		return "", fmt.Errorf("GitLab CLI is required for interactive authentication: %w", err)
	}
	host, err := glabHostname(apiURL)
	if err != nil {
		return "", err
	}
	spec := glabCommand(store)
	login := spec
	login.Args = []string{"auth", "login", "--hostname", host, "--use-keyring=false"}
	if err := runner.Run(ctx, login); err != nil {
		return "", fmt.Errorf("authenticate to GitLab: %w", err)
	}
	return readGitLabToken(ctx, runner, spec, host, store)
}

// RefreshGitLabToken refreshes an OAuth token and stores the current value in store.
func RefreshGitLabToken(ctx context.Context, runner clusterauth.CommandRunner, store *Store, apiURL string) (string, error) {
	if runner == nil {
		return "", fmt.Errorf("command runner is required")
	}
	if store == nil || store.Directory() == "" {
		return "", fmt.Errorf("credential store is required")
	}
	if _, err := runner.LookPath("glab"); err != nil {
		return "", fmt.Errorf("GitLab CLI is required for token refresh: %w", err)
	}
	if _, err := os.Stat(filepath.Join(store.Directory(), glabConfigSubdir)); err != nil {
		return "", fmt.Errorf("GitLab login state is missing: %w", err)
	}
	host, err := glabHostname(apiURL)
	if err != nil {
		return "", err
	}
	spec := glabCommand(store)
	refresh := spec
	refresh.Args = []string{"api", "user"}
	if _, err := runner.Stdout(ctx, refresh); err != nil {
		return "", fmt.Errorf("refresh the GitLab token: %w", err)
	}
	return readGitLabToken(ctx, runner, spec, host, store)
}

// glabCommand returns the isolated glab invocation shared by login and refresh.
func glabCommand(store *Store) clusterauth.CommandSpec {
	return clusterauth.CommandSpec{
		Name: "glab",
		Env:  []string{"GLAB_CONFIG_DIR=" + filepath.Join(store.Directory(), glabConfigSubdir), "GLAB_CHECK_UPDATE=false"},
		ClearEnv: []string{
			"GLAB_CONFIG_DIR", "GLAB_CHECK_UPDATE", "GLAB_ENABLE_CI_AUTOLOGIN",
			"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN", "GITLAB_HOST",
		},
	}
}

// readGitLabToken reads the stored token and updates the run-scoped token file.
func readGitLabToken(ctx context.Context, runner clusterauth.CommandRunner, spec clusterauth.CommandSpec, host string, store *Store) (string, error) {
	readToken := spec
	readToken.Args = []string{"config", "get", "token", "--host", host}
	output, err := runner.Stdout(ctx, readToken)
	if err != nil {
		return "", fmt.Errorf("read the GitLab token: %w", err)
	}
	token := strings.TrimSpace(output)
	if token == "" {
		return "", errors.New("glab returned an empty token")
	}
	if err := store.Write(gitlabTokenFilename, token); err != nil {
		return "", err
	}
	return token, nil
}

// glabHostname returns the GitLab instance hostname for glab's interactive login.
func glabHostname(apiURL string) (string, error) {
	apiURL = strings.TrimSpace(apiURL)
	if apiURL == "" {
		apiURL = defaultGitLabAPIURL
	}
	parsed, err := url.Parse(apiURL)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid GitLab API URL %q", apiURL)
	}
	if parsed.Scheme != "https" || parsed.Port() != "" {
		return "", fmt.Errorf("glab interactive login requires a standard HTTPS GitLab URL; set GITLAB_BOT_TOKEN for %s", apiURL)
	}
	if parsed.Hostname() != "gitlab.com" {
		return "", fmt.Errorf("glab interactive login is only configured for gitlab.com; set GITLAB_BOT_TOKEN for %s", apiURL)
	}
	return parsed.Hostname(), nil
}
