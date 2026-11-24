package cli

import (
	"bytes"
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/cleanup"
	"github.com/redhat-appstudio/lkmabt/internal/collector"
	"github.com/redhat-appstudio/lkmabt/internal/evidence"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

// deferredDeletionClient returns a fake dynamic client with list kinds for
// namespaces and every resource finalizer stripping visits.
func deferredDeletionClient(objects ...runtime.Object) *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		cleanup.NamespaceGVR: "NamespaceList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "applications"}:      "ApplicationList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "components"}:        "ComponentList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "imagerepositories"}: "ImageRepositoryList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "releases"}:          "ReleaseList",
		{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}:                      "PipelineRunList",
	}, objects...)
}

var _ = Describe("NamespaceDeletions", func() {
	When("every queued namespace is approved", func() {
		It("should prompt once per namespace and delete them", func() {
			client := deferredDeletionClient(namespaceObject("tenant-a", "run-a"), namespaceObject("tenant-b", "run-b"))
			service := cleanup.NamespaceService{Dynamic: client}
			infoA, err := service.Inspect(context.Background(), "tenant-a")
			Expect(err).ShouldNot(HaveOccurred())
			infoB, err := service.Inspect(context.Background(), "tenant-b")
			Expect(err).ShouldNot(HaveOccurred())
			queue := &NamespaceDeletions{}
			var out, log bytes.Buffer
			queue.Add("cluster-a", "https://a.example", service, infoA, &out, nil)
			queue.Add("cluster-b", "https://b.example", service, infoB, &out, nil)
			prompt := &recordingPrompt{approved: true}

			Expect(queue.Finalize(context.Background(), prompt, &log)).Should(Succeed())

			Expect(prompt.calls).Should(Equal(2))
			Expect(prompt.messages[0]).Should(ContainSubstring("tenant-a"))
			Expect(prompt.messages[0]).Should(ContainSubstring("cluster-a"))
			for _, name := range []string{"tenant-a", "tenant-b"} {
				_, getErr := client.Resource(cleanup.NamespaceGVR).Get(context.Background(), name, metav1.GetOptions{})
				Expect(apierrors.IsNotFound(getErr)).Should(BeTrue())
			}
			Expect(out.String()).Should(BeEmpty())
			Expect(log.String()).Should(BeEmpty())
		})
	})

	When("the prompt is declined", func() {
		It("should retain the namespace and record one notice", func() {
			client := deferredDeletionClient(namespaceObject("tenant-a", "run-a"))
			service := cleanup.NamespaceService{Dynamic: client}
			info, err := service.Inspect(context.Background(), "tenant-a")
			Expect(err).ShouldNot(HaveOccurred())
			queue := &NamespaceDeletions{}
			var out, log bytes.Buffer
			queue.Add("cluster-a", "https://a.example", service, info, &out, nil)
			prompt := &recordingPrompt{approved: false}

			Expect(queue.Finalize(context.Background(), prompt, &log)).Should(Succeed())

			Expect(out.String()).Should(ContainSubstring("retained tenant namespace tenant-a on cluster cluster-a\n"))
			Expect(log.String()).Should(Equal(out.String()))
			_, getErr := client.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant-a", metav1.GetOptions{})
			Expect(getErr).ShouldNot(HaveOccurred())
		})
	})

	When("the namespace was adopted by another run before the prompt", func() {
		It("should refuse the deletion and keep it", func() {
			client := deferredDeletionClient(namespaceObject("tenant-a", "run-a"))
			service := cleanup.NamespaceService{Dynamic: client}
			info, err := service.Inspect(context.Background(), "tenant-a")
			Expect(err).ShouldNot(HaveOccurred())
			queue := &NamespaceDeletions{}
			queue.Add("cluster-a", "https://a.example", service, info, nil, nil)
			namespace, err := client.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant-a", metav1.GetOptions{})
			Expect(err).ShouldNot(HaveOccurred())
			labels := namespace.GetLabels()
			labels[cleanup.RunIDLabel] = "run-other"
			namespace.SetLabels(labels)
			_, err = client.Resource(cleanup.NamespaceGVR).Update(context.Background(), namespace, metav1.UpdateOptions{})
			Expect(err).ShouldNot(HaveOccurred())

			err = queue.Finalize(context.Background(), &recordingPrompt{approved: true}, nil)

			Expect(err).Should(MatchError(ContainSubstring("refusing to delete namespace")))
			_, getErr := client.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant-a", metav1.GetOptions{})
			Expect(getErr).ShouldNot(HaveOccurred())
		})
	})

	When("the queued namespace still holds its tenant lock", func() {
		It("should release the lock only after the prompt is answered", func() {
			client := deferredDeletionClient(namespaceObject("tenant-a", "run-a"))
			service := cleanup.NamespaceService{Dynamic: client}
			info, err := service.Inspect(context.Background(), "tenant-a")
			Expect(err).ShouldNot(HaveOccurred())
			queue := &NamespaceDeletions{}
			released := 0
			queue.Add("cluster-a", "https://a.example", service, info, nil, func() { released++ })

			Expect(released).Should(Equal(0))
			Expect(queue.Finalize(context.Background(), &recordingPrompt{approved: false}, nil)).Should(Succeed())
			Expect(released).Should(Equal(1))
		})
	})

	When("the prompt fails", func() {
		It("should record a retention notice and release the lock", func() {
			client := deferredDeletionClient(namespaceObject("tenant-a", "run-a"))
			service := cleanup.NamespaceService{Dynamic: client}
			info, err := service.Inspect(context.Background(), "tenant-a")
			Expect(err).ShouldNot(HaveOccurred())
			queue := &NamespaceDeletions{}
			var out bytes.Buffer
			released := 0
			queue.Add("cluster-a", "https://a.example", service, info, &out, func() { released++ })

			err = queue.Finalize(context.Background(), errorPrompt{err: context.Canceled}, nil)

			Expect(err).Should(MatchError(context.Canceled))
			Expect(out.String()).Should(ContainSubstring("retained tenant namespace tenant-a on cluster cluster-a: cleanup prompt failed"))
			Expect(released).Should(Equal(1))
		})
	})
})

// errorPrompt returns a fixed confirmation error.
type errorPrompt struct {
	err error
}

// Confirm returns the configured error.
func (p errorPrompt) Confirm(context.Context, string) (bool, error) {
	return false, p.err
}

var _ = Describe("Lifecycle deferred namespace deletion", func() {
	It("should queue the prompt instead of asking during finalization", func() {
		stateDir := GinkgoT().TempDir()
		client := deferredDeletionClient(namespaceObject("tenant-deferred", "run-1"))
		prompt := &recordingPrompt{approved: true}
		queue := &NamespaceDeletions{}
		manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseCompleted, Fixture: model.FixtureIdentity{TenantNamespace: "tenant-deferred"}}
		store := evidence.NewManifestStore(stateDir)
		Expect(store.Create(&manifest)).Should(Succeed())
		released := 0
		lifecycle := Lifecycle{Store: store, Namespace: cleanup.NamespaceService{Dynamic: client}, Collector: collector.Collector{StateDir: stateDir}, Prompt: prompt, Deletions: queue, Cluster: "cluster-a", LockServer: "https://a.example", Release: func() { released++ }, Sources: lifecycleTestSources()}

		Expect(lifecycle.Finalize(context.Background(), manifest, nil)).Should(Succeed())

		Expect(prompt.calls).Should(BeZero())
		Expect(queue.pending).Should(HaveLen(1))
		Expect(released).Should(Equal(0))
		Expect(queue.Finalize(context.Background(), prompt, nil)).Should(Succeed())
		Expect(prompt.calls).Should(Equal(1))
		Expect(released).Should(Equal(1))
		Expect(prompt.messages[0]).Should(ContainSubstring("tenant-deferred"))
		_, err := client.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant-deferred", metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).Should(BeTrue())
	})

	It("should not queue when artifact collection fails", func() {
		stateDir := GinkgoT().TempDir()
		client := deferredDeletionClient(namespaceObject("tenant-deferred", "run-1"))
		queue := &NamespaceDeletions{}
		manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseFailed, Fixture: model.FixtureIdentity{TenantNamespace: "tenant-deferred"}}
		lifecycle := Lifecycle{Store: evidence.NewManifestStore(stateDir), Namespace: cleanup.NamespaceService{Dynamic: client}, Collector: collector.Collector{StateDir: stateDir}, Prompt: &recordingPrompt{approved: true}, Deletions: queue, Cluster: "cluster-a", LockServer: "https://a.example", Sources: []collector.Source{{Name: "applications", Path: "workload/applications.json", Collect: func(context.Context, string) error { return errors.New("api unavailable") }}}}

		Expect(lifecycle.Finalize(context.Background(), manifest, errors.New("preflight failed"))).ShouldNot(Succeed())
		Expect(queue.pending).Should(BeEmpty())
	})
})
