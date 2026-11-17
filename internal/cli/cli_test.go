package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/model"
)

// providerRunner records run requests and simulates provider behavior.
type providerRunner struct {
	mu       sync.Mutex
	requests []config.Request
	logs     map[string]string     // provider -> text to write into its RunLog
	started  chan string           // optional
	release  <-chan struct{}       // optional
	failures map[string]error      // provider -> error
	outcomes map[string]RunOutcome // provider -> outcome override
}

// Run records the request, optionally blocks, writes provider output to the log, and returns the configured outcome.
func (r *providerRunner) Run(ctx context.Context, request config.Request, log RunLog) (RunOutcome, error) {
	r.mu.Lock()
	r.requests = append(r.requests, request)
	r.mu.Unlock()
	if r.started != nil {
		r.started <- request.Provider
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return RunOutcome{}, ctx.Err()
		}
	}
	if text, ok := r.logs[request.Provider]; ok {
		_ = log.Attach(filepath.Join(request.StateDir, request.Provider+".log"))
		fmt.Fprintf(log, "%s", text)
	}
	outcome := r.outcomes[request.Provider]
	if outcome.Provider == "" {
		outcome.Provider = request.Provider
	}
	if outcome.Stage == "" {
		outcome.Stage = model.PhaseCompleted
	}
	return outcome, r.failures[request.Provider]
}

// cleanupRecordingRunner records cleanup dispatches and satisfies Runner.
type cleanupRecordingRunner struct {
	cleanupCalls int
	request      config.Request
}

// Run reports no run behavior; the cleanup tests never invoke it.
func (*cleanupRecordingRunner) Run(context.Context, config.Request, RunLog) (RunOutcome, error) {
	return RunOutcome{}, nil
}

// Cleanup records the cleanup request.
func (r *cleanupRecordingRunner) Cleanup(_ context.Context, request config.Request) error {
	r.cleanupCalls++
	r.request = request
	return nil
}

var _ = Describe("providerNames", func() {
	DescribeTable("should list the selected providers in report order",
		func(provider string, want []string) {
			Expect(providerNames(provider)).Should(Equal(want))
		},
		Entry("github", "github", []string{"github"}),
		Entry("gitlab", "gitlab", []string{"gitlab"}),
		Entry("both", "both", []string{"github", "gitlab"}),
	)
})

var _ = Describe("runProviders", func() {
	When("both providers are selected", func() {
		It("should start them concurrently and keep fixed order", func() {
			release := make(chan struct{})
			runner := &providerRunner{started: make(chan string, 2), release: release}
			request := config.Request{Provider: config.ProviderBoth}

			var results []ProviderResult
			done := make(chan struct{})
			go func() {
				defer close(done)
				results = runProviders(context.Background(), request, runner)
			}()

			for range 2 {
				Eventually(runner.started, 1).Should(Receive())
			}
			close(release)
			Eventually(done).Should(BeClosed())

			Expect(results).Should(HaveLen(2))
			Expect(results[0].Provider).Should(Equal("github"))
			Expect(results[1].Provider).Should(Equal("gitlab"))
		})

		It("should give each provider its own namespace, repository, and run ID", func() {
			for key, value := range map[string]string{"GITHUB_TOKEN": "gh-token", "GITLAB_BOT_TOKEN": "gl-token"} {
				original, had := os.LookupEnv(key)
				Expect(os.Setenv(key, value)).Should(Succeed())
				defer func(key, original string, had bool) {
					if had {
						Expect(os.Setenv(key, original)).Should(Succeed())
					} else {
						Expect(os.Unsetenv(key)).Should(Succeed())
					}
				}(key, original, had)
			}
			request, err := config.Parse([]string{"run", "both"})
			Expect(err).ShouldNot(HaveOccurred())
			request.StateDir = GinkgoT().TempDir()
			runner := &providerRunner{}

			results := runProviders(context.Background(), request, runner)
			Expect(results).Should(HaveLen(2))
			Expect(runner.requests).Should(HaveLen(2))

			byProvider := map[string]config.Request{}
			for _, recorded := range runner.requests {
				byProvider[recorded.Provider] = recorded
			}
			github, gitlab := byProvider["github"], byProvider["gitlab"]
			Expect(github.TenantNamespace).Should(Equal(config.DefaultGitHubTenantNamespace))
			Expect(gitlab.TenantNamespace).Should(Equal(config.DefaultGitLabTenantNamespace))
			Expect(github.TenantNamespace).ShouldNot(Equal(gitlab.TenantNamespace))
			Expect(github.SourceRepository).Should(Equal(config.DefaultGitHubRepo))
			Expect(gitlab.SourceRepository).Should(Equal(config.DefaultGitLabRepo))
			Expect(github.SourceRepository).ShouldNot(Equal(gitlab.SourceRepository))
			Expect(github.StateDir).Should(Equal(gitlab.StateDir))
			Expect(github.RunID).ShouldNot(BeEmpty())
			Expect(gitlab.RunID).ShouldNot(BeEmpty())
			Expect(github.RunID).ShouldNot(Equal(gitlab.RunID))
		})

		It("should write each provider's output to its own log", func() {
			request := config.Request{Provider: config.ProviderBoth, StateDir: GinkgoT().TempDir()}
			runner := &providerRunner{
				logs: map[string]string{"github": "github-only\n", "gitlab": "gitlab-only\n"},
			}

			results := runProviders(context.Background(), request, runner)
			Expect(results).Should(HaveLen(2))

			for _, provider := range []string{"github", "gitlab"} {
				path := filepath.Join(request.StateDir, provider+".log")
				Expect(readFile(path)).Should(Equal(provider + "-only\n"))
			}
		})
	})
	When("one provider fails", func() {
		It("should return both results with the failure only on that provider", func() {
			request := config.Request{Provider: config.ProviderBoth, StateDir: GinkgoT().TempDir()}
			runner := &providerRunner{failures: map[string]error{"github": errors.New("github failed")}}

			results := runProviders(context.Background(), request, runner)
			Expect(results).Should(HaveLen(2))
			Expect(results[0].Provider).Should(Equal("github"))
			Expect(results[0].Err).Should(MatchError("github failed"))
			Expect(results[1].Provider).Should(Equal("gitlab"))
			Expect(results[1].Err).ShouldNot(HaveOccurred())
			Expect(runner.requests).Should(HaveLen(2))
		})
	})
})

var _ = Describe("providerResult", func() {
	DescribeTable("should normalize results",
		func(outcome RunOutcome, err error, wantStage model.Phase, wantErr bool) {
			result := providerResult("github", outcome, err)
			Expect(result.Provider).Should(Equal("github"))
			Expect(result.RunID).Should(Equal(outcome.RunID))
			Expect(result.ArtifactDirectory).Should(Equal(outcome.ArtifactDirectory))
			Expect(result.Stage).Should(Equal(wantStage))
			if wantErr {
				Expect(result.Err).Should(HaveOccurred())
			} else {
				Expect(result.Err).ShouldNot(HaveOccurred())
			}
		},
		Entry("empty stage defaults to preflight", RunOutcome{}, nil, model.PhasePreflight, false),
		Entry("fields are copied", RunOutcome{Stage: model.PhaseBuildsVerified, RunID: "run-1", ArtifactDirectory: "github-run-x"}, nil, model.PhaseBuildsVerified, false),
		Entry("error is set and stage unchanged", RunOutcome{Stage: model.PhaseBuildsVerified}, errors.New("boom"), model.PhaseBuildsVerified, true),
	)
})

var _ = Describe("Execute", func() {
	When("a run is requested", func() {
		It("should run every listed cluster", func() {
			home := GinkgoT().TempDir()
			dir := GinkgoT().TempDir()
			// Set HOME so clusterfile resolution and the auth ladder see a clean home.
			originalHome, hadHome := os.LookupEnv("HOME")
			Expect(os.Setenv("HOME", home)).Should(Succeed())
			defer func() {
				if hadHome {
					Expect(os.Setenv("HOME", originalHome)).Should(Succeed())
				} else {
					Expect(os.Unsetenv("HOME")).Should(Succeed())
				}
			}()

			kubeconfigC1 := writeKubeconfig(dir, "c1")
			kubeconfigC2 := writeKubeconfig(dir, "c2")
			clustersPath := filepath.Join(dir, "clusters.yaml")
			Expect(os.WriteFile(clustersPath, []byte(fmt.Sprintf("clusters:\n  - name: c1\n    kubeconfig: %s\n    server: https://api.c1.example\n  - name: c2\n    kubeconfig: %s\n    server: https://api.c2.example\n", kubeconfigC1, kubeconfigC2)), 0o640)).Should(Succeed())

			request := config.Defaults()
			request.Command = config.CommandRun
			request.Provider = config.ProviderGitHub
			request.StateDir = dir
			request.ClustersFile = clustersPath
			request.Credentials.GitHubToken = "token"

			runner := &providerRunner{}
			Expect(Execute(context.Background(), request, runner, &Console{Out: io.Discard})).Should(Succeed())
			Expect(runner.requests).Should(HaveLen(2))
			// Clusters run concurrently; both must be present regardless of order.
			names := []string{runner.requests[0].ClusterName, runner.requests[1].ClusterName}
			Expect(names).Should(ConsistOf("c1", "c2"))
		})
	})

	When("cleanup is requested", func() {
		It("should dispatch to the cleanup runner", func() {
			runner := &cleanupRecordingRunner{}
			request := config.Request{Command: config.CommandCleanup, RunID: "run-123", ClusterName: "c1", ClustersFile: "x", StateDir: GinkgoT().TempDir()}
			Expect(Execute(context.Background(), request, runner, &Console{Out: io.Discard})).Should(Succeed())
			Expect(runner.cleanupCalls).Should(Equal(1))
			Expect(runner.request.RunID).Should(Equal("run-123"))
		})
	})

	When("cleanup services are missing", func() {
		It("should report the missing services", func() {
			runner := &providerRunner{}
			request := config.Request{Command: config.CommandCleanup, RunID: "run-123", ClusterName: "c1", ClustersFile: "x", StateDir: GinkgoT().TempDir()}
			err := Execute(context.Background(), request, runner, &Console{Out: io.Discard})
			Expect(err).Should(MatchError(ContainSubstring("cleanup services are not configured")))
		})
	})

	When("no runner is supplied", func() {
		It("should report unconfigured workflow services", func() {
			dir := GinkgoT().TempDir()
			home := GinkgoT().TempDir()
			kubeconfig := writeKubeconfig(dir, "c1")
			clustersPath := filepath.Join(dir, "clusters.yaml")
			Expect(os.WriteFile(clustersPath, []byte(fmt.Sprintf("clusters:\n  - name: c1\n    kubeconfig: %s\n    server: https://api.c1.example\n", kubeconfig)), 0o640)).Should(Succeed())

			originalHome, hadHome := os.LookupEnv("HOME")
			Expect(os.Setenv("HOME", home)).Should(Succeed())
			defer func() {
				if hadHome {
					Expect(os.Setenv("HOME", originalHome)).Should(Succeed())
				} else {
					Expect(os.Unsetenv("HOME")).Should(Succeed())
				}
			}()

			request := config.Defaults()
			request.Command = config.CommandRun
			request.Provider = config.ProviderGitHub
			request.StateDir = dir
			request.ClustersFile = clustersPath
			request.Credentials.GitHubToken = "token"

			err := Execute(context.Background(), request, nil, &Console{Out: io.Discard})
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).Should(ContainSubstring("workflow services are not configured"))
		})
	})

	When("the command is unknown", func() {
		It("should reject it", func() {
			request := config.Request{Command: "deploy", StateDir: GinkgoT().TempDir()}
			err := Execute(context.Background(), request, &providerRunner{}, &Console{Out: io.Discard})
			Expect(err).Should(MatchError("unsupported command"))
		})
	})
})

// writeKubeconfig writes a minimal kubeconfig for cluster name and returns its path.
func writeKubeconfig(dir, name string) string {
	return writeSharedServerKubeconfig(dir, name, "https://api."+name+".example")
}
