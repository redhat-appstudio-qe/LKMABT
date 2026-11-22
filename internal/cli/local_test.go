package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/cleanup"
	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/workflow"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	kubetesting "k8s.io/client-go/testing"
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

var _ = Describe("providerLabel", func() {
	It("should label a both split", func() {
		Expect(providerLabel(config.Request{Provider: config.ProviderGitHub, MultiProvider: true})).Should(Equal(config.ProviderGitHub))
	})

	It("should leave a single-provider run unlabeled", func() {
		Expect(providerLabel(config.Request{Provider: config.ProviderGitHub})).Should(BeEmpty())
	})
})

var _ = Describe("requestForProvider", func() {
	It("should mark both-provider splits", func() {
		request := requestForProvider(config.Request{Provider: config.ProviderBoth}, config.ProviderGitLab)

		Expect(request.Provider).Should(Equal(config.ProviderGitLab))
		Expect(request.MultiProvider).Should(BeTrue())
	})

	It("should keep a single-provider request unlabeled", func() {
		request := requestForProvider(config.Request{Provider: config.ProviderGitHub}, config.ProviderGitHub)

		Expect(request.MultiProvider).Should(BeFalse())
	})
})

var _ = Describe("promptFor", func() {
	When("the output is a console and the runner has a prompt", func() {
		It("should preserve the caller's confirmation policy", func() {
			consolePrompt := &recordingPrompt{approved: true}
			runnerPrompt := &recordingPrompt{approved: false}
			console := &Console{Out: io.Discard, Prompt: consolePrompt}
			runner := LiveRunner{Prompt: runnerPrompt, Out: console}

			prompt := promptFor(runner)
			Expect(prompt).ShouldNot(BeIdenticalTo(console))
			approved, err := prompt.Confirm(context.Background(), "delete tenant")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(approved).Should(BeFalse())
			Expect(runnerPrompt.calls).Should(Equal(1))
			Expect(consolePrompt.calls).Should(BeZero())
		})

		It("should not fall back to the console prompt when the runner has none", func() {
			runner := LiveRunner{Prompt: nil, Out: &Console{Out: io.Discard, Prompt: &recordingPrompt{approved: true}}}

			Expect(promptFor(runner)).Should(BeNil())
		})
	})

	When("the runner prompt is the output console", func() {
		It("should return the console without wrapping it", func() {
			console := &Console{Out: io.Discard, Prompt: &recordingPrompt{approved: true}}
			runner := LiveRunner{Prompt: console, Out: console}

			Expect(promptFor(runner)).Should(BeIdenticalTo(console))
		})
	})

	When("the output is not a console", func() {
		It("should keep the runner prompt", func() {
			prompt := &recordingPrompt{}
			runner := LiveRunner{Prompt: prompt, Out: io.Discard}

			Expect(promptFor(runner)).Should(BeIdenticalTo(prompt))
		})
	})

	When("the console has no prompt", func() {
		It("should still route the runner prompt through the console", func() {
			prompt := &recordingPrompt{approved: true}
			runner := LiveRunner{Prompt: prompt, Out: &Console{Out: io.Discard}}

			routed := promptFor(runner)
			Expect(routed).ShouldNot(BeIdenticalTo(prompt))
			approved, err := routed.Confirm(context.Background(), "delete tenant")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(approved).Should(BeTrue())
			Expect(prompt.calls).Should(Equal(1))
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

// namespaceListKinds registers the namespace and finalizer-strip list kinds the
// deferred cleanup deletion touches.
func namespaceListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		cleanup.NamespaceGVR: "NamespaceList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "applications"}:      "ApplicationList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "components"}:        "ComponentList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "imagerepositories"}: "ImageRepositoryList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "releases"}:          "ReleaseList",
		{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}:                      "PipelineRunList",
	}
}

var _ = Describe("finalizeDeferredRun", func() {
	When("the tenant namespace is owned by the deferred run", func() {
		It("should delete it without prompting or collecting artifacts", func() {
			owned := namespaceObject("tenant", "run-1")
			owned.SetUID("uid-1")
			owned.SetResourceVersion("7")
			client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), namespaceListKinds(), owned)
			prompt := &fakePrompt{approved: false}
			stage := &workflow.LiveStage{Namespace: cleanup.NamespaceService{Dynamic: client}, Prompt: prompt}
			stateDir := GinkgoT().TempDir()
			manifest := model.RunManifest{RunID: "run-1", ArtifactDirectory: "github-run-1", Fixture: model.FixtureIdentity{TenantNamespace: "tenant"}}
			log := &bytes.Buffer{}

			err := (LiveRunner{}).finalizeDeferredRun(context.Background(), stage, manifest, log)
			Expect(err).NotTo(HaveOccurred())

			Expect(prompt.calls).Should(BeZero())
			_, inspectErr := stage.Namespace.Inspect(context.Background(), "tenant")
			Expect(apierrors.IsNotFound(inspectErr)).Should(BeTrue())
			Expect(log.String()).Should(ContainSubstring("deleted tenant namespace tenant from deferred run run-1"))
			entries, readErr := os.ReadDir(stateDir)
			Expect(readErr).NotTo(HaveOccurred())
			Expect(entries).Should(BeEmpty())
		})
	})

	When("the tenant namespace belongs to another run", func() {
		It("should refuse to delete it", func() {
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), namespaceObject("tenant", "run-2"))
			prompt := &fakePrompt{approved: true}
			stage := &workflow.LiveStage{Namespace: cleanup.NamespaceService{Dynamic: client}, Prompt: prompt}
			manifest := model.RunManifest{RunID: "run-1", Fixture: model.FixtureIdentity{TenantNamespace: "tenant"}}

			err := (LiveRunner{}).finalizeDeferredRun(context.Background(), stage, manifest, io.Discard)
			Expect(err).Should(MatchError(ContainSubstring("refusing to delete tenant namespace tenant")))
			Expect(prompt.calls).Should(BeZero())
			_, inspectErr := stage.Namespace.Inspect(context.Background(), "tenant")
			Expect(inspectErr).NotTo(HaveOccurred())
		})
	})

	When("the tenant namespace is already gone", func() {
		It("should succeed without prompting", func() {
			client := fake.NewSimpleDynamicClient(runtime.NewScheme())
			prompt := &fakePrompt{approved: true}
			stage := &workflow.LiveStage{Namespace: cleanup.NamespaceService{Dynamic: client}, Prompt: prompt}
			manifest := model.RunManifest{RunID: "run-1", Fixture: model.FixtureIdentity{TenantNamespace: "tenant"}}

			Expect((LiveRunner{}).finalizeDeferredRun(context.Background(), stage, manifest, io.Discard)).Should(Succeed())
			Expect(prompt.calls).Should(BeZero())
		})
	})

	When("the namespace is replaced after inspection", func() {
		It("should refuse to modify or delete the replacement", func() {
			original := namespaceObject("tenant", "run-1")
			original.SetUID("uid-1")
			original.SetResourceVersion("7")
			replacement := namespaceObject("tenant", "run-2")
			replacement.SetUID("uid-2")
			replacement.SetResourceVersion("8")
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), original)
			var gets atomic.Int32
			client.PrependReactor("get", "namespaces", func(kubetesting.Action) (bool, runtime.Object, error) {
				if gets.Add(1) == 1 {
					return false, nil, nil
				}
				return true, replacement, nil
			})
			var deletes atomic.Int32
			client.PrependReactor("delete", "namespaces", func(kubetesting.Action) (bool, runtime.Object, error) {
				deletes.Add(1)
				return true, nil, nil
			})
			stage := &workflow.LiveStage{Namespace: cleanup.NamespaceService{Dynamic: client}}
			manifest := model.RunManifest{RunID: "run-1", Fixture: model.FixtureIdentity{TenantNamespace: "tenant"}}

			err := (LiveRunner{}).finalizeDeferredRun(context.Background(), stage, manifest, io.Discard)
			Expect(err).Should(MatchError(ContainSubstring("changed after verification")))
			Expect(deletes.Load()).Should(BeZero())
		})
	})
})
