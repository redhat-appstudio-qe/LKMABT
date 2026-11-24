package cluster

import (
	"context"
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

// EgressProbeImage is the frontend fixture's base image. The probe reuses it so
// a pull failure is the same condition the frontend build would hit.
const EgressProbeImage = "docker.io/library/node:20-alpine"

// DefaultEgressProbeTimeout bounds probe pod startup, the outbound request, and
// polling. A private cluster commonly fails at the image pull, which is far
// cheaper than the build it would otherwise start.
const DefaultEgressProbeTimeout = 90 * time.Second

// egressProbePollInterval bounds how often the probe pod phase is read.
const egressProbePollInterval = 2 * time.Second

// egressProbeCleanupTimeout bounds the best-effort probe pod deletion.
const egressProbeCleanupTimeout = 15 * time.Second

// egressProbeRequestTimeout bounds the in-pod request so a blackholed route
// fails instead of holding the pod open.
const egressProbeRequestTimeout = 10 * time.Second

// egressProbeContainer names the probe container whose log carries the failure
// reason.
const egressProbeContainer = "probe"

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
// own runtime performs the request the way the fixture build would.
var egressProbeScript = fmt.Sprintf(`fetch(process.argv[1], {signal: AbortSignal.timeout(%d)}).then((response) => process.exit(response.ok ? 0 : 1)).catch((error) => { console.error(error.message); process.exit(1) })`, egressProbeRequestTimeout.Milliseconds())

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
	// Timeout overrides the probe deadline; zero uses DefaultEgressProbeTimeout.
	Timeout time.Duration
	// PollInterval overrides the phase check interval; zero uses
	// egressProbePollInterval.
	PollInterval time.Duration
}

// Probe creates a probe pod in namespace, waits for its outcome, and returns
// whether target was reachable. An unreachable target, a pod that never starts,
// and a pod that times out all return Reachable false with a detail instead of
// an error; errors are reserved for API failures that leave the answer unknown.
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
	pods := p.Kubernetes.CoreV1().Pods(namespace)
	pod, err := pods.Create(ctx, p.pod(namespace, image, target, timeout), metav1.CreateOptions{})
	if err != nil {
		return EgressResult{}, fmt.Errorf("create egress probe pod in namespace %s: %w", namespace, err)
	}
	defer p.deletePod(ctx, pods, pod.Name)
	return p.wait(ctx, pods, pod.Name, timeout, interval)
}

// pod builds the probe pod. It requests no service account token and leaves
// runAsUser to the namespace's SCC: the frontend's image has no non-root USER,
// so only the SCC can supply the UID range. An active deadline stops a stuck
// pod.
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

// wait polls the probe pod until it succeeds, fails, the probe deadline
// expires, or the caller's context ends. A started pod that has not finished
// by the deadline reports why instead of an error.
func (p EgressProbe) wait(ctx context.Context, pods typedcorev1.PodInterface, name string, timeout, interval time.Duration) (EgressResult, error) {
	deadline := time.Now().Add(timeout)
	var (
		lastPhase     corev1.PodPhase
		waiting       string
		unschedulable string
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
				return EgressResult{Detail: p.failureDetail(ctx, pods, pod)}, nil
			}
			if detail, terminal := terminalWaitingDetail(pod); terminal {
				return EgressResult{Detail: "probe pod cannot start: " + detail}, nil
			}
			if message := unschedulableDetail(pod); message != "" {
				unschedulable = message
			}
			if reason := containerWaitingReason(pod); reason != "" {
				waiting = reason
			}
		case !apierrors.IsNotFound(err):
			return EgressResult{}, fmt.Errorf("read egress probe pod %s: %w", name, err)
		}
		if time.Now().After(deadline) {
			return EgressResult{Detail: unfinishedDetail(lastPhase, waiting, unschedulable, timeout)}, nil
		}
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return EgressResult{Detail: unfinishedDetail(lastPhase, waiting, unschedulable, timeout)}, nil
			}
			return EgressResult{}, ctx.Err()
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
	if reason := strings.TrimSpace(pod.Status.Reason); reason != "" {
		return reason
	}
	return strings.TrimSpace(pod.Status.Message)
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

// unfinishedDetail explains a probe that did not reach a terminal phase within
// the deadline. A scheduler message is the most specific cause, then a
// container waiting reason.
func unfinishedDetail(phase corev1.PodPhase, waiting, unschedulable string, timeout time.Duration) string {
	switch {
	case unschedulable != "":
		return "probe pod cannot start: " + unschedulable
	case waiting != "":
		return "probe pod cannot start: " + waiting
	case phase == corev1.PodRunning:
		return fmt.Sprintf("probe did not finish within %s", timeout)
	default:
		return fmt.Sprintf("probe pod did not start within %s", timeout)
	}
}

// deletePod removes the probe pod with no grace period. The namespace is
// deleted at run end, so a failed deletion does not fail the probe.
func (p EgressProbe) deletePod(ctx context.Context, pods typedcorev1.PodInterface, name string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), egressProbeCleanupTimeout)
	defer cancel()
	_ = pods.Delete(cleanupCtx, name, metav1.DeleteOptions{GracePeriodSeconds: new(int64(0))})
}

// truncateDetail shortens a one-line probe detail to limit bytes.
func truncateDetail(detail string, limit int) string {
	if len(detail) <= limit {
		return detail
	}
	return detail[:limit]
}
