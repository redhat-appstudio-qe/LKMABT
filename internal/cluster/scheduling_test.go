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
	When("the controller is ready", func() {
		It("checks only the controller without platform requests", func() {
			clients := schedulingClients(nil)
			result := (&RealCluster{Clients: &ClientSet{Kubernetes: clients}}).CheckScheduling(context.Background(), SchedulingSpec{
				Namespace:  MultiPlatformControllerNamespace,
				Deployment: MultiPlatformControllerDeployment,
				ConfigMap:  MultiPlatformControllerConfigMap,
			})
			Expect(result.Checks).Should(HaveLen(1))
			Expect(result.Checks[0].Passed).Should(BeTrue(), result.Checks[0].Name)
			Expect(result.Platforms).Should(BeEmpty())
		})

		It("supports dynamic platforms with a provider type", func() {
			clients := schedulingClients(map[string]string{
				"dynamic-platforms":        "linux/amd64,linux/arm64",
				"dynamic.linux-amd64.type": "aws",
				"dynamic.linux-arm64.type": "aws",
			})
			result := (&RealCluster{Clients: &ClientSet{Kubernetes: clients}}).CheckScheduling(context.Background(), SchedulingSpec{
				Namespace:  MultiPlatformControllerNamespace,
				Deployment: MultiPlatformControllerDeployment,
				ConfigMap:  MultiPlatformControllerConfigMap,
				Platforms:  []string{"linux/amd64", "linux/arm64"},
			})
			Expect(result.Checks).Should(HaveLen(1))
			Expect(result.Checks[0].Passed).Should(BeTrue(), result.Checks[0].Name)
			Expect(result.Platforms).Should(HaveLen(2))
			for _, support := range result.Platforms {
				Expect(support.Configured).Should(BeTrue(), support.Platform)
				Expect(support.Supported).Should(BeTrue(), support.Platform)
				Expect(support.Details).Should(ContainSubstring("dynamic"))
			}
		})

		It("supports platforms provided by complete static hosts", func() {
			clients := schedulingClients(map[string]string{
				"host.s390x-dev.address":    "127.0.0.1",
				"host.s390x-dev.user":       "cloud-user",
				"host.s390x-dev.secret":     "s390x-ssh-key",
				"host.s390x-dev.platform":   "linux/s390x",
				"host.ppc64le-dev.address":  "127.0.0.1",
				"host.ppc64le-dev.user":     "cloud-user",
				"host.ppc64le-dev.secret":   "ppc64le-ssh-key",
				"host.ppc64le-dev.platform": "linux/ppc64le",
				"host.amd64-dev.address":    "127.0.0.1",
				"host.amd64-dev.user":       "cloud-user",
				"host.amd64-dev.secret":     "amd64-ssh-key",
				"host.amd64-dev.platform":   "linux/x86_64",
			})
			result := (&RealCluster{Clients: &ClientSet{Kubernetes: clients}}).CheckScheduling(context.Background(), SchedulingSpec{
				Namespace:  MultiPlatformControllerNamespace,
				Deployment: MultiPlatformControllerDeployment,
				ConfigMap:  MultiPlatformControllerConfigMap,
				Platforms:  []string{"linux/s390x", "linux/ppc64le", "linux/amd64"},
			})
			Expect(result.Checks).Should(HaveLen(1))
			Expect(result.Checks[0].Passed).Should(BeTrue())
			Expect(result.Platforms).Should(HaveLen(3))
			for _, support := range result.Platforms {
				Expect(support.Configured).Should(BeTrue(), support.Platform)
				Expect(support.Supported).Should(BeTrue(), support.Platform)
				Expect(support.Details).Should(ContainSubstring("static"))
			}
		})

		It("uses a complete static host when another matching host is incomplete", func() {
			clients := schedulingClients(map[string]string{
				"host.bad.platform":  "linux/s390x",
				"host.good.address":  "127.0.0.1",
				"host.good.user":     "cloud-user",
				"host.good.secret":   "s390x-ssh-key",
				"host.good.platform": "linux/s390x",
			})
			result := (&RealCluster{Clients: &ClientSet{Kubernetes: clients}}).CheckScheduling(context.Background(), SchedulingSpec{
				Namespace:  MultiPlatformControllerNamespace,
				Deployment: MultiPlatformControllerDeployment,
				ConfigMap:  MultiPlatformControllerConfigMap,
				Platforms:  []string{"linux/s390x"},
			})
			Expect(result.Platforms).Should(HaveLen(1))
			Expect(result.Platforms[0].Configured).Should(BeTrue())
			Expect(result.Platforms[0].Supported).Should(BeTrue())
			Expect(result.Platforms[0].Details).Should(ContainSubstring("good"))
		})
	})

	When("the controller cannot schedule a requested platform", func() {
		It("marks an undeclared platform unconfigured and unsupported", func() {
			clients := schedulingClients(map[string]string{"dynamic-platforms": "linux/amd64", "dynamic.linux-amd64.type": "aws"})
			result := (&RealCluster{Clients: &ClientSet{Kubernetes: clients}}).CheckScheduling(context.Background(), SchedulingSpec{
				Namespace:  MultiPlatformControllerNamespace,
				Deployment: MultiPlatformControllerDeployment,
				ConfigMap:  MultiPlatformControllerConfigMap,
				Platforms:  []string{"linux/arm64"},
			})
			Expect(result.Checks[0].Passed).Should(BeTrue())
			Expect(result.Platforms).Should(HaveLen(1))
			Expect(result.Platforms[0].Configured).Should(BeFalse())
			Expect(result.Platforms[0].Supported).Should(BeFalse())
			Expect(result.Platforms[0].Details).Should(ContainSubstring("not configured"))
		})

		It("marks an incomplete static host configured and unsupported", func() {
			clients := schedulingClients(map[string]string{
				"host.s390x-dev.address":  "127.0.0.1",
				"host.s390x-dev.platform": "linux/s390x",
			})
			result := (&RealCluster{Clients: &ClientSet{Kubernetes: clients}}).CheckScheduling(context.Background(), SchedulingSpec{
				Namespace:  MultiPlatformControllerNamespace,
				Deployment: MultiPlatformControllerDeployment,
				ConfigMap:  MultiPlatformControllerConfigMap,
				Platforms:  []string{"linux/s390x"},
			})
			Expect(result.Platforms).Should(HaveLen(1))
			Expect(result.Platforms[0].Configured).Should(BeTrue())
			Expect(result.Platforms[0].Supported).Should(BeFalse())
			Expect(result.Platforms[0].Details).Should(ContainSubstring("s390x-dev"))
			Expect(result.Platforms[0].Details).Should(ContainSubstring("missing user, secret"))
		})

		It("marks a dynamic platform without a provider type configured and unsupported", func() {
			clients := schedulingClients(map[string]string{"dynamic-platforms": "linux/arm64"})
			result := (&RealCluster{Clients: &ClientSet{Kubernetes: clients}}).CheckScheduling(context.Background(), SchedulingSpec{
				Namespace:  MultiPlatformControllerNamespace,
				Deployment: MultiPlatformControllerDeployment,
				ConfigMap:  MultiPlatformControllerConfigMap,
				Platforms:  []string{"linux/arm64"},
			})
			Expect(result.Platforms).Should(HaveLen(1))
			Expect(result.Platforms[0].Configured).Should(BeTrue())
			Expect(result.Platforms[0].Supported).Should(BeFalse())
			Expect(result.Platforms[0].Details).Should(ContainSubstring("no provider type"))
		})
	})
})

// schedulingClients returns a ready controller with the supplied host-config data.
func schedulingClients(data map[string]string) *kubernetesfake.Clientset {
	replicas := int32(1)
	return kubernetesfake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: MultiPlatformControllerDeployment, Namespace: MultiPlatformControllerNamespace, Generation: 1},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: MultiPlatformControllerConfigMap, Namespace: MultiPlatformControllerNamespace, Labels: map[string]string{MultiPlatformControllerConfigLabel: "hosts"}},
			Data:       data,
		},
	)
}
