package report

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// reportDocument builds the standard two-cluster report fixture used by the
// Markdown, HTML, and document tests.
func reportDocument() Document {
	return Document{
		RunID:         "run-1",
		ClustersFile:  ".lkmabt-clusters.yaml",
		Mode:          "full",
		Fixtures:      "both",
		Architectures: []string{"linux/amd64", "linux/arm64"},
		StartedAt:     time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC),
		EndedAt:       time.Date(2026, 10, 8, 10, 5, 0, 0, time.UTC),
		AggregateLog:  Link{Label: "aggregate run.log", Target: "./run.log"},
		Rows: []Row{
			{Cluster: "c1", Provider: "github", Passed: true, Stage: "completed", Platforms: []string{"linux/amd64", "linux/arm64"},
				Links: []Link{{Label: "run.log", Target: "./c1/run.log"}, {Label: "c1/github/run.log", Target: "./c1/github/run.log"}}},
			{Cluster: "c1", Provider: "gitlab", Passed: true, Stage: "completed", Platforms: []string{"linux/amd64", "linux/arm64"},
				Links: []Link{{Label: "run.log", Target: "./c1/run.log"}, {Label: "c1/gitlab/run.log", Target: "./c1/gitlab/run.log"}}},
			{Cluster: "c2", Provider: "gitlab", Passed: false, Stage: "authentication",
				Error: "authentication failed: no usable kubeconfig at /home/u/.kube/config-c2"},
		},
	}
}

var _ = Describe("Row.Result", func() {
	DescribeTable("should render the outcome",
		func(row Row, want string) {
			Expect(row.Result()).Should(Equal(want))
		},
		Entry("passed row", Row{Passed: true}, "passed"),
		Entry("failed row", Row{Passed: false}, "failed"),
	)
})

var _ = Describe("Document.ArchitecturesText", func() {
	DescribeTable("should render the requested architectures",
		func(document Document, want string) {
			Expect(document.ArchitecturesText()).Should(Equal(want))
		},
		Entry("no architectures", Document{}, "none"),
		Entry("one architecture", Document{Architectures: []string{"linux/arm64"}}, "linux/arm64"),
		Entry("several architectures", Document{Architectures: []string{"linux/amd64", "linux/s390x"}}, "linux/amd64, linux/s390x"),
	)
})

var _ = Describe("Document.Clusters", func() {
	When("rows interleave providers for the same cluster", func() {
		It("should group them in first-seen cluster order", func() {
			clusters := reportDocument().Clusters()

			Expect(clusters).Should(HaveLen(2))
			Expect(clusters[0].Name).Should(Equal("c1"))
			Expect(clusters[0].Rows).Should(HaveLen(2))
			Expect(clusters[1].Name).Should(Equal("c2"))
			Expect(clusters[1].Rows).Should(HaveLen(1))
		})
	})

	When("the document has no rows", func() {
		It("should return no clusters", func() {
			Expect(Document{}.Clusters()).Should(BeEmpty())
		})
	})
})

var _ = Describe("ClusterView", func() {
	Describe("Result", func() {
		DescribeTable("should aggregate the provider rows",
			func(rows []Row, want string) {
				Expect(ClusterView{Name: "c1", Rows: rows}.Result()).Should(Equal(want))
			},
			Entry("no rows", nil, "passed"),
			Entry("every row passed", []Row{{Passed: true}, {Passed: true}}, "passed"),
			Entry("one row failed", []Row{{Passed: true}, {Passed: false}}, "failed"),
		)
	})

	Describe("Failures", func() {
		It("should render a single provider's error without naming it", func() {
			cluster := ClusterView{Name: "c1", Rows: []Row{{Provider: "github", Passed: false, Stage: "builds-verified", Error: "build failed"}}}
			Expect(cluster.Failures()).Should(Equal([]string{"build failed"}))
		})

		It("should name the provider when the cluster ran both", func() {
			cluster := ClusterView{Name: "c1", Rows: []Row{
				{Provider: "github", Passed: true, Stage: "completed"},
				{Provider: "gitlab", Passed: false, Stage: "builds-verified", Error: "build failed"},
			}}
			Expect(cluster.Failures()).Should(Equal([]string{"gitlab: build failed"}))
		})

		It("should report an identical failure once", func() {
			cluster := ClusterView{Name: "c1", Rows: []Row{
				{Provider: "github", Passed: false, Stage: "authentication", Error: "authentication failed"},
				{Provider: "gitlab", Passed: false, Stage: "authentication", Error: "authentication failed"},
			}}
			Expect(cluster.Failures()).Should(Equal([]string{"authentication failed"}))
		})

		It("should fall back to the stage when the error is empty", func() {
			cluster := ClusterView{Name: "c1", Rows: []Row{{Provider: "github", Passed: false, Stage: "preflight"}}}
			Expect(cluster.Failures()).Should(Equal([]string{"failed at preflight"}))
		})
	})

	Describe("Anchor", func() {
		DescribeTable("should keep identifier characters",
			func(name, want string) {
				Expect(ClusterView{Name: name}.Anchor()).Should(Equal(want))
			},
			Entry("cluster-file name", "kflux-rhel-p01", "cluster-kflux-rhel-p01"),
			Entry("dots and underscores", "c_1.prod", "cluster-c_1.prod"),
		)

		It("should suffix an unsafe name so sanitized names cannot collide", func() {
			unsafe := ClusterView{Name: `c1" onmouseover="x`}.Anchor()
			Expect(unsafe).Should(MatchRegexp(`^cluster-c1--onmouseover--x-[0-9a-f]{8}$`))
			Expect(unsafe).ShouldNot(Equal(ClusterView{Name: "c1--onmouseover--x"}.Anchor()))
		})

		It("should give different unsafe names different anchors", func() {
			Expect(ClusterView{Name: "a/b"}.Anchor()).ShouldNot(Equal(ClusterView{Name: "a-b"}.Anchor()))
		})
	})

	Describe("RunArchitecturesText", func() {
		It("should union the tested architectures", func() {
			cluster := ClusterView{Name: "c1", Rows: []Row{{Platforms: []string{"linux/amd64"}}, {Platforms: []string{"linux/amd64", "linux/arm64"}}}}
			Expect(cluster.RunArchitecturesText([]string{"linux/amd64", "linux/arm64"})).Should(Equal("linux/amd64, linux/arm64"))
		})

		It("should name a requested architecture the cluster skipped", func() {
			cluster := ClusterView{Name: "c1", Rows: []Row{{Platforms: []string{"linux/arm64"}}}}
			Expect(cluster.RunArchitecturesText([]string{"linux/amd64", "linux/arm64"})).Should(Equal("linux/arm64; missing linux/amd64"))
		})

		It("should report an unknown selection as not determined", func() {
			cluster := ClusterView{Name: "c1", Rows: []Row{{Passed: false}}}
			Expect(cluster.RunArchitecturesText([]string{"linux/amd64"})).Should(Equal("not determined"))
		})
	})

	Describe("Notes", func() {
		It("should keep distinct non-empty notes once", func() {
			cluster := ClusterView{Name: "c1", Rows: []Row{{Note: "first attempt deferred: admission pending"}, {Note: "first attempt deferred: admission pending"}, {Note: ""}}}
			Expect(cluster.Notes()).Should(Equal([]string{"first attempt deferred: admission pending"}))
		})
	})

	Describe("Links", func() {
		It("should deduplicate targets while keeping row order", func() {
			cluster := ClusterView{Name: "c1", Rows: []Row{
				{Links: []Link{{Label: "run.log", Target: "./c1/run.log"}, {Label: "a", Target: "./a"}}},
				{Links: []Link{{Label: "run.log", Target: "./c1/run.log"}, {Label: "b", Target: "./b"}}},
			}}
			Expect(cluster.Links()).Should(Equal([]Link{
				{Label: "run.log", Target: "./c1/run.log"},
				{Label: "a", Target: "./a"},
				{Label: "b", Target: "./b"},
			}))
		})

		It("should split log files from evidence", func() {
			cluster := ClusterView{Name: "c1", Rows: []Row{{Links: []Link{
				{Label: "run.log", Target: "./c1/run.log"},
				{Label: "run directory", Target: "./c1/run/"},
				{Label: "manifest.json", Target: "./c1/run/manifest.json"},
			}}}}
			Expect(cluster.LogLinks()).Should(Equal([]Link{{Label: "run.log", Target: "./c1/run.log"}}))
			Expect(cluster.ArtifactLinks()).Should(Equal([]Link{
				{Label: "run directory", Target: "./c1/run/"},
				{Label: "manifest.json", Target: "./c1/run/manifest.json"},
			}))
		})
	})
})

var _ = Describe("StartedAtText", func() {
	When("the time is in a non-UTC zone", func() {
		It("should render RFC 3339 in UTC", func() {
			document := Document{StartedAt: time.Date(2026, 10, 8, 13, 0, 0, 0, time.FixedZone("IDT", 3*3600))}
			Expect(document.StartedAtText()).Should(Equal("2026-10-08T10:00:00Z"))
		})
	})
})

var _ = Describe("EndedAtText", func() {
	When("the time is in a non-UTC zone", func() {
		It("should render RFC 3339 in UTC", func() {
			document := Document{EndedAt: time.Date(2026, 10, 8, 13, 5, 0, 0, time.FixedZone("IDT", 3*3600))}
			Expect(document.EndedAtText()).Should(Equal("2026-10-08T10:05:00Z"))
		})
	})
})

var _ = Describe("Write", func() {
	When("the document is valid", func() {
		It("should write run-named report.md and report.html", func() {
			directory := GinkgoT().TempDir()
			runName := "multi-run-10:46_8.10.2026_run-a"
			document := reportDocument()

			Expect(Write(directory, document, runName)).Should(Succeed())

			for _, name := range []string{runName + "-report.md", runName + "-report.html"} {
				info, err := os.Stat(filepath.Join(directory, name))
				Expect(err).ShouldNot(HaveOccurred())
				Expect(info.Mode().Perm()).Should(Equal(os.FileMode(0o640)))
			}
			markdown, err := os.ReadFile(filepath.Join(directory, runName+"-report.md"))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(string(markdown)).Should(ContainSubstring("# LKMABT aggregate run run-1"))
			Expect(string(markdown)).Should(ContainSubstring("- Mode: full"))
			Expect(string(markdown)).Should(ContainSubstring("- Fixtures: both"))
			html, err := os.ReadFile(filepath.Join(directory, runName+"-report.html"))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(string(html)).Should(ContainSubstring("<!DOCTYPE html>"))
			Expect(string(html)).Should(ContainSubstring("<li>Mode: full</li>"))
		})
	})
})
