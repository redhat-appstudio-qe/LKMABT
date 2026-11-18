package cleanup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/testing"
)

var _ = Describe("NamespaceService", func() {
	It("deletes only exact owned namespace", func() {
		client := cleanupClient(namespace("tenant", "run-1", true))
		service := NamespaceService{Dynamic: client}
		if err := service.Delete(context.Background(), "tenant", "run-1"); err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(service.WaitDeleted(context.Background(), "tenant")).To(Succeed())
	})

	It("strips finalizers before delete", func() {
		appGVR := schema.GroupVersionResource{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "applications"}
		client := cleanupClient(
			namespace("tenant", "run-1", true),
			&unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "appstudio.redhat.com/v1alpha1",
				"kind":       "Application",
				"metadata": map[string]any{
					"name": "app-1", "namespace": "tenant", "finalizers": []any{"appstudio.redhat.com/finalizer"},
				},
			}},
		)

		if err := (NamespaceService{Dynamic: client}).Delete(context.Background(), "tenant", "run-1"); err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		updated, err := client.Resource(appGVR).Namespace("tenant").Get(context.Background(), "app-1", metav1.GetOptions{})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(updated.GetFinalizers()).To(BeEmpty())
	})

	It("refuses unowned namespace", func() {
		client := cleanupClient(namespace("tenant", "run-1", false))
		err := (NamespaceService{Dynamic: client}).Delete(context.Background(), "tenant", "run-1")
		Expect(err).To(MatchError(ContainSubstring("refusing")))
		if _, err := client.Resource(NamespaceGVR).Get(context.Background(), "tenant", metav1.GetOptions{}); err != nil {
			Fail("namespace was deleted")
		}
	})

	It("requires run ID for deletion", func() {
		client := cleanupClient(namespace("tenant", "run-1", true))
		err := (NamespaceService{Dynamic: client}).Delete(context.Background(), "tenant", "")
		Expect(err).To(HaveOccurred())
	})

	It("uses CLI output for namespace names", func() {
		script := filepath.Join(GinkgoT().TempDir(), "oc")
		content := "#!/bin/sh\n[ \"$1 $2 $3 $4 $5\" = \"get namespaces -o name --no-headers\" ] || exit 1\nprintf '%s\\n' namespace/mathwizz-test-github namespace/mathwizz-test-github-20261005b\n"
		if err := os.WriteFile(script, []byte(content), 0700); err != nil {
			Expect(err).NotTo(HaveOccurred())
		}

		names, err := (OCNamespaceNameLister{Command: script}).List(context.Background())
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		want := []string{"mathwizz-test-github", "mathwizz-test-github-20261005b"}
		Expect(names).To(Equal(want))
	})
})

type failingNameLister struct{}

// List reports the configured failure.
func (failingNameLister) List(context.Context) ([]string, error) {
	return nil, errors.New("oc missing")
}

var _ = Describe("NamespaceService.FindScoped", func() {
	When("scoped namespaces are listed", func() {
		It("should return the exact name and every prefixed name in sorted order", func() {
			client := cleanupClient(
				namespace("mathwizz-test-github", "run-1", true),
				namespace("mathwizz-test-github-20261005b", "run-2", true),
				namespace("other-tenant", "run-3", true),
			)
			service := NamespaceService{Dynamic: client, NameLister: staticNameLister{names: []string{"mathwizz-test-github-20261005b", "other-tenant", "mathwizz-test-github"}}}
			scoped, err := service.FindScoped(context.Background(), "mathwizz-test-github")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(scoped).Should(HaveLen(2))
			Expect(scoped[0].Name).Should(Equal("mathwizz-test-github"))
			Expect(scoped[1].Name).Should(Equal("mathwizz-test-github-20261005b"))
		})
	})

	When("a scoped namespace is unowned", func() {
		It("should include it", func() {
			client := cleanupClient(namespace("tenant", "run-1", false))
			service := NamespaceService{Dynamic: client, NameLister: staticNameLister{names: []string{"tenant"}}}
			scoped, err := service.FindScoped(context.Background(), "tenant")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(scoped).Should(HaveLen(1))
			Expect(scoped[0].Owned()).Should(BeFalse())
			Expect(scoped[0].RunID).Should(Equal("run-1"))
		})
	})

	When("a listed scoped name does not exist", func() {
		It("should skip it", func() {
			client := cleanupClient(namespace("tenant", "run-1", true))
			service := NamespaceService{Dynamic: client, NameLister: staticNameLister{names: []string{"tenant", "tenant-gone"}}}
			scoped, err := service.FindScoped(context.Background(), "tenant")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(scoped).Should(HaveLen(1))
		})
	})

	When("the tenant namespace is empty", func() {
		It("should reject the request", func() {
			service := NamespaceService{Dynamic: cleanupClient(), NameLister: staticNameLister{names: nil}}
			_, err := service.FindScoped(context.Background(), "  ")
			Expect(err).Should(MatchError(ContainSubstring("tenant namespace is required")))
		})
	})

	When("no dynamic client is configured", func() {
		It("should reject the request", func() {
			service := NamespaceService{NameLister: staticNameLister{}}
			_, err := service.FindScoped(context.Background(), "tenant")
			Expect(err).Should(MatchError(ContainSubstring("dynamic client is required")))
		})
	})

	When("the name lister fails", func() {
		It("should propagate the error", func() {
			service := NamespaceService{Dynamic: cleanupClient(), NameLister: failingNameLister{}}
			_, err := service.FindScoped(context.Background(), "tenant")
			Expect(err).Should(MatchError(ContainSubstring("oc missing")))
		})
	})
})

var _ = Describe("NamespaceInfo.Owned", func() {
	DescribeTable("should report exact ownership",
		func(labels map[string]string, want bool) {
			info := NamespaceInfo{Labels: labels, RunID: labels[RunIDLabel]}
			Expect(info.Owned()).Should(Equal(want))
		},
		Entry("owned", map[string]string{ManagedByLabel: ManagedByValue, RunIDLabel: "run-1"}, true),
		Entry("missing managed-by", map[string]string{RunIDLabel: "run-1"}, false),
		Entry("missing run ID", map[string]string{ManagedByLabel: "lkmabt"}, false),
		Entry("different manager", map[string]string{ManagedByLabel: "other-tool", RunIDLabel: "run-1"}, false),
		Entry("blank run ID", map[string]string{ManagedByLabel: "lkmabt", RunIDLabel: "   "}, false),
		Entry("no labels", map[string]string{}, false),
	)
})

var _ = Describe("NamespaceService.DeleteApproved", func() {
	When("the namespace is unowned but approved", func() {
		It("should delete it despite missing ownership labels", func() {
			client := cleanupClient(namespace("tenant-old", "run-1", false))
			service := NamespaceService{Dynamic: client}
			info, err := service.Inspect(context.Background(), "tenant-old")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(service.DeleteApproved(context.Background(), info)).Should(Succeed())
			_, err = client.Resource(NamespaceGVR).Get(context.Background(), "tenant-old", metav1.GetOptions{})
			Expect(apierrors.IsNotFound(err)).Should(BeTrue())
		})
	})

	When("the labels changed after approval", func() {
		It("should refuse and keep the namespace", func() {
			client := cleanupClient(namespace("tenant-x", "run-1", false))
			service := NamespaceService{Dynamic: client}
			snapshot, err := service.Inspect(context.Background(), "tenant-x")
			Expect(err).ShouldNot(HaveOccurred())
			object, err := client.Resource(NamespaceGVR).Get(context.Background(), "tenant-x", metav1.GetOptions{})
			Expect(err).ShouldNot(HaveOccurred())
			object.SetLabels(map[string]string{ManagedByLabel: ManagedByValue, RunIDLabel: "run-1"})
			_, err = client.Resource(NamespaceGVR).Update(context.Background(), object, metav1.UpdateOptions{})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(service.DeleteApproved(context.Background(), snapshot)).Should(MatchError(ContainSubstring("changed after approval")))
			_, err = client.Resource(NamespaceGVR).Get(context.Background(), "tenant-x", metav1.GetOptions{})
			Expect(err).ShouldNot(HaveOccurred())
		})
	})

	When("the run ID changed after approval", func() {
		It("should refuse and keep the namespace", func() {
			client := cleanupClient(namespace("tenant-x", "run-1", false))
			service := NamespaceService{Dynamic: client}
			snapshot, err := service.Inspect(context.Background(), "tenant-x")
			Expect(err).ShouldNot(HaveOccurred())
			object, err := client.Resource(NamespaceGVR).Get(context.Background(), "tenant-x", metav1.GetOptions{})
			Expect(err).ShouldNot(HaveOccurred())
			object.SetLabels(map[string]string{RunIDLabel: "run-2"})
			_, err = client.Resource(NamespaceGVR).Update(context.Background(), object, metav1.UpdateOptions{})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(service.DeleteApproved(context.Background(), snapshot)).Should(MatchError(ContainSubstring("changed after approval")))
			_, err = client.Resource(NamespaceGVR).Get(context.Background(), "tenant-x", metav1.GetOptions{})
			Expect(err).ShouldNot(HaveOccurred())
		})
	})

	When("the name is empty", func() {
		It("should reject the request", func() {
			service := NamespaceService{Dynamic: cleanupClient()}
			Expect(service.DeleteApproved(context.Background(), NamespaceInfo{})).Should(MatchError(ContainSubstring("namespace name is required")))
		})
	})

	When("finalizer stripping fails", func() {
		It("should report the strip failure with the namespace and keep the namespace", func() {
			client := cleanupClient(namespace("tenant-strip", "run-1", false))
			client.PrependReactor("list", "applications", func(testing.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("list blocked")
			})
			service := NamespaceService{Dynamic: client}
			snapshot, err := service.Inspect(context.Background(), "tenant-strip")
			Expect(err).ShouldNot(HaveOccurred())

			err = service.DeleteApproved(context.Background(), snapshot)
			Expect(err).Should(MatchError(ContainSubstring("strip finalizers in namespace tenant-strip")))
			Expect(err).Should(MatchError(ContainSubstring("list blocked")))
			_, getErr := client.Resource(NamespaceGVR).Get(context.Background(), "tenant-strip", metav1.GetOptions{})
			Expect(getErr).ShouldNot(HaveOccurred())
		})
	})

	When("the namespace is already gone", func() {
		It("should report the re-read failure", func() {
			client := cleanupClient()
			service := NamespaceService{Dynamic: client}
			snapshot := NamespaceInfo{Name: "tenant-gone", RunID: "run-1"}
			Expect(service.DeleteApproved(context.Background(), snapshot)).Should(MatchError(ContainSubstring("re-read namespace tenant-gone")))
		})
	})

	When("the namespace is unchanged", func() {
		It("should send the approved UID and resourceVersion as delete preconditions", func() {
			object := namespace("tenant-pre", "run-1", false)
			object.SetUID("uid-1")
			object.SetResourceVersion("7")
			client := cleanupClient(object)
			service := NamespaceService{Dynamic: client}
			snapshot, err := service.Inspect(context.Background(), "tenant-pre")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(service.DeleteApproved(context.Background(), snapshot)).Should(Succeed())

			// The dynamic fake records DeleteActionImpl by value, so assert through the interface.
			var deleted testing.DeleteAction
			for _, action := range client.Actions() {
				if deleteAction, ok := action.(testing.DeleteAction); ok && deleteAction.GetResource().Resource == "namespaces" {
					deleted = deleteAction
				}
			}
			Expect(deleted).ShouldNot(BeNil())
			Expect(deleted.GetDeleteOptions().Preconditions).ShouldNot(BeNil())
			Expect(string(*deleted.GetDeleteOptions().Preconditions.UID)).Should(Equal("uid-1"))
			Expect(*deleted.GetDeleteOptions().Preconditions.ResourceVersion).Should(Equal("7"))
		})
	})

	When("the identity changed after approval", func() {
		DescribeTable("should refuse and send no delete",
			func(name string, mutate func(*unstructured.Unstructured)) {
				object := namespace(name, "run-1", false)
				object.SetUID("uid-1")
				object.SetResourceVersion("7")
				client := cleanupClient(object)
				service := NamespaceService{Dynamic: client}
				snapshot, err := service.Inspect(context.Background(), name)
				Expect(err).ShouldNot(HaveOccurred())

				object, err = client.Resource(NamespaceGVR).Get(context.Background(), name, metav1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())
				mutate(object)
				_, err = client.Resource(NamespaceGVR).Update(context.Background(), object, metav1.UpdateOptions{})
				Expect(err).ShouldNot(HaveOccurred())

				Expect(service.DeleteApproved(context.Background(), snapshot)).Should(MatchError(ContainSubstring("changed after approval")))
				for _, action := range client.Actions() {
					if deleteAction, ok := action.(testing.DeleteAction); ok && deleteAction.GetResource().Resource == "namespaces" {
						Fail("delete sent for drifted namespace")
					}
				}
				_, getErr := client.Resource(NamespaceGVR).Get(context.Background(), name, metav1.GetOptions{})
				Expect(getErr).ShouldNot(HaveOccurred())
			},
			Entry("when the resource version changed", "tenant-pre", func(object *unstructured.Unstructured) {
				object.SetResourceVersion("8")
			}),
			Entry("when the UID changed", "tenant-pre-uid", func(object *unstructured.Unstructured) {
				object.SetUID("uid-2")
			}),
		)
	})
})

type staticNameLister struct {
	names []string
}

// List returns a copy of the configured namespace names.
func (l staticNameLister) List(context.Context) ([]string, error) {
	return append([]string(nil), l.names...), nil
}

// namespace builds a test Namespace with the requested run ID and optional ownership label.
func namespace(name, runID string, owned bool) *unstructured.Unstructured {
	labels := map[string]any{RunIDLabel: runID}
	if owned {
		labels[ManagedByLabel] = ManagedByValue
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": name, "labels": labels}}}
}

// cleanupClient creates a dynamic fake with list kinds for cleanup resource types.
func cleanupClient(objects ...runtime.Object) *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		NamespaceGVR: "NamespaceList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "applications"}:      "ApplicationList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "components"}:        "ComponentList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "imagerepositories"}: "ImageRepositoryList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "releases"}:          "ReleaseList",
		{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}:                      "PipelineRunList",
	}, objects...)
}

var _ = Describe("WaitDeleted timeout", func() {
	It("names the namespace when the deletion wait times out", func() {
		service := NamespaceService{Dynamic: cleanupClient(namespace("tenant", "run-1", true)), PollInterval: time.Millisecond}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		err := service.WaitDeleted(ctx, "tenant")

		Expect(err).Should(MatchError(ContainSubstring("wait for namespace tenant deletion")))
		Expect(err.Error()).Should(ContainSubstring("deadline exceeded"))
	})
})

var _ = Describe("DeletionBlocker", func() {
	It("summarizes remaining resources and finalizers", func() {
		object := namespace("tenant", "run-1", true)
		Expect(unstructured.SetNestedSlice(object.Object, []any{
			map[string]any{"type": "NamespaceContentRemaining", "status": "True", "message": "Some resources are remaining: pipelineruns.tekton.dev has 3 resource instances"},
			map[string]any{"type": "NamespaceFinalizersRemaining", "status": "True", "message": "Some content in the namespace has finalizers remaining: chains.tekton.dev/pipelinerun in 3 resource instances"},
			map[string]any{"type": "NamespaceContentRemaining", "status": "False", "message": "already resolved"},
			map[string]any{"type": "NamespaceDeletionDiscoveryFailure", "status": "False", "message": "ignored"},
		}, "status", "conditions")).Should(Succeed())
		service := NamespaceService{Dynamic: cleanupClient(object)}

		blocker := service.DeletionBlocker(context.Background(), "tenant")

		Expect(blocker).Should(ContainSubstring("pipelineruns.tekton.dev has 3 resource instances"))
		Expect(blocker).Should(ContainSubstring("chains.tekton.dev/pipelinerun in 3 resource instances"))
		Expect(blocker).ShouldNot(ContainSubstring("already resolved"))
		Expect(blocker).ShouldNot(ContainSubstring("ignored"))
	})

	It("returns empty for a missing namespace", func() {
		Expect((NamespaceService{Dynamic: cleanupClient()}).DeletionBlocker(context.Background(), "absent")).Should(BeEmpty())
	})
})

var _ = Describe("WaitDeleted finalizer re-strip", func() {
	It("removes finalizers re-added after the initial strip", func() {
		pipelineRun := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "tekton.dev/v1", "kind": "PipelineRun",
			"metadata": map[string]any{"name": "run-1", "namespace": "tenant", "finalizers": []any{"chains.tekton.dev/pipelinerun"}},
		}}
		client := cleanupClient(namespace("tenant", "run-1", true), pipelineRun)
		previous := finalizerStripInterval
		finalizerStripInterval = 10 * time.Millisecond
		DeferCleanup(func() { finalizerStripInterval = previous })
		service := NamespaceService{Dynamic: client, PollInterval: time.Millisecond}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		_ = service.WaitDeleted(ctx, "tenant")

		got, err := client.Resource(finalizerGVRs[4]).Namespace("tenant").Get(context.Background(), "run-1", metav1.GetOptions{})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(got.GetFinalizers()).Should(BeEmpty())
	})
})
