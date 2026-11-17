package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"

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
		runner = &fakeRunner{stdout: "glpat_token\n"}
		tempRoot = GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tempRoot)
		var err error
		store, err = NewStore()
		Expect(err).ShouldNot(HaveOccurred())
		DeferCleanup(func() { _ = store.Destroy() })
	})

	When("the browser login succeeds", func() {
		It("should return the token and keep only a private token file", func() {
			token, err := GitLabToken(context.Background(), runner, store, true, "")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(token).Should(Equal("glpat_token"))
			Expect(runner.runSpecs).Should(HaveLen(1))
			Expect(runner.runSpecs[0].Name).Should(Equal("glab"))
			Expect(runner.runSpecs[0].Args).Should(Equal([]string{"auth", "login", "--hostname", "gitlab.com"}))
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
			Expect(string(content)).Should(Equal("glpat_token"))
			Expect(filepath.Join(store.Directory(), glabConfigSubdir)).ShouldNot(BeADirectory())

			Expect(store.Destroy()).Should(Succeed())
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
		})
	})

	When("a custom HTTPS API URL is configured", func() {
		It("should pass the hostname to glab", func() {
			_, err := GitLabToken(context.Background(), runner, store, true, "https://gitlab.example.com/api/v4")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(runner.runSpecs[0].Args).Should(Equal([]string{"auth", "login", "--hostname", "gitlab.example.com"}))
			Expect(runner.stdoutSpecs[0].Args).Should(Equal([]string{"config", "get", "token", "--host", "gitlab.example.com"}))
		})
	})

	When("the API URL requires a custom protocol or port", func() {
		It("should reject it and point at the token fallback", func() {
			_, err := GitLabToken(context.Background(), runner, store, true, "http://gitlab.example.com:8080/api/v4")
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
		Entry("custom HTTPS host", "https://gitlab.example.com/api/v4", "gitlab.example.com", ""),
		Entry("http requires a token", "http://gitlab.example.com/api/v4", "", "set GITLAB_BOT_TOKEN"),
		Entry("custom port requires a token", "https://gitlab.example.com:8443/api/v4", "", "set GITLAB_BOT_TOKEN"),
		Entry("missing scheme", "gitlab.com/api/v4", "", "invalid GitLab API URL"),
		Entry("unsupported scheme", "ftp://gitlab.com/api/v4", "", "invalid GitLab API URL"),
		Entry("missing host", "https:///api/v4", "", "invalid GitLab API URL"),
	)
})
