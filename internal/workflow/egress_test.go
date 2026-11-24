package workflow

import (
	"bytes"
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/providers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

// fullSelectionRequest returns a request selecting the whole fixture with a
// cluster name, as a live run would.
func fullSelectionRequest() config.Request {
	names, paths, tektonPaths := config.FullComponents()
	return config.Request{
		ClusterName:     "kflux-osp-p01",
		TenantNamespace: "tenant",
		ComponentNames:  names,
		ComponentPaths:  paths,
		TektonPaths:     tektonPaths,
	}
}

// recordingEgressProbe records every probe and returns a fixed result.
type recordingEgressProbe struct {
	result     cluster.EgressResult
	err        error
	namespaces []string
	targets    []string
}

// probe records one call and returns the configured outcome.
func (p *recordingEgressProbe) probe(_ context.Context, namespace, target string) (cluster.EgressResult, error) {
	p.namespaces = append(p.namespaces, namespace)
	p.targets = append(p.targets, target)
	return p.result, p.err
}

var _ = Describe("LiveStage component narrowing", func() {
	It("should drop a component whose registry is unreachable and report the new selection", func() {
		sink := &recordingAlertSink{}
		log := &bytes.Buffer{}
		probe := &recordingEgressProbe{result: cluster.EgressResult{Detail: "connection timed out"}}
		stage := &LiveStage{Request: fullSelectionRequest(), ProbeEgress: probe.probe, Out: sink, Log: log}

		Expect(stage.limitToInternetFreeComponents(context.Background(), "tenant")).Should(Succeed())

		Expect(probe.namespaces).Should(Equal([]string{"tenant"}))
		Expect(probe.targets).Should(Equal([]string{"https://registry.npmjs.org/-/ping"}))
		Expect(stage.Request.ComponentNames).Should(Equal([]string{"mathwizz-web-server", "mathwizz-history-worker"}))
		Expect(stage.Request.ComponentPaths).Should(Equal([]string{"web-server/Dockerfile", "history-worker/Dockerfile"}))
		Expect(stage.Request.TektonPaths).Should(HaveLen(2))
		Expect(stage.Request.DroppedComponents).Should(Equal([]string{"mathwizz-frontend"}))
		Expect(stage.Request.ExpectedComponents()).Should(Equal(2))
		Expect(sink.calls).Should(HaveLen(1))
		Expect(sink.calls[0].key).Should(Equal("component:kflux-osp-p01:mathwizz-frontend->:mathwizz-web-server,mathwizz-history-worker"))
		Expect(sink.calls[0].title).Should(Equal("kflux-osp-p01: component selection narrowed"))
		Expect(sink.calls[0].lines).Should(Equal([]string{
			"not built: mathwizz-frontend",
			"reason: registry.npmjs.org is unreachable from the cluster: connection timed out",
			"testing: mathwizz-web-server, mathwizz-history-worker",
		}))
		Expect(log.String()).Should(Equal(" ! kflux-osp-p01: component selection narrowed\n" +
			"   not built: mathwizz-frontend\n" +
			"   reason: registry.npmjs.org is unreachable from the cluster: connection timed out\n" +
			"   testing: mathwizz-web-server, mathwizz-history-worker\n"))
	})

	It("should keep the full selection when every registry is reachable", func() {
		sink := &recordingAlertSink{}
		log := &bytes.Buffer{}
		probe := &recordingEgressProbe{result: cluster.EgressResult{Reachable: true}}
		stage := &LiveStage{Request: fullSelectionRequest(), ProbeEgress: probe.probe, Out: sink, Log: log}

		Expect(stage.limitToInternetFreeComponents(context.Background(), "tenant")).Should(Succeed())

		Expect(probe.targets).Should(Equal([]string{"https://registry.npmjs.org/-/ping"}))
		Expect(stage.Request.ComponentNames).Should(HaveLen(3))
		Expect(stage.Request.DroppedComponents).Should(BeEmpty())
		Expect(sink.calls).Should(BeEmpty())
		Expect(log.String()).Should(BeEmpty())
	})

	It("should not probe a selection whose builds need no public endpoint", func() {
		names, paths, tektonPaths := config.SmokeComponents()
		probe := &recordingEgressProbe{}
		stage := &LiveStage{
			Request:     config.Request{ComponentNames: names, ComponentPaths: paths, TektonPaths: tektonPaths},
			ProbeEgress: probe.probe,
		}

		Expect(stage.limitToInternetFreeComponents(context.Background(), "tenant")).Should(Succeed())

		Expect(probe.targets).Should(BeEmpty())
		Expect(stage.Request.ComponentNames).Should(Equal(names))
	})

	It("should fail the stage when the probe itself fails", func() {
		probe := &recordingEgressProbe{err: fmt.Errorf("forbidden")}
		stage := &LiveStage{Request: fullSelectionRequest(), ProbeEgress: probe.probe}

		err := stage.limitToInternetFreeComponents(context.Background(), "tenant")

		Expect(err).Should(MatchError(ContainSubstring("probe registry.npmjs.org access for mathwizz-frontend: forbidden")))
		Expect(stage.Request.ComponentNames).Should(HaveLen(3))
	})
})

var _ = Describe("LiveStage internet preflight", func() {
	It("should drop the frontend before creating any fixture component on a private cluster", func() {
		fixture := providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"}
		dynamic := namespaceDynamicClient(namespaceObject("tenant", "run-1"))
		log := &bytes.Buffer{}
		probe := &recordingEgressProbe{result: cluster.EgressResult{Detail: "connection timed out"}}
		request := fullSelectionRequest()
		request.Provider = config.ProviderGitHub
		request.ApplicationName = "app"
		request.SourceBranch = "main"
		stage := &LiveStage{
			Request:     request,
			Clients:     &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetesfake.NewSimpleClientset()},
			ProbeEgress: probe.probe,
			Provider:    &githubAppProviderStub{fixture: fixture},
			Fixture:     fixture,
			Log:         log,
		}
		manifest := &model.RunManifest{RunID: "run-1"}

		Expect(stage.EnsureFixture(context.Background(), manifest)).Should(Succeed())

		Expect(manifest.Fixture.Components).Should(Equal([]string{"mathwizz-web-server", "mathwizz-history-worker"}))
		Expect(stage.Request.DroppedComponents).Should(Equal([]string{"mathwizz-frontend"}))
		_, err := dynamic.Resource(cluster.ComponentGVR).Namespace("tenant").Get(context.Background(), "mathwizz-frontend", metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).Should(BeTrue())
		Expect(log.String()).Should(ContainSubstring("not built: mathwizz-frontend"))
	})

	It("should authorize the probe pod lifecycle only when a component needs internet", func() {
		stage := &LiveStage{Request: fullSelectionRequest()}
		checks := stage.accessChecks()

		Expect(checks).Should(HaveLen(5))
		Expect(checks[4].Name).Should(Equal("egress probe pods"))
		Expect(checks[4].Namespace).Should(Equal("tenant"))
		Expect(checks[4].Verbs).Should(Equal([]string{"create", "delete"}))
		Expect(checks[4].Guidance).Should(ContainSubstring("internet access"))

		names, paths, tektonPaths := config.SmokeComponents()
		offline := &LiveStage{Request: config.Request{ComponentNames: names, ComponentPaths: paths, TektonPaths: tektonPaths}}
		Expect(offline.accessChecks()).Should(HaveLen(4))
	})
})
