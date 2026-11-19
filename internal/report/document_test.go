package report

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Row.Result", func() {
	DescribeTable("should render the outcome",
		func(row Row, want string) {
			Expect(row.Result()).Should(Equal(want))
		},
		Entry("passed row", Row{Passed: true}, "passed"),
		Entry("failed row", Row{Passed: false}, "failed"),
	)
})

var _ = Describe("Document.Result", func() {
	DescribeTable("should render the outcome",
		func(document Document, want string) {
			Expect(document.Result()).Should(Equal(want))
		},
		Entry("passed document", Document{Passed: true}, "passed"),
		Entry("failed document", Document{Passed: false}, "failed"),
	)
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
			document := Document{
				RunID:        "run-1",
				ClustersFile: ".lkmabt-clusters.yaml",
				StartedAt:    time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC),
				EndedAt:      time.Date(2026, 10, 8, 10, 5, 0, 0, time.UTC),
				Passed:       false,
				Rows: []Row{
					{Cluster: "c1", Provider: "github", Passed: true, Stage: "completed"},
					{Cluster: "c2", Provider: "gitlab", Passed: false, Stage: "builds-verified", Error: "boom"},
				},
			}

			Expect(Write(directory, document, runName)).Should(Succeed())

			for _, name := range []string{runName + "-report.md", runName + "-report.html"} {
				info, err := os.Stat(filepath.Join(directory, name))
				Expect(err).ShouldNot(HaveOccurred())
				Expect(info.Mode().Perm()).Should(Equal(os.FileMode(0o640)))
			}
			markdown, err := os.ReadFile(filepath.Join(directory, runName+"-report.md"))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(string(markdown)).Should(ContainSubstring("# LKMABT aggregate run run-1"))
			html, err := os.ReadFile(filepath.Join(directory, runName+"-report.html"))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(string(html)).Should(ContainSubstring("<!DOCTYPE html>"))
		})
	})
})
