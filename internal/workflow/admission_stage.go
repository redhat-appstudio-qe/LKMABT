package workflow

import (
	"context"

	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// admissionDiagnostic returns a cached Kueue quota-message resolver for one
// build wait. Kueue is diagnostic only: an absent API, forbidden list, or
// missing Workload yields an empty reason, and the caller falls back to the
// PipelineRun condition. Caching keeps a minute heartbeat from listing
// Workloads for every pending component on every tick.
func (s *LiveStage) admissionDiagnostic(ctx context.Context, namespace string) func(*unstructured.Unstructured) string {
	if s.Clients == nil || s.Clients.Dynamic == nil {
		return nil
	}
	inspector := cluster.AdmissionInspector{Dynamic: s.Clients.Dynamic}
	cache := make(map[string]string)
	return func(pipelineRun *unstructured.Unstructured) string {
		if pipelineRun == nil {
			return ""
		}
		key := pipelineRun.GetNamespace() + "/" + pipelineRun.GetName() + "/" + string(pipelineRun.GetUID())
		if reason, ok := cache[key]; ok {
			return reason
		}
		reason, err := inspector.QuotaMessage(ctx, namespace, pipelineRun)
		if err != nil {
			// Do not cache a transient or forbidden read: a later poll may
			// recover the message. The PipelineRun condition remains the
			// fallback while Kueue is unreadable.
			return ""
		}
		if reason != "" {
			// An empty result can mean the Workload or its QuotaReserved
			// condition has not appeared yet; do not pin the fallback forever.
			cache[key] = reason
		}
		return reason
	}
}
