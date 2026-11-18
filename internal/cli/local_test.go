package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/model"
)

var _ = Describe("outcomeFromManifest", func() {
	When("the manifest completed", func() {
		It("should report the completed phase", func() {
			manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseCompleted, ArtifactDirectory: "github-run-x", Provider: "github"}
			Expect(outcomeFromManifest("github", manifest)).Should(Equal(RunOutcome{Provider: "github", RunID: "run-1", ArtifactDirectory: "github-run-x", Stage: model.PhaseCompleted}))
		})
	})

	When("the manifest recorded a failure", func() {
		It("should report the failure phase", func() {
			manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseFailed, Failure: &model.Failure{Phase: model.PhasePreflight, Message: "denied"}}
			outcome := outcomeFromManifest("github", manifest)
			Expect(outcome.Stage).Should(Equal(model.PhasePreflight))
		})
	})

	When("both a run phase and a failure phase exist", func() {
		It("should prefer the failure phase over the run phase", func() {
			manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseCompleted, Failure: &model.Failure{Phase: model.PhaseBuildsVerified}}
			outcome := outcomeFromManifest("github", manifest)
			Expect(outcome.Stage).Should(Equal(model.PhaseBuildsVerified))
		})
	})
})

var _ = Describe("runErrorWithLogFailure", func() {
	When("no log attachment failed", func() {
		It("should return the workflow error unchanged", func() {
			runErr := fmt.Errorf("build failed")
			Expect(runErrorWithLogFailure(runErr, nil)).Should(BeIdenticalTo(runErr))
			Expect(runErrorWithLogFailure(nil, nil)).Should(BeNil())
		})
	})

	When("only the log attachment failed", func() {
		It("should return an error naming the attachment and its path", func() {
			err := runErrorWithLogFailure(nil, fmt.Errorf("flush run log /runs/run.log: disk full"))
			Expect(err).Should(MatchError(ContainSubstring("attach run log")))
			Expect(err).Should(MatchError(ContainSubstring("/runs/run.log")))
			Expect(err).ShouldNot(BeNil())
		})
	})

	When("both the workflow and the attachment failed", func() {
		It("should return an error naming both", func() {
			err := runErrorWithLogFailure(fmt.Errorf("build failed"), fmt.Errorf("flush run log /runs/run.log: disk full"))
			Expect(err).Should(MatchError(ContainSubstring("build failed")))
			Expect(err).Should(MatchError(ContainSubstring("attach run log")))
		})
	})
})

// failingAttachLog records the requested run-log path and fails every attachment.
type failingAttachLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	path   string
}

func (l *failingAttachLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(p)
}

func (l *failingAttachLog) Attach(path string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.path = path
	return fmt.Errorf("flush run log %s: disk full", path)
}

var _ = Describe("NewLiveRunner", func() {
	When("an interactive session is requested", func() {
		It("should carry the output and interactivity into the runner", func() {
			out := &bytes.Buffer{}
			runner := NewLiveRunner(nil, out, true).(LiveRunner)

			Expect(runner.Out).Should(BeIdenticalTo(out))
			Expect(runner.Interactive).Should(BeTrue())
		})
	})
})

var _ = Describe("LiveRunner.Run", func() {
	When("the provider run log cannot be attached", func() {
		It("should report the attachment failure in the run error", func() {
			stateDir := GinkgoT().TempDir()
			request := config.Defaults()
			request.RunID = "run-attach"
			request.Provider = config.ProviderGitHub
			request.StateDir = stateDir
			request.Kubeconfig = writeKubeconfig(GinkgoT().TempDir(), "c1")
			request.ClusterServer = ""
			request.TenantNamespace = ""
			request.Credentials = config.Credentials{GitHubToken: "token"}
			log := &failingAttachLog{}

			_, err := (LiveRunner{Out: io.Discard}).Run(context.Background(), request, log)

			Expect(err).Should(HaveOccurred())
			Expect(err).Should(MatchError(ContainSubstring("attach run log")))
			Expect(err).Should(MatchError(ContainSubstring("failed at preflight")))
			Expect(log.path).Should(HaveSuffix("run.log"))
			Expect(log.path).Should(ContainSubstring("github-run-"))
			Expect(err).Should(MatchError(ContainSubstring(log.path)))
		})
	})
})
