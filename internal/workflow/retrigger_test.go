package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/providers"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

// retriggeringProvider records whether a second trigger commit was written.
type retriggeringProvider struct {
	*recordingProvider
	retriggered *atomic.Bool
}

// UpdateFile marks the retrigger before delegating to the recording provider.
func (p *retriggeringProvider) UpdateFile(ctx context.Context, fixture providers.FixtureRepository, path, branch, content, expectedSHA, message string) (providers.Commit, error) {
	commit, err := p.recordingProvider.UpdateFile(ctx, fixture, path, branch, content, expectedSHA, message)
	if err == nil {
		p.retriggered.Store(true)
	}
	return commit, err
}

var _ = Describe("retriggerComponent", func() {
	It("commits a fresh trigger and supersedes the previous SHA", func() {
		provider := &recordingProvider{files: map[string]string{"web-server/Dockerfile": "FROM base\n"}}
		stage := &LiveStage{
			Request:  config.Request{ComponentPaths: []string{"web-server/Dockerfile"}, SourceBranch: "main"},
			Fixture:  providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"},
			Provider: provider,
		}
		manifest := &model.RunManifest{
			RunID:          "run-1",
			Fixture:        model.FixtureIdentity{Components: []string{"mathwizz-web-server"}},
			TriggerCommits: []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "old-sha"}},
		}

		Expect(stage.retriggerComponent(context.Background(), manifest, "mathwizz-web-server")).Should(Succeed())
		Expect(manifest.TriggerCommits[0].SHA).Should(Equal("setup-1"))
		Expect(manifest.TriggerCommits[0].Retriggers).Should(Equal(1))
		Expect(manifest.TriggerCommits[0].PrerequisiteSHAs).Should(ContainElement("old-sha"))
		Expect(provider.messages["web-server/Dockerfile"]).Should(Equal(providers.CommitMessageTrigger))
	})
})

var _ = Describe("VerifyBuilds retrigger", func() {
	It("retriggers once when PaC never reconciled the PipelineRun", func() {
		const (
			component = "mathwizz-web-server"
			oldSHA    = "old-sha"
			newSHA    = "setup-1"
		)
		oldRun := annotatedPipelineRun("run-old", component, oldSHA, "uid-old", "Unknown", "git-auth-secret", time.Now().Add(-5*time.Minute))
		newRun := pipelineRun("run-new", component, newSHA, "uid-new", "True")
		build := buildTaskRunForWorkflow("build-task", "uid-new", "True", "buildah-oci-ta")
		var retriggered atomic.Bool
		dynamic := namespaceDynamicClient(&oldRun)
		dynamic.PrependReactor("list", "pipelineruns", func(kubetesting.Action) (bool, runtime.Object, error) {
			items := []unstructured.Unstructured{oldRun}
			if retriggered.Load() {
				items = []unstructured.Unstructured{newRun}
			}
			return true, &unstructured.UnstructuredList{Items: items}, nil
		})
		dynamic.PrependReactor("list", "taskruns", func(kubetesting.Action) (bool, runtime.Object, error) {
			items := []unstructured.Unstructured{}
			if retriggered.Load() {
				items = append(items, build)
			}
			return true, &unstructured.UnstructuredList{Items: items}, nil
		})
		provider := &retriggeringProvider{
			recordingProvider: &recordingProvider{files: map[string]string{"web-server/Dockerfile": "FROM base\n"}},
			retriggered:       &retriggered,
		}
		stage := &LiveStage{
			Request: config.Request{
				ComponentPaths: []string{"web-server/Dockerfile"},
				SourceBranch:   "main",
				Timeouts:       config.Timeouts{Trigger: time.Second, Build: time.Second},
			},
			Fixture:  providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"},
			Clients:  &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetesfake.NewSimpleClientset()},
			Provider: provider,
		}
		manifest := &model.RunManifest{
			RunID: "run-1",
			Fixture: model.FixtureIdentity{
				TenantNamespace: "mathwizz-test",
				Components:      []string{component},
			},
			TriggerCommits: []model.TriggerCommit{{Component: component, SHA: oldSHA}},
		}

		Expect(stage.VerifyBuilds(context.Background(), manifest)).Should(Succeed())
		Expect(retriggered.Load()).Should(BeTrue())
		Expect(manifest.TriggerCommits[0].SHA).Should(Equal(newSHA))
		Expect(manifest.TriggerCommits[0].Retriggers).Should(Equal(1))
		Expect(manifest.PipelineRuns).Should(HaveLen(1))
		Expect(manifest.PipelineRuns[0].Name).Should(Equal("run-new"))
	})

	It("does not retrigger when a duplicate run for the commit failed", func() {
		const (
			component = "mathwizz-web-server"
			sha       = "old-sha"
		)
		failed := pipelineRun("run-failed", component, sha, "uid-failed", "False")
		pending := annotatedPipelineRun("run-pending", component, sha, "uid-pending", "Unknown", "git-auth-secret", time.Now().Add(-5*time.Minute))
		var retriggered atomic.Bool
		provider := &retriggeringProvider{
			recordingProvider: &recordingProvider{files: map[string]string{"web-server/Dockerfile": "FROM base\n"}},
			retriggered:       &retriggered,
		}
		stage := &LiveStage{
			Request: config.Request{
				ComponentPaths: []string{"web-server/Dockerfile"},
				SourceBranch:   "main",
				Timeouts:       config.Timeouts{Trigger: time.Second, Build: time.Second},
			},
			Fixture:  providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"},
			Clients:  &cluster.ClientSet{Dynamic: namespaceDynamicClient(&failed, &pending), Kubernetes: kubernetesfake.NewSimpleClientset()},
			Provider: provider,
		}
		manifest := &model.RunManifest{
			RunID: "run-1",
			Fixture: model.FixtureIdentity{
				TenantNamespace: "mathwizz-test",
				Components:      []string{component},
			},
			TriggerCommits: []model.TriggerCommit{{Component: component, SHA: sha}},
		}

		err := stage.VerifyBuilds(context.Background(), manifest)
		var failure *PipelineRunFailedError
		Expect(errors.As(err, &failure)).Should(BeTrue())
		Expect(failure.Name).Should(Equal("run-failed"))
		Expect(retriggered.Load()).Should(BeFalse())
	})
})
