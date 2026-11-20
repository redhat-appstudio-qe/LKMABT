package cluster

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// KueueWorkloadGVR identifies the Kueue Workload that holds a PipelineRun in
// admission. Kueue is diagnostic: an absent API, forbidden list, or missing
// Workload falls back to the PipelineRun's own condition message.
var KueueWorkloadGVR = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "workloads"}

// AdmissionInspector reads the Kueue Workload owned by a pending PipelineRun.
type AdmissionInspector struct {
	Dynamic dynamic.Interface
}

// QuotaMessage returns the QuotaReserved=False message for the Workload owned by
// pipelineRun in namespace. It returns an empty message when Kueue reports
// reserved quota or no matching Workload exists; callers fall back to the
// PipelineRun condition. A list error is returned for diagnostics only.
func (i AdmissionInspector) QuotaMessage(ctx context.Context, namespace string, pipelineRun *unstructured.Unstructured) (string, error) {
	if i.Dynamic == nil {
		return "", fmt.Errorf("dynamic client is required to read Kueue Workloads")
	}
	if pipelineRun == nil {
		return "", nil
	}
	workloads, err := i.Dynamic.Resource(KueueWorkloadGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list Kueue Workloads in namespace %s: %w", namespace, err)
	}
	for index := range workloads.Items {
		workload := &workloads.Items[index]
		if !workloadOwnedByPipelineRun(workload, pipelineRun) {
			continue
		}
		if message := quotaReservedMessage(workload); message != "" {
			return message, nil
		}
	}
	return "", nil
}

// workloadOwnedByPipelineRun reports whether the Workload's owner references
// name the PipelineRun. A UID match is strongest; a name match tolerates
// clients that do not populate UIDs.
func workloadOwnedByPipelineRun(workload, pipelineRun *unstructured.Unstructured) bool {
	for _, owner := range workload.GetOwnerReferences() {
		if owner.Kind != "PipelineRun" {
			continue
		}
		if pipelineRun.GetUID() != "" && owner.UID == pipelineRun.GetUID() {
			return true
		}
		if owner.Name != "" && owner.Name == pipelineRun.GetName() {
			return true
		}
	}
	return false
}

// quotaReservedMessage returns the message from a QuotaReserved=False
// condition, or empty when quota is reserved or the condition is absent.
func quotaReservedMessage(workload *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(workload.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok || condition["type"] != "QuotaReserved" {
			continue
		}
		status, _ := condition["status"].(string)
		if status != "False" {
			return ""
		}
		message, _ := condition["message"].(string)
		return strings.TrimSpace(message)
	}
	return ""
}
