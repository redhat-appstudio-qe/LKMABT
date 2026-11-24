package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// EgressProbeImage is the probe container image. It provides the Node runtime
// that performs the request; because the same image is not a build dependency
// of every target, its pull failure is an unknown outcome, never an unreachable
// endpoint.
const EgressProbeImage = "docker.io/library/node:20-alpine"

// DefaultEgressProbeTimeout bounds probe pod startup, the outbound request, and
// polling. A private cluster commonly fails at the image pull, which is far
// cheaper than the build it would otherwise start.
const DefaultEgressProbeTimeout = 90 * time.Second

// egressProbePollInterval bounds how often the probe pod phase is read.
const egressProbePollInterval = 2 * time.Second

// egressProbeCleanupTimeout bounds the best-effort probe pod deletion.
const egressProbeCleanupTimeout = 15 * time.Second

// egressProbeDeleteAttempts bounds how often a failed probe pod deletion is
// retried inside the cleanup window.
const egressProbeDeleteAttempts = 3

// egressProbeDeleteBackoff is the first retry delay; later attempts add one
// more delay each.
const egressProbeDeleteBackoff = 500 * time.Millisecond

// egressProbeRequestTimeout bounds the in-pod request so a blackholed route
// fails instead of holding the pod open.
const egressProbeRequestTimeout = 10 * time.Second

// egressProbeContainer names the probe container whose log carries the failure
// reason.
const egressProbeContainer = "probe"

// egressProbeFailureExit is the probe container's exit code for a failed
// request. Kubernetes can also terminate the container after it starts
// (eviction, OOM, deadline), so only this code proves the request ran and
// failed; any other exit leaves the endpoint's reachability unknown.
const egressProbeFailureExit = 42

// TenantWorkloadNodeLabel and TenantWorkloadNodeValue select the nodes that run
// Konflux tenant builds. The probe must run there: on a private cluster the
// tenant nodes have no internet egress while general nodes do, so a probe on a
// general node would report a reachability the build pod does not have.
const (
	TenantWorkloadNodeLabel = "konflux-ci.dev/workload"
	TenantWorkloadNodeValue = "konflux-tenants"
)

// egressProbeScript requests target and exits non-zero on any failure. The
// target arrives as argv[1] so no shell quoting is involved, and the image's
// own runtime performs the request the way the fixture build would. Failures
// print the HTTP status or the underlying cause (DNS, TLS, reset) so the alert
// distinguishes them.
var egressProbeScript = fmt.Sprintf(`fetch(process.argv[1], {signal: AbortSignal.timeout(%d)}).then((response) => { if (!response.ok) { console.error('HTTP ' + response.status); process.exit(%d) } process.exit(0) }).catch((error) => { console.error([error.message, error.cause && (error.cause.code || error.cause.message)].filter(Boolean).join(': ')); process.exit(%d) })`, egressProbeRequestTimeout.Milliseconds(), egressProbeFailureExit, egressProbeFailureExit)

// EgressResult reports whether a cluster pod reached the probe target. Detail
// explains an unreachable target in one short line; it is empty on success.
type EgressResult struct {
	Reachable bool   `json:"reachable"`
	Detail    string `json:"detail,omitempty"`
}

// EgressProbe runs one short-lived pod in a namespace and reports whether it
// reached an external URL over the cluster's internet egress. Kubernetes is
// required. The pod is deleted after every attempt, including failures.
type EgressProbe struct {
	Kubernetes kubernetes.Interface
	// Image overrides the probe container image; empty uses EgressProbeImage.
	Image string
	// Timeout overrides the total probe deadline, including pod creation;
	// zero uses DefaultEgressProbeTimeout.
	Timeout time.Duration
	// PollInterval overrides the phase check interval; zero uses
	// egressProbePollInterval.
	PollInterval time.Duration
}

// Probe creates a probe pod in namespace, waits for its outcome, and returns
// whether target was reachable. A pod that ran and failed is an unreachable
// target with a detail. A pod that could not run, disappeared, or exhausted the
// probe deadline without proving a network failure is an error: the harness
// must not mistake its own environment for a registry outage. Errors are also
// returned for API failures and for a probe pod that cannot be deleted.
func (p EgressProbe) Probe(ctx context.Context, namespace, target string) (EgressResult, error) {
	if p.Kubernetes == nil {
		return EgressResult{}, fmt.Errorf("kubernetes client is required for the egress probe")
	}
	if strings.TrimSpace(namespace) == "" {
		return EgressResult{}, fmt.Errorf("namespace is required for the egress probe")
	}
	if strings.TrimSpace(target) == "" {
		return EgressResult{}, fmt.Errorf("target is required for the egress probe")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultEgressProbeTimeout
	}
	interval := p.PollInterval
	if interval <= 0 {
		interval = egressProbePollInterval
	}
	image := p.Image
	if image == "" {
		image = EgressProbeImage
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	pods := p.Kubernetes.CoreV1().Pods(namespace)
	pod, err := pods.Create(probeCtx, p.pod(namespace, image, target, timeout), metav1.CreateOptions{})
	if err != nil {
		return EgressResult{}, fmt.Errorf("create egress probe pod in namespace %s: %w", namespace, err)
	}
	result, waitErr := p.wait(probeCtx, ctx, pods, pod.Name, timeout, interval)
	if deleteErr := p.deletePod(ctx, pods, pod.Name); deleteErr != nil {
		waitErr = errors.Join(waitErr, deleteErr)
	}
	return result, waitErr
}

// pod builds the probe pod. It requests no service account token and leaves
// runAsUser to the namespace's SCC: the frontend's image has no non-root USER,
// so only the SCC can supply the UID range. An active deadline stops a stuck
// pod, and the toleration lets it land on the tainted tenant nodes.
func (p EgressProbe) pod(namespace, image, target string, timeout time.Duration) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "lkmabt-egress-probe-",
			Namespace:    namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "lkmabt",
				"app.kubernetes.io/name":       "lkmabt-egress-probe",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  new(false),
			TerminationGracePeriodSeconds: new(int64(0)),
			ActiveDeadlineSeconds:         new(int64(timeout.Seconds())),
			NodeSelector:                  map[string]string{TenantWorkloadNodeLabel: TenantWorkloadNodeValue},
			Tolerations: []corev1.Toleration{{
				Key:      TenantWorkloadNodeLabel,
				Operator: corev1.TolerationOpEqual,
				Value:    TenantWorkloadNodeValue,
				Effect:   corev1.TaintEffectNoSchedule,
			}},
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    egressProbeContainer,
				Image:   image,
				Command: []string{"node", "-e", egressProbeScript, target},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: new(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
			}},
		},
	}
}

// wait polls the probe pod until it succeeds, fails, the probe or caller
// deadline expires, or the pod disappears. A pod that ran and failed reports an
// unreachable target; an unknown outcome returns an error. The probe context
// bounds every read and poll; the parent context distinguishes a caller
// deadline, which must fail the stage, from the probe's own budget.
func (p EgressProbe) wait(ctx, parent context.Context, pods typedcorev1.PodInterface, name string, timeout, interval time.Duration) (EgressResult, error) {
	var (
		lastPhase corev1.PodPhase
		waiting   string
	)
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		pod, err := pods.Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			lastPhase = pod.Status.Phase
			switch pod.Status.Phase {
			case corev1.PodSucceeded:
				return EgressResult{Reachable: true}, nil
			case corev1.PodFailed:
				detail := p.failureDetail(ctx, pods, pod)
				if !probeReportedFailure(pod) {
					return EgressResult{}, fmt.Errorf("probe pod failed without a request result: %s; cluster internet access is unknown", detail)
				}
				return EgressResult{Detail: detail}, nil
			}
			if detail, terminal := terminalWaitingDetail(pod); terminal {
				return EgressResult{}, fmt.Errorf("probe pod cannot start: %s", detail)
			}
			if message := unschedulableDetail(pod); message != "" {
				return EgressResult{}, fmt.Errorf("probe pod cannot be scheduled: %s", message)
			}
			if reason := containerWaitingReason(pod); reason != "" {
				waiting = reason
			}
		case apierrors.IsNotFound(err):
			return EgressResult{}, fmt.Errorf("egress probe pod %s disappeared before its outcome was known", name)
		case parent.Err() != nil:
			return EgressResult{}, parent.Err()
		case ctx.Err() != nil:
			return unfinished(lastPhase, waiting, timeout)
		default:
			return EgressResult{}, fmt.Errorf("read egress probe pod %s: %w", name, err)
		}
		select {
		case <-parent.Done():
			return EgressResult{}, parent.Err()
		case <-ctx.Done():
			if parent.Err() != nil {
				return EgressResult{}, parent.Err()
			}
			return unfinished(lastPhase, waiting, timeout)
		case <-timer.C:
			timer.Reset(interval)
		}
	}
}

// failureDetail prefers the probe container's own error line, then its
// termination reason, then the pod reason.
func (p EgressProbe) failureDetail(ctx context.Context, pods typedcorev1.PodInterface, pod *corev1.Pod) string {
	if line := p.lastLogLine(ctx, pods, pod.Name); line != "" {
		return line
	}
	for _, status := range pod.Status.ContainerStatuses {
		if terminated := status.State.Terminated; terminated != nil {
			if message := strings.TrimSpace(terminated.Message); message != "" {
				return message
			}
			if terminated.Reason != "" {
				return terminated.Reason
			}
		}
	}
	if reason := containerWaitingReason(pod); reason != "" {
		return reason
	}
	if reason := strings.TrimSpace(pod.Status.Reason); reason != "" {
		return reason
	}
	return strings.TrimSpace(pod.Status.Message)
}

// probeReportedFailure reports whether the probe container exited with the
// script's failure code. Eviction, OOM, and deadline termination also produce a
// terminated container, so the exit code is the only proof that the request
// actually failed.
func probeReportedFailure(pod *corev1.Pod) bool {
	for _, status := range pod.Status.ContainerStatuses {
		if terminated := status.State.Terminated; terminated != nil {
			return terminated.ExitCode == egressProbeFailureExit
		}
	}
	return false
}

// lastLogLine returns the probe container's final non-empty log line, bounded
// so a stack trace cannot flood the alert. A denied or pruned log is not an
// error: the caller falls back to the pod status.
func (p EgressProbe) lastLogLine(ctx context.Context, pods typedcorev1.PodInterface, name string) string {
	raw, err := pods.GetLogs(name, &corev1.PodLogOptions{Container: egressProbeContainer}).Do(ctx).Raw()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if line := strings.TrimSpace(lines[index]); line != "" {
			return truncateDetail(line, 200)
		}
	}
	return ""
}

// containerWaitingReason returns the first container waiting reason, such as
// ImagePullBackOff, which explains a pod that never started.
func containerWaitingReason(pod *corev1.Pod) string {
	for _, status := range pod.Status.ContainerStatuses {
		if waiting := status.State.Waiting; waiting != nil && waiting.Reason != "" {
			return waiting.Reason
		}
	}
	return ""
}

// terminalWaitingDetail returns the message of a waiting state that cannot
// resolve without a pod change, such as a rejected security context. Waiting
// out the probe deadline would hide the actual cause behind a generic reason.
func terminalWaitingDetail(pod *corev1.Pod) (string, bool) {
	for _, status := range pod.Status.ContainerStatuses {
		waiting := status.State.Waiting
		if waiting == nil {
			continue
		}
		switch waiting.Reason {
		case "CreateContainerConfigError", "InvalidImageName":
			if message := strings.TrimSpace(waiting.Message); message != "" {
				return message, true
			}
			return waiting.Reason, true
		}
	}
	return "", false
}

// unschedulableDetail returns the scheduler's explanation when the pod cannot
// be placed on any node.
func unschedulableDetail(pod *corev1.Pod) string {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && condition.Reason == "Unschedulable" {
			return truncateDetail(strings.TrimSpace(condition.Message), 200)
		}
	}
	return ""
}

// unfinished classifies a probe that exhausted its own deadline. A probe that
// is still running reached the network and stalled, so the target is
// unreachable; a probe that never started leaves the answer unknown. The probe
// image is the frontend's, so its pull failure says nothing about a backend
// target and must not drop backend components.
func unfinished(phase corev1.PodPhase, waiting string, timeout time.Duration) (EgressResult, error) {
	if phase == corev1.PodRunning {
		return EgressResult{Detail: fmt.Sprintf("probe did not finish within %s", timeout)}, nil
	}
	if waiting != "" {
		return EgressResult{}, fmt.Errorf("probe pod cannot start: %s; cluster internet access is unknown", waiting)
	}
	return EgressResult{}, fmt.Errorf("probe pod did not finish within %s; cluster internet access is unknown", timeout)
}

// deletePod removes the probe pod with no grace period, retrying a failed
// deletion inside the cleanup window. A pod left in an adopted namespace would
// outlive the run, so the error is reported instead of discarded.
func (p EgressProbe) deletePod(ctx context.Context, pods typedcorev1.PodInterface, name string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), egressProbeCleanupTimeout)
	defer cancel()
	var deleteErr error
	for attempt := 1; attempt <= egressProbeDeleteAttempts; attempt++ {
		deleteErr = pods.Delete(cleanupCtx, name, metav1.DeleteOptions{GracePeriodSeconds: new(int64(0))})
		if deleteErr == nil || apierrors.IsNotFound(deleteErr) {
			return nil
		}
		select {
		case <-cleanupCtx.Done():
			return fmt.Errorf("delete egress probe pod %s: %w", name, deleteErr)
		case <-time.After(egressProbeDeleteBackoff * time.Duration(attempt)):
		}
	}
	return fmt.Errorf("delete egress probe pod %s: %w", name, deleteErr)
}

// truncateDetail shortens a one-line probe detail to limit bytes.
func truncateDetail(detail string, limit int) string {
	if len(detail) <= limit {
		return detail
	}
	return detail[:limit]
}
