package githubauth

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
	githubHostname = "github.com"
	configSubdir   = "gh"
	tokenFilename  = "token"
)

// Authenticate runs the gh web login in an isolated configuration directory and
// returns the token plus a cleanup function that destroys every stored credential.
func Authenticate(ctx context.Context, runner clusterauth.CommandRunner, interactive bool) (string, func() error, error) {
	if !interactive {
		return "", nil, fmt.Errorf("GitHub authentication is required: set GITHUB_TOKEN or run in a terminal to use the gh browser login")
	}
	if runner == nil {
		return "", nil, fmt.Errorf("command runner is required")
	}
	if _, err := runner.LookPath("gh"); err != nil {
		return "", nil, fmt.Errorf("GitHub CLI is required for interactive authentication: %w", err)
	}
	directory, err := os.MkdirTemp("", "lkmabt-github-auth-")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary GitHub credential directory: %w", err)
	}
	cleanup := func() error { return destroy(directory) }
	configDirectory := filepath.Join(directory, configSubdir)
	base := clusterauth.CommandSpec{
		Name:     "gh",
		Env:      []string{"GH_CONFIG_DIR=" + configDirectory},
		ClearEnv: []string{"GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"},
	}
	login := base
	login.Args = []string{"auth", "login", "--hostname", githubHostname, "--git-protocol", "https", "--web", "--insecure-storage", "--scopes", "repo,read:org,read:user"}
	if err := runner.Run(ctx, login); err != nil {
		return "", nil, errors.Join(fmt.Errorf("authenticate to GitHub: %w", err), cleanup())
	}
	readToken := base
	readToken.Args = []string{"auth", "token", "--hostname", githubHostname}
	output, err := runner.Stdout(ctx, readToken)
	if err != nil {
		return "", nil, errors.Join(fmt.Errorf("read the GitHub token: %w", err), cleanup())
	}
	token := strings.TrimSpace(output)
	if token == "" {
		return "", nil, errors.Join(errors.New("GitHub CLI returned an empty token"), cleanup())
	}
	// The gh configuration holds a second copy; keep only the token file.
	if err := os.RemoveAll(configDirectory); err != nil {
		return "", nil, errors.Join(fmt.Errorf("remove the temporary GitHub CLI configuration: %w", err), cleanup())
	}
	if err := os.WriteFile(filepath.Join(directory, tokenFilename), []byte(token), 0o600); err != nil {
		return "", nil, errors.Join(fmt.Errorf("store the temporary GitHub token: %w", err), cleanup())
	}
	return token, cleanup, nil
}

// destroy removes the credential directory and reports whether it is gone.
func destroy(directory string) error {
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("delete temporary GitHub credentials in %s: %w", directory, err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("temporary GitHub credential directory %s is still present", directory)
	}
	return nil
}
