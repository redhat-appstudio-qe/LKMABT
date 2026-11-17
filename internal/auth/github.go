package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/redhat-appstudio/lkmabt/internal/clusterauth"
)

const (
	githubHostname      = "github.com"
	githubTokenFilename = "github-token"
	ghConfigSubdir      = "gh"
)

// ExistingGitHubToken returns the token stored by the user's normal gh login.
// It clears provider token variables so gh reads its own credential instead of
// an ambient GH_TOKEN or GITHUB_TOKEN.
func ExistingGitHubToken(ctx context.Context, runner clusterauth.CommandRunner) (string, error) {
	if runner == nil {
		return "", fmt.Errorf("command runner is required")
	}
	if _, err := runner.LookPath("gh"); err != nil {
		return "", fmt.Errorf("GitHub CLI is required to reuse an existing login: %w", err)
	}
	output, err := runner.Stdout(ctx, clusterauth.CommandSpec{
		Name:     "gh",
		Args:     []string{"auth", "token", "--hostname", githubHostname},
		ClearEnv: []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"},
	})
	if err != nil {
		return "", fmt.Errorf("read the existing GitHub token: %w", err)
	}
	token := strings.TrimSpace(output)
	if token == "" {
		return "", errors.New("GitHub CLI returned an empty token")
	}
	return token, nil
}

// GitHubToken runs the gh web login in an isolated configuration directory and
// stores the resulting token in store.
func GitHubToken(ctx context.Context, runner clusterauth.CommandRunner, store *Store, interactive bool) (string, error) {
	if !interactive {
		return "", fmt.Errorf("GitHub authentication is required: set GITHUB_TOKEN or run in a terminal to use the gh browser login")
	}
	if runner == nil {
		return "", fmt.Errorf("command runner is required")
	}
	if store == nil || store.Directory() == "" {
		return "", fmt.Errorf("credential store is required")
	}
	if _, err := runner.LookPath("gh"); err != nil {
		return "", fmt.Errorf("GitHub CLI is required for interactive authentication: %w", err)
	}
	configDirectory := filepath.Join(store.Directory(), ghConfigSubdir)
	defer func() { _ = os.RemoveAll(configDirectory) }()
	base := clusterauth.CommandSpec{
		Name:     "gh",
		Env:      []string{"GH_CONFIG_DIR=" + configDirectory},
		ClearEnv: []string{"GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"},
	}
	login := base
	login.Args = []string{"auth", "login", "--hostname", githubHostname, "--git-protocol", "https", "--web", "--insecure-storage", "--scopes", "repo,read:org,read:user"}
	if err := runner.Run(ctx, login); err != nil {
		return "", fmt.Errorf("authenticate to GitHub: %w", err)
	}
	readToken := base
	readToken.Args = []string{"auth", "token", "--hostname", githubHostname}
	output, err := runner.Stdout(ctx, readToken)
	if err != nil {
		return "", fmt.Errorf("read the GitHub token: %w", err)
	}
	token := strings.TrimSpace(output)
	if token == "" {
		return "", errors.New("GitHub CLI returned an empty token")
	}
	if err := store.Write(githubTokenFilename, token); err != nil {
		return "", err
	}
	return token, nil
}
