package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/clusterauth"
)

type fakeRunner struct {
	lookPathErr error
	runSpecs    []clusterauth.CommandSpec
	runErr      error
	stdoutSpecs []clusterauth.CommandSpec
	stdout      string
	stdoutErr   error
}

// LookPath returns the fake gh path or the configured lookup error.
func (f *fakeRunner) LookPath(string) (string, error) {
	if f.lookPathErr != nil {
		return "", f.lookPathErr
	}
	return "/usr/bin/gh", nil
}

// Run records the login spec, simulates glab creating its config directory, and
// returns the configured error.
func (f *fakeRunner) Run(_ context.Context, spec clusterauth.CommandSpec) error {
	f.runSpecs = append(f.runSpecs, spec)
	if spec.Name == "glab" && len(spec.Args) > 0 && spec.Args[0] == "auth" {
		for _, entry := range spec.Env {
			if value, ok := strings.CutPrefix(entry, "GLAB_CONFIG_DIR="); ok {
				if err := os.MkdirAll(value, 0o700); err != nil {
					return err
				}
			}
		}
	}
	return f.runErr
}

// Output returns empty output; the auth flow reads the token from Stdout.
func (f *fakeRunner) Output(context.Context, clusterauth.CommandSpec) (string, error) {
	return "", nil
}

// Stdout records the token spec and returns the configured output or error.
func (f *fakeRunner) Stdout(_ context.Context, spec clusterauth.CommandSpec) (string, error) {
	f.stdoutSpecs = append(f.stdoutSpecs, spec)
	return f.stdout, f.stdoutErr
}

var _ = Describe("ExistingGitHubToken", func() {
	When("gh stores a token", func() {
		It("should read it from the default configuration", func() {
			runner := &fakeRunner{stdout: "gho_existing\n"}

			token, err := ExistingGitHubToken(context.Background(), runner)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(token).Should(Equal("gho_existing"))
			Expect(runner.stdoutSpecs).Should(HaveLen(1))
			Expect(runner.stdoutSpecs[0].Args).Should(Equal([]string{"auth", "token", "--hostname", "github.com"}))
			Expect(runner.stdoutSpecs[0].Env).Should(BeEmpty())
			Expect(runner.stdoutSpecs[0].ClearEnv).Should(ContainElements("GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"))
			Expect(runner.stdoutSpecs[0].ClearEnv).ShouldNot(ContainElement("GH_CONFIG_DIR"))
		})
	})

	When("gh is not installed", func() {
		It("should report the missing CLI", func() {
			runner := &fakeRunner{lookPathErr: errors.New("not found")}

			_, err := ExistingGitHubToken(context.Background(), runner)
			Expect(err).Should(MatchError(ContainSubstring("GitHub CLI is required to reuse an existing login")))
		})
	})

	When("gh has no stored token", func() {
		It("should return the command error", func() {
			runner := &fakeRunner{stdoutErr: errors.New("not logged in")}

			_, err := ExistingGitHubToken(context.Background(), runner)
			Expect(err).Should(MatchError(ContainSubstring("read the existing GitHub token")))
		})
	})

	When("gh returns an empty token", func() {
		It("should fail", func() {
			runner := &fakeRunner{stdout: " \n"}

			_, err := ExistingGitHubToken(context.Background(), runner)
			Expect(err).Should(MatchError("GitHub CLI returned an empty token"))
		})
	})
})

var _ = Describe("GitHubToken", func() {
	var (
		runner   *fakeRunner
		store    *Store
		tempRoot string
	)
	BeforeEach(func() {
		runner = &fakeRunner{stdout: "gho_token\n"}
		tempRoot = GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tempRoot)
		var err error
		store, err = NewStore()
		Expect(err).ShouldNot(HaveOccurred())
		DeferCleanup(func() { _ = store.Destroy() })
	})

	When("the browser login succeeds", func() {
		It("should return the token and keep only a private token file", func() {
			token, err := GitHubToken(context.Background(), runner, store, true)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(token).Should(Equal("gho_token"))
			Expect(runner.runSpecs).Should(HaveLen(1))
			Expect(runner.runSpecs[0].Args).Should(ContainElements("auth", "login", "--hostname", "github.com", "--web", "--insecure-storage", "--scopes", "repo,read:org,read:user"))
			Expect(runner.runSpecs[0].ClearEnv).Should(ContainElements("GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"))

			tokenPath := filepath.Join(store.Directory(), githubTokenFilename)
			info, statErr := os.Stat(tokenPath)
			Expect(statErr).ShouldNot(HaveOccurred())
			Expect(info.Mode().Perm() & 0o077).Should(BeZero())
			content, readErr := os.ReadFile(tokenPath)
			Expect(readErr).ShouldNot(HaveOccurred())
			Expect(string(content)).Should(Equal("gho_token"))
			Expect(filepath.Join(store.Directory(), ghConfigSubdir)).ShouldNot(BeADirectory())

			Expect(runner.stdoutSpecs).Should(HaveLen(1))
			Expect(runner.stdoutSpecs[0].Args).Should(Equal([]string{"auth", "token", "--hostname", "github.com"}))
			Expect(runner.stdoutSpecs[0].Env).Should(Equal(runner.runSpecs[0].Env))

			Expect(store.Destroy()).Should(Succeed())
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
		})
	})

	When("the login fails", func() {
		It("should return the error and remove the gh configuration", func() {
			runner.runErr = errors.New("login denied")

			_, err := GitHubToken(context.Background(), runner, store, true)
			Expect(err).Should(MatchError(ContainSubstring("authenticate to GitHub")))
			Expect(filepath.Join(store.Directory(), ghConfigSubdir)).ShouldNot(BeADirectory())
		})
	})

	When("the token cannot be read", func() {
		It("should return the error", func() {
			runner.stdoutErr = errors.New("token denied")

			_, err := GitHubToken(context.Background(), runner, store, true)
			Expect(err).Should(MatchError(ContainSubstring("read the GitHub token")))
		})
	})

	When("the token is empty", func() {
		It("should fail", func() {
			runner.stdout = " \n"

			_, err := GitHubToken(context.Background(), runner, store, true)
			Expect(err).Should(MatchError(ContainSubstring("empty token")))
		})
	})

	When("gh is unavailable", func() {
		It("should fail before creating credentials", func() {
			runner.lookPathErr = errors.New("not found")

			_, err := GitHubToken(context.Background(), runner, store, true)
			Expect(err).Should(MatchError(ContainSubstring("GitHub CLI is required")))
			Expect(runner.runSpecs).Should(BeEmpty())
		})
	})

	When("the session is not interactive", func() {
		It("should explain the token alternative", func() {
			_, err := GitHubToken(context.Background(), runner, store, false)
			Expect(err).Should(MatchError(ContainSubstring("set GITHUB_TOKEN")))
			Expect(runner.runSpecs).Should(BeEmpty())
		})
	})

	When("no credential store is supplied", func() {
		It("should fail", func() {
			_, err := GitHubToken(context.Background(), runner, nil, true)
			Expect(err).Should(MatchError("credential store is required"))
		})
	})

	When("no command runner is supplied", func() {
		It("should fail", func() {
			_, err := GitHubToken(context.Background(), nil, store, true)
			Expect(err).Should(MatchError("command runner is required"))
		})
	})
})
