package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/auth"
	"github.com/redhat-appstudio/lkmabt/internal/clusterauth"
	"github.com/redhat-appstudio/lkmabt/internal/clusterfile"
	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/report"
)

// artifactWritingRunner writes run artifacts for each request and records the outcome.
type artifactWritingRunner struct {
	mu       sync.Mutex
	requests []config.Request
	logLines map[string]string // key "<cluster>/<provider>" -> text for the provider RunLog
	fail     map[string]error  // key "<cluster>/<provider>"
	stage    map[string]model.Phase
}

// Run records the request, writes <StateDir>/artifacts-<provider>/ artifacts, and returns the outcome.
func (r *artifactWritingRunner) Run(ctx context.Context, req config.Request, log RunLog) (RunOutcome, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.mu.Unlock()

	key := req.ClusterName + "/" + req.Provider
	dir := filepath.Join(req.StateDir, "artifacts-"+req.Provider)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return RunOutcome{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "run.log"), []byte("provider log\n"), 0o640); err != nil {
		return RunOutcome{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{}"), 0o640); err != nil {
		return RunOutcome{}, err
	}
	if text, ok := r.logLines[key]; ok {
		if _, err := fmt.Fprintf(log, "%s", text); err != nil {
			return RunOutcome{}, err
		}
	}

	stage := model.PhaseCompleted
	if s, ok := r.stage[key]; ok {
		stage = s
	}
	return RunOutcome{Provider: req.Provider, RunID: req.RunID, ArtifactDirectory: filepath.Base(dir), Stage: stage}, r.fail[key]
}

// gatedRunner blocks each Run until release closes and records peak concurrency.
type gatedRunner struct {
	mu        sync.Mutex
	active    int
	maxActive int
	calls     int
	started   chan struct{}
	release   <-chan struct{}
}

// Run blocks until release closes, tracking peak concurrency.
func (r *gatedRunner) Run(ctx context.Context, req config.Request, log RunLog) (RunOutcome, error) {
	r.mu.Lock()
	r.active++
	if r.active > r.maxActive {
		r.maxActive = r.active
	}
	r.calls++
	r.mu.Unlock()
	r.started <- struct{}{}

	select {
	case <-r.release:
	case <-ctx.Done():
		r.mu.Lock()
		r.active--
		r.mu.Unlock()
		return RunOutcome{}, ctx.Err()
	}

	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	return RunOutcome{Provider: req.Provider, RunID: req.RunID, Stage: model.PhaseCompleted}, nil
}

type githubAuthStub struct {
	token string
	err   error
	calls int
}

// authenticate records the call and returns the configured token or error.
func (s *githubAuthStub) authenticate(_ context.Context, _ clusterauth.CommandRunner, _ *auth.Store, _ bool) (string, error) {
	s.calls++
	return s.token, s.err
}

type gitlabAuthStub struct {
	token  string
	err    error
	calls  int
	apiURL string
}

// authenticate records the call and returns the configured token or error.
func (s *gitlabAuthStub) authenticate(_ context.Context, _ clusterauth.CommandRunner, _ *auth.Store, _ bool, apiURL string) (string, error) {
	s.calls++
	s.apiURL = apiURL
	return s.token, s.err
}

type gitlabRefreshStub struct {
	token string
	err   error
	calls int
}

// refresh records the call and returns the configured token or error.
func (s *gitlabRefreshStub) refresh(_ context.Context, _ clusterauth.CommandRunner, _ *auth.Store, _ string) (string, error) {
	s.calls++
	return s.token, s.err
}

var (
	defaultGitHubAuth  *githubAuthStub
	defaultGitLabAuth  *gitlabAuthStub
	defaultGitLabToken *gitlabRefreshStub
)

var _ = BeforeEach(func() {
	defaultGitHubAuth = &githubAuthStub{token: "test-github-token"}
	defaultGitLabAuth = &gitlabAuthStub{token: "test-gitlab-token"}
	defaultGitLabToken = &gitlabRefreshStub{token: "test-gitlab-token"}
	originalGitHub, originalGitLab, originalRefresh, originalDestroy, originalCheck := authenticateGitHub, authenticateGitLab, refreshGitLabToken, destroyCredentials, checkGitHubAccess
	authenticateGitHub = defaultGitHubAuth.authenticate
	authenticateGitLab = defaultGitLabAuth.authenticate
	refreshGitLabToken = defaultGitLabToken.refresh
	checkGitHubAccess = func(context.Context, string) error { return nil }
	DeferCleanup(func() {
		authenticateGitHub = originalGitHub
		authenticateGitLab = originalGitLab
		refreshGitLabToken = originalRefresh
		destroyCredentials = originalDestroy
		checkGitHubAccess = originalCheck
	})
})

// writeClustersFile writes a clusters file with entries and returns its path.
func writeClustersFile(dir string, entries ...clusterfile.Entry) string {
	path := filepath.Join(dir, "clusters.yaml")
	var builder strings.Builder
	builder.WriteString("clusters:\n")
	for _, entry := range entries {
		fmt.Fprintf(&builder, "  - name: %s\n", entry.Name)
		if entry.Kubeconfig != "" {
			fmt.Fprintf(&builder, "    kubeconfig: %s\n", entry.Kubeconfig)
		}
		if entry.Server != "" {
			fmt.Fprintf(&builder, "    server: %s\n", entry.Server)
		}
	}
	Expect(os.WriteFile(path, []byte(builder.String()), 0o600)).Should(Succeed())
	return path
}

// writeSharedServerKubeconfig writes a kubeconfig for name pointing at server.
func writeSharedServerKubeconfig(dir, name, server string) string {
	path := filepath.Join(dir, "kubeconfig-"+name)
	content := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n  - name: %s\n    cluster:\n      server: %s\ncontexts:\n  - name: %s\n    context:\n      cluster: %s\n      user: %s\ncurrent-context: %s\nusers:\n  - name: %s\n    user: {}\n", name, server, name, name, name, name, name)
	Expect(os.WriteFile(path, []byte(content), 0o600)).Should(Succeed())
	return path
}

var _ = Describe("runCluster", func() {
	When("the entry declares its server", func() {
		It("should derive the per-cluster request and run log", func() {
			aggregateDirectory := GinkgoT().TempDir()
			entry := clusterfile.Entry{Name: "c1", Kubeconfig: "/keys/config-c1", Server: "https://api.c1.example/"}
			request := config.Request{Provider: config.ProviderGitHub}
			runner := &artifactWritingRunner{}

			results := runCluster(context.Background(), request, entry, aggregateDirectory, runner)
			Expect(results).Should(HaveLen(1))
			Expect(results[0].Err).Should(BeNil())

			Expect(runner.requests).Should(HaveLen(1))
			Expect(runner.requests[0].ClusterName).Should(Equal("c1"))
			Expect(runner.requests[0].Kubeconfig).Should(Equal("/keys/config-c1"))
			Expect(runner.requests[0].ClusterServer).Should(Equal("https://api.c1.example/"))
			Expect(runner.requests[0].StateDir).Should(Equal(filepath.Join(aggregateDirectory, "c1")))

			Expect(readFile(filepath.Join(aggregateDirectory, "c1", "run.log"))).Should(ContainSubstring("cluster c1: running github against https://api.c1.example/ with kubeconfig /keys/config-c1"))
		})
	})

	When("the server cannot be resolved", func() {
		It("should fail every selected provider at preflight", func() {
			aggregateDirectory := GinkgoT().TempDir()
			entry := clusterfile.Entry{Name: "c2", Kubeconfig: filepath.Join(GinkgoT().TempDir(), "absent"), Server: ""}
			request := config.Request{Provider: config.ProviderBoth}

			results := runCluster(context.Background(), request, entry, aggregateDirectory, &artifactWritingRunner{})
			Expect(results).Should(HaveLen(2))
			providers := []string{results[0].Provider, results[1].Provider}
			Expect(providers).Should(ConsistOf("github", "gitlab"))
			for _, result := range results {
				Expect(result.Stage).Should(Equal(model.PhasePreflight))
				Expect(result.Err).Should(MatchError(ContainSubstring("resolve API server for cluster c2")))
			}
			Expect(readFile(filepath.Join(aggregateDirectory, "c2", "run.log"))).Should(ContainSubstring("resolve API server:"))
		})
	})
})

var _ = Describe("scheduleClusters", func() {
	When("multiple clusters are listed", func() {
		It("should run them one at a time and preserve order", func() {
			entries := []clusterfile.Entry{
				{Name: "c1", Server: "https://api.c1.example"},
				{Name: "c2", Server: "https://api.c2.example"},
				{Name: "c3", Server: "https://api.c3.example"},
				{Name: "c4", Server: "https://api.c4.example"},
			}
			request := config.Request{Provider: config.ProviderGitHub}
			release := make(chan struct{})
			runner := &gatedRunner{started: make(chan struct{}, 4), release: release}
			console := &Console{Out: io.Discard}
			aggregate := &aggregateRun{Directory: GinkgoT().TempDir()}

			var runs []clusterRun
			done := make(chan struct{})
			go func() {
				defer close(done)
				runs = scheduleClusters(context.Background(), request, entries, nil, aggregate, runner, console, nil)
			}()

			Eventually(runner.started, time.Second).Should(Receive())
			Consistently(runner.started, 250*time.Millisecond).ShouldNot(Receive())
			close(release)
			Eventually(done, 5*time.Second).Should(BeClosed())

			Expect(runner.maxActive).Should(Equal(1))
			Expect(runner.calls).Should(Equal(4))
			Expect(runs).Should(HaveLen(4))
			for index, run := range runs {
				Expect(run.Entry.Name).Should(Equal("c" + string(rune('1'+index))))
				Expect(run.AuthErr).Should(BeNil())
				Expect(run.Results).Should(HaveLen(1))
				Expect(run.Results[0].Err).Should(BeNil())
			}
		})
	})

	When("a cluster failed authentication", func() {
		It("should skip it and keep the others running", func() {
			entries := []clusterfile.Entry{
				{Name: "c1", Server: "https://api.c1.example"},
				{Name: "c2", Server: "https://api.c2.example"},
				{Name: "c3", Server: "https://api.c3.example"},
			}
			authFailures := map[string]error{"c2": errors.New("authentication failed: no readable kubeconfig at /home/u/.kube/config-c2")}
			request := config.Request{Provider: config.ProviderGitHub}
			runner := &artifactWritingRunner{}
			console := &Console{Out: &bytes.Buffer{}}
			aggregate := &aggregateRun{Directory: GinkgoT().TempDir()}

			runs := scheduleClusters(context.Background(), request, entries, authFailures, aggregate, runner, console, nil)

			Expect(runs[1].AuthErr).ShouldNot(BeNil())
			Expect(runs[1].Results).Should(BeEmpty())
			Expect(runs[0].Results).Should(HaveLen(1))
			Expect(runs[0].Results[0].Err).Should(BeNil())
			Expect(runs[2].Results).Should(HaveLen(1))
			Expect(runs[2].Results[0].Err).Should(BeNil())

			output := console.Out.(*bytes.Buffer).String()
			Expect(output).Should(ContainSubstring("cluster c2: authentication failed"))
			Expect(output).ShouldNot(ContainSubstring("cluster c2: started"))
		})
	})

	When("a cluster is authenticated", func() {
		It("should print serialized status lines per cluster", func() {
			entries := []clusterfile.Entry{{Name: "c1", Server: "https://api.c1.example"}}
			request := config.Request{Provider: config.ProviderGitHub}
			runner := &artifactWritingRunner{}
			console := &Console{Out: &bytes.Buffer{}}
			aggregate := &aggregateRun{Directory: GinkgoT().TempDir()}

			runs := scheduleClusters(context.Background(), request, entries, nil, aggregate, runner, console, nil)
			Expect(runs).Should(HaveLen(1))

			output := console.Out.(*bytes.Buffer).String()
			Expect(output).Should(ContainSubstring("cluster c1: authenticated"))
			Expect(output).Should(ContainSubstring("cluster c1: started"))
			Expect(output).Should(ContainSubstring("cluster c1: finished (1 passed, 0 failed)"))
		})
	})
})

var _ = Describe("resultSummary", func() {
	DescribeTable("summarizing provider results",
		func(results []ProviderResult, expected string) {
			Expect(resultSummary(results)).Should(Equal(expected))
		},
		Entry("no results", nil, "0 passed, 0 failed"),
		Entry("one passed and one failed", []ProviderResult{{Err: nil}, {Err: errors.New("boom")}}, "1 passed, 1 failed"),
	)
})

var _ = Describe("reportRows", func() {
	When("a cluster failed authentication", func() {
		It("should emit one authentication row per provider with aggregate links only", func() {
			aggregateDirectory := GinkgoT().TempDir()
			Expect(os.WriteFile(filepath.Join(aggregateDirectory, "run.log"), []byte("aggregate\n"), 0o640)).Should(Succeed())
			runs := []clusterRun{{Entry: clusterfile.Entry{Name: "c1"}, AuthErr: errors.New("authentication failed: no readable kubeconfig at X")}}

			rows := reportRows(aggregateDirectory, runs, []string{"github", "gitlab"})
			Expect(rows).Should(HaveLen(2))
			for _, row := range rows {
				Expect(row.Cluster).Should(Equal("c1"))
				Expect(row.Passed).Should(BeFalse())
				Expect(row.Stage).Should(Equal(report.StageAuthentication))
				Expect(row.Error).Should(Equal("authentication failed: no readable kubeconfig at X"))
				Expect(row.Links).Should(Equal([]report.Link{{Label: "aggregate run.log", Target: "./run.log"}}))
			}
		})
	})

	When("provider results exist", func() {
		It("should map stage, error, and artifact links", func() {
			aggregateDirectory := GinkgoT().TempDir()
			Expect(os.WriteFile(filepath.Join(aggregateDirectory, "run.log"), []byte("aggregate\n"), 0o640)).Should(Succeed())
			clusterDirectory := filepath.Join(aggregateDirectory, "c1")
			artifacts := filepath.Join(clusterDirectory, "artifacts-github")
			Expect(os.MkdirAll(artifacts, 0o750)).Should(Succeed())
			Expect(os.WriteFile(filepath.Join(clusterDirectory, "run.log"), []byte("cluster\n"), 0o640)).Should(Succeed())
			Expect(os.WriteFile(filepath.Join(artifacts, "run.log"), []byte("provider\n"), 0o640)).Should(Succeed())
			Expect(os.WriteFile(filepath.Join(artifacts, "manifest.json"), []byte("{}"), 0o640)).Should(Succeed())
			runs := []clusterRun{{Entry: clusterfile.Entry{Name: "c1"}, Results: []ProviderResult{{Provider: "github", Stage: model.PhaseBuildsVerified, ArtifactDirectory: "artifacts-github", Err: errors.New("build failed")}}}}

			rows := reportRows(aggregateDirectory, runs, []string{"github"})
			Expect(rows).Should(HaveLen(1))
			row := rows[0]
			Expect(row.Passed).Should(BeFalse())
			Expect(row.Stage).Should(Equal("builds-verified"))
			Expect(row.Error).Should(Equal("build failed"))
			Expect(row.Links).Should(ContainElement(report.Link{Label: "run directory", Target: "./c1/artifacts-github/"}))
			Expect(row.Links).Should(ContainElement(report.Link{Label: "manifest.json", Target: "./c1/artifacts-github/manifest.json"}))
			for _, link := range row.Links {
				Expect(link.Label).ShouldNot(Equal("collection-report.json"))
			}
		})
	})

	When("two clusters are listed", func() {
		It("should keep clusters-file order across clusters", func() {
			aggregateDirectory := GinkgoT().TempDir()
			Expect(os.WriteFile(filepath.Join(aggregateDirectory, "run.log"), []byte("aggregate\n"), 0o640)).Should(Succeed())
			runs := []clusterRun{
				{Entry: clusterfile.Entry{Name: "c1"}, Results: []ProviderResult{{Provider: "github", Stage: model.PhaseCompleted}}},
				{Entry: clusterfile.Entry{Name: "c2"}, Results: []ProviderResult{{Provider: "github", Stage: model.PhaseCompleted}}},
			}

			rows := reportRows(aggregateDirectory, runs, []string{"github"})
			Expect(rows).Should(HaveLen(2))
			Expect(rows[0].Cluster).Should(Equal("c1"))
			Expect(rows[0].Provider).Should(Equal("github"))
			Expect(rows[1].Cluster).Should(Equal("c2"))
			Expect(rows[1].Provider).Should(Equal("github"))
		})
	})
})

var _ = Describe("allPassed", func() {
	DescribeTable("reporting the aggregate outcome",
		func(rows []report.Row, expected bool) {
			Expect(allPassed(rows)).Should(Equal(expected))
		},
		Entry("all passed", []report.Row{{Passed: true}, {Passed: true}}, true),
		Entry("one failed", []report.Row{{Passed: true}, {Passed: false}}, false),
		Entry("empty", nil, true),
	)
})

var _ = Describe("errorText", func() {
	DescribeTable("rendering errors",
		func(err error, expected string) {
			Expect(errorText(err)).Should(Equal(expected))
		},
		Entry("nil error", nil, ""),
		Entry("error", errors.New("boom"), "boom"),
	)
})

var _ = Describe("aggregateError", func() {
	When("clusters and providers failed", func() {
		It("should join failures in order", func() {
			runs := []clusterRun{
				{Entry: clusterfile.Entry{Name: "c1"}, Results: []ProviderResult{{Provider: "gitlab", Err: errors.New("build failed")}}},
				{Entry: clusterfile.Entry{Name: "c2"}, AuthErr: errors.New("authentication failed")},
			}

			err := aggregateError(runs)
			Expect(err).Should(MatchError(ContainSubstring("cluster c2: authentication failed")))
			Expect(err).Should(MatchError(ContainSubstring("cluster c1 gitlab run: build failed")))
			message := err.Error()
			Expect(strings.Index(message, "cluster c1")).Should(BeNumerically("<", strings.Index(message, "cluster c2")))
		})
	})
})

var _ = Describe("finishAggregateRun", func() {
	When("every row passed", func() {
		It("should write both reports, print the summary, and return nil", func() {
			stateDir := GinkgoT().TempDir()
			aggregate, err := newAggregateRun(stateDir)
			Expect(err).ShouldNot(HaveOccurred())
			request := config.Request{Provider: config.ProviderGitHub}
			runs := []clusterRun{{Entry: clusterfile.Entry{Name: "c1"}, Results: []ProviderResult{{Provider: "github", Stage: model.PhaseCompleted}}}}
			console := &Console{Out: &bytes.Buffer{}}

			Expect(finishAggregateRun(aggregate, request, runs, console, nil)).Should(Succeed())

			for _, name := range []string{"report.md", "report.html"} {
				info, statErr := os.Stat(filepath.Join(aggregate.Directory, name))
				Expect(statErr).ShouldNot(HaveOccurred())
				Expect(info.Mode().Perm()).Should(Equal(os.FileMode(0o640)))
			}
			output := console.Out.(*bytes.Buffer).String()
			Expect(output).Should(ContainSubstring("c1 github: passed (completed)"))
			Expect(output).Should(ContainSubstring("report: " + filepath.Join(aggregate.Directory, "report.md")))
			Expect(output).Should(ContainSubstring("report: " + filepath.Join(aggregate.Directory, "report.html")))
		})
	})

	When("a row failed", func() {
		It("should return the joined failure", func() {
			stateDir := GinkgoT().TempDir()
			aggregate, err := newAggregateRun(stateDir)
			Expect(err).ShouldNot(HaveOccurred())
			request := config.Request{Provider: config.ProviderGitHub}
			runs := []clusterRun{{Entry: clusterfile.Entry{Name: "c1"}, Results: []ProviderResult{{Provider: "github", Stage: model.PhaseCompleted, Err: errors.New("build failed")}}}}
			console := &Console{Out: io.Discard}

			err = finishAggregateRun(aggregate, request, runs, console, nil)
			Expect(err).Should(MatchError(ContainSubstring("cluster c1 github run: build failed")))
			_, statErr := os.Stat(filepath.Join(aggregate.Directory, "report.md"))
			Expect(statErr).ShouldNot(HaveOccurred())
		})
	})
})

var _ = Describe("RunClusters", func() {
	When("two clusters and both providers are configured", func() {
		It("should produce an aggregate run with both reports", func() {
			dir := GinkgoT().TempDir()
			home := GinkgoT().TempDir()
			stateDir := GinkgoT().TempDir()
			GinkgoT().Setenv("HOME", home)
			stubOC()
			entries := []clusterfile.Entry{
				{Name: "c1", Kubeconfig: writeKubeconfig(dir, "c1"), Server: "https://api.c1.example"},
				{Name: "c2", Kubeconfig: writeKubeconfig(dir, "c2"), Server: "https://api.c2.example"},
			}
			clustersPath := writeClustersFile(dir, entries...)
			request := config.Request{Command: config.CommandRun, Provider: config.ProviderBoth, StateDir: stateDir, ClustersFile: clustersPath, Timeouts: config.Defaults().Timeouts}
			runner := &artifactWritingRunner{}

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})).Should(Succeed())

			aggregates := []string{}
			entriesList, err := os.ReadDir(stateDir)
			Expect(err).ShouldNot(HaveOccurred())
			for _, entry := range entriesList {
				if entry.Name() == "latest" || strings.HasPrefix(entry.Name(), ".") {
					continue
				}
				aggregates = append(aggregates, entry.Name())
			}
			Expect(aggregates).Should(HaveLen(1))
			Expect(aggregates[0]).Should(MatchRegexp(`^multi-run-`))
			aggregateDirectory := filepath.Join(stateDir, aggregates[0])

			for _, name := range []string{"report.md", "report.html", "run.log", "c1", "c2"} {
				_, statErr := os.Stat(filepath.Join(aggregateDirectory, name))
				Expect(statErr).ShouldNot(HaveOccurred())
			}
			Expect(os.Readlink(filepath.Join(stateDir, "latest"))).Should(Equal(aggregates[0]))
			Expect(readFile(filepath.Join(aggregateDirectory, "c1", "run.log"))).Should(ContainSubstring("cluster c1: running both"))
			Expect(readFile(filepath.Join(aggregateDirectory, "c2", "run.log"))).Should(ContainSubstring("cluster c2: running both"))

			markdown := readFile(filepath.Join(aggregateDirectory, "report.md"))
			positions := make([]int, 0, 4)
			for _, row := range []string{"| c1 | github |", "| c1 | gitlab |", "| c2 | github |", "| c2 | gitlab |"} {
				index := strings.Index(markdown, row)
				Expect(index).Should(BeNumerically(">=", 0), row)
				positions = append(positions, index)
			}
			for index := 1; index < len(positions); index++ {
				Expect(positions[index]).Should(BeNumerically(">", positions[index-1]))
			}
			Expect(markdown).Should(ContainSubstring("- Result: **passed**"))
		})
	})

	When("one cluster's provider fails", func() {
		It("should isolate the failure and exit non-zero", func() {
			dir := GinkgoT().TempDir()
			home := GinkgoT().TempDir()
			stateDir := GinkgoT().TempDir()
			GinkgoT().Setenv("HOME", home)
			stubOC()
			entries := []clusterfile.Entry{
				{Name: "c1", Kubeconfig: writeKubeconfig(dir, "c1"), Server: "https://api.c1.example"},
				{Name: "c2", Kubeconfig: writeKubeconfig(dir, "c2"), Server: "https://api.c2.example"},
			}
			clustersPath := writeClustersFile(dir, entries...)
			request := config.Request{Command: config.CommandRun, Provider: config.ProviderBoth, StateDir: stateDir, ClustersFile: clustersPath, Timeouts: config.Defaults().Timeouts}
			runner := &artifactWritingRunner{fail: map[string]error{"c2/gitlab": errors.New("gitlab failed")}}

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})
			Expect(err).Should(MatchError(ContainSubstring("cluster c2 gitlab run: gitlab failed")))

			aggregateDirectory := singleAggregateDirectory(stateDir)
			markdown := readFile(filepath.Join(aggregateDirectory, "report.md"))
			Expect(markdown).Should(ContainSubstring("| c1 | github | passed |"))
			Expect(markdown).Should(ContainSubstring("| c2 | gitlab | failed |"))
			Expect(markdown).Should(ContainSubstring("| c1 | gitlab | passed |"))
			Expect(markdown).Should(ContainSubstring("| c2 | github | passed |"))
			Expect(markdown).Should(ContainSubstring("- Result: **failed**"))
		})
	})

	When("one cluster has no kubeconfig and the ladder cannot help", func() {
		It("should report an authentication row and still write the reports", func() {
			dir := GinkgoT().TempDir()
			home := GinkgoT().TempDir()
			emptyPath := GinkgoT().TempDir()
			stateDir := GinkgoT().TempDir()
			GinkgoT().Setenv("HOME", home)
			GinkgoT().Setenv("PATH", emptyPath)
			clustersPath := writeClustersFile(dir, clusterfile.Entry{Name: "c-missing"})
			request := config.Request{Command: config.CommandRun, Provider: config.ProviderGitHub, StateDir: stateDir, ClustersFile: clustersPath, Timeouts: config.Defaults().Timeouts}
			runner := &artifactWritingRunner{}

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})
			Expect(err).Should(HaveOccurred())

			aggregateDirectory := singleAggregateDirectory(stateDir)
			markdown := readFile(filepath.Join(aggregateDirectory, "report.md"))
			Expect(markdown).Should(ContainSubstring("| c-missing | github | failed | authentication |"))
			_, statErr := os.Stat(filepath.Join(aggregateDirectory, "c-missing"))
			Expect(statErr).Should(HaveOccurred())
		})
	})

	When("the clusters file is invalid", func() {
		It("should create no aggregate run and no report", func() {
			dir := GinkgoT().TempDir()
			stateDir := GinkgoT().TempDir()
			clustersPath := filepath.Join(dir, "clusters.yaml")
			Expect(os.WriteFile(clustersPath, []byte("clusters: []\n"), 0o600)).Should(Succeed())
			request := config.Request{Command: config.CommandRun, Provider: config.ProviderGitHub, StateDir: stateDir, ClustersFile: clustersPath, Timeouts: config.Defaults().Timeouts}

			err := RunClusters(context.Background(), request, &artifactWritingRunner{}, &Console{Out: io.Discard})
			Expect(err).Should(MatchError(ContainSubstring("must list at least one cluster")))

			entries, readErr := os.ReadDir(stateDir)
			Expect(readErr).ShouldNot(HaveOccurred())
			Expect(entries).Should(BeEmpty())
		})
	})
})

var _ = Describe("rejectDuplicateServers", func() {
	When("two entries resolve to the same API server", func() {
		It("should fail before any worker starts and write no report", func() {
			dir := GinkgoT().TempDir()
			home := GinkgoT().TempDir()
			stateDir := GinkgoT().TempDir()
			GinkgoT().Setenv("HOME", home)
			stubOC()
			entries := []clusterfile.Entry{
				{Name: "c1", Kubeconfig: writeSharedServerKubeconfig(dir, "c1", "https://api.shared.example")},
				{Name: "c2", Kubeconfig: writeSharedServerKubeconfig(dir, "c2", "https://api.shared.example")},
			}
			clustersPath := writeClustersFile(dir, entries...)
			request := config.Request{Command: config.CommandRun, Provider: config.ProviderGitHub, StateDir: stateDir, ClustersFile: clustersPath, Timeouts: config.Defaults().Timeouts}
			runner := &artifactWritingRunner{}

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})
			Expect(err).Should(MatchError(ContainSubstring(`clusters "c1" and "c2" resolve to the same API server https://api.shared.example`)))
			Expect(runner.requests).Should(BeEmpty())
			entriesList, readErr := os.ReadDir(stateDir)
			Expect(readErr).ShouldNot(HaveOccurred())
			for _, entry := range entriesList {
				_, statErr := os.Stat(filepath.Join(stateDir, entry.Name(), "report.md"))
				Expect(statErr).Should(HaveOccurred())
			}
		})
	})

	When("entries resolve to distinct servers", func() {
		It("should not reject them", func() {
			dir := GinkgoT().TempDir()
			entries := []clusterfile.Entry{
				{Name: "c1", Kubeconfig: writeKubeconfig(dir, "c1")},
				{Name: "c2", Kubeconfig: writeKubeconfig(dir, "c2")},
			}

			Expect(rejectDuplicateServers(entries, nil)).Should(Succeed())
			Expect(rejectDuplicateServers(nil, nil)).Should(Succeed())
		})
	})
})

var _ = Describe("RunClusters under the aggregate layout", func() {
	When("two config-safe clusters are configured", func() {
		It("should produce both reports and the status lines", func() {
			dir := GinkgoT().TempDir()
			home := GinkgoT().TempDir()
			stateDir := GinkgoT().TempDir()
			GinkgoT().Setenv("HOME", home)
			stubOC()
			entries := []clusterfile.Entry{
				{Name: "c1", Kubeconfig: writeKubeconfig(dir, "c1"), Server: "https://api.c1.example"},
				{Name: "c2", Kubeconfig: writeKubeconfig(dir, "c2"), Server: "https://api.c2.example"},
			}
			clustersPath := writeClustersFile(dir, entries...)
			request := config.Request{Command: config.CommandRun, Provider: config.ProviderBoth, StateDir: stateDir, ClustersFile: clustersPath, Timeouts: config.Defaults().Timeouts}
			runner := &artifactWritingRunner{}
			output := &bytes.Buffer{}

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: output})).Should(Succeed())

			Expect(output.String()).Should(ContainSubstring("cluster c1: started"))
			Expect(output.String()).Should(ContainSubstring("cluster c1: finished (2 passed, 0 failed)"))
			Expect(output.String()).Should(ContainSubstring("cluster c2: started"))
			Expect(output.String()).Should(ContainSubstring("report: "))
			aggregateDirectory := singleAggregateDirectory(stateDir)
			for _, name := range []string{"report.md", "report.html"} {
				_, statErr := os.Stat(filepath.Join(aggregateDirectory, name))
				Expect(statErr).ShouldNot(HaveOccurred())
			}
		})
	})
})

var _ = Describe("RunClusters credential authentication", func() {
	When("no GitHub token is configured", func() {
		It("should authenticate once, reuse the token, and delete the credentials", func() {
			request, runner := runClustersFixture(2)
			tempRoot := credentialTempRoot()

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})).Should(Succeed())

			Expect(defaultGitHubAuth.calls).Should(Equal(1))
			Expect(defaultGitLabAuth.calls).Should(Equal(0))
			Expect(defaultGitLabToken.calls).Should(Equal(0))
			Expect(runner.requests).Should(HaveLen(2))
			for _, runRequest := range runner.requests {
				Expect(runRequest.Credentials.GitHubToken).Should(Equal("test-github-token"))
			}
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
		})
	})

	When("a configured GitHub token cannot list App installations", func() {
		It("should fall back to the gh login in an interactive session", func() {
			request, runner := runClustersFixture(1)
			request.Credentials.GitHubToken = "bad-token"
			tempRoot := credentialTempRoot()
			checkGitHubAccess = func(_ context.Context, token string) error {
				Expect(token).Should(Equal("bad-token"))
				return errors.New("404 Not Found")
			}

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: io.Discard, interactive: true})).Should(Succeed())

			Expect(defaultGitHubAuth.calls).Should(Equal(1))
			Expect(runner.requests).Should(HaveLen(1))
			Expect(runner.requests[0].Credentials.GitHubToken).Should(Equal("test-github-token"))
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
		})
	})

	When("a configured GitHub token can list App installations", func() {
		It("should use it without a login", func() {
			request, runner := runClustersFixture(1)
			request.Credentials.GitHubToken = "good-token"
			checkGitHubAccess = func(context.Context, string) error { return nil }

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: io.Discard, interactive: true})).Should(Succeed())

			Expect(defaultGitHubAuth.calls).Should(Equal(0))
			Expect(runner.requests).Should(HaveLen(1))
			Expect(runner.requests[0].Credentials.GitHubToken).Should(Equal("good-token"))
		})
	})

	When("a GitHub token is configured", func() {
		It("should not authenticate interactively", func() {
			request, runner := runClustersFixture(1)
			request.Credentials.GitHubToken = "configured-token"

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})).Should(Succeed())

			Expect(defaultGitHubAuth.calls).Should(Equal(0))
			Expect(runner.requests).Should(HaveLen(1))
			Expect(runner.requests[0].Credentials.GitHubToken).Should(Equal("configured-token"))
		})
	})

	When("the provider is GitLab without a token", func() {
		It("should prompt once, reuse the token, and delete the credentials", func() {
			request, runner := runClustersFixture(1)
			request.Provider = config.ProviderGitLab
			request.Credentials.GitLabAPIURL = "https://gitlab.example/api/v4"
			tempRoot := credentialTempRoot()

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})).Should(Succeed())

			Expect(defaultGitLabAuth.calls).Should(Equal(1))
			Expect(defaultGitLabAuth.apiURL).Should(Equal("https://gitlab.example/api/v4"))
			Expect(defaultGitHubAuth.calls).Should(Equal(0))
			Expect(defaultGitLabToken.calls).Should(Equal(1))
			Expect(runner.requests).Should(HaveLen(1))
			Expect(runner.requests[0].Credentials.GitLabToken).Should(Equal("test-gitlab-token"))
			Expect(runner.requests[0].Credentials.GitLabTokenEphemeral).Should(BeTrue())
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
		})
	})

	When("a GitLab token is configured", func() {
		It("should not prompt for a token", func() {
			request, runner := runClustersFixture(1)
			request.Provider = config.ProviderGitLab
			request.Credentials.GitLabToken = "configured-gitlab-token"

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})).Should(Succeed())

			Expect(defaultGitLabAuth.calls).Should(Equal(0))
			Expect(defaultGitLabToken.calls).Should(Equal(0))
			Expect(runner.requests[0].Credentials.GitLabToken).Should(Equal("configured-gitlab-token"))
			Expect(runner.requests[0].Credentials.GitLabTokenEphemeral).Should(BeFalse())
		})
	})

	When("both providers are selected without tokens", func() {
		It("should authenticate both once and reuse the tokens", func() {
			request, runner := runClustersFixture(1)
			request.Provider = config.ProviderBoth
			tempRoot := credentialTempRoot()

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})).Should(Succeed())

			Expect(defaultGitHubAuth.calls).Should(Equal(1))
			Expect(defaultGitLabAuth.calls).Should(Equal(1))
			Expect(defaultGitLabToken.calls).Should(Equal(1))
			Expect(runner.requests).Should(HaveLen(2))
			for _, runRequest := range runner.requests {
				Expect(runRequest.Credentials.GitHubToken).Should(Equal("test-github-token"))
				Expect(runRequest.Credentials.GitLabToken).Should(Equal("test-gitlab-token"))
			}
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
		})
	})

	When("GitHub authentication fails", func() {
		It("should stop before running any cluster", func() {
			request, runner := runClustersFixture(1)
			tempRoot := credentialTempRoot()
			defaultGitHubAuth.err = errors.New("browser login denied")

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})
			Expect(err).Should(MatchError(ContainSubstring("browser login denied")))
			Expect(runner.requests).Should(BeEmpty())
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
			Expect(os.ReadDir(request.StateDir)).Should(BeEmpty())
		})
	})

	When("GitLab authentication fails", func() {
		It("should stop before running any cluster", func() {
			request, runner := runClustersFixture(1)
			request.Provider = config.ProviderGitLab
			tempRoot := credentialTempRoot()
			defaultGitLabAuth.err = errors.New("token entry cancelled")

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})
			Expect(err).Should(MatchError(ContainSubstring("token entry cancelled")))
			Expect(runner.requests).Should(BeEmpty())
			Expect(defaultGitLabToken.calls).Should(Equal(0))
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
			Expect(os.ReadDir(request.StateDir)).Should(BeEmpty())
		})
	})

	When("the GitLab token refresh fails", func() {
		It("should fail the cluster at authentication and still report", func() {
			request, runner := runClustersFixture(2)
			request.Provider = config.ProviderGitLab
			tempRoot := credentialTempRoot()
			defaultGitLabToken.err = errors.New("refresh denied")

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})
			Expect(err).Should(MatchError(ContainSubstring("refresh denied")))
			Expect(defaultGitLabToken.calls).Should(Equal(2))
			Expect(runner.requests).Should(BeEmpty())
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
			aggregateDirectory := singleAggregateDirectory(request.StateDir)
			markdown := readFile(filepath.Join(aggregateDirectory, "report.md"))
			Expect(markdown).Should(ContainSubstring("| c1 | gitlab | failed | authentication | refresh denied"))
			Expect(markdown).Should(ContainSubstring("| c2 | gitlab | failed | authentication | refresh denied"))
		})
	})

	When("deleting the credentials fails", func() {
		It("should fail the run and the report even when every cluster passes", func() {
			request, runner := runClustersFixture(1)
			original := destroyCredentials
			destroyCredentials = func(store *auth.Store) error {
				Expect(store.Destroy()).Should(Succeed())
				return errors.New("temporary credentials are still present")
			}
			DeferCleanup(func() { destroyCredentials = original })

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})
			Expect(err).Should(MatchError(ContainSubstring("temporary credentials are still present")))
			Expect(runner.requests).Should(HaveLen(1))
			aggregateDirectory := singleAggregateDirectory(request.StateDir)
			markdown := readFile(filepath.Join(aggregateDirectory, "report.md"))
			Expect(markdown).Should(ContainSubstring("| credentials | auth | failed | cleanup | temporary credentials are still present |"))
			Expect(markdown).Should(ContainSubstring("- Result: **failed**"))
		})
	})
})

// credentialTempRoot redirects temporary credential files into an observable directory.
func credentialTempRoot() string {
	tempRoot := GinkgoT().TempDir()
	GinkgoT().Setenv("TMPDIR", tempRoot)
	return tempRoot
}

// runClustersFixture builds a run over clusterCount stubbed clusters.
func runClustersFixture(clusterCount int) (config.Request, *artifactWritingRunner) {
	dir := GinkgoT().TempDir()
	GinkgoT().Setenv("HOME", GinkgoT().TempDir())
	stubOC()
	entries := make([]clusterfile.Entry, 0, clusterCount)
	for index := 1; index <= clusterCount; index++ {
		name := fmt.Sprintf("c%d", index)
		entries = append(entries, clusterfile.Entry{Name: name, Kubeconfig: writeKubeconfig(dir, name), Server: "https://api." + name + ".example"})
	}
	request := config.Request{
		Command:      config.CommandRun,
		Provider:     config.ProviderGitHub,
		StateDir:     GinkgoT().TempDir(),
		ClustersFile: writeClustersFile(dir, entries...),
		Timeouts:     config.Defaults().Timeouts,
	}
	return request, &artifactWritingRunner{}
}

// singleAggregateDirectory returns the one non-latest directory under stateDir.
func singleAggregateDirectory(stateDir string) string {
	entries, err := os.ReadDir(stateDir)
	Expect(err).ShouldNot(HaveOccurred())
	names := []string{}
	for _, entry := range entries {
		if entry.Name() == "latest" || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		names = append(names, entry.Name())
	}
	Expect(names).Should(HaveLen(1))
	return filepath.Join(stateDir, names[0])
}
