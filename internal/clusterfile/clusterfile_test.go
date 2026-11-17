package clusterfile_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redhat-appstudio/lkmabt/internal/clusterfile"
)

// validClustersYAML exercises defaults, ~ expansion, relative paths, and absolute paths.
const validClustersYAML = `clusters:
  - name: c1
  - name: " c2 "
    kubeconfig: ~/alt/config-c2
    server: https://api.c2.example:6443/
  - name: c3
    kubeconfig: kube/config-c3
  - name: c4
    kubeconfig: /opt/kube/config-c4
`

// writeClustersFile writes the given YAML content to a clusters file in dir.
func writeClustersFile(dir, name, yaml string) string {
	path := filepath.Join(dir, name)
	Expect(os.WriteFile(path, []byte(yaml), 0o600)).Should(Succeed())
	return path
}

// testHome returns a fresh HOME directory and points os.UserHomeDir at it.
func testHome() string {
	home := GinkgoT().TempDir()
	GinkgoT().Setenv("HOME", home)
	return home
}

var _ = Describe("ValidName", func() {
	DescribeTable("should accept only directory-safe names",
		func(name string, valid bool) {
			Expect(clusterfile.ValidName(name)).Should(Equal(valid))
		},
		Entry("lowercase with hyphens and digits", "kflux-lw-p01", true),
		Entry("mixed characters", "a.b_c-1", true),
		Entry("empty", "", false),
		Entry("path separator", "a/b", false),
		Entry("space", "a b", false),
		Entry("colon", "a:b", false),
		Entry("non-ASCII", "café", false),
	)
})

var _ = Describe("DefaultKubeconfig", func() {
	It("should place the per-cluster config under ~/.kube", func() {
		Expect(clusterfile.DefaultKubeconfig("/home/tester", "c1")).Should(Equal(filepath.Join("/home/tester", ".kube", "config-c1")))
	})
})

var _ = Describe("Load", func() {
	When("the file is valid", func() {
		It("should apply defaults, expand ~, resolve relative paths, and trim trailing slashes", func() {
			dir := GinkgoT().TempDir()
			home := testHome()
			path := writeClustersFile(dir, "clusters.yaml", validClustersYAML)
			entries, err := clusterfile.Load(path)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(entries).Should(HaveLen(4))
			Expect(entries[0].Name).Should(Equal("c1"))
			Expect(entries[0].Kubeconfig).Should(Equal(filepath.Join(home, ".kube", "config-c1")))
			Expect(entries[0].Server).Should(Equal(""))
			Expect(entries[1].Name).Should(Equal("c2"))
			Expect(entries[1].Kubeconfig).Should(Equal(filepath.Join(home, "alt", "config-c2")))
			Expect(entries[1].Server).Should(Equal("https://api.c2.example:6443"))
			Expect(entries[2].Name).Should(Equal("c3"))
			Expect(entries[2].Kubeconfig).Should(Equal(filepath.Join(dir, "kube", "config-c3")))
			Expect(entries[2].Server).Should(Equal(""))
			Expect(entries[3].Name).Should(Equal("c4"))
			Expect(entries[3].Kubeconfig).Should(Equal("/opt/kube/config-c4"))
			Expect(entries[3].Server).Should(Equal(""))
		})

		It("should treat \"~\" alone as the home directory", func() {
			dir := GinkgoT().TempDir()
			home := testHome()
			path := writeClustersFile(dir, "clusters.yaml", "clusters:\n  - name: c1\n    kubeconfig: \"~\"\n")

			entries, err := clusterfile.Load(path)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(entries[0].Kubeconfig).Should(Equal(home))
		})

		It("should report which kubeconfig is the default one", func() {
			dir := GinkgoT().TempDir()
			home := testHome()
			path := writeClustersFile(dir, "clusters.yaml", validClustersYAML)

			entries, err := clusterfile.Load(path)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(entries[0].UsesDefaultKubeconfig(home)).Should(BeTrue())
			Expect(entries[1].UsesDefaultKubeconfig(home)).Should(BeFalse())
			Expect(entries[2].UsesDefaultKubeconfig(home)).Should(BeFalse())
			Expect(entries[3].UsesDefaultKubeconfig(home)).Should(BeFalse())
		})

		When("an entry names a kflux catalog cluster", func() {
			It("should derive the default kubeconfig path from that name", func() {
				dir := GinkgoT().TempDir()
				home := testHome()
				path := writeClustersFile(dir, "clusters.yaml", "clusters:\n  - name: kflux-ocp-p01\n    kflux: ocp-p01\n")

				entries, err := clusterfile.Load(path)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(entries[0].Name).Should(Equal("kflux-ocp-p01"))
				Expect(entries[0].KfluxName()).Should(Equal("ocp-p01"))
				Expect(entries[0].Kubeconfig).Should(Equal(filepath.Join(home, ".kube", "config-ocp-p01")))
				Expect(entries[0].UsesDefaultKubeconfig(home)).Should(BeTrue())
			})

			It("should keep an explicit kubeconfig path off the kflux path", func() {
				dir := GinkgoT().TempDir()
				home := testHome()
				path := writeClustersFile(dir, "clusters.yaml", "clusters:\n  - name: kflux-ocp-p01\n    kflux: ocp-p01\n    kubeconfig: ./config-own\n")

				entries, err := clusterfile.Load(path)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(entries[0].Kubeconfig).Should(Equal(filepath.Join(dir, "config-own")))
				Expect(entries[0].UsesDefaultKubeconfig(home)).Should(BeFalse())
			})
		})

		It("should default the kflux name to the entry name", func() {
			dir := GinkgoT().TempDir()
			testHome()
			path := writeClustersFile(dir, "clusters.yaml", validClustersYAML)

			entries, err := clusterfile.Load(path)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(entries[0].KfluxName()).Should(Equal("c1"))
			Expect(entries[0].Kflux).Should(Equal("c1"))
		})
	})

	When("an entry declares a cluster type", func() {
		It("should default to ocp", func() {
			dir := GinkgoT().TempDir()
			testHome()
			entries, err := clusterfile.Load(writeClustersFile(dir, "clusters.yaml", "clusters:\n  - name: c1\n"))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(entries[0].Type).Should(Equal(clusterfile.TypeOCP))
			Expect(entries[0].IsROKS()).Should(BeFalse())
			Expect(entries[0].IBMCloud).Should(BeEmpty())
			Expect(entries[0].IBMCloudEndpoint).Should(BeEmpty())
		})

		It("should default the ROKS IBM Cloud cluster to the entry name and the endpoint to vpe", func() {
			dir := GinkgoT().TempDir()
			testHome()
			entries, err := clusterfile.Load(writeClustersFile(dir, "clusters.yaml", "clusters:\n  - name: kflux-roks-p01\n    type: roks\n"))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(entries[0].IsROKS()).Should(BeTrue())
			Expect(entries[0].IBMCloud).Should(Equal("kflux-roks-p01"))
			Expect(entries[0].IBMCloudEndpoint).Should(Equal(clusterfile.DefaultIBMCloudEndpoint))
		})

		It("should keep explicit IBM Cloud values and the expected server", func() {
			dir := GinkgoT().TempDir()
			testHome()
			yaml := "clusters:\n  - name: c1\n    type: roks\n    ibmcloud: cluster-id\n    ibmcloud-endpoint: private\n    server: https://c1.vpe.private.example:31533/\n"
			entries, err := clusterfile.Load(writeClustersFile(dir, "clusters.yaml", yaml))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(entries[0].IBMCloud).Should(Equal("cluster-id"))
			Expect(entries[0].IBMCloudEndpoint).Should(Equal("private"))
			Expect(entries[0].Server).Should(Equal("https://c1.vpe.private.example:31533"))
		})

		DescribeTable("should reject an unusable IBM Cloud configuration",
			func(yaml, wantSubstring string) {
				dir := GinkgoT().TempDir()
				testHome()

				_, err := clusterfile.Load(writeClustersFile(dir, "clusters.yaml", yaml))
				Expect(err).Should(MatchError(ContainSubstring(wantSubstring)))
			},
			Entry("unknown type", "clusters:\n  - name: c1\n    type: rosa\n", `type "rosa" must be ocp or roks`),
			Entry("ibmcloud on ocp", "clusters:\n  - name: c1\n    ibmcloud: cluster-id\n", `sets ibmcloud fields but type is "ocp"; set type: roks`),
			Entry("endpoint on ocp", "clusters:\n  - name: c1\n    ibmcloud-endpoint: vpe\n", `sets ibmcloud fields but type is "ocp"; set type: roks`),
			Entry("unknown endpoint", "clusters:\n  - name: c1\n    type: roks\n    ibmcloud-endpoint: public\n", `ibmcloud-endpoint "public" must be vpe, private, or link`),
		)
	})

	When("the file is missing", func() {
		It("should name the flag and the default path", func() {
			dir := GinkgoT().TempDir()
			path := filepath.Join(dir, "absent.yaml")

			_, err := clusterfile.Load(path)
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).Should(ContainSubstring("--clusters-file"))
			Expect(err.Error()).Should(ContainSubstring(".lkmabt-clusters.yaml"))
			Expect(err.Error()).Should(ContainSubstring(path))
		})
	})

	When("the YAML is malformed", func() {
		It("should reject unbalanced YAML", func() {
			dir := GinkgoT().TempDir()
			path := writeClustersFile(dir, "clusters.yaml", "clusters: [\n")

			_, err := clusterfile.Load(path)
			Expect(err).Should(MatchError(ContainSubstring("parse clusters file")))
		})
	})

	When("an unknown key is present", func() {
		It("should reject unknown keys", func() {
			dir := GinkgoT().TempDir()
			path := writeClustersFile(dir, "clusters.yaml", "clusters:\n  - name: c1\n    token: secret\n")

			_, err := clusterfile.Load(path)
			Expect(err).Should(MatchError(ContainSubstring("parse clusters file")))
		})
	})

	When("the cluster list is empty", func() {
		DescribeTable("should reject the file",
			func(yaml string) {
				dir := GinkgoT().TempDir()
				path := writeClustersFile(dir, "clusters.yaml", yaml)

				_, err := clusterfile.Load(path)
				Expect(err).Should(MatchError(ContainSubstring("must list at least one cluster")))
			},
			Entry("empty list", "clusters: []\n"),
			Entry("no clusters key", "other: 1\n"),
		)
	})

	When("an entry is invalid", func() {
		DescribeTable("should reject the entry",
			func(yaml, wantSubstring string) {
				dir := GinkgoT().TempDir()
				path := writeClustersFile(dir, "clusters.yaml", yaml)

				_, err := clusterfile.Load(path)
				Expect(err).Should(MatchError(ContainSubstring(wantSubstring)))
			},
			Entry("empty name", "clusters:\n  - name: \"\"\n", "entry 1 requires a name"),
			Entry("missing name", "clusters:\n  - kubeconfig: /k\n", "entry 1 requires a name"),
			Entry("duplicate name", "clusters:\n  - name: c1\n  - name: c1\n", `duplicate cluster name "c1"`),
			Entry("invalid characters", "clusters:\n  - name: bad/name\n", "must match [A-Za-z0-9._-]+"),
			Entry("glob kflux name", "clusters:\n  - name: kflux-ocp-p01\n    kflux: \"stg-*\"\n", `kflux name "stg-*" must match [A-Za-z0-9._-]+`),
		)
	})

	When("two entries share a kubeconfig path", func() {
		It("should reject the collision", func() {
			dir := GinkgoT().TempDir()
			testHome()
			path := writeClustersFile(dir, "clusters.yaml", "clusters:\n  - name: c1\n    kubeconfig: /opt/shared/config\n  - name: c2\n    kubeconfig: /opt/shared/config\n")

			_, err := clusterfile.Load(path)
			Expect(err).Should(MatchError(ContainSubstring(`clusters "c1" and "c2" resolve to the same kubeconfig path /opt/shared/config`)))
		})

		It("should reject two names mapping to one kflux cluster", func() {
			dir := GinkgoT().TempDir()
			home := testHome()
			path := writeClustersFile(dir, "clusters.yaml", "clusters:\n  - name: kflux-ocp-p01\n    kflux: ocp-p01\n  - name: ocp-p01\n")

			_, err := clusterfile.Load(path)
			Expect(err).Should(MatchError(ContainSubstring("resolve to the same kubeconfig path " + filepath.Join(home, ".kube", "config-ocp-p01"))))
		})
	})

	When("entries resolve to distinct paths", func() {
		It("should load every entry", func() {
			dir := GinkgoT().TempDir()
			testHome()
			path := writeClustersFile(dir, "valid.yaml", validClustersYAML)

			entries, err := clusterfile.Load(path)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(entries).Should(HaveLen(4))
		})
	})

	When("the server is not a usable API server URL", func() {
		DescribeTable("should reject the entry",
			func(serverLine string) {
				dir := GinkgoT().TempDir()
				testHome()
				path := writeClustersFile(dir, "clusters.yaml", "clusters:\n  - name: c1\n    "+serverLine)

				_, err := clusterfile.Load(path)
				Expect(err).Should(MatchError(ContainSubstring(`cluster "c1" server`)))
				Expect(err).Should(MatchError(ContainSubstring("must be an absolute http or https URL")))
			},
			Entry("trailing slash only", "server: /\n"),
			Entry("padded trailing slash only", "server: \" / \"\n"),
			Entry("missing scheme", "server: ://bad\n"),
			Entry("non-http scheme", "server: file:///tmp/x\n"),
		)
	})

	When("the server is valid or omitted", func() {
		It("should accept and normalize it", func() {
			dir := GinkgoT().TempDir()
			testHome()
			path := writeClustersFile(dir, "clusters.yaml", "clusters:\n  - name: c1\n  - name: c2\n    server: https://api.c2.example:6443/\n")

			entries, err := clusterfile.Load(path)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(entries).Should(HaveLen(2))
			Expect(entries[0].Server).Should(Equal(""))
			Expect(entries[1].Server).Should(Equal("https://api.c2.example:6443"))
		})
	})

	When("the file is unreadable", func() {
		It("should wrap the read error", func() {
			if os.Geteuid() == 0 {
				Skip("root ignores file permissions")
			}
			dir := GinkgoT().TempDir()
			path := writeClustersFile(dir, "clusters.yaml", validClustersYAML)
			Expect(os.Chmod(path, 0o000)).Should(Succeed())
			DeferCleanup(func() { _ = os.Chmod(path, 0o600) })

			_, err := clusterfile.Load(path)
			Expect(err).Should(MatchError(ContainSubstring("read clusters file")))
		})
	})
})

var _ = Describe("Find", func() {
	When("the cluster is listed", func() {
		It("should return the matching entry", func() {
			dir := GinkgoT().TempDir()
			testHome()
			entries, err := clusterfile.Load(writeClustersFile(dir, "clusters.yaml", validClustersYAML))
			Expect(err).ShouldNot(HaveOccurred())

			entry, err := clusterfile.Find(entries, "c3")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(entry.Name).Should(Equal("c3"))
		})
	})

	When("the cluster is absent", func() {
		It("should report the missing cluster", func() {
			dir := GinkgoT().TempDir()
			testHome()
			entries, err := clusterfile.Load(writeClustersFile(dir, "clusters.yaml", validClustersYAML))
			Expect(err).ShouldNot(HaveOccurred())

			_, err = clusterfile.Find(entries, "c9")
			Expect(err).Should(MatchError(`clusters file does not list cluster "c9"`))
		})
	})
})
