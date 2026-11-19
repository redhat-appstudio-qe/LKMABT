package cluster

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

var _ = Describe("RealCluster scheduling", func() {
	It("uses controller configuration without Arm nodes", func() {
		replicas := int32(1)
		clients := kubernetesfake.NewSimpleClientset(
			&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: MultiPlatformControllerDeployment, Namespace: MultiPlatformControllerNamespace, Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
				Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1},
			},
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: MultiPlatformControllerConfigMap, Namespace: MultiPlatformControllerNamespace, Labels: map[string]string{MultiPlatformControllerConfigLabel: "hosts"}},
				Data: map[string]string{
					"dynamic-platforms":        "linux/amd64,linux/arm64",
					"dynamic.linux-amd64.type": "aws",
					"dynamic.linux-arm64.type": "aws",
				},
			},
		)
		result := (&RealCluster{Clients: &ClientSet{Kubernetes: clients}}).CheckScheduling(context.Background(), SchedulingSpec{
			Namespace:  MultiPlatformControllerNamespace,
			Deployment: MultiPlatformControllerDeployment,
			ConfigMap:  MultiPlatformControllerConfigMap,
			Platforms:  []string{"linux/amd64", "linux/arm64"},
		})
		Expect(result).To(HaveLen(3))
		for _, check := range result {
			Expect(check.Passed).To(BeTrue(), check.Name)
		}
	})

	It("accepts platforms provided by static hosts", func() {
		replicas := int32(1)
		clients := kubernetesfake.NewSimpleClientset(
			&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: MultiPlatformControllerDeployment, Namespace: MultiPlatformControllerNamespace, Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
				Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1},
			},
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: MultiPlatformControllerConfigMap, Namespace: MultiPlatformControllerNamespace, Labels: map[string]string{MultiPlatformControllerConfigLabel: "hosts"}},
				Data: map[string]string{
					"host.s390x-dev.address":    "127.0.0.1",
					"host.s390x-dev.platform":   "linux/s390x",
					"host.ppc64le-dev.address":  "127.0.0.1",
					"host.ppc64le-dev.platform": "linux/ppc64le",
					"host.amd64-dev.address":    "127.0.0.1",
					"host.amd64-dev.platform":   "linux/x86_64",
				},
			},
		)
		result := (&RealCluster{Clients: &ClientSet{Kubernetes: clients}}).CheckScheduling(context.Background(), SchedulingSpec{
			Namespace:  MultiPlatformControllerNamespace,
			Deployment: MultiPlatformControllerDeployment,
			ConfigMap:  MultiPlatformControllerConfigMap,
			Platforms:  []string{"linux/s390x", "linux/ppc64le", "linux/amd64"},
		})
		Expect(result).To(HaveLen(4))
		Expect(result[0].Passed).Should(BeTrue())
		for _, check := range result[1:] {
			Expect(check.Passed).To(BeTrue(), check.Name)
			Expect(check.Details).Should(ContainSubstring("static"))
		}
	})

	It("rejects an unconfigured platform", func() {
		replicas := int32(1)
		clients := kubernetesfake.NewSimpleClientset(
			&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: MultiPlatformControllerDeployment, Namespace: MultiPlatformControllerNamespace, Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
				Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1},
			},
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: MultiPlatformControllerConfigMap, Namespace: MultiPlatformControllerNamespace, Labels: map[string]string{MultiPlatformControllerConfigLabel: "hosts"}},
				Data:       map[string]string{"dynamic-platforms": "linux/amd64", "dynamic.linux-amd64.type": "aws"},
			},
		)
		result := (&RealCluster{Clients: &ClientSet{Kubernetes: clients}}).CheckScheduling(context.Background(), SchedulingSpec{
			Namespace:  MultiPlatformControllerNamespace,
			Deployment: MultiPlatformControllerDeployment,
			ConfigMap:  MultiPlatformControllerConfigMap,
			Platforms:  []string{"linux/arm64"},
		})
		Expect(result).To(HaveLen(2))
		Expect(result[1].Passed).To(BeFalse())
		Expect(result[1].Details).NotTo(BeEmpty())
	})
})
