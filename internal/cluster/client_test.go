package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	kubetesting "k8s.io/client-go/testing"
)

var _ = Describe("NewClientSetFromConfig", func() {
	It("bounds default HTTP requests without mutating the input", func() {
		config := &rest.Config{Host: "https://api.example"}
		clients, err := NewClientSetFromConfig(config)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(clients.Config.Timeout).Should(Equal(20 * time.Second))
		Expect(config.Timeout).Should(BeZero())
	})
})

// kubeconfigYAML renders a minimal single-cluster kubeconfig pointing at server.
func kubeconfigYAML(server string) string {
	return "apiVersion: v1\nkind: Config\nclusters:\n- cluster: {server: " + server + "}\n  name: c\ncontexts:\n- context: {cluster: c, user: u}\n  name: ctx\ncurrent-context: ctx\nusers:\n- name: u\n  user: {}\n"
}

// writeKubeconfig writes a minimal kubeconfig for server at path.
func writeKubeconfig(path, server string) {
	Expect(os.WriteFile(path, []byte(kubeconfigYAML(server)), 0o600)).Should(Succeed())
}

var _ = Describe("ServerFromKubeconfig", func() {
	When("the kubeconfig declares a server", func() {
		It("should return the server without a trailing slash", func() {
			path := filepath.Join(GinkgoT().TempDir(), "config")
			writeKubeconfig(path, "https://api.example:6443/")

			server, err := ServerFromKubeconfig(path)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(server).Should(Equal("https://api.example:6443"))
		})
	})

	When("the path is empty", func() {
		It("should fall back to the KUBECONFIG default", func() {
			path := filepath.Join(GinkgoT().TempDir(), "config")
			writeKubeconfig(path, "https://from-env.example:6443")
			GinkgoT().Setenv("KUBECONFIG", path)
			GinkgoT().Setenv("HOME", GinkgoT().TempDir())

			server, err := ServerFromKubeconfig("")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(server).Should(Equal("https://from-env.example:6443"))
		})
	})

	When("the path does not exist", func() {
		It("should wrap the load error", func() {
			_, err := ServerFromKubeconfig(filepath.Join(GinkgoT().TempDir(), "absent"))
			Expect(err).Should(MatchError(ContainSubstring("load kubeconfig")))
		})
	})
})

var _ = Describe("NewClientSet", func() {
	When("an explicit kubeconfig is given", func() {
		It("should use its server and ignore KUBECONFIG", func() {
			dir := GinkgoT().TempDir()
			explicit := filepath.Join(dir, "explicit")
			writeKubeconfig(explicit, "https://explicit.example:6443")
			ambient := filepath.Join(dir, "ambient")
			writeKubeconfig(ambient, "https://env.example:6443")
			GinkgoT().Setenv("KUBECONFIG", ambient)

			clients, err := NewClientSet(explicit)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(clients.Config.Host).Should(Equal("https://explicit.example:6443"))
		})
	})

	When("the explicit kubeconfig is missing", func() {
		It("should fail", func() {
			_, err := NewClientSet(filepath.Join(GinkgoT().TempDir(), "absent"))
			Expect(err).Should(MatchError(ContainSubstring("load kubeconfig")))
		})
	})
})

// accessTestCluster builds a cluster whose discovery serves each resource.
func accessTestCluster(resources ...schema.GroupVersionResource) (*RealCluster, *fake.Clientset) {
	clientset := fake.NewSimpleClientset()
	discovery := clientset.Discovery().(*fakediscovery.FakeDiscovery)
	for _, resource := range resources {
		discovery.Resources = append(discovery.Resources, &metav1.APIResourceList{
			GroupVersion: resource.GroupVersion().String(),
			APIResources: []metav1.APIResource{{Name: resource.Resource, Namespaced: true}},
		})
	}
	return &RealCluster{Clients: &ClientSet{Kubernetes: clientset, Discovery: clientset.Discovery()}}, clientset
}

// respondSSAR makes every SelfSubjectAccessReview create return status or err.
func respondSSAR(clientset *fake.Clientset, status authorizationv1.SubjectAccessReviewStatus, err error) {
	clientset.PrependReactor("create", "selfsubjectaccessreviews", func(kubetesting.Action) (bool, runtime.Object, error) {
		if err != nil {
			return true, nil, err
		}
		return true, &authorizationv1.SelfSubjectAccessReview{Status: status}, nil
	})
}

var _ = Describe("CheckAccess", func() {
	secrets := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

	When("every requested verb is allowed", func() {
		It("should pass the check", func() {
			cluster, clientset := accessTestCluster(secrets)
			respondSSAR(clientset, authorizationv1.SubjectAccessReviewStatus{Allowed: true}, nil)

			results := cluster.CheckAccess(context.Background(), []AccessCheck{{
				Name: "gitlab SCM Secret", Resource: secrets, Namespace: "tenant",
				Verbs: []string{"get", "create", "update"}, Guidance: "grant Secret access in the tenant namespace",
			}})

			Expect(results).Should(HaveLen(1))
			Expect(results[0].Passed).Should(BeTrue())
			Expect(results[0].Details).Should(BeEmpty())
		})
	})

	When("a requested verb is denied", func() {
		It("should report the denial reason and guidance", func() {
			cluster, clientset := accessTestCluster(secrets)
			respondSSAR(clientset, authorizationv1.SubjectAccessReviewStatus{Reason: "rbac denied", EvaluationError: "no matching policy"}, nil)

			results := cluster.CheckAccess(context.Background(), []AccessCheck{{
				Name: "gitlab SCM Secret", Resource: secrets, Namespace: "tenant",
				Verbs: []string{"update"}, Guidance: "grant Secret access in the tenant namespace",
			}})

			Expect(results[0].Passed).Should(BeFalse())
			Expect(results[0].Details).Should(ContainSubstring("update secrets in namespace tenant denied: rbac denied"))
			Expect(results[0].Details).Should(ContainSubstring("evaluation error: no matching policy"))
			Expect(results[0].Details).Should(ContainSubstring("grant Secret access in the tenant namespace"))
		})
	})

	When("the authorization review fails", func() {
		It("should surface the error and guidance", func() {
			cluster, clientset := accessTestCluster(secrets)
			respondSSAR(clientset, authorizationv1.SubjectAccessReviewStatus{}, errors.New("admission webhook unavailable"))

			results := cluster.CheckAccess(context.Background(), []AccessCheck{{
				Name: "gitlab SCM Secret", Resource: secrets, Namespace: "tenant",
				Verbs: []string{"get"}, Guidance: "grant Secret access in the tenant namespace",
			}})

			Expect(results[0].Passed).Should(BeFalse())
			Expect(results[0].Details).Should(ContainSubstring("get secrets in namespace tenant: admission webhook unavailable"))
			Expect(results[0].Details).Should(ContainSubstring("grant Secret access in the tenant namespace"))
		})
	})

	When("no verbs are requested", func() {
		It("should only require discovery", func() {
			cluster, _ := accessTestCluster(secrets)
			cluster.Clients.Kubernetes = nil

			results := cluster.CheckAccess(context.Background(), []AccessCheck{{Name: "secrets", Resource: secrets}})

			Expect(results).Should(HaveLen(1))
			Expect(results[0].Passed).Should(BeTrue())
			Expect(results[0].Details).Should(BeEmpty())
		})
	})
})
