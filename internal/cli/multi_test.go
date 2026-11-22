package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/redhat-appstudio/lkmabt/internal/evidence"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/platform"
	"github.com/redhat-appstudio/lkmabt/internal/providers"
	"github.com/redhat-appstudio/lkmabt/internal/report"
	"github.com/redhat-appstudio/lkmabt/internal/workflow"
)

// testAggregateLog is the run-named aggregate log used by report-row fixtures.
const testAggregateLog = "multi-run-10:46_8.10.2026_run-a-run.log"

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

// deferringRunner records requests and returns an admission deferral while the
// configured call budget for a cluster/provider remains, then succeeds.
type deferringRunner struct {
	mu         sync.Mutex
	requests   []config.Request
	remaining  map[string]int // key "<cluster>/<provider>" -> remaining deferrals
	cleanupErr map[string]error
	reason     string
}

// Run records the request and defers while the configured budget is positive.
func (r *deferringRunner) Run(_ context.Context, req config.Request, _ RunLog) (RunOutcome, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	key := req.ClusterName + "/" + req.Provider
	deferred := r.remaining[key] > 0
	if deferred {
		r.remaining[key]--
	}
	cleanupErr := r.cleanupErr[key]
	r.mu.Unlock()

	reason := r.reason
	if reason == "" {
		reason = "admission pending"
	}
	outcome := RunOutcome{Provider: req.Provider, RunID: req.RunID, ArtifactDirectory: "artifacts-" + req.Provider, Stage: model.PhaseTriggered}
	if deferred {
		outcome.Deferred = true
		outcome.DeferredReason = reason
		outcome.CleanupErr = cleanupErr
		return outcome, &workflow.AdmissionPendingError{Namespace: "tenant", Pending: []workflow.AdmissionPending{{Component: "component", PipelineRun: "pipeline-run", Reason: reason}}}
	}
	return outcome, nil
}

// requestKeys returns the recorded cluster/provider keys in call order.
func (r *deferringRunner) requestKeys() []string {
	keys := make([]string, len(r.requests))
	for index, request := range r.requests {
		keys[index] = request.ClusterName + "/" + request.Provider
	}
	return keys
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

type gitlabPromptStub struct {
	token string
	err   error
	calls int
}

// read records the call and returns the configured token or error.
func (s *gitlabPromptStub) read() (string, error) {
	s.calls++
	return s.token, s.err
}

type tokenPageStub struct {
	url   string
	err   error
	calls int
}

// open records the URL and returns the configured error.
func (s *tokenPageStub) open(rawURL string) error {
	s.calls++
	s.url = rawURL
	return s.err
}

type gitlabRevokeStub struct {
	err   error
	calls int
}

// revoke records the call and returns the configured error.
func (s *gitlabRevokeStub) revoke(context.Context, string, string) error {
	s.calls++
	return s.err
}

var (
	defaultGitHubAuth   *githubAuthStub
	defaultGitLabPrompt *gitlabPromptStub
	defaultGitLabRevoke *gitlabRevokeStub
	defaultTokenPage    *tokenPageStub
)

var _ = BeforeEach(func() {
	defaultGitHubAuth = &githubAuthStub{token: "test-github-token"}
	defaultGitLabPrompt = &gitlabPromptStub{token: "glpat-test-token"}
	defaultGitLabRevoke = &gitlabRevokeStub{}
	defaultTokenPage = &tokenPageStub{}
	originalGitHub, originalPrompt, originalRevoke, originalPage, originalDestroy, originalCheck := authenticateGitHub, readGitLabToken, revokeGitLabToken, openGitLabTokenPage, destroyCredentials, checkGitHubAccess
	originalRead := readExistingGitHubToken
	authenticateGitHub = defaultGitHubAuth.authenticate
	readGitLabToken = defaultGitLabPrompt.read
	revokeGitLabToken = defaultGitLabRevoke.revoke
	openGitLabTokenPage = defaultTokenPage.open
	readExistingGitHubToken = func(context.Context, clusterauth.CommandRunner) (string, error) {
		return "", errors.New("no existing gh login")
	}
	checkGitHubAccess = func(context.Context, string) error { return nil }
	DeferCleanup(func() {
		authenticateGitHub = originalGitHub
		readGitLabToken = originalPrompt
		revokeGitLabToken = originalRevoke
		openGitLabTokenPage = originalPage
		readExistingGitHubToken = originalRead
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
				runs = scheduleClusters(context.Background(), request, entries, nil, aggregate, runner, console)
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

			runs := scheduleClusters(context.Background(), request, entries, authFailures, aggregate, runner, console)

			Expect(runs[1].AuthErr).ShouldNot(BeNil())
			Expect(runs[1].Results).Should(BeEmpty())
			Expect(runs[0].Results).Should(HaveLen(1))
			Expect(runs[0].Results[0].Err).Should(BeNil())
			Expect(runs[2].Results).Should(HaveLen(1))
			Expect(runs[2].Results[0].Err).Should(BeNil())

			output := console.Out.(*bytes.Buffer).String()
			Expect(output).Should(ContainSubstring("✗ Authenticate cluster c2 🔐"))
			Expect(output).Should(ContainSubstring("authentication failed: no readable kubeconfig"))
			Expect(output).ShouldNot(ContainSubstring("\ncluster c2\n"))
		})
	})

	When("a cluster is authenticated", func() {
		It("should print serialized status lines per cluster", func() {
			entries := []clusterfile.Entry{{Name: "c1", Server: "https://api.c1.example"}}
			request := config.Request{Provider: config.ProviderGitHub}
			runner := &artifactWritingRunner{}
			console := &Console{Out: &bytes.Buffer{}}
			aggregate := &aggregateRun{Directory: GinkgoT().TempDir()}

			runs := scheduleClusters(context.Background(), request, entries, nil, aggregate, runner, console)
			Expect(runs).Should(HaveLen(1))

			output := console.Out.(*bytes.Buffer).String()
			Expect(output).Should(ContainSubstring("\ncluster c1\n"))
			Expect(output).Should(ContainSubstring("cluster c1: finished (1 passed, 0 failed)"))
		})
	})
	When("a cluster defers on its first attempt", func() {
		It("should retry it after the first pass with a fresh run ID", func() {
			entries := []clusterfile.Entry{
				{Name: "c1", Server: "https://api.c1.example"},
				{Name: "c2", Server: "https://api.c2.example"},
			}
			request := config.Request{Provider: config.ProviderGitHub}
			runner := &deferringRunner{remaining: map[string]int{"c1/github": 1}}
			console := &Console{Out: &bytes.Buffer{}}
			aggregate := &aggregateRun{Directory: GinkgoT().TempDir()}

			runs := scheduleClusters(context.Background(), request, entries, nil, aggregate, runner, console)

			Expect(runner.requestKeys()).Should(Equal([]string{"c1/github", "c2/github", "c1/github"}))
			Expect(runner.requests[0].RunID).ShouldNot(Equal(runner.requests[2].RunID))

			Expect(runs[0].FirstAttempt).Should(HaveLen(1))
			Expect(runs[0].FirstAttempt[0].Deferred).Should(BeTrue())
			Expect(runs[0].FirstAttempt[0].RunID).Should(Equal(runner.requests[0].RunID))
			Expect(runs[0].Results).Should(HaveLen(1))
			Expect(runs[0].Results[0].Deferred).Should(BeFalse())
			Expect(runs[0].Results[0].Err).Should(BeNil())
			Expect(runs[0].Results[0].RunID).Should(Equal(runner.requests[2].RunID))
			Expect(runs[1].FirstAttempt).Should(BeEmpty())
			Expect(runs[1].Results[0].Err).Should(BeNil())

			output := console.Out.(*bytes.Buffer).String()
			Expect(output).Should(ContainSubstring("cluster c1: deferred (admission pending); continuing with the next cluster"))
			Expect(output).Should(ContainSubstring("cluster c1: will retry after the first pass"))
			Expect(output).Should(ContainSubstring("cluster c1: retrying deferred attempt (fresh run)"))
			Expect(output).Should(ContainSubstring("cluster c1: retry finished (1 passed, 0 failed)"))
		})
	})

	When("a deferred cleanup failed", func() {
		It("should fail the cluster even when the retry succeeds", func() {
			entries := []clusterfile.Entry{{Name: "c1", Server: "https://api.c1.example"}}
			request := config.Request{Provider: config.ProviderGitHub}
			runner := &deferringRunner{
				remaining:  map[string]int{"c1/github": 1},
				cleanupErr: map[string]error{"c1/github": errors.New("delete failed")},
			}
			console := &Console{Out: io.Discard}
			aggregate := &aggregateRun{Directory: GinkgoT().TempDir()}

			runs := scheduleClusters(context.Background(), request, entries, nil, aggregate, runner, console)

			Expect(runs).Should(HaveLen(1))
			Expect(runs[0].Results).Should(HaveLen(1))
			Expect(runs[0].Results[0].Err).Should(MatchError(ContainSubstring("first attempt deferred cleanup failed: delete failed")))
			Expect(aggregateError(runs)).Should(MatchError(ContainSubstring("delete failed")))
			rows := reportRows(aggregate.Directory, aggregate.runLogName(), runs, []string{"github"})
			Expect(rows).Should(HaveLen(1))
			Expect(rows[0].Passed).Should(BeFalse())
		})
	})

	When("a retry defers again", func() {
		It("should become a terminal error with no further retry", func() {
			entries := []clusterfile.Entry{{Name: "c1", Server: "https://api.c1.example"}}
			request := config.Request{Provider: config.ProviderGitHub}
			runner := &deferringRunner{remaining: map[string]int{"c1/github": 2}}
			console := &Console{Out: io.Discard}
			aggregate := &aggregateRun{Directory: GinkgoT().TempDir()}

			runs := scheduleClusters(context.Background(), request, entries, nil, aggregate, runner, console)

			Expect(runner.requests).Should(HaveLen(2))
			Expect(runs[0].Results).Should(HaveLen(1))
			Expect(runs[0].Results[0].Deferred).Should(BeFalse())
			Expect(runs[0].Results[0].Err).Should(MatchError(ContainSubstring("admission-pending")))

			rows := reportRows(aggregate.Directory, aggregate.runLogName(), runs, []string{"github"})
			Expect(rows).Should(HaveLen(1))
			Expect(rows[0].Passed).Should(BeFalse())
			Expect(rows[0].Note).Should(ContainSubstring("first attempt deferred"))
		})
	})

	When("one provider defers while its sibling passes", func() {
		It("should retry both and keep one row per provider", func() {
			entries := []clusterfile.Entry{{Name: "c1", Server: "https://api.c1.example"}}
			request := config.Request{Provider: config.ProviderBoth}
			runner := &deferringRunner{remaining: map[string]int{"c1/github": 1}}
			console := &Console{Out: io.Discard}
			aggregate := &aggregateRun{Directory: GinkgoT().TempDir()}

			runs := scheduleClusters(context.Background(), request, entries, nil, aggregate, runner, console)

			Expect(runner.requests).Should(HaveLen(4))
			Expect(runs[0].FirstAttempt).Should(HaveLen(2))
			Expect(runs[0].Results).Should(HaveLen(2))
			runIDs := map[string][]string{}
			for _, request := range runner.requests {
				runIDs[request.Provider] = append(runIDs[request.Provider], request.RunID)
			}
			for _, provider := range []string{"github", "gitlab"} {
				Expect(runIDs[provider]).Should(HaveLen(2))
				Expect(runIDs[provider][0]).ShouldNot(Equal(runIDs[provider][1]))
			}

			rows := reportRows(aggregate.Directory, aggregate.runLogName(), runs, []string{"github", "gitlab"})
			Expect(rows).Should(HaveLen(2))
			byProvider := map[string]report.Row{}
			for _, row := range rows {
				byProvider[row.Provider] = row
			}
			Expect(byProvider).Should(HaveLen(2))
			Expect(byProvider["github"].Passed).Should(BeTrue())
			Expect(byProvider["github"].Note).Should(ContainSubstring("first attempt deferred: admission pending"))
			Expect(byProvider["gitlab"].Passed).Should(BeTrue())
			Expect(byProvider["gitlab"].Note).Should(BeEmpty())
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
		Entry("a deferred provider", []ProviderResult{{Err: nil}, {Deferred: true, DeferredReason: "admission pending"}}, "1 passed, 1 deferred"),
		Entry("a failed and a deferred provider", []ProviderResult{{Err: errors.New("boom")}, {Deferred: true, DeferredReason: "admission pending"}}, "0 passed, 1 failed, 1 deferred"),
	)
})

var _ = Describe("reportRows", func() {
	When("a cluster failed authentication", func() {
		It("should emit one authentication row per provider with aggregate links only", func() {
			aggregateDirectory := GinkgoT().TempDir()
			Expect(os.WriteFile(filepath.Join(aggregateDirectory, testAggregateLog), []byte("aggregate\n"), 0o640)).Should(Succeed())
			runs := []clusterRun{{Entry: clusterfile.Entry{Name: "c1"}, AuthErr: errors.New("authentication failed: no readable kubeconfig at X")}}

			rows := reportRows(aggregateDirectory, testAggregateLog, runs, []string{"github", "gitlab"})
			Expect(rows).Should(HaveLen(2))
			for _, row := range rows {
				Expect(row.Cluster).Should(Equal("c1"))
				Expect(row.Passed).Should(BeFalse())
				Expect(row.Stage).Should(Equal(report.StageAuthentication))
				Expect(row.Error).Should(Equal("authentication failed: no readable kubeconfig at X"))
				Expect(row.Links).Should(Equal([]report.Link{{Label: "aggregate run.log", Target: "./" + testAggregateLog}}))
			}
		})
	})

	When("provider results exist", func() {
		It("should map stage, error, and artifact links", func() {
			aggregateDirectory := GinkgoT().TempDir()
			Expect(os.WriteFile(filepath.Join(aggregateDirectory, testAggregateLog), []byte("aggregate\n"), 0o640)).Should(Succeed())
			clusterDirectory := filepath.Join(aggregateDirectory, "c1")
			artifacts := filepath.Join(clusterDirectory, "artifacts-github")
			Expect(os.MkdirAll(artifacts, 0o750)).Should(Succeed())
			Expect(os.WriteFile(filepath.Join(clusterDirectory, "run.log"), []byte("cluster\n"), 0o640)).Should(Succeed())
			Expect(os.WriteFile(filepath.Join(artifacts, "run.log"), []byte("provider\n"), 0o640)).Should(Succeed())
			Expect(os.WriteFile(filepath.Join(artifacts, "manifest.json"), []byte("{}"), 0o640)).Should(Succeed())
			runs := []clusterRun{{Entry: clusterfile.Entry{Name: "c1"}, Results: []ProviderResult{{Provider: "github", Stage: model.PhaseBuildsVerified, ArtifactDirectory: "artifacts-github", Err: errors.New("build failed")}}}}

			rows := reportRows(aggregateDirectory, testAggregateLog, runs, []string{"github"})
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

	When("a first attempt deferred", func() {
		It("should note the deferral and keep both attempts' links", func() {
			aggregateDirectory := GinkgoT().TempDir()
			Expect(os.WriteFile(filepath.Join(aggregateDirectory, testAggregateLog), []byte("aggregate\n"), 0o640)).Should(Succeed())
			firstDirectory := filepath.Join(aggregateDirectory, "c1", "artifacts-github-first")
			finalDirectory := filepath.Join(aggregateDirectory, "c1", "artifacts-github-final")
			for _, directory := range []string{firstDirectory, finalDirectory} {
				Expect(os.MkdirAll(directory, 0o750)).Should(Succeed())
				Expect(os.WriteFile(filepath.Join(directory, "run.log"), []byte("provider\n"), 0o640)).Should(Succeed())
			}
			runs := []clusterRun{{
				Entry:        clusterfile.Entry{Name: "c1"},
				FirstAttempt: []ProviderResult{{Provider: "github", Stage: model.PhaseTriggered, Deferred: true, DeferredReason: "admission pending", ArtifactDirectory: "artifacts-github-first"}},
				Results:      []ProviderResult{{Provider: "github", Stage: model.PhaseCompleted, ArtifactDirectory: "artifacts-github-final"}},
			}}

			rows := reportRows(aggregateDirectory, testAggregateLog, runs, []string{"github"})
			Expect(rows).Should(HaveLen(1))
			Expect(rows[0].Passed).Should(BeTrue())
			Expect(rows[0].Note).Should(Equal("first attempt deferred: admission pending"))
			Expect(rows[0].Links).Should(ContainElement(report.Link{Label: "run directory", Target: "./c1/artifacts-github-final/"}))
			Expect(rows[0].Links).Should(ContainElement(report.Link{Label: "first attempt run directory", Target: "./c1/artifacts-github-first/"}))
			Expect(rows[0].Links).Should(ContainElement(report.Link{Label: "first attempt run.log", Target: "./c1/artifacts-github-first/run.log"}))
		})
	})

	When("a cluster narrowed its architecture selection", func() {
		It("should report the tested subset", func() {
			aggregateDirectory := GinkgoT().TempDir()
			Expect(os.WriteFile(filepath.Join(aggregateDirectory, testAggregateLog), []byte("aggregate\n"), 0o640)).Should(Succeed())
			runs := []clusterRun{{
				Entry:              clusterfile.Entry{Name: "c1"},
				RequestedPlatforms: []string{"linux/amd64", "linux/s390x"},
				Results:            []ProviderResult{{Provider: "github", Stage: model.PhaseCompleted, Platforms: []string{"linux/amd64"}}},
			}}

			rows := reportRows(aggregateDirectory, testAggregateLog, runs, []string{"github"})
			Expect(rows).Should(HaveLen(1))
			Expect(rows[0].Note).Should(Equal("tested: linux/amd64"))
		})

		It("should not note a default full selection", func() {
			aggregateDirectory := GinkgoT().TempDir()
			Expect(os.WriteFile(filepath.Join(aggregateDirectory, testAggregateLog), []byte("aggregate\n"), 0o640)).Should(Succeed())
			runs := []clusterRun{{
				Entry:   clusterfile.Entry{Name: "c1"},
				Results: []ProviderResult{{Provider: "github", Stage: model.PhaseCompleted, Platforms: platform.All()}},
			}}

			rows := reportRows(aggregateDirectory, testAggregateLog, runs, []string{"github"})
			Expect(rows).Should(HaveLen(1))
			Expect(rows[0].Note).Should(BeEmpty())
		})
	})

	When("the first attempt recorded an error", func() {
		It("should aggregate only the final attempt", func() {
			runs := []clusterRun{{
				Entry:        clusterfile.Entry{Name: "c1"},
				FirstAttempt: []ProviderResult{{Provider: "github", Err: errors.New("first attempt failed")}},
				Results:      []ProviderResult{{Provider: "github", Stage: model.PhaseCompleted}},
			}}
			Expect(aggregateError(runs)).Should(BeNil())
		})

		It("should report a final attempt failure", func() {
			runs := []clusterRun{{
				Entry:        clusterfile.Entry{Name: "c1"},
				FirstAttempt: []ProviderResult{{Provider: "github", Stage: model.PhaseCompleted}},
				Results:      []ProviderResult{{Provider: "github", Stage: model.PhaseBuildsVerified, Err: errors.New("final attempt failed")}},
			}}
			err := aggregateError(runs)
			Expect(err).Should(MatchError(ContainSubstring("final attempt failed")))
			Expect(err).ShouldNot(MatchError(ContainSubstring("first attempt failed")))
		})
	})

	When("two clusters are listed", func() {
		It("should keep clusters-file order across clusters", func() {
			aggregateDirectory := GinkgoT().TempDir()
			Expect(os.WriteFile(filepath.Join(aggregateDirectory, testAggregateLog), []byte("aggregate\n"), 0o640)).Should(Succeed())
			runs := []clusterRun{
				{Entry: clusterfile.Entry{Name: "c1"}, Results: []ProviderResult{{Provider: "github", Stage: model.PhaseCompleted}}},
				{Entry: clusterfile.Entry{Name: "c2"}, Results: []ProviderResult{{Provider: "github", Stage: model.PhaseCompleted}}},
			}

			rows := reportRows(aggregateDirectory, testAggregateLog, runs, []string{"github"})
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
			request := config.Request{Provider: config.ProviderGitHub, Platforms: []string{"linux/arm64"}}
			runs := []clusterRun{{Entry: clusterfile.Entry{Name: "c1"}, Results: []ProviderResult{{Provider: "github", Stage: model.PhaseCompleted}}}}
			console := &Console{Out: &bytes.Buffer{}}

			Expect(finishAggregateRun(aggregate, request, runs, console, nil)).Should(Succeed())

			markdownPath, htmlPath := aggregate.reportPaths()
			for _, path := range []string{markdownPath, htmlPath} {
				info, statErr := os.Stat(path)
				Expect(statErr).ShouldNot(HaveOccurred())
				Expect(info.Mode().Perm()).Should(Equal(os.FileMode(0o640)))
			}
			output := console.Out.(*bytes.Buffer).String()
			Expect(output).Should(ContainSubstring("c1 github: passed (completed); requested architectures: linux/arm64"))
			Expect(output).Should(ContainSubstring("report: " + markdownPath))
			Expect(output).Should(ContainSubstring("report: " + htmlPath))
			markdown, readErr := os.ReadFile(markdownPath)
			Expect(readErr).ShouldNot(HaveOccurred())
			Expect(string(markdown)).Should(ContainSubstring("- Requested architectures: linux/arm64"))
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
			markdownPath, _ := aggregate.reportPaths()
			_, statErr := os.Stat(markdownPath)
			Expect(statErr).ShouldNot(HaveOccurred())
		})
	})
	When("a failed row also reports a finalization error", func() {
		It("should print the row without repeating the error", func() {
			stateDir := GinkgoT().TempDir()
			aggregate, err := newAggregateRun(stateDir)
			Expect(err).ShouldNot(HaveOccurred())
			request := config.Request{Provider: config.ProviderGitHub}
			runs := []clusterRun{{Entry: clusterfile.Entry{Name: "c1"}, Results: []ProviderResult{{Provider: "github", Stage: model.PhaseCompleted, Err: errors.New("build failed; finalization failed: delete denied")}}}}
			console := &Console{Out: &bytes.Buffer{}}

			err = finishAggregateRun(aggregate, request, runs, console, nil)
			Expect(err).Should(MatchError(ContainSubstring("build failed; finalization failed: delete denied")))
			output := console.Out.(*bytes.Buffer).String()
			Expect(output).Should(ContainSubstring("c1 github: failed (completed)"))
			Expect(output).ShouldNot(ContainSubstring("finalization failed"))
			markdownPath, _ := aggregate.reportPaths()
			markdown := readFile(markdownPath)
			Expect(markdown).Should(ContainSubstring("finalization failed: delete denied"))
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
			request := config.Request{Command: config.CommandRun, Provider: config.ProviderBoth, StateDir: stateDir, ClustersFile: clustersPath, Timeouts: config.Defaults().Timeouts, Credentials: config.Credentials{GitLabToken: "glpat-configured-token"}}
			runner := &artifactWritingRunner{}

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})).Should(Succeed())

			aggregateDirectory := singleAggregateDirectory(stateDir)
			runName := latestRunName(stateDir)

			for _, name := range []string{runName + "-report.md", runName + "-report.html", runName + "-run.log", "c1", "c2"} {
				_, statErr := os.Stat(filepath.Join(aggregateDirectory, name))
				Expect(statErr).ShouldNot(HaveOccurred())
			}
			Expect(readFile(filepath.Join(aggregateDirectory, "c1", "run.log"))).Should(ContainSubstring("cluster c1: running both"))
			Expect(readFile(filepath.Join(aggregateDirectory, "c2", "run.log"))).Should(ContainSubstring("cluster c2: running both"))

			markdown := readFile(filepath.Join(aggregateDirectory, runName+"-report.md"))
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
			request := config.Request{Command: config.CommandRun, Provider: config.ProviderBoth, StateDir: stateDir, ClustersFile: clustersPath, Timeouts: config.Defaults().Timeouts, Credentials: config.Credentials{GitLabToken: "glpat-configured-token"}}
			runner := &artifactWritingRunner{fail: map[string]error{"c2/gitlab": errors.New("gitlab failed")}}

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})
			Expect(err).Should(MatchError(ContainSubstring("cluster c2 gitlab run: gitlab failed")))

			markdown := readFile(latestReportPath(stateDir, "md"))
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
			markdown := readFile(latestReportPath(stateDir, "md"))
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
				reports, globErr := filepath.Glob(filepath.Join(stateDir, entry.Name(), "*-report.md"))
				Expect(globErr).ShouldNot(HaveOccurred())
				Expect(reports).Should(BeEmpty())
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
			request := config.Request{Command: config.CommandRun, Provider: config.ProviderBoth, StateDir: stateDir, ClustersFile: clustersPath, Timeouts: config.Defaults().Timeouts, Credentials: config.Credentials{GitLabToken: "glpat-configured-token"}}
			runner := &artifactWritingRunner{}
			output := &bytes.Buffer{}

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: output})).Should(Succeed())

			Expect(output.String()).Should(ContainSubstring("\ncluster c1\n"))
			Expect(output.String()).Should(ContainSubstring("cluster c1: finished (2 passed, 0 failed)"))
			Expect(output.String()).Should(ContainSubstring("\ncluster c2\n"))
			Expect(output.String()).Should(ContainSubstring("report: "))
			aggregateDirectory := singleAggregateDirectory(stateDir)
			runName := latestRunName(stateDir)
			for _, name := range []string{runName + "-report.md", runName + "-report.html"} {
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
				return fmt.Errorf("%w: 404 Not Found", providers.ErrInsufficientGitHubAccess)
			}

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: io.Discard, interactive: true})).Should(Succeed())

			Expect(defaultGitHubAuth.calls).Should(Equal(1))
			Expect(runner.requests).Should(HaveLen(1))
			Expect(runner.requests[0].Credentials.GitHubToken).Should(Equal("test-github-token"))
			Expect(os.ReadDir(tempRoot)).Should(BeEmpty())
		})
	})

	When("a configured GitHub token cannot list App installations but the existing gh login can", func() {
		It("should reuse the existing login without a browser flow", func() {
			request, runner := runClustersFixture(1)
			request.Credentials.GitHubToken = "bad-token"
			output := &bytes.Buffer{}
			readExistingGitHubToken = func(context.Context, clusterauth.CommandRunner) (string, error) {
				return "existing-token", nil
			}
			checkGitHubAccess = func(_ context.Context, token string) error {
				if token == "bad-token" {
					return fmt.Errorf("%w: 404 Not Found", providers.ErrInsufficientGitHubAccess)
				}
				Expect(token).Should(Equal("existing-token"))
				return nil
			}

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: output, interactive: true})).Should(Succeed())

			Expect(defaultGitHubAuth.calls).Should(Equal(0))
			Expect(runner.requests).Should(HaveLen(1))
			Expect(runner.requests[0].Credentials.GitHubToken).Should(Equal("existing-token"))
			Expect(output.String()).Should(ContainSubstring("github: using the existing gh login"))
		})
	})

	When("a configured GitHub token cannot list a selected installation's repositories but the existing gh login can", func() {
		It("should reuse the existing login without a browser flow", func() {
			request, runner := runClustersFixture(1)
			request.Credentials.GitHubToken = "bad-token"
			output := &bytes.Buffer{}
			readExistingGitHubToken = func(context.Context, clusterauth.CommandRunner) (string, error) {
				return "existing-token", nil
			}
			checkGitHubAccess = func(_ context.Context, token string) error {
				if token == "bad-token" {
					return fmt.Errorf("%w: list repositories for GitHub App installation 7: 403 (the GitHub token needs the read:user scope)", providers.ErrInsufficientGitHubAccess)
				}
				Expect(token).Should(Equal("existing-token"))
				return nil
			}

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: output, interactive: true})).Should(Succeed())

			Expect(defaultGitHubAuth.calls).Should(Equal(0))
			Expect(runner.requests).Should(HaveLen(1))
			Expect(runner.requests[0].Credentials.GitHubToken).Should(Equal("existing-token"))
			Expect(output.String()).Should(ContainSubstring("github: using the existing gh login"))
		})
	})

	When("a configured GitHub token and the existing gh login cannot list App installations", func() {
		It("should start the gh browser login", func() {
			request, runner := runClustersFixture(1)
			request.Credentials.GitHubToken = "bad-token"
			output := &bytes.Buffer{}
			readExistingGitHubToken = func(context.Context, clusterauth.CommandRunner) (string, error) {
				return "existing-token", nil
			}
			checkGitHubAccess = func(context.Context, string) error {
				return fmt.Errorf("%w: 403 Forbidden", providers.ErrInsufficientGitHubAccess)
			}

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: output, interactive: true})).Should(Succeed())

			Expect(defaultGitHubAuth.calls).Should(Equal(1))
			Expect(runner.requests).Should(HaveLen(1))
			Expect(runner.requests[0].Credentials.GitHubToken).Should(Equal("test-github-token"))
			Expect(output.String()).Should(ContainSubstring("github: existing gh login is not usable"))
		})
	})

	When("reading the existing gh login exceeds its deadline", func() {
		It("should abort without clearing the configured token", func() {
			request, runner := runClustersFixture(1)
			request.Credentials.GitHubToken = "bad-token"
			previousTimeout := githubAccessCheckTimeout
			githubAccessCheckTimeout = 10 * time.Millisecond
			defer func() { githubAccessCheckTimeout = previousTimeout }()
			checkGitHubAccess = func(_ context.Context, token string) error {
				if token == "bad-token" {
					return fmt.Errorf("%w: 403 Forbidden", providers.ErrInsufficientGitHubAccess)
				}
				return nil
			}
			readExistingGitHubToken = func(ctx context.Context, _ clusterauth.CommandRunner) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			}

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard, interactive: true})
			Expect(err).Should(MatchError(ContainSubstring("read the existing gh login")))
			Expect(errors.Is(err, context.DeadlineExceeded)).Should(BeTrue())
			Expect(defaultGitHubAuth.calls).Should(Equal(0))
			Expect(runner.requests).Should(BeEmpty())
		})
	})

	When("the existing gh login cannot be read", func() {
		It("should start the gh browser login", func() {
			request, runner := runClustersFixture(1)
			request.Credentials.GitHubToken = "bad-token"
			checkGitHubAccess = func(context.Context, string) error {
				return fmt.Errorf("%w: 403 Forbidden", providers.ErrInsufficientGitHubAccess)
			}
			readExistingGitHubToken = func(context.Context, clusterauth.CommandRunner) (string, error) {
				return "", errors.New("gh: not logged in")
			}

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: io.Discard, interactive: true})).Should(Succeed())
			Expect(defaultGitHubAuth.calls).Should(Equal(1))
			Expect(runner.requests).Should(HaveLen(1))
			Expect(runner.requests[0].Credentials.GitHubToken).Should(Equal("test-github-token"))
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

	When("the GitHub access check fails transiently", func() {
		It("should keep the configured token and skip the browser login", func() {
			request, runner := runClustersFixture(1)
			request.Credentials.GitHubToken = "good-token"
			readAttempted := false
			readExistingGitHubToken = func(context.Context, clusterauth.CommandRunner) (string, error) {
				readAttempted = true
				return "existing-token", nil
			}
			checkGitHubAccess = func(context.Context, string) error { return errors.New("502 Bad Gateway") }

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard, interactive: true})
			Expect(err).Should(MatchError(ContainSubstring("check the configured GitHub token")))
			Expect(defaultGitHubAuth.calls).Should(Equal(0))
			Expect(runner.requests).Should(BeEmpty())
			Expect(readAttempted).Should(BeFalse())
		})
	})

	When("the GitHub access check exceeds its deadline", func() {
		It("should treat the timeout as a check failure instead of a rejected credential", func() {
			request, runner := runClustersFixture(1)
			request.Credentials.GitHubToken = "good-token"
			checkGitHubAccess = func(ctx context.Context, _ string) error {
				_, bounded := ctx.Deadline()
				Expect(bounded).Should(BeTrue())
				return context.DeadlineExceeded
			}

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard, interactive: true})
			Expect(err).Should(MatchError(ContainSubstring("check the configured GitHub token")))
			Expect(errors.Is(err, context.DeadlineExceeded)).Should(BeTrue())
			Expect(defaultGitHubAuth.calls).Should(Equal(0))
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
		It("should open the prefilled token page and explain the one-day PAT", func() {
			request, runner := runClustersFixture(1)
			request.Provider = config.ProviderGitLab
			_ = credentialTempRoot()
			out := &bytes.Buffer{}

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: out, interactive: true})).Should(Succeed())

			Expect(defaultTokenPage.calls).Should(Equal(1))
			Expect(defaultTokenPage.url).Should(Equal(GitLabTokenURL))
			Expect(defaultTokenPage.url).Should(ContainSubstring("personal_access_tokens/legacy/new"))
			Expect(defaultTokenPage.url).Should(ContainSubstring("scopes=api%2Cwrite_repository"))
			Expect(out.String()).Should(ContainSubstring("personal access token page"))
			Expect(out.String()).Should(ContainSubstring("expiration to 1 day"))
			Expect(defaultGitLabPrompt.calls).Should(Equal(1))
		})

		It("should print the token page URL when the browser cannot open", func() {
			defaultTokenPage.err = errors.New("no browser")
			request, runner := runClustersFixture(1)
			request.Provider = config.ProviderGitLab
			_ = credentialTempRoot()
			out := &bytes.Buffer{}

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: out, interactive: true})).Should(Succeed())

			Expect(out.String()).Should(ContainSubstring("could not open the token page: no browser"))
			Expect(out.String()).Should(ContainSubstring(GitLabTokenURL))
		})

		It("should revoke the acquired token once after every cluster", func() {
			request, runner := runClustersFixture(2)
			request.Provider = config.ProviderGitLab

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: io.Discard, interactive: true})).Should(Succeed())

			Expect(defaultGitLabRevoke.calls).Should(Equal(1))
			Expect(runner.requests).Should(HaveLen(2))
		})

		It("should fail before prompting for a GitLab token when GitHub authentication fails", func() {
			defaultGitHubAuth.err = errors.New("gh failed")
			request, runner := runClustersFixture(1)
			request.Provider = config.ProviderBoth

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard, interactive: true})

			Expect(err).Should(MatchError(ContainSubstring("gh failed")))
			Expect(defaultTokenPage.calls).Should(Equal(0))
			Expect(defaultGitLabPrompt.calls).Should(Equal(0))
			Expect(defaultGitLabRevoke.calls).Should(Equal(0))
		})

		It("should report a revocation failure in the aggregate report and revoke once", func() {
			defaultGitLabRevoke.err = errors.New("revoke denied")
			request, runner := runClustersFixture(2)
			request.Provider = config.ProviderGitLab

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard, interactive: true})

			Expect(err).Should(MatchError(ContainSubstring("revoke GitLab token: revoke denied")))
			Expect(defaultGitLabRevoke.calls).Should(Equal(1))
			Expect(runner.requests).Should(HaveLen(2))
			markdown := readFile(latestReportPath(request.StateDir, "md"))
			Expect(markdown).Should(ContainSubstring("- Result: **failed**"))
			Expect(markdown).Should(ContainSubstring("revoke GitLab token: revoke denied"))
		})

		It("should prompt once, reuse the token, and revoke it", func() {
			request, runner := runClustersFixture(1)
			request.Provider = config.ProviderGitLab

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: io.Discard, interactive: true})).Should(Succeed())

			Expect(defaultGitLabPrompt.calls).Should(Equal(1))
			Expect(defaultGitHubAuth.calls).Should(Equal(0))
			Expect(runner.requests).Should(HaveLen(1))
			Expect(runner.requests[0].Credentials.GitLabToken).Should(Equal("glpat-test-token"))
			Expect(runner.requests[0].Credentials.GitLabTokenEphemeral).Should(BeTrue())
			Expect(defaultGitLabRevoke.calls).Should(Equal(1))
		})
	})

	When("a GitLab token is configured", func() {
		It("should not prompt for a token", func() {
			request, runner := runClustersFixture(1)
			request.Provider = config.ProviderGitLab
			request.Credentials.GitLabToken = "configured-gitlab-token"

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})).Should(Succeed())

			Expect(defaultGitLabPrompt.calls).Should(Equal(0))
			Expect(defaultGitLabRevoke.calls).Should(Equal(0))
			Expect(runner.requests[0].Credentials.GitLabToken).Should(Equal("configured-gitlab-token"))
			Expect(runner.requests[0].Credentials.GitLabTokenEphemeral).Should(BeFalse())
		})
	})

	When("both providers are selected without tokens", func() {
		It("should authenticate both once and reuse the tokens", func() {
			request, runner := runClustersFixture(1)
			request.Provider = config.ProviderBoth
			tempRoot := credentialTempRoot()

			Expect(RunClusters(context.Background(), request, runner, &Console{Out: io.Discard, interactive: true})).Should(Succeed())

			Expect(defaultGitHubAuth.calls).Should(Equal(1))
			Expect(defaultGitLabPrompt.calls).Should(Equal(1))
			Expect(runner.requests).Should(HaveLen(2))
			for _, runRequest := range runner.requests {
				Expect(runRequest.Credentials.GitHubToken).Should(Equal("test-github-token"))
				Expect(runRequest.Credentials.GitLabToken).Should(Equal("glpat-test-token"))
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

	When("GitLab token entry fails", func() {
		It("should stop before running any cluster and not revoke", func() {
			request, runner := runClustersFixture(1)
			request.Provider = config.ProviderGitLab
			defaultGitLabPrompt.err = errors.New("token entry cancelled")

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard, interactive: true})
			Expect(err).Should(MatchError(ContainSubstring("read GitLab personal access token: token entry cancelled")))
			Expect(runner.requests).Should(BeEmpty())
			Expect(defaultGitLabRevoke.calls).Should(Equal(0))
			Expect(os.ReadDir(request.StateDir)).Should(BeEmpty())
		})
	})

	When("GitLab authentication is non-interactive", func() {
		It("should require GITLAB_BOT_TOKEN without opening the browser", func() {
			request, runner := runClustersFixture(1)
			request.Provider = config.ProviderGitLab

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard})
			Expect(err).Should(MatchError(ContainSubstring("set GITLAB_BOT_TOKEN")))
			Expect(defaultTokenPage.calls).Should(Equal(0))
			Expect(defaultGitLabPrompt.calls).Should(Equal(0))
			Expect(runner.requests).Should(BeEmpty())
		})
	})

	When("the GitLab API URL is self-managed", func() {
		It("should require GITLAB_BOT_TOKEN before opening the token page", func() {
			request, runner := runClustersFixture(1)
			request.Provider = config.ProviderGitLab
			request.Credentials.GitLabAPIURL = "https://gitlab.example.com/api/v4"

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard, interactive: true})

			Expect(err).Should(MatchError(ContainSubstring("set GITLAB_BOT_TOKEN")))
			Expect(defaultTokenPage.calls).Should(Equal(0))
			Expect(defaultGitLabPrompt.calls).Should(Equal(0))
		})
	})

	When("a non-PAT value is entered", func() {
		It("should reject it before running any cluster", func() {
			defaultGitLabPrompt.token = "oauth-token"
			request, runner := runClustersFixture(1)
			request.Provider = config.ProviderGitLab

			err := RunClusters(context.Background(), request, runner, &Console{Out: io.Discard, interactive: true})

			Expect(err).Should(MatchError(ContainSubstring("requires a personal access token")))
			Expect(runner.requests).Should(BeEmpty())
			Expect(defaultGitLabRevoke.calls).Should(Equal(0))
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
			markdown := readFile(latestReportPath(request.StateDir, "md"))
			Expect(markdown).Should(ContainSubstring("| credentials | auth | failed | cleanup |  | temporary credentials are still present |"))
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

// singleAggregateDirectory returns the real latest aggregate directory under stateDir.
func singleAggregateDirectory(stateDir string) string {
	return filepath.Join(stateDir, "latest")
}

// latestRunName returns the aggregate run name recorded in the latest directory.
func latestRunName(stateDir string) string {
	var metadata struct {
		RunID     string    `json:"runID"`
		StartedAt time.Time `json:"startedAt"`
	}
	Expect(json.Unmarshal([]byte(readFile(filepath.Join(stateDir, "latest", ".aggregate.json"))), &metadata)).Should(Succeed())
	return evidence.ArtifactDirectoryName("multi", metadata.StartedAt, metadata.RunID)
}

// latestReportPath returns a run-named report path under the latest directory.
func latestReportPath(stateDir, extension string) string {
	return filepath.Join(singleAggregateDirectory(stateDir), latestRunName(stateDir)+"-report."+extension)
}
