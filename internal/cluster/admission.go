package cluster

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// KueueWorkloadGVR identifies the Kueue Workload that holds a PipelineRun in
// admission. Kueue is diagnostic: an absent API, forbidden list, or missing
// Workload falls back to the PipelineRun's own condition message.
var KueueWorkloadGVR = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "workloads"}

// KueueWorkloadV1Beta1GVR is the older Kueue Workload API. QuotaMessage falls
// back to it when v1beta2 is not served.
var KueueWorkloadV1Beta1GVR = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta1", Resource: "workloads"}

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
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		// Kueue pre-0.15 serves only v1beta1; the owner and QuotaReserved
		// fields are unchanged.
		workloads, err = i.Dynamic.Resource(KueueWorkloadV1Beta1GVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return "", fmt.Errorf("list Kueue Workloads in namespace %s (v1beta2 and v1beta1): %w", namespace, err)
		}
	} else if err != nil {
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
		// A non-empty owner UID identifies the exact object. Do not fall back
		// to the name when that UID belongs to a replaced PipelineRun.
		if pipelineRun.GetUID() != "" && owner.UID != "" {
			if owner.UID == pipelineRun.GetUID() {
				return true
			}
			continue
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
