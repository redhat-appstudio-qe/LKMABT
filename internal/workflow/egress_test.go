package workflow

import (
	"bytes"
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/cleanup"
	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/providers"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

const (
	goProxyTarget = "https://proxy.golang.org/golang.org/x/text/@v/list"
	alpineTarget  = "https://dl-cdn.alpinelinux.org/alpine/"
	npmTarget     = "https://registry.npmjs.org/-/ping"
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

// recordingEgressProbe records every probe and returns the configured result per
// target. An unlisted target is reachable.
type recordingEgressProbe struct {
	results    map[string]cluster.EgressResult
	err        error
	namespaces []string
	targets    []string
}

// probe records one call and returns the configured outcome.
func (p *recordingEgressProbe) probe(_ context.Context, namespace, target string) (cluster.EgressResult, error) {
	p.namespaces = append(p.namespaces, namespace)
	p.targets = append(p.targets, target)
	if p.err != nil {
		return cluster.EgressResult{}, p.err
	}
	if result, ok := p.results[target]; ok {
		return result, nil
	}
	return cluster.EgressResult{Reachable: true}, nil
}

var _ = Describe("LiveStage component narrowing", func() {
	It("should drop a component whose registry is unreachable and report the new selection", func() {
		sink := &recordingAlertSink{}
		log := &bytes.Buffer{}
		probe := &recordingEgressProbe{results: map[string]cluster.EgressResult{
			npmTarget: {Detail: "connection timed out"},
		}}
		stage := &LiveStage{Request: fullSelectionRequest(), ProbeEgress: probe.probe, Out: sink, Log: log}

		Expect(stage.limitToInternetFreeComponents(context.Background(), "tenant")).Should(Succeed())

		Expect(probe.namespaces).Should(Equal([]string{"tenant", "tenant", "tenant"}))
		Expect(probe.targets).Should(Equal([]string{goProxyTarget, alpineTarget, npmTarget}))
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

	It("should drop every component that needs an unreachable endpoint", func() {
		sink := &recordingAlertSink{}
		probe := &recordingEgressProbe{results: map[string]cluster.EgressResult{
			goProxyTarget: {Detail: "fetch failed: ENOTFOUND"},
		}}
		stage := &LiveStage{Request: fullSelectionRequest(), ProbeEgress: probe.probe, Out: sink}

		Expect(stage.limitToInternetFreeComponents(context.Background(), "tenant")).Should(Succeed())

		Expect(stage.Request.ComponentNames).Should(Equal([]string{"mathwizz-frontend"}))
		Expect(stage.Request.DroppedComponents).Should(Equal([]string{"mathwizz-web-server", "mathwizz-history-worker"}))
		Expect(sink.calls).Should(HaveLen(1))
		Expect(sink.calls[0].lines).Should(Equal([]string{
			"not built: mathwizz-web-server, mathwizz-history-worker",
			"reason: proxy.golang.org is unreachable from the cluster: fetch failed: ENOTFOUND",
			"testing: mathwizz-frontend",
		}))
	})

	It("should fail when no selected component can build", func() {
		sink := &recordingAlertSink{}
		probe := &recordingEgressProbe{results: map[string]cluster.EgressResult{
			goProxyTarget: {Detail: "fetch failed"},
			alpineTarget:  {Detail: "fetch failed"},
			npmTarget:     {Detail: "fetch failed"},
		}}
		stage := &LiveStage{Request: fullSelectionRequest(), ProbeEgress: probe.probe, Out: sink}

		err := stage.limitToInternetFreeComponents(context.Background(), "tenant")

		Expect(err).Should(MatchError(ContainSubstring("cannot reach the build dependencies of any selected component")))
		Expect(err.Error()).Should(ContainSubstring("proxy.golang.org"))
		Expect(err.Error()).Should(ContainSubstring("registry.npmjs.org"))
		Expect(err.Error()).Should(ContainSubstring("nothing to build"))
		Expect(stage.Request.ComponentNames).Should(BeEmpty())
		Expect(sink.calls).Should(BeEmpty())
	})

	It("should keep the full selection when every endpoint is reachable", func() {
		sink := &recordingAlertSink{}
		log := &bytes.Buffer{}
		probe := &recordingEgressProbe{}
		stage := &LiveStage{Request: fullSelectionRequest(), ProbeEgress: probe.probe, Out: sink, Log: log}

		Expect(stage.limitToInternetFreeComponents(context.Background(), "tenant")).Should(Succeed())

		Expect(probe.targets).Should(Equal([]string{goProxyTarget, alpineTarget, npmTarget}))
		Expect(stage.Request.ComponentNames).Should(HaveLen(3))
		Expect(stage.Request.DroppedComponents).Should(BeEmpty())
		Expect(sink.calls).Should(BeEmpty())
		Expect(log.String()).Should(BeEmpty())
	})

	It("should not probe a selection whose components declare no endpoints", func() {
		probe := &recordingEgressProbe{}
		stage := &LiveStage{
			Request:     config.Request{ComponentNames: []string{"custom"}, ComponentPaths: []string{"Dockerfile"}, TektonPaths: []string{"pipeline.yaml"}},
			ProbeEgress: probe.probe,
		}

		Expect(stage.limitToInternetFreeComponents(context.Background(), "tenant")).Should(Succeed())

		Expect(probe.targets).Should(BeEmpty())
		Expect(stage.Request.ComponentNames).Should(Equal([]string{"custom"}))
	})

	It("should fail the stage when the probe itself fails", func() {
		probe := &recordingEgressProbe{err: fmt.Errorf("forbidden")}
		stage := &LiveStage{Request: fullSelectionRequest(), ProbeEgress: probe.probe}

		err := stage.limitToInternetFreeComponents(context.Background(), "tenant")

		Expect(err).Should(MatchError(ContainSubstring("probe proxy.golang.org access for mathwizz-web-server, mathwizz-history-worker: forbidden")))
		Expect(stage.Request.ComponentNames).Should(HaveLen(3))
	})
})

var _ = Describe("LiveStage internet preflight", func() {
	It("should drop the frontend before creating any fixture component on a private cluster", func() {
		fixture := providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"}
		dynamic := namespaceDynamicClient(namespaceObject("tenant", "run-1"))
		log := &bytes.Buffer{}
		probe := &recordingEgressProbe{results: map[string]cluster.EgressResult{
			npmTarget: {Detail: "connection timed out"},
		}}
		request := fullSelectionRequest()
		request.Provider = config.ProviderGitHub
		request.ApplicationName = "app"
		request.SourceBranch = "main"
		kubernetes := kubernetesfake.NewSimpleClientset()
		allowEgressProbeAccess(kubernetes)
		stage := &LiveStage{
			Request:     request,
			Clients:     &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetes},
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

	It("should check probe pod access during preflight", func() {
		fixture := providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"}
		dynamic := namespaceDynamicClient(namespaceObject("tenant", "run-1"))
		kubernetes := kubernetesfake.NewSimpleClientset()
		kubernetes.PrependReactor("create", "selfsubjectaccessreviews", func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Reason: "rbac denied"}}, nil
		})
		request := fullSelectionRequest()
		request.Provider = config.ProviderGitLab
		request.ClusterServer = "https://api.example"
		stage := &LiveStage{
			Request:          request,
			Clients:          &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetes},
			PreflightCluster: fakePreflightCluster{server: request.ClusterServer},
			Namespace:        cleanup.NamespaceService{Dynamic: dynamic, NameLister: testNameLister{}},
			Provider:         &githubAppProviderStub{fixture: fixture},
			Fixture:          fixture,
		}
		manifest := &model.RunManifest{RunID: "run-1"}

		err := stage.Preflight(context.Background(), manifest)

		Expect(err).Should(MatchError(ContainSubstring("egress probe access check failed")))
		Expect(err).Should(MatchError(ContainSubstring("rbac denied")))
		Expect(manifest.Fixture.TenantNamespace).Should(Equal("tenant"))
		_, getErr := dynamic.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant", metav1.GetOptions{})
		Expect(getErr).ShouldNot(HaveOccurred())
	})
})
