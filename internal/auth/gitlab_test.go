package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("GitLabToken", func() {
	var (
		runner   *fakeRunner
		store    *Store
		tempRoot string
	)
	BeforeEach(func() {
		runner = &fakeRunner{stdout: "glpat-test-token\n"}
		tempRoot = GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tempRoot)
		var err error
		store, err = NewStore()
		Expect(err).ShouldNot(HaveOccurred())
		DeferCleanup(func() { _ = store.Destroy() })
	})

	When("the browser login succeeds", func() {
		It("should return the token and keep the isolatable login state", func() {
			token, err := GitLabToken(context.Background(), runner, store, true, "")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(token).Should(Equal("glpat-test-token"))
			Expect(runner.runSpecs).Should(HaveLen(1))
			Expect(runner.runSpecs[0].Name).Should(Equal("glab"))
			Expect(runner.runSpecs[0].Args).Should(Equal([]string{"auth", "login", "--hostname", "gitlab.com", "--use-keyring=false"}))
			Expect(runner.runSpecs[0].Env).Should(ContainElements("GLAB_CONFIG_DIR="+filepath.Join(store.Directory(), glabConfigSubdir), "GLAB_CHECK_UPDATE=false"))
			Expect(runner.runSpecs[0].ClearEnv).Should(ContainElements("GLAB_CONFIG_DIR", "GLAB_CHECK_UPDATE", "GLAB_ENABLE_CI_AUTOLOGIN", "GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN", "GITLAB_HOST"))

			Expect(runner.stdoutSpecs).Should(HaveLen(1))
			Expect(runner.stdoutSpecs[0].Name).Should(Equal("glab"))
			Expect(runner.stdoutSpecs[0].Args).Should(Equal([]string{"config", "get", "token", "--host", "gitlab.com"}))
			Expect(runner.stdoutSpecs[0].Env).Should(Equal(runner.runSpecs[0].Env))

			tokenPath := filepath.Join(store.Directory(), gitlabTokenFilename)
			info, statErr := os.Stat(tokenPath)
			Expect(statErr).ShouldNot(HaveOccurred())
			Expect(info.Mode().Perm() & 0o077).Should(BeZero())
			content, readErr := os.ReadFile(tokenPath)
			Expect(readErr).ShouldNot(HaveOccurred())
			Expect(string(content)).Should(Equal("glpat-test-token"))
			Expect(filepath.Join(store.Directory(), glabConfigSubdir)).Should(BeADirectory())

			Expect(store.Destroy()).Should(Succeed())
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
		})
	})

	When("glab returns an OAuth token", func() {
		It("should reject it and require a personal access token", func() {
			runner.stdout = strings.Repeat("a", 64) + "\n"

			_, err := GitLabToken(context.Background(), runner, store, true, "")

			Expect(err).Should(MatchError(ContainSubstring("choose Token")))
		})
	})

	When("the API URL is self-managed", func() {
		It("should reject it and point at the token fallback", func() {
			_, err := GitLabToken(context.Background(), runner, store, true, "https://gitlab.example.com/api/v4")
			Expect(err).Should(MatchError(ContainSubstring("set GITLAB_BOT_TOKEN")))
			Expect(runner.runSpecs).Should(BeEmpty())
		})
	})

	When("the API URL requires a custom protocol or port", func() {
		It("should reject it and point at the token fallback", func() {
			_, err := GitLabToken(context.Background(), runner, store, true, "http://gitlab.com:8080/api/v4")
			Expect(err).Should(MatchError(ContainSubstring("set GITLAB_BOT_TOKEN")))
			Expect(runner.runSpecs).Should(BeEmpty())
		})
	})

	When("the login fails", func() {
		It("should return the error", func() {
			runner.runErr = errors.New("login denied")

			_, err := GitLabToken(context.Background(), runner, store, true, "")
			Expect(err).Should(MatchError(ContainSubstring("authenticate to GitLab")))
		})
	})

	When("the token cannot be read", func() {
		It("should return the error", func() {
			runner.stdoutErr = errors.New("token denied")

			_, err := GitLabToken(context.Background(), runner, store, true, "")
			Expect(err).Should(MatchError(ContainSubstring("read the GitLab token")))
		})
	})

	When("the token is empty", func() {
		It("should fail", func() {
			runner.stdout = " \n"

			_, err := GitLabToken(context.Background(), runner, store, true, "")
			Expect(err).Should(MatchError(ContainSubstring("empty token")))
		})
	})

	When("glab is unavailable", func() {
		It("should fail before creating credentials", func() {
			runner.lookPathErr = errors.New("not found")

			_, err := GitLabToken(context.Background(), runner, store, true, "")
			Expect(err).Should(MatchError(ContainSubstring("GitLab CLI is required")))
			Expect(runner.runSpecs).Should(BeEmpty())
		})
	})

	When("the session is not interactive", func() {
		It("should explain the token alternative", func() {
			_, err := GitLabToken(context.Background(), runner, store, false, "")
			Expect(err).Should(MatchError(ContainSubstring("set GITLAB_BOT_TOKEN")))
			Expect(runner.runSpecs).Should(BeEmpty())
		})
	})

	When("no credential store is supplied", func() {
		It("should fail", func() {
			_, err := GitLabToken(context.Background(), runner, nil, true, "")
			Expect(err).Should(MatchError("credential store is required"))
		})
	})

	When("no command runner is supplied", func() {
		It("should fail", func() {
			_, err := GitLabToken(context.Background(), nil, store, true, "")
			Expect(err).Should(MatchError("command runner is required"))
		})
	})
})

var _ = Describe("RefreshGitLabToken", func() {
	var (
		runner   *fakeRunner
		store    *Store
		tempRoot string
	)
	BeforeEach(func() {
		runner = &fakeRunner{stdout: "glpat-initial\n"}
		tempRoot = GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tempRoot)
		var err error
		store, err = NewStore()
		Expect(err).ShouldNot(HaveOccurred())
		DeferCleanup(func() { _ = store.Destroy() })
		_, err = GitLabToken(context.Background(), runner, store, true, "")
		Expect(err).ShouldNot(HaveOccurred())
		runner.stdout = "glpat-refreshed\n"
	})

	When("the stored login can refresh", func() {
		It("should trigger an API call and return the new token", func() {
			token, err := RefreshGitLabToken(context.Background(), runner, store, "")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(token).Should(Equal("glpat-refreshed"))
			Expect(runner.stdoutSpecs).Should(HaveLen(3))
			Expect(runner.stdoutSpecs[1].Args).Should(Equal([]string{"api", "user"}))
			Expect(runner.stdoutSpecs[2].Args).Should(Equal([]string{"config", "get", "token", "--host", "gitlab.com"}))

			content, readErr := os.ReadFile(filepath.Join(store.Directory(), gitlabTokenFilename))
			Expect(readErr).ShouldNot(HaveOccurred())
			Expect(string(content)).Should(Equal("glpat-refreshed"))
		})
	})

	When("the login state is missing", func() {
		It("should fail", func() {
			missing, err := NewStore()
			Expect(err).ShouldNot(HaveOccurred())
			DeferCleanup(func() { _ = missing.Destroy() })

			_, err = RefreshGitLabToken(context.Background(), runner, missing, "")
			Expect(err).Should(MatchError(ContainSubstring("login state is missing")))
		})
	})

	When("the refresh call fails", func() {
		It("should return the error", func() {
			runner.stdoutErr = errors.New("refresh denied")
			_, err := RefreshGitLabToken(context.Background(), runner, store, "")
			Expect(err).Should(MatchError(ContainSubstring("refresh the GitLab token")))
		})
	})

	When("no command runner is supplied", func() {
		It("should fail", func() {
			_, err := RefreshGitLabToken(context.Background(), nil, store, "")
			Expect(err).Should(MatchError("command runner is required"))
		})
	})
})

var _ = Describe("glabHostname", func() {
	DescribeTable("API URL parsing",
		func(apiURL, wantHost, wantErr string) {
			host, err := glabHostname(apiURL)
			if wantErr != "" {
				Expect(err).Should(MatchError(ContainSubstring(wantErr)))
				return
			}
			Expect(err).ShouldNot(HaveOccurred())
			Expect(host).Should(Equal(wantHost))
		},
		Entry("empty URL uses gitlab.com", "", "gitlab.com", ""),
		Entry("default API URL", "https://gitlab.com/api/v4", "gitlab.com", ""),
		Entry("trailing slash", "https://gitlab.com/api/v4/", "gitlab.com", ""),
		Entry("self-managed host requires a token", "https://gitlab.example.com/api/v4", "", "only configured for gitlab.com"),
		Entry("http requires a token", "http://gitlab.com/api/v4", "", "requires a standard HTTPS"),
		Entry("custom port requires a token", "https://gitlab.com:8443/api/v4", "", "requires a standard HTTPS"),
		Entry("missing scheme", "gitlab.com/api/v4", "", "invalid GitLab API URL"),
		Entry("unsupported scheme", "ftp://gitlab.com/api/v4", "", "invalid GitLab API URL"),
		Entry("missing host", "https:///api/v4", "", "invalid GitLab API URL"),
	)
})
