package report

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// linkFixtureProvider is the provider run-directory name used by the link fixtures.
const linkFixtureProvider = "github-run-10:46_8.10.2026_run-a"

// linkFixtureAggregateLog is the run-named aggregate log used by the link fixtures.
const linkFixtureAggregateLog = "multi-run-10:46_8.10.2026_run-a-run.log"

// writeFixtureFile creates a fixture file with parent directories.
func writeFixtureFile(path string) {
	Expect(os.MkdirAll(filepath.Dir(path), 0o750)).Should(Succeed())
	Expect(os.WriteFile(path, []byte("x"), 0o640)).Should(Succeed())
}

// writeLinkFixture populates root with the standard aggregate/cluster/provider artifact layout.
func writeLinkFixture(root string) {
	writeFixtureFile(filepath.Join(root, linkFixtureAggregateLog))
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
		It("should list the cluster run.log first, then logs, then evidence", func() {
			aggregate := GinkgoT().TempDir()
			writeLinkFixture(aggregate)

			links := Links(aggregate, linkFixtureAggregateLog, "c1", linkFixtureProvider)

			Expect(links).Should(Equal([]Link{
				{Label: "run.log", Target: "./c1/run.log"},
				{Label: linkFixtureProvider + "/run.log", Target: "./c1/" + linkFixtureProvider + "/run.log"},
				{Label: linkFixtureProvider + "/failed-builds/build-task_step-build.log", Target: "./c1/" + linkFixtureProvider + "/failed-builds/build-task_step-build.log"},
				{Label: "run directory", Target: "./c1/" + linkFixtureProvider + "/"},
				{Label: linkFixtureProvider + "/manifest.json", Target: "./c1/" + linkFixtureProvider + "/manifest.json"},
				{Label: linkFixtureProvider + "/status.json", Target: "./c1/" + linkFixtureProvider + "/status.json"},
				{Label: linkFixtureProvider + "/collection-report.json", Target: "./c1/" + linkFixtureProvider + "/collection-report.json"},
			}))
		})

		It("should keep pod logs behind the run-directory link", func() {
			aggregate := GinkgoT().TempDir()
			writeLinkFixture(aggregate)

			for _, link := range Links(aggregate, linkFixtureAggregateLog, "c1", linkFixtureProvider) {
				Expect(link.Target).ShouldNot(ContainSubstring("/logs/"))
			}
		})

		It("should not repeat the aggregate log inside a cluster section", func() {
			aggregate := GinkgoT().TempDir()
			writeLinkFixture(aggregate)

			for _, link := range Links(aggregate, linkFixtureAggregateLog, "c1", linkFixtureProvider) {
				Expect(link.Target).ShouldNot(Equal("./" + linkFixtureAggregateLog))
			}
		})
	})

	When("a provider artifact is missing", func() {
		It("should omit the missing link", func() {
			aggregate := GinkgoT().TempDir()
			writeLinkFixture(aggregate)
			Expect(os.Remove(filepath.Join(aggregate, "c1", linkFixtureProvider, "collection-report.json"))).Should(Succeed())

			links := Links(aggregate, linkFixtureAggregateLog, "c1", linkFixtureProvider)

			for _, link := range links {
				Expect(link.Target).ShouldNot(ContainSubstring("collection-report.json"))
			}
		})
	})

	When("no provider directory is given", func() {
		It("should list only the cluster run.log", func() {
			aggregate := GinkgoT().TempDir()
			writeLinkFixture(aggregate)

			links := Links(aggregate, linkFixtureAggregateLog, "c1", "")

			Expect(links).Should(Equal([]Link{{Label: "run.log", Target: "./c1/run.log"}}))
		})
	})

	When("no cluster is given", func() {
		It("should list only the aggregate log", func() {
			aggregate := GinkgoT().TempDir()
			writeLinkFixture(aggregate)

			links := Links(aggregate, linkFixtureAggregateLog, "", "")
			Expect(links).Should(Equal([]Link{{Label: "aggregate run.log", Target: "./" + linkFixtureAggregateLog}}))

			Expect(os.Remove(filepath.Join(aggregate, linkFixtureAggregateLog))).Should(Succeed())
			links = Links(aggregate, linkFixtureAggregateLog, "", "")
			Expect(links).ShouldNot(BeNil())
			Expect(links).Should(HaveLen(0))
		})
	})

	When("the logs tree has nested directories", func() {
		It("should leave every pod log behind the run-directory link", func() {
			aggregate := GinkgoT().TempDir()
			writeLinkFixture(aggregate)
			writeFixtureFile(filepath.Join(aggregate, "c1", linkFixtureProvider, "logs", "sub", "deep.log"))

			links := Links(aggregate, linkFixtureAggregateLog, "c1", linkFixtureProvider)

			for _, link := range links {
				Expect(link.Target).ShouldNot(ContainSubstring("deep.log"))
			}
			Expect(links).Should(ContainElement(Link{Label: "run directory", Target: "./c1/" + linkFixtureProvider + "/"}))
		})
	})

	When("the aggregate run has no cluster directory", func() {
		It("should not link a directory that does not exist", func() {
			aggregate := GinkgoT().TempDir()
			writeLinkFixture(aggregate)

			links := Links(aggregate, linkFixtureAggregateLog, "c9", linkFixtureProvider)

			Expect(links).Should(BeEmpty())
		})
	})
})
