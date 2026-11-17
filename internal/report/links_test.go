package report

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// linkFixtureProvider is the provider run-directory name used by the link fixtures.
const linkFixtureProvider = "github-run-10:46_8.10.2026_run-a"

// writeFixtureFile creates a fixture file with parent directories.
func writeFixtureFile(path string) {
	Expect(os.MkdirAll(filepath.Dir(path), 0o750)).Should(Succeed())
	Expect(os.WriteFile(path, []byte("x"), 0o640)).Should(Succeed())
}

// writeLinkFixture populates root with the standard aggregate/cluster/provider artifact layout.
func writeLinkFixture(root string) {
	writeFixtureFile(filepath.Join(root, "run.log"))
	writeFixtureFile(filepath.Join(root, "c1", "run.log"))
	provider := linkFixtureProvider
	writeFixtureFile(filepath.Join(root, "c1", provider, "run.log"))
	writeFixtureFile(filepath.Join(root, "c1", provider, "manifest.json"))
	writeFixtureFile(filepath.Join(root, "c1", provider, "status.json"))
	writeFixtureFile(filepath.Join(root, "c1", provider, "collection-report.json"))
	writeFixtureFile(filepath.Join(root, "c1", provider, "logs", "pod-a.log"))
	writeFixtureFile(filepath.Join(root, "c1", provider, "failed-builds", "build-task_step-build.log"))
}

var _ = Describe("Links", func() {
	When("cluster and provider directory are given", func() {
		It("should list every existing artifact in order", func() {
			aggregate := GinkgoT().TempDir()
			writeLinkFixture(aggregate)

			links := Links(aggregate, "c1", linkFixtureProvider)

			Expect(links[:8]).Should(Equal([]Link{
				{Label: "aggregate run.log", Target: "./run.log"},
				{Label: "cluster directory", Target: "./c1/"},
				{Label: "cluster run.log", Target: "./c1/run.log"},
				{Label: "run directory", Target: "./c1/" + linkFixtureProvider + "/"},
				{Label: "run.log", Target: "./c1/" + linkFixtureProvider + "/run.log"},
				{Label: "manifest.json", Target: "./c1/" + linkFixtureProvider + "/manifest.json"},
				{Label: "status.json", Target: "./c1/" + linkFixtureProvider + "/status.json"},
				{Label: "collection-report.json", Target: "./c1/" + linkFixtureProvider + "/collection-report.json"},
			}))
			Expect(links[8:]).Should(ConsistOf(
				Link{Label: "c1/" + linkFixtureProvider + "/logs/pod-a.log", Target: "./c1/" + linkFixtureProvider + "/logs/pod-a.log"},
				Link{Label: "c1/" + linkFixtureProvider + "/failed-builds/build-task_step-build.log", Target: "./c1/" + linkFixtureProvider + "/failed-builds/build-task_step-build.log"},
			))
		})
	})

	When("a provider artifact is missing", func() {
		It("should omit the missing link", func() {
			aggregate := GinkgoT().TempDir()
			writeLinkFixture(aggregate)
			Expect(os.Remove(filepath.Join(aggregate, "c1", linkFixtureProvider, "collection-report.json"))).Should(Succeed())
			Expect(os.Remove(filepath.Join(aggregate, "c1", linkFixtureProvider, "logs", "pod-a.log"))).Should(Succeed())

			links := Links(aggregate, "c1", linkFixtureProvider)

			for _, link := range links {
				Expect(link.Target).ShouldNot(ContainSubstring("collection-report.json"))
				Expect(link.Target).ShouldNot(ContainSubstring("pod-a.log"))
			}
		})
	})

	When("no provider directory is given", func() {
		It("should list only aggregate and cluster artifacts", func() {
			aggregate := GinkgoT().TempDir()
			writeLinkFixture(aggregate)

			links := Links(aggregate, "c1", "")

			Expect(links).Should(Equal([]Link{
				{Label: "aggregate run.log", Target: "./run.log"},
				{Label: "cluster directory", Target: "./c1/"},
				{Label: "cluster run.log", Target: "./c1/run.log"},
			}))
			Expect(links).Should(HaveLen(3))
		})
	})

	When("no cluster is given", func() {
		It("should list only the aggregate log", func() {
			aggregate := GinkgoT().TempDir()
			writeLinkFixture(aggregate)

			links := Links(aggregate, "", "")
			Expect(links).Should(Equal([]Link{{Label: "aggregate run.log", Target: "./run.log"}}))

			Expect(os.Remove(filepath.Join(aggregate, "run.log"))).Should(Succeed())
			links = Links(aggregate, "", "")
			Expect(links).ShouldNot(BeNil())
			Expect(links).Should(HaveLen(0))
		})
	})

	When("the logs tree has nested directories", func() {
		It("should include each regular file with a root-relative label", func() {
			aggregate := GinkgoT().TempDir()
			writeLinkFixture(aggregate)
			writeFixtureFile(filepath.Join(aggregate, "c1", linkFixtureProvider, "logs", "sub", "deep.log"))

			links := Links(aggregate, "c1", linkFixtureProvider)

			Expect(links).Should(ContainElement(Link{
				Label:  "c1/" + linkFixtureProvider + "/logs/sub/deep.log",
				Target: "./c1/" + linkFixtureProvider + "/logs/sub/deep.log",
			}))
		})
	})

	When("the aggregate run has no cluster directory", func() {
		It("should not link a directory that does not exist", func() {
			aggregate := GinkgoT().TempDir()
			writeLinkFixture(aggregate)

			links := Links(aggregate, "c9", linkFixtureProvider)

			for _, link := range links {
				Expect(link.Target).ShouldNot(Equal("./c9/"))
			}
		})
	})
})
