package cluster

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/rest"
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
