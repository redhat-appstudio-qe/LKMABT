package config

import (
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/platform"
)

var _ = Describe("Parse", func() {
	BeforeEach(func() {
		original, err := os.Getwd()
		Expect(err).ShouldNot(HaveOccurred())
		Expect(os.Chdir(GinkgoT().TempDir())).Should(Succeed())
		DeferCleanup(func() {
			Expect(os.Chdir(original)).Should(Succeed())
		})
	})

	When("the run command is used", func() {
		It("uses canonical fixture for run command", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			got, err := Parse([]string{"github"})
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
			githubRequest, err := Parse([]string{"github", "--source-repository", DefaultGitHubRepo})
			Expect(err).ShouldNot(HaveOccurred())
			gitlabRequest, err := Parse([]string{"gitlab"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(gitlabRequest.SourceRepository).Should(Equal(DefaultGitLabRepo))
			Expect(githubRequest.TenantNamespace).ShouldNot(Equal(gitlabRequest.TenantNamespace))
		})

		It("accepts both fixed fixture providers", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "github-token")
			GinkgoT().Setenv("GITLAB_BOT_TOKEN", "gitlab-token")
			got, err := Parse([]string{"both"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.Provider).Should(Equal(ProviderBoth))
		})

		It("defaults to every supported build platform", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			got, err := Parse([]string{"github"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.Platforms).Should(Equal([]string{"linux/amd64", "linux/arm64", "linux/s390x", "linux/ppc64le"}))
		})

		DescribeTable("selects architectures from the second argument",
			func(architecture string, want []string) {
				GinkgoT().Setenv("GITHUB_TOKEN", "token")
				GinkgoT().Setenv("GITLAB_BOT_TOKEN", "token")
				got, err := Parse([]string{ProviderGitHub, architecture, "--clusters-file", "clusters.yaml"})
				Expect(err).ShouldNot(HaveOccurred())
				Expect(got.Platforms).Should(Equal(want))
				Expect(got.ClustersFile).Should(Equal("clusters.yaml"))
			},
			Entry("arm64", "arm64", []string{platform.LinuxARM64}),
			Entry("amd64", "amd64", []string{platform.LinuxAMD64}),
			Entry("s390x", "s390x", []string{platform.LinuxS390X}),
			Entry("ppc64le", "ppc64le", []string{platform.LinuxPPC64LE}),
			Entry("aws", "aws", []string{platform.LinuxAMD64, platform.LinuxARM64}),
			Entry("ibm", "ibm", []string{platform.LinuxS390X, platform.LinuxPPC64LE}),
		)

		DescribeTable("combines repeated architecture arguments",
			func(arguments []string, want []string) {
				GinkgoT().Setenv("GITHUB_TOKEN", "token")
				args := append([]string{ProviderGitHub}, arguments...)
				args = append(args, "--clusters-file", "clusters.yaml")
				got, err := Parse(args)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(got.Platforms).Should(Equal(want))
			},
			Entry("space separated", []string{"s390x", "arm64"}, []string{platform.LinuxS390X, platform.LinuxARM64}),
			Entry("comma separated", []string{"s390x,arm64"}, []string{platform.LinuxS390X, platform.LinuxARM64}),
			Entry("comma followed by a shell-split argument", []string{"s390x,", "arm64"}, []string{platform.LinuxS390X, platform.LinuxARM64}),
			Entry("duplicates expand once", []string{"s390x", "s390x,arm64"}, []string{platform.LinuxS390X, platform.LinuxARM64}),
			Entry("groups and architectures mix", []string{"ibm", "arm64"}, []string{platform.LinuxS390X, platform.LinuxPPC64LE, platform.LinuxARM64}),
		)

		It("rejects an unsupported architecture among several", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			_, err := Parse([]string{"github", "arm64", "riscv64"})
			Expect(err).Should(MatchError(ContainSubstring("unsupported architecture")))
			Expect(err).Should(MatchError(ContainSubstring("riscv64")))
		})

		DescribeTable("rejects malformed architecture lists",
			func(arguments []string) {
				GinkgoT().Setenv("GITHUB_TOKEN", "token")
				_, err := Parse(append([]string{ProviderGitHub}, arguments...))
				Expect(err).Should(MatchError(ContainSubstring("contains an empty entry")))
			},
			Entry("leading empty entry", []string{",arm64"}),
			Entry("interior empty entry", []string{"arm64,,s390x"}),
			Entry("multiple trailing commas", []string{"arm64,,"}),
			Entry("only commas", []string{","}),
			Entry("empty argument", []string{""}),
		)

		It("rejects an unsupported architecture", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			_, err := Parse([]string{"github", "riscv64"})
			Expect(err).Should(MatchError(ContainSubstring("unsupported architecture")))
			Expect(err).Should(MatchError(ContainSubstring("ppc64le")))
		})

		It("does not accept an architecture after flags", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			_, err := Parse([]string{"github", "--clusters-file", "clusters.yaml", "arm64"})
			Expect(err).Should(MatchError(ContainSubstring("unexpected arguments: arm64")))
		})

		It("should default the clusters file to the working directory", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			got, err := Parse([]string{"github"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.ClustersFile).Should(Equal(".lkmabt-clusters.yaml"))
		})

		It("should honor the clusters-file flag", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			got, err := Parse([]string{"github", "--clusters-file", "/tmp/clusters.yaml"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.ClustersFile).Should(Equal("/tmp/clusters.yaml"))
		})

		It("rejects a non-canonical fixture", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			_, err := Parse([]string{"github", "--source-repository", "https://github.com/other/repo"})
			Expect(err).Should(MatchError(ContainSubstring("fixture must be")))
		})

		It("honors explicit tenant namespace", func() {
			GinkgoT().Setenv("GITLAB_BOT_TOKEN", "token")
			got, err := Parse([]string{"gitlab", "--tenant-namespace", "custom-tenant"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.TenantNamespace).Should(Equal("custom-tenant"))
		})

		It("should not read KONFLUX_CLUSTER_SERVER from the environment", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			GinkgoT().Setenv("KONFLUX_CLUSTER_SERVER", "https://api.example")
			got, err := Parse([]string{"github"})
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

		It("should select only the smoke component", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "token")
			got, err := Parse([]string{"github", "--smoke"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.Smoke).Should(BeTrue())
			Expect(got.ExpectedComponents()).Should(Equal(1))
			Expect(got.ComponentNames).Should(Equal([]string{"mathwizz-history-worker"}))
			Expect(got.ComponentPaths).Should(Equal([]string{"history-worker/Dockerfile"}))
			Expect(got.TektonPaths).Should(Equal([]string{".tekton/konflux-test-mathwizz-history-worker-push.yaml"}))
		})

		DescribeTable("applying the smoke selection alongside other run settings",
			func(args []string, wantProvider string, wantRepository string, wantTenant string, wantBranch string, wantApplication string, wantPlatforms []string) {
				GinkgoT().Setenv("GITHUB_TOKEN", "token")
				GinkgoT().Setenv("GITLAB_BOT_TOKEN", "token")
				got, err := Parse(args)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(got.Provider).Should(Equal(wantProvider))
				Expect(got.SourceRepository).Should(Equal(wantRepository))
				Expect(got.TenantNamespace).Should(Equal(wantTenant))
				Expect(got.SourceBranch).Should(Equal(wantBranch))
				Expect(got.ApplicationName).Should(Equal(wantApplication))
				Expect(got.Platforms).Should(Equal(wantPlatforms))
				Expect(got.Smoke).Should(BeTrue())
				Expect(got.ExpectedComponents()).Should(Equal(1))
				Expect(got.ComponentNames).Should(Equal([]string{"mathwizz-history-worker"}))
				Expect(got.ComponentPaths).Should(Equal([]string{"history-worker/Dockerfile"}))
				Expect(got.TektonPaths).Should(Equal([]string{".tekton/konflux-test-mathwizz-history-worker-push.yaml"}))
			},
			Entry("github with amd64", []string{"github", "amd64", "--smoke"}, ProviderGitHub, "https://github.com/redhat-appstudio-qe/dr_test_mathwizz", "mathwizz-test-github", "main", "mathwizz", []string{platform.LinuxAMD64}),
			Entry("gitlab", []string{"gitlab", "--smoke"}, ProviderGitLab, "https://gitlab.com/konflux-qe/dr_test_mathwizz_gl", "mathwizz-test-gitlab", "main", "mathwizz", []string{platform.LinuxAMD64, platform.LinuxARM64, platform.LinuxS390X, platform.LinuxPPC64LE}),
			Entry("both", []string{"both", "--smoke"}, ProviderBoth, "https://github.com/redhat-appstudio-qe/dr_test_mathwizz", "mathwizz-test-github", "main", "mathwizz", []string{platform.LinuxAMD64, platform.LinuxARM64, platform.LinuxS390X, platform.LinuxPPC64LE}),
			Entry("github with flags", []string{"github", "s390x", "--smoke", "--branch", "release-1", "--tenant-namespace", "custom-tenant", "--application", "custom-app"}, ProviderGitHub, "https://github.com/redhat-appstudio-qe/dr_test_mathwizz", "custom-tenant", "release-1", "custom-app", []string{platform.LinuxS390X}),
		)

		DescribeTable("keeping the full selection when smoke is absent or disabled",
			func(args []string) {
				GinkgoT().Setenv("GITHUB_TOKEN", "token")
				got, err := Parse(args)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(got.Smoke).Should(BeFalse())
				Expect(got.ExpectedComponents()).Should(Equal(3))
				Expect(got.ComponentNames).Should(Equal([]string{"mathwizz-web-server", "mathwizz-history-worker", "mathwizz-frontend"}))
				Expect(got.ComponentPaths).Should(Equal([]string{"web-server/Dockerfile", "history-worker/Dockerfile", "frontend/Dockerfile"}))
				Expect(got.TektonPaths).Should(Equal([]string{".tekton/konflux-test-mathwizz-web-server-push.yaml", ".tekton/konflux-test-mathwizz-history-worker-push.yaml", ".tekton/konflux-test-mathwizz-frontend-push.yaml"}))
			},
			Entry("without the flag", []string{"github"}),
			Entry("with an explicit false", []string{"github", "--smoke=false"}),
		)
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

		It("should not read the env file for cleanup", func() {
			Expect(os.WriteFile(DefaultEnvFile, []byte("not-an-assignment"), 0o600)).Should(Succeed())

			got, err := Parse([]string{"cleanup", "--run-id", "run-123", "--cluster", "c1"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.Command).Should(Equal(CommandCleanup))
		})

		It("should reject the run-only env-file flag", func() {
			_, err := Parse([]string{"cleanup", "--run-id", "run-123", "--cluster", "c1", "--env-file", "other.env"})
			Expect(err).Should(MatchError(ContainSubstring("flag provided but not defined: -env-file")))
		})
		It("should reject the run-only smoke flag", func() {
			_, err := Parse([]string{"cleanup", "--run-id", "run-123", "--cluster", "c1", "--smoke"})
			Expect(err).Should(MatchError(ContainSubstring("flag provided but not defined: -smoke")))
		})

	})
	When("credentials are loaded", func() {
		It("defers both provider credentials to run time", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "")
			GinkgoT().Setenv("GITLAB_BOT_TOKEN", "")
			for _, provider := range []string{ProviderGitHub, ProviderGitLab, ProviderBoth} {
				got, err := Parse([]string{provider})
				Expect(err).ShouldNot(HaveOccurred(), provider)
				Expect(got.Credentials.GitHubToken).Should(BeEmpty(), provider)
				Expect(got.Credentials.GitLabToken).Should(BeEmpty(), provider)
			}
		})

		It("loads credentials from env file", func() {
			envFile := GinkgoT().TempDir() + "/konflux.env"
			Expect(os.WriteFile(envFile, []byte("GITLAB_BOT_TOKEN='file-token'\nGITLAB_API_URL=https://gitlab.example/api/v4\n"), 0o600)).Should(Succeed())
			GinkgoT().Setenv("GITLAB_BOT_TOKEN", "")
			GinkgoT().Setenv("GITLAB_API_URL", "")
			got, err := Parse([]string{"gitlab", "--env-file", envFile})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.Credentials.GitLabToken).Should(Equal("file-token"))
			Expect(got.Credentials.GitLabAPIURL).Should(Equal("https://gitlab.example/api/v4"))
		})

		It("loads credentials from the default env file", func() {
			GinkgoT().Setenv("GITLAB_BOT_TOKEN", "")
			GinkgoT().Setenv("GITLAB_API_URL", "")
			Expect(os.WriteFile(DefaultEnvFile, []byte("GITLAB_BOT_TOKEN=default-token\nGITLAB_API_URL=https://gitlab.example/api/v4\n"), 0o600)).Should(Succeed())
			got, err := Parse([]string{"gitlab"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.Credentials.GitLabToken).Should(Equal("default-token"))
			Expect(got.Credentials.GitLabAPIURL).Should(Equal("https://gitlab.example/api/v4"))

			GinkgoT().Setenv("GITLAB_BOT_TOKEN", "env-token")
			GinkgoT().Setenv("GITLAB_API_URL", "https://env.example/api/v4")
			got, err = Parse([]string{"gitlab"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.Credentials.GitLabToken).Should(Equal("env-token"))
			Expect(got.Credentials.GitLabAPIURL).Should(Equal("https://env.example/api/v4"))
		})

		It("ignores a missing default env file", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "env-token")
			got, err := Parse([]string{"github"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.Credentials.GitHubToken).Should(Equal("env-token"))
		})

		It("rejects a missing explicit env file", func() {
			GinkgoT().Setenv("GITHUB_TOKEN", "env-token")
			_, err := Parse([]string{"github", "--env-file", "missing.env"})
			Expect(err).Should(MatchError(ContainSubstring("read env file")))
		})

		It("prefers a process GitLab URL when no token is configured", func() {
			envFile := GinkgoT().TempDir() + "/other.env"
			Expect(os.WriteFile(envFile, []byte("GITLAB_API_URL=https://file.example/api/v4\n"), 0o600)).Should(Succeed())
			GinkgoT().Setenv("GITLAB_BOT_TOKEN", "")
			GinkgoT().Setenv("GITLAB_API_URL", "https://gitlab.com/api/v4")

			got, err := Parse([]string{"gitlab", "--env-file", envFile})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.Credentials.GitLabToken).Should(BeEmpty())
			Expect(got.Credentials.GitLabAPIURL).Should(Equal("https://gitlab.com/api/v4"))
		})

		It("ignores mixed GitLab sources for a GitHub-only run", func() {
			envFile := GinkgoT().TempDir() + "/other.env"
			Expect(os.WriteFile(envFile, []byte("GITLAB_API_URL=https://file.example/api/v4\n"), 0o600)).Should(Succeed())
			GinkgoT().Setenv("GITHUB_TOKEN", "github-token")
			GinkgoT().Setenv("GITLAB_BOT_TOKEN", "process-token")
			GinkgoT().Setenv("GITLAB_API_URL", "")

			got, err := Parse([]string{"github", "--env-file", envFile})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(got.Credentials.GitHubToken).Should(Equal("github-token"))
		})

		It("rejects a process GitLab token with only a file API URL", func() {
			envFile := GinkgoT().TempDir() + "/other.env"
			Expect(os.WriteFile(envFile, []byte("GITLAB_BOT_TOKEN=file-token\nGITLAB_API_URL=https://file.example/api/v4\n"), 0o600)).Should(Succeed())
			GinkgoT().Setenv("GITLAB_BOT_TOKEN", "process-token")
			GinkgoT().Setenv("GITLAB_API_URL", "")

			_, err := Parse([]string{"gitlab", "--env-file", envFile})
			Expect(err).Should(MatchError(ContainSubstring("set both in the same source")))
		})

		It("rejects a file GitLab token with only a process API URL", func() {
			envFile := GinkgoT().TempDir() + "/other.env"
			Expect(os.WriteFile(envFile, []byte("GITLAB_BOT_TOKEN=file-token\n"), 0o600)).Should(Succeed())
			GinkgoT().Setenv("GITLAB_BOT_TOKEN", "")
			GinkgoT().Setenv("GITLAB_API_URL", "https://process.example/api/v4")

			_, err := Parse([]string{"gitlab", "--env-file", envFile})
			Expect(err).Should(MatchError(ContainSubstring("set both in the same source")))
		})
	})

	When("input is invalid", func() {
		It("rejects the removed run subcommand with a migration hint", func() {
			_, err := Parse([]string{"run", "github"})
			Expect(err).Should(MatchError(ContainSubstring("lkmabt github")))
			Expect(err).Should(MatchError(ContainSubstring("lkmabt gitlab")))
			Expect(err).Should(MatchError(ContainSubstring("lkmabt both")))
		})

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
			Entry("unsupported command", []string{"bitbucket"}, "unsupported command"),
			Entry("invalid duration", []string{"github", "--build-timeout", "0s"}, "build timeout"),
			Entry("empty fixture", []string{"github", "--application", ""}, "application"),
			Entry("removed resume flag", []string{"github", "--resume", "run-1"}, "flag provided but not defined: -resume"),
			Entry("cleanup requires run ID", []string{"cleanup", "--cluster", "kflux-lw-p01"}, "--run-id"),
			Entry("cleanup without cluster", []string{"cleanup", "--run-id", "run-123"}, "--cluster"),
			Entry("invalid cleanup cluster name", []string{"cleanup", "--run-id", "run-123", "--cluster", "bad/name"}, "--cluster"),
			Entry("removed cluster-server flag", []string{"github", "--cluster-server", "https://api.example"}, "flag provided but not defined: -cluster-server"),
			Entry("run rejects the cleanup cluster flag", []string{"github", "--cluster", "c1"}, "flag provided but not defined: -cluster"),
		)
	})
})

var _ = Describe("Request.ExpectedComponents", func() {
	DescribeTable("should report one expected component in smoke mode and three otherwise",
		func(req Request, want int) {
			Expect(req.ExpectedComponents()).Should(Equal(want))
		},
		Entry("default request", Request{}, 3),
		Entry("smoke request", Request{Smoke: true}, 1),
		Entry("smoke request with a full component path list", Request{Smoke: true, ComponentPaths: []string{"a", "b", "c"}}, 1),
	)
})

var _ = Describe("Request component selection validation", func() {
	It("should return the three aligned fixture entries as fresh slices", func() {
		got := Defaults()
		Expect(got.ComponentNames).Should(Equal([]string{"mathwizz-web-server", "mathwizz-history-worker", "mathwizz-frontend"}))
		Expect(got.ComponentPaths).Should(Equal([]string{"web-server/Dockerfile", "history-worker/Dockerfile", "frontend/Dockerfile"}))
		Expect(got.TektonPaths).Should(Equal([]string{".tekton/konflux-test-mathwizz-web-server-push.yaml", ".tekton/konflux-test-mathwizz-history-worker-push.yaml", ".tekton/konflux-test-mathwizz-frontend-push.yaml"}))

		got.ComponentNames[0] = "mutated"
		got.ComponentPaths[0] = "mutated"
		fresh := Defaults()
		Expect(fresh.ComponentNames).Should(Equal([]string{"mathwizz-web-server", "mathwizz-history-worker", "mathwizz-frontend"}))
		Expect(fresh.ComponentPaths).Should(Equal([]string{"web-server/Dockerfile", "history-worker/Dockerfile", "frontend/Dockerfile"}))
	})

	It("should accept the smoke selection", func() {
		req := Defaults()
		req.Command = CommandRun
		req.Provider = ProviderGitHub
		req.Smoke = true
		req.ComponentNames, req.ComponentPaths, req.TektonPaths = SmokeComponents()
		Expect(req.Validate()).Should(Succeed())
	})

	DescribeTable("rejecting a component selection that does not match ExpectedComponents",
		func(smoke bool, names, paths, tektonPaths []string, wantSubstring string) {
			req := Defaults()
			req.Command = CommandRun
			req.Provider = ProviderGitHub
			req.Smoke = smoke
			req.ComponentNames = names
			req.ComponentPaths = paths
			req.TektonPaths = tektonPaths
			err := req.Validate()
			Expect(err).Should(MatchError(ContainSubstring(wantSubstring)))
		},
		Entry("normal with no selection", false, nil, nil, nil, "exactly 3 component names, Dockerfile paths, and Tekton pipeline paths are required; got 0, 0, 0"),
		Entry("normal with two names", false, []string{"mathwizz-web-server", "mathwizz-history-worker"}, []string{"web-server/Dockerfile", "history-worker/Dockerfile", "frontend/Dockerfile"}, []string{".tekton/konflux-test-mathwizz-web-server-push.yaml", ".tekton/konflux-test-mathwizz-history-worker-push.yaml", ".tekton/konflux-test-mathwizz-frontend-push.yaml"}, "exactly 3 component names, Dockerfile paths, and Tekton pipeline paths are required; got 2, 3, 3"),
		Entry("normal with two Tekton paths", false, []string{"mathwizz-web-server", "mathwizz-history-worker", "mathwizz-frontend"}, []string{"web-server/Dockerfile", "history-worker/Dockerfile", "frontend/Dockerfile"}, []string{".tekton/konflux-test-mathwizz-web-server-push.yaml", ".tekton/konflux-test-mathwizz-history-worker-push.yaml"}, "exactly 3 component names, Dockerfile paths, and Tekton pipeline paths are required; got 3, 3, 2"),
		Entry("normal with one of each", false, []string{"mathwizz-history-worker"}, []string{"history-worker/Dockerfile"}, []string{".tekton/konflux-test-mathwizz-history-worker-push.yaml"}, "exactly 3 component names, Dockerfile paths, and Tekton pipeline paths are required; got 1, 1, 1"),
		Entry("smoke with the full selection", true, []string{"mathwizz-web-server", "mathwizz-history-worker", "mathwizz-frontend"}, []string{"web-server/Dockerfile", "history-worker/Dockerfile", "frontend/Dockerfile"}, []string{".tekton/konflux-test-mathwizz-web-server-push.yaml", ".tekton/konflux-test-mathwizz-history-worker-push.yaml", ".tekton/konflux-test-mathwizz-frontend-push.yaml"}, "exactly 1 component names, Dockerfile paths, and Tekton pipeline paths are required; got 3, 3, 3"),
		Entry("smoke with two of each", true, []string{"mathwizz-web-server", "mathwizz-history-worker"}, []string{"web-server/Dockerfile", "history-worker/Dockerfile"}, []string{".tekton/konflux-test-mathwizz-web-server-push.yaml", ".tekton/konflux-test-mathwizz-history-worker-push.yaml"}, "exactly 1 component names, Dockerfile paths, and Tekton pipeline paths are required; got 2, 2, 2"),
	)

	DescribeTable("validating the canonical smoke selection",
		func(names, paths, tektonPaths []string, wantErr string) {
			req := Defaults()
			req.Command = CommandRun
			req.Provider = ProviderGitHub
			req.Smoke = true
			req.ComponentNames = names
			req.ComponentPaths = paths
			req.TektonPaths = tektonPaths
			err := req.Validate()
			if wantErr == "" {
				Expect(err).Should(Succeed())
				return
			}
			Expect(err).Should(MatchError(Equal(wantErr)))
		},
		Entry("canonical selection",
			[]string{"mathwizz-history-worker"},
			[]string{"history-worker/Dockerfile"},
			[]string{".tekton/konflux-test-mathwizz-history-worker-push.yaml"},
			""),
		Entry("different component",
			[]string{"mathwizz-web-server"},
			[]string{"web-server/Dockerfile"},
			[]string{".tekton/konflux-test-mathwizz-web-server-push.yaml"},
			`smoke runs require the mathwizz-history-worker selection; got component "mathwizz-web-server", Dockerfile "web-server/Dockerfile", and Tekton pipeline ".tekton/konflux-test-mathwizz-web-server-push.yaml"`),
		Entry("different Dockerfile for the canonical component",
			[]string{"mathwizz-history-worker"},
			[]string{"web-server/Dockerfile"},
			[]string{".tekton/konflux-test-mathwizz-history-worker-push.yaml"},
			`smoke runs require the mathwizz-history-worker selection; got component "mathwizz-history-worker", Dockerfile "web-server/Dockerfile", and Tekton pipeline ".tekton/konflux-test-mathwizz-history-worker-push.yaml"`),
		Entry("different Tekton pipeline for the canonical component",
			[]string{"mathwizz-history-worker"},
			[]string{"history-worker/Dockerfile"},
			[]string{".tekton/konflux-test-mathwizz-frontend-push.yaml"},
			`smoke runs require the mathwizz-history-worker selection; got component "mathwizz-history-worker", Dockerfile "history-worker/Dockerfile", and Tekton pipeline ".tekton/konflux-test-mathwizz-frontend-push.yaml"`),
	)

	It("should reject duplicate component names", func() {
		req := Defaults()
		req.Command = CommandRun
		req.Provider = ProviderGitHub
		req.ComponentNames[2] = req.ComponentNames[0]
		err := req.Validate()
		Expect(err).Should(MatchError(Equal(`duplicate component name "mathwizz-web-server"`)))
	})
})
