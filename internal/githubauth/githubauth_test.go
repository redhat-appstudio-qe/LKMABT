package githubauth

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

// Run records the login spec and returns the configured error.
func (f *fakeRunner) Run(_ context.Context, spec clusterauth.CommandSpec) error {
	f.runSpecs = append(f.runSpecs, spec)
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

// authDirectory extracts the temporary credential directory from a recorded spec.
func authDirectory(spec clusterauth.CommandSpec) string {
	for _, entry := range spec.Env {
		if value, ok := strings.CutPrefix(entry, "GH_CONFIG_DIR="); ok {
			return filepath.Dir(value)
		}
	}
	return ""
}

var _ = Describe("Authenticate", func() {
	var (
		runner   *fakeRunner
		tempRoot string
	)
	BeforeEach(func() {
		runner = &fakeRunner{stdout: "gho_token\n"}
		tempRoot = GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tempRoot)
	})

	When("the browser login succeeds", func() {
		It("should return the token and keep only a private token file", func() {
			token, cleanup, err := Authenticate(context.Background(), runner, true)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(token).Should(Equal("gho_token"))
			Expect(runner.runSpecs).Should(HaveLen(1))
			Expect(runner.runSpecs[0].Args).Should(ContainElements("auth", "login", "--hostname", "github.com", "--web", "--insecure-storage", "--scopes", "repo,read:org,read:user"))
			Expect(runner.runSpecs[0].ClearEnv).Should(ContainElements("GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"))

			directory := authDirectory(runner.runSpecs[0])
			Expect(directory).ShouldNot(BeEmpty())
			tokenPath := filepath.Join(directory, tokenFilename)
			info, statErr := os.Stat(tokenPath)
			Expect(statErr).ShouldNot(HaveOccurred())
			Expect(info.Mode().Perm()).Should(Equal(os.FileMode(0o600)))
			content, readErr := os.ReadFile(tokenPath)
			Expect(readErr).ShouldNot(HaveOccurred())
			Expect(string(content)).Should(Equal("gho_token"))
			Expect(filepath.Join(directory, configSubdir)).ShouldNot(BeADirectory())

			Expect(runner.stdoutSpecs).Should(HaveLen(1))
			Expect(runner.stdoutSpecs[0].Args).Should(Equal([]string{"auth", "token", "--hostname", "github.com"}))
			Expect(authDirectory(runner.stdoutSpecs[0])).Should(Equal(directory))

			Expect(cleanup()).Should(Succeed())
			Expect(directory).ShouldNot(BeADirectory())
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
			Expect(cleanup()).Should(Succeed())
		})
	})

	When("the login fails", func() {
		It("should remove the temporary credentials", func() {
			runner.runErr = errors.New("login denied")

			_, _, err := Authenticate(context.Background(), runner, true)
			Expect(err).Should(MatchError(ContainSubstring("authenticate to GitHub")))
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
		})
	})

	When("the token cannot be read", func() {
		It("should remove the temporary credentials", func() {
			runner.stdoutErr = errors.New("token denied")

			_, _, err := Authenticate(context.Background(), runner, true)
			Expect(err).Should(MatchError(ContainSubstring("read the GitHub token")))
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
		})
	})

	When("the token is empty", func() {
		It("should remove the temporary credentials", func() {
			runner.stdout = " \n"

			_, _, err := Authenticate(context.Background(), runner, true)
			Expect(err).Should(MatchError(ContainSubstring("empty token")))
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
		})
	})

	When("gh is unavailable", func() {
		It("should fail before creating credentials", func() {
			runner.lookPathErr = errors.New("not found")

			_, _, err := Authenticate(context.Background(), runner, true)
			Expect(err).Should(MatchError(ContainSubstring("GitHub CLI is required")))
			Expect(runner.runSpecs).Should(BeEmpty())
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
		})
	})

	When("the session is not interactive", func() {
		It("should explain the token alternative", func() {
			_, _, err := Authenticate(context.Background(), runner, false)
			Expect(err).Should(MatchError(ContainSubstring("set GITHUB_TOKEN")))
			Expect(runner.runSpecs).Should(BeEmpty())
		})
	})

	When("no command runner is supplied", func() {
		It("should fail", func() {
			_, _, err := Authenticate(context.Background(), nil, true)
			Expect(err).Should(MatchError("command runner is required"))
		})
	})
})
