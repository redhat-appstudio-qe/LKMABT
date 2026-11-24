package cluster

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

// probePod returns a probe pod at the given phase.
func probePod(phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "lkmabt-egress-probe-test", Namespace: "tenant"},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

// egressTestClient returns a fake clientset whose pod create names the pod and
// delegates every other pod call to reactor.
func egressTestClient(reactor kubetesting.ReactionFunc) *kubernetesfake.Clientset {
	client := kubernetesfake.NewSimpleClientset()
	client.PrependReactor("create", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
		pod := action.(kubetesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Name = "lkmabt-egress-probe-test"
		return true, pod, nil
	})
	if reactor != nil {
		client.PrependReactor("get", "pods", reactor)
	}
	return client
}

// phaseReactor returns a pod in phase on every read.
func phaseReactor(phase corev1.PodPhase) kubetesting.ReactionFunc {
	return func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, probePod(phase), nil
	}
}

var _ = Describe("EgressProbe", func() {
	target := "https://registry.npmjs.org/-/ping"

	When("the probe pod succeeds", func() {
		It("should report the target reachable and delete the pod", func() {
			client := egressTestClient(phaseReactor(corev1.PodSucceeded))
			probe := EgressProbe{Kubernetes: client, Timeout: time.Second, PollInterval: time.Millisecond}

			result, err := probe.Probe(context.Background(), "tenant", target)

			Expect(err).ShouldNot(HaveOccurred())
			Expect(result.Reachable).Should(BeTrue())
			Expect(result.Detail).Should(BeEmpty())
			deleted := false
			for _, action := range client.Actions() {
				if action.GetVerb() == "delete" && action.GetResource().Resource == "pods" {
					deleted = true
				}
			}
			Expect(deleted).Should(BeTrue())
		})
	})

	When("the probe pod fails", func() {
		It("should report the target unreachable with a failure detail", func() {
			client := egressTestClient(func(kubetesting.Action) (bool, runtime.Object, error) {
				pod := probePod(corev1.PodFailed)
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  egressProbeContainer,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", Message: "fetch failed"}},
				}}
				return true, pod, nil
			})
			probe := EgressProbe{Kubernetes: client, Timeout: time.Second, PollInterval: time.Millisecond}

			result, err := probe.Probe(context.Background(), "tenant", target)

			Expect(err).ShouldNot(HaveOccurred())
			Expect(result.Reachable).Should(BeFalse())
			// The fake clientset serves a container log, which the probe prefers
			// over the pod status; the exact text is client-go's, not ours.
			Expect(result.Detail).ShouldNot(BeEmpty())
		})
	})

	When("the probe pod never starts", func() {
		It("should report the waiting reason after the timeout", func() {
			client := egressTestClient(func(kubetesting.Action) (bool, runtime.Object, error) {
				pod := probePod(corev1.PodPending)
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  egressProbeContainer,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
				}}
				return true, pod, nil
			})
			probe := EgressProbe{Kubernetes: client, Timeout: 40 * time.Millisecond, PollInterval: 5 * time.Millisecond}

			result, err := probe.Probe(context.Background(), "tenant", target)

			Expect(err).ShouldNot(HaveOccurred())
			Expect(result.Reachable).Should(BeFalse())
			Expect(result.Detail).Should(Equal("probe pod cannot start: ImagePullBackOff"))
		})
	})

	When("the probe pod has a terminal configuration error", func() {
		It("should report the container message instead of waiting for the timeout", func() {
			client := egressTestClient(func(kubetesting.Action) (bool, runtime.Object, error) {
				pod := probePod(corev1.PodPending)
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: egressProbeContainer,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason:  "CreateContainerConfigError",
						Message: "container has runAsNonRoot and image will run as root",
					}},
				}}
				return true, pod, nil
			})
			probe := EgressProbe{Kubernetes: client, Timeout: time.Minute, PollInterval: time.Millisecond}

			result, err := probe.Probe(context.Background(), "tenant", target)

			Expect(err).ShouldNot(HaveOccurred())
			Expect(result.Reachable).Should(BeFalse())
			Expect(result.Detail).Should(Equal("probe pod cannot start: container has runAsNonRoot and image will run as root"))
		})
	})

	When("the probe pod cannot be scheduled", func() {
		It("should report the scheduler message", func() {
			client := egressTestClient(func(kubetesting.Action) (bool, runtime.Object, error) {
				pod := probePod(corev1.PodPending)
				pod.Status.Conditions = []corev1.PodCondition{{
					Type:    corev1.PodScheduled,
					Status:  corev1.ConditionFalse,
					Reason:  "Unschedulable",
					Message: "0/22 nodes are available: 5 node(s) had untolerated taint {konflux-ci.dev/workload: konflux-tenants}",
				}}
				return true, pod, nil
			})
			probe := EgressProbe{Kubernetes: client, Timeout: 40 * time.Millisecond, PollInterval: 5 * time.Millisecond}

			result, err := probe.Probe(context.Background(), "tenant", target)

			Expect(err).ShouldNot(HaveOccurred())
			Expect(result.Reachable).Should(BeFalse())
			Expect(result.Detail).Should(Equal("probe pod cannot start: 0/22 nodes are available: 5 node(s) had untolerated taint {konflux-ci.dev/workload: konflux-tenants}"))
		})
	})

	When("the probe pod does not finish in time", func() {
		It("should report the deadline instead of an error", func() {
			client := egressTestClient(phaseReactor(corev1.PodRunning))
			probe := EgressProbe{Kubernetes: client, Timeout: 40 * time.Millisecond, PollInterval: 5 * time.Millisecond}

			result, err := probe.Probe(context.Background(), "tenant", target)

			Expect(err).ShouldNot(HaveOccurred())
			Expect(result.Reachable).Should(BeFalse())
			Expect(result.Detail).Should(Equal("probe did not finish within 40ms"))
		})
	})

	When("the probe pod cannot be created", func() {
		It("should return the API error", func() {
			client := kubernetesfake.NewSimpleClientset()
			client.PrependReactor("create", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "pods", nil)
			})
			probe := EgressProbe{Kubernetes: client}

			_, err := probe.Probe(context.Background(), "tenant", target)

			Expect(err).Should(MatchError(ContainSubstring("create egress probe pod in namespace tenant")))
		})
	})

	When("the caller cancels the probe", func() {
		It("should return the context error", func() {
			client := egressTestClient(phaseReactor(corev1.PodPending))
			probe := EgressProbe{Kubernetes: client, Timeout: time.Minute, PollInterval: time.Second}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			_, err := probe.Probe(ctx, "tenant", target)

			Expect(err).Should(MatchError(context.Canceled))
		})
	})

	DescribeTable("rejects an incomplete probe",
		func(probe EgressProbe, namespace, target, want string) {
			_, err := probe.Probe(context.Background(), namespace, target)

			Expect(err).Should(MatchError(ContainSubstring(want)))
		},
		Entry("no client", EgressProbe{}, "tenant", "https://example.com", "kubernetes client is required"),
		Entry("no namespace", EgressProbe{Kubernetes: kubernetesfake.NewSimpleClientset()}, "", "https://example.com", "namespace is required"),
		Entry("no target", EgressProbe{Kubernetes: kubernetesfake.NewSimpleClientset()}, "tenant", "", "target is required"),
	)

	It("builds a probe pod that requests no token and uses the frontend image", func() {
		probe := EgressProbe{}
		pod := probe.pod("tenant", EgressProbeImage, target, 30*time.Second)

		Expect(pod.Spec.AutomountServiceAccountToken).ShouldNot(BeNil())
		Expect(*pod.Spec.AutomountServiceAccountToken).Should(BeFalse())
		Expect(pod.Spec.RestartPolicy).Should(Equal(corev1.RestartPolicyNever))
		Expect(*pod.Spec.ActiveDeadlineSeconds).Should(Equal(int64(30)))
		Expect(pod.Spec.Containers).Should(HaveLen(1))
		Expect(pod.Spec.Containers[0].Image).Should(Equal(EgressProbeImage))
		Expect(pod.Spec.Containers[0].Command).Should(Equal([]string{"node", "-e", egressProbeScript, target}))
		Expect(pod.Spec.NodeSelector).Should(Equal(map[string]string{"konflux-ci.dev/workload": "konflux-tenants"}))
		Expect(pod.Spec.Tolerations).Should(Equal([]corev1.Toleration{{
			Key:      "konflux-ci.dev/workload",
			Operator: corev1.TolerationOpEqual,
			Value:    "konflux-tenants",
			Effect:   corev1.TaintEffectNoSchedule,
		}}))
		Expect(pod.Labels).Should(HaveKeyWithValue("app.kubernetes.io/managed-by", "lkmabt"))
	})
})
