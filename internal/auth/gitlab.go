package auth

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

const (
	gitlabTokenFilename = "gitlab-token"
	defaultGitLabAPIURL = "https://gitlab.com/api/v4"
)

// readSecret reads a secret from the terminal without echo; tests replace it.
var readSecret = func() ([]byte, error) {
	return term.ReadPassword(int(os.Stdin.Fd()))
}

// GitLabToken prompts for a GitLab token without echo and stores it in store.
func GitLabToken(store *Store, interactive bool, apiURL string, out io.Writer) (string, error) {
	if !interactive {
		return "", fmt.Errorf("GitLab authentication is required: set GITLAB_BOT_TOKEN or run in a terminal to enter a token")
	}
	if store == nil || store.Directory() == "" {
		return "", fmt.Errorf("credential store is required")
	}
	apiURL = strings.TrimSpace(apiURL)
	if apiURL == "" {
		apiURL = defaultGitLabAPIURL
	}
	if out == nil {
		out = io.Discard
	}
	fmt.Fprintf(out, "GitLab token for %s (needs api scope): ", apiURL)
	secret, err := readSecret()
	fmt.Fprintln(out)
	if err != nil {
		return "", fmt.Errorf("read GitLab token: %w", err)
	}
	token := strings.TrimSpace(string(secret))
	if token == "" {
		return "", fmt.Errorf("GitLab token is required")
	}
	if err := store.Write(gitlabTokenFilename, token); err != nil {
		return "", err
	}
	return token, nil
}
