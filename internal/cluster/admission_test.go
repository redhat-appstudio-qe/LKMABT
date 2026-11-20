package cluster

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

var _ = Describe("AdmissionInspector", func() {
	When("a Workload owned by the PipelineRun is waiting for quota", func() {
		It("returns the QuotaReserved message matched by UID", func() {
			pipelineRun := pendingPipelineRun("build-abc", "uid-abc")
			workload := kueueWorkload("build-abc-workload", "build-abc", "uid-abc", "False", "insufficient quota for flavor default")
			inspector := AdmissionInspector{Dynamic: kueueClients(workload)}

			message, err := inspector.QuotaMessage(context.Background(), "tenant", pipelineRun)

			Expect(err).ShouldNot(HaveOccurred())
			Expect(message).Should(Equal("insufficient quota for flavor default"))
		})

		It("returns the QuotaReserved message matched by name when the owner UID is absent", func() {
			pipelineRun := pendingPipelineRun("build-abc", "uid-abc")
			workload := kueueWorkload("build-abc-workload", "build-abc", "", "False", "cluster queue is full")
			inspector := AdmissionInspector{Dynamic: kueueClients(workload)}

			message, err := inspector.QuotaMessage(context.Background(), "tenant", pipelineRun)

			Expect(err).ShouldNot(HaveOccurred())
			Expect(message).Should(Equal("cluster queue is full"))
		})
	})

	When("Kueue is not reporting a quota problem", func() {
		It("returns an empty message when quota is reserved", func() {
			pipelineRun := pendingPipelineRun("build-abc", "uid-abc")
			workload := kueueWorkload("build-abc-workload", "build-abc", "uid-abc", "True", "")
			inspector := AdmissionInspector{Dynamic: kueueClients(workload)}

			message, err := inspector.QuotaMessage(context.Background(), "tenant", pipelineRun)

			Expect(err).ShouldNot(HaveOccurred())
			Expect(message).Should(BeEmpty())
		})

		It("returns an empty message when the QuotaReserved condition is absent", func() {
			pipelineRun := pendingPipelineRun("build-abc", "uid-abc")
			workload := kueueWorkload("build-abc-workload", "build-abc", "uid-abc", "", "")
			inspector := AdmissionInspector{Dynamic: kueueClients(workload)}

			message, err := inspector.QuotaMessage(context.Background(), "tenant", pipelineRun)

			Expect(err).ShouldNot(HaveOccurred())
			Expect(message).Should(BeEmpty())
		})

		It("returns an empty message when no Workload is owned by the PipelineRun", func() {
			pipelineRun := pendingPipelineRun("build-abc", "uid-abc")
			workload := kueueWorkload("other-workload", "build-xyz", "uid-xyz", "False", "insufficient quota")
			inspector := AdmissionInspector{Dynamic: kueueClients(workload)}

			message, err := inspector.QuotaMessage(context.Background(), "tenant", pipelineRun)

			Expect(err).ShouldNot(HaveOccurred())
			Expect(message).Should(BeEmpty())
		})
	})

	When("the Workload list cannot be read", func() {
		It("returns the list error for the caller to fall back on", func() {
			clients := kueueClients()
			clients.PrependReactor("list", "workloads", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("connection reset")
			})
			inspector := AdmissionInspector{Dynamic: clients}

			_, err := inspector.QuotaMessage(context.Background(), "tenant", pendingPipelineRun("build-abc", "uid-abc"))

			Expect(err).Should(MatchError(ContainSubstring("list Kueue Workloads in namespace tenant")))
			Expect(err.Error()).Should(ContainSubstring("connection reset"))
		})

		It("returns a forbidden error so the caller can fall back", func() {
			clients := kueueClients()
			clients.PrependReactor("list", "workloads", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(KueueWorkloadGVR.GroupResource(), "", errors.New("access denied"))
			})
			inspector := AdmissionInspector{Dynamic: clients}

			_, err := inspector.QuotaMessage(context.Background(), "tenant", pendingPipelineRun("build-abc", "uid-abc"))

			Expect(err).Should(HaveOccurred())
			Expect(apierrors.IsForbidden(err)).Should(BeTrue())
		})
	})
})

// pendingPipelineRun returns the PipelineRun identity Kueue links against.
func pendingPipelineRun(name, uid string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tekton.dev/v1",
		"kind":       "PipelineRun",
		"metadata":   map[string]any{"name": name, "namespace": "tenant", "uid": uid},
	}}
}

// kueueWorkload returns a Workload owned by a PipelineRun with the supplied
// QuotaReserved condition status; an empty status omits the condition.
func kueueWorkload(name, ownerName, ownerUID, quotaStatus, message string) *unstructured.Unstructured {
	owner := map[string]any{"apiVersion": "tekton.dev/v1", "kind": "PipelineRun", "name": ownerName}
	if ownerUID != "" {
		owner["uid"] = ownerUID
	}
	workload := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": KueueWorkloadGVR.Group + "/" + KueueWorkloadGVR.Version,
		"kind":       "Workload",
		"metadata": map[string]any{
			"name":            name,
			"namespace":       "tenant",
			"ownerReferences": []any{owner},
		},
	}}
	if quotaStatus != "" {
		condition := map[string]any{"type": "QuotaReserved", "status": quotaStatus}
		if message != "" {
			condition["message"] = message
		}
		workload.Object["status"] = map[string]any{"conditions": []any{condition}}
	}
	return workload
}

// kueueClients returns a dynamic client whose Workload list kind is registered.
func kueueClients(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{KueueWorkloadGVR: "WorkloadList"},
		objects...,
	)
}
