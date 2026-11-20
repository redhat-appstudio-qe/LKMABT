package workflow

import (
	"fmt"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/model"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// admissionPendingThreshold bounds how long a PipelineRun may wait for
// scheduler admission before the cluster attempt is deferred. Tests shorten it.
var admissionPendingThreshold = 10 * time.Minute

// pipelineRunPendingStatus is the Tekton hold value Kueue uses while a
// PipelineRun waits for quota.
const pipelineRunPendingStatus = "PipelineRunPending"

// AdmissionPendingError reports PipelineRuns held in admission past the defer
// threshold. It unwraps to ErrDeferred, so callers can distinguish a scheduler
// wait from a failed build.
type AdmissionPendingError struct {
	Namespace string
	Pending   []AdmissionPending
}

// AdmissionPending describes one PipelineRun held in admission.
type AdmissionPending struct {
	Component   string
	PipelineRun string
	Pending     time.Duration
	// Reason is the Kueue quota message when available, otherwise the
	// PipelineRun condition message.
	Reason string
}

// Error names every held PipelineRun, its age, and the scheduler reason.
func (e *AdmissionPendingError) Error() string {
	if e == nil {
		return ErrDeferred.Error()
	}
	parts := make([]string, 0, len(e.Pending))
	for _, pending := range e.Pending {
		description := fmt.Sprintf("%s (PipelineRun %s pending %s", pending.Component, pending.PipelineRun, pending.Pending.Round(time.Second))
		if pending.Reason != "" {
			description += "; " + pending.Reason
		}
		parts = append(parts, description+")")
	}
	if len(parts) == 0 {
		return "PipelineRun admission pending"
	}
	return "PipelineRun admission pending: " + strings.Join(parts, ", ")
}

// Unwrap marks the error as a deferral.
func (e *AdmissionPendingError) Unwrap() error {
	return ErrDeferred
}

// admissionTracker remembers when each component's PipelineRun was first seen
// admission-pending and turns an over-threshold wait into a deferral.
type admissionTracker struct {
	threshold time.Duration
	since     map[string]time.Time
	// diagnose returns a scheduler reason such as the Kueue QuotaReserved
	// message. A nil or failing diagnostic falls back to the PipelineRun status.
	diagnose func(*unstructured.Unstructured) string
}

// newAdmissionTracker returns a tracker using threshold, or the package default
// when threshold is non-positive.
func newAdmissionTracker(threshold time.Duration, diagnose func(*unstructured.Unstructured) string) *admissionTracker {
	if threshold <= 0 {
		threshold = admissionPendingThreshold
	}
	return &admissionTracker{threshold: threshold, since: make(map[string]time.Time), diagnose: diagnose}
}

// deferred reports an error when at least one uncaptured component's matched
// PipelineRun has been admission-pending past the threshold. Components that
// stopped being pending are forgotten.
func (t *admissionTracker) deferred(namespace string, now time.Time, commits []model.TriggerCommit, pipelineRuns, taskRuns []unstructured.Unstructured, captured func(string) bool) *AdmissionPendingError {
	pendingRuns := make([]AdmissionPending, 0, len(commits))
	for _, commit := range commits {
		if captured != nil && captured(commit.Component) {
			delete(t.since, commit.Component)
			continue
		}
		candidates := matchingPipelineRuns(pipelineRuns, commit)
		if len(candidates) != 1 || !admissionPending(candidates[0], taskRuns) {
			delete(t.since, commit.Component)
			continue
		}
		candidate := candidates[0]
		since := candidate.GetCreationTimestamp().Time
		if since.IsZero() {
			observed, ok := t.since[commit.Component]
			if !ok {
				observed = now
				t.since[commit.Component] = observed
			}
			since = observed
		} else {
			t.since[commit.Component] = since
		}
		elapsed := now.Sub(since)
		if elapsed <= t.threshold {
			continue
		}
		reason := admissionConditionReason(candidate)
		if t.diagnose != nil {
			if diagnostic := t.diagnose(candidate); diagnostic != "" {
				reason = diagnostic
			}
		}
		pendingRuns = append(pendingRuns, AdmissionPending{
			Component:   commit.Component,
			PipelineRun: candidate.GetName(),
			Pending:     elapsed,
			Reason:      reason,
		})
	}
	if len(pendingRuns) == 0 {
		return nil
	}
	return &AdmissionPendingError{Namespace: namespace, Pending: pendingRuns}
}

// admissionPending reports whether a PipelineRun is held by admission: Tekton
// marks it PipelineRunPending or reports that reason, it has not started, and it
// has no child TaskRuns.
func admissionPending(object *unstructured.Unstructured, taskRuns []unstructured.Unstructured) bool {
	if object == nil {
		return false
	}
	specStatus, _, _ := unstructured.NestedString(object.Object, "spec", "status")
	_, reason, _ := pipelineRunSucceededCondition(object)
	if specStatus != pipelineRunPendingStatus && reason != pipelineRunPendingStatus {
		return false
	}
	if startTime, found, _ := unstructured.NestedString(object.Object, "status", "startTime"); found && strings.TrimSpace(startTime) != "" {
		return false
	}
	return len(taskRunsForPipelineRun(taskRuns, object)) == 0
}

// admissionStatusDetail renders an admission-pending component for a build
// heartbeat, including the scheduler reason when one is available.
func admissionStatusDetail(object *unstructured.Unstructured, taskRuns []unstructured.Unstructured, now time.Time, diagnose func(*unstructured.Unstructured) string) string {
	if !admissionPending(object, taskRuns) {
		return ""
	}
	detail := fmt.Sprintf("PipelineRunPending for %s (PipelineRun %s", buildAge(object, now), object.GetName())
	reason := admissionConditionReason(object)
	if diagnose != nil {
		if diagnostic := diagnose(object); diagnostic != "" {
			reason = diagnostic
		}
	}
	if reason != "" {
		detail += "; " + reason
	}
	return detail + ")"
}

// admissionConditionReason returns the PipelineRun's scheduler explanation,
// ignoring the bare PipelineRunPending reason because it adds no information.
func admissionConditionReason(object *unstructured.Unstructured) string {
	_, reason, message := pipelineRunSucceededCondition(object)
	if message != "" {
		return message
	}
	if reason != "" && reason != pipelineRunPendingStatus {
		return reason
	}
	specStatus, _, _ := unstructured.NestedString(object.Object, "spec", "status")
	if specStatus != "" && specStatus != pipelineRunPendingStatus {
		return specStatus
	}
	return ""
}

// pipelineRunSucceededCondition returns the Succeeded condition's status,
// reason, and message, or empty values when the condition is absent.
func pipelineRunSucceededCondition(object *unstructured.Unstructured) (status, reason, message string) {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok || condition["type"] != "Succeeded" {
			continue
		}
		status, _ = condition["status"].(string)
		reason, _ = condition["reason"].(string)
		message, _ = condition["message"].(string)
		return status, reason, message
	}
	return "", "", ""
}
