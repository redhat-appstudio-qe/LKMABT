package config

import (
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Parse", func() {
	When("the run command is used", func() {
		It("uses canonical fixture for run command", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			got, err := Parse([]string{"run", "github"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.Command).Should(Equal(CommandRun))
			Expect(got.Provider).Should(Equal(ProviderGitHub))
			Expect(got.ClustersFile).Should(Equal(".lkmabt-clusters.yaml"))
			Expect(got.SourceRepository).Should(Equal(DefaultGitHubRepo))
			Expect(got.TenantNamespace).Should(Equal(DefaultGitHubTenantNamespace))
			Expect(got.Timeouts.Trigger).Should(Equal(5 * time.Minute))
		})

		It("uses provider-specific fixtures and namespaces", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			GinkgoT().Setenv("GITLAB_BOT_TOKEN", "token")
			githubRequest, err := Parse([]string{"run", "github", "--source-repository", DefaultGitHubRepo})
			Expect(err).ShouldNot(HaveOccurred())
			gitlabRequest, err := Parse([]string{"run", "gitlab"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(gitlabRequest.SourceRepository).Should(Equal(DefaultGitLabRepo))
			Expect(githubRequest.TenantNamespace).ShouldNot(Equal(gitlabRequest.TenantNamespace))
		})

		It("accepts both fixed fixture providers", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "github-token")
			GinkgoT().Setenv("GITLAB_BOT_TOKEN", "gitlab-token")
			got, err := Parse([]string{"run", "both"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.Provider).Should(Equal(ProviderBoth))
		})

		It("should default the clusters file to the working directory", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			got, err := Parse([]string{"run", "github"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.ClustersFile).Should(Equal(".lkmabt-clusters.yaml"))
		})

		It("should honor the clusters-file flag", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			got, err := Parse([]string{"run", "github", "--clusters-file", "/tmp/clusters.yaml"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.ClustersFile).Should(Equal("/tmp/clusters.yaml"))
		})

		It("rejects a non-canonical fixture", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			_, err := Parse([]string{"run", "github", "--source-repository", "https://github.com/other/repo"})
			Expect(err).Should(MatchError(ContainSubstring("fixture must be")))
		})

		It("honors explicit tenant namespace", func() {
			GinkgoT().Setenv("GITLAB_BOT_TOKEN", "token")
			got, err := Parse([]string{"run", "gitlab", "--tenant-namespace", "custom-tenant"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.TenantNamespace).Should(Equal("custom-tenant"))
		})

		It("should not read KONFLUX_CLUSTER_SERVER from the environment", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			GinkgoT().Setenv("KONFLUX_CLUSTER_SERVER", "https://api.example")
			got, err := Parse([]string{"run", "github"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.ClusterServer).Should(Equal(""))
		})

		It("requires positive timeouts", func() {
			req := Defaults()
			req.Command = CommandRun
			req.Provider = ProviderGitHub
			req.Credentials.GitHubToken = "token"
			req.Timeouts.Build = -time.Second
			err := req.Validate()
			Expect(err).Should(MatchError(ContainSubstring("build timeout")))

			req.Timeouts.Build = time.Minute
			req.Timeouts.Trigger = -time.Second
			err = req.Validate()
			Expect(err).Should(MatchError(ContainSubstring("trigger timeout")))
		})

		It("requires provider tokens", func() {
			for _, provider := range []string{ProviderGitHub, ProviderGitLab} {
				req := Defaults()
				req.Command = CommandRun
				req.Provider = provider
				err := req.Validate()
				Expect(err).Should(HaveOccurred(), provider)
			}
		})
	})

	When("the cleanup command is used", func() {
		It("parses cleanup by exact run ID", func() {
			got, err := Parse([]string{"cleanup", "--run-id", "run-123", "--cluster", "kflux-lw-p01"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.Command).Should(Equal(CommandCleanup))
			Expect(got.RunID).Should(Equal("run-123"))
			Expect(got.ClusterName).Should(Equal("kflux-lw-p01"))
		})

		It("should honor the clusters-file flag for cleanup", func() {
			got, err := Parse([]string{"cleanup", "--run-id", "run-1", "--cluster", "c1", "--clusters-file", "/tmp/c.yaml"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.ClusterName).Should(Equal("c1"))
			Expect(got.ClustersFile).Should(Equal("/tmp/c.yaml"))
		})
	})

	When("credentials are loaded", func() {
		It("requires credentials for both providers", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "github-token")
			GinkgoT().Setenv("GITLAB_BOT_TOKEN", "")
			_, err := Parse([]string{"run", "both"})
			Expect(err).Should(MatchError(ContainSubstring("GITLAB_BOT_TOKEN")))
		})

		It("loads credentials from env file", func() {
			envFile := GinkgoT().TempDir() + "/konflux.env"
			Expect(os.WriteFile(envFile, []byte("GITLAB_BOT_TOKEN='file-token'\nGITLAB_API_URL=https://gitlab.example/api/v4\n"), 0o600)).Should(Succeed())
			GinkgoT().Setenv("GITLAB_BOT_TOKEN", "")
			GinkgoT().Setenv("GITLAB_API_URL", "")
			got, err := Parse([]string{"run", "gitlab", "--env-file", envFile})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.Credentials.GitLabToken).Should(Equal("file-token"))
			Expect(got.Credentials.GitLabAPIURL).Should(Equal("https://gitlab.example/api/v4"))
		})

		It("should fail before any cluster selection", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "")
			_, err := Parse([]string{"run", "github", "--clusters-file", "/nonexistent.yaml"})
			Expect(err).Should(MatchError(ContainSubstring("GITHUB_TOKEN")))
		})
	})

	When("input is invalid", func() {
		It("rejects obsolete commands", func() {
			for _, command := range []string{"legacy-collection", "legacy-cleanup"} {
				_, err := Parse([]string{command, "run-123", "--cluster", "c1"})
				Expect(err).Should(HaveOccurred(), command)
			}
		})

		DescribeTable("rejects invalid input",
			func(args []string, wantSubstring string) {
				_, err := Parse(args)
				Expect(err).Should(MatchError(ContainSubstring(wantSubstring)))
			},
			Entry("invalid provider", []string{"run", "bitbucket"}, "provider"),
			Entry("invalid duration", []string{"run", "github", "--build-timeout", "0s"}, "build timeout"),
			Entry("empty fixture", []string{"run", "github", "--application", ""}, "application"),
			Entry("removed resume flag", []string{"run", "github", "--resume", "run-1"}, "flag provided but not defined: -resume"),
			Entry("cleanup requires run ID", []string{"cleanup", "--cluster", "kflux-lw-p01"}, "--run-id"),
			Entry("cleanup without cluster", []string{"cleanup", "--run-id", "run-123"}, "--cluster"),
			Entry("invalid cleanup cluster name", []string{"cleanup", "--run-id", "run-123", "--cluster", "bad/name"}, "--cluster"),
			Entry("removed cluster-server flag", []string{"run", "github", "--cluster-server", "https://api.example"}, "flag provided but not defined: -cluster-server"),
			Entry("run rejects the cleanup cluster flag", []string{"run", "github", "--cluster", "c1"}, "flag provided but not defined: -cluster"),
		)
	})
})
