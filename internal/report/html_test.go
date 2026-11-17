package report

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// htmlDocument builds the standard unsafe/escaped HTML fixture.
func htmlDocument() Document {
	return Document{
		RunID:        "run-1",
		ClustersFile: ".lkmabt-clusters.yaml",
		StartedAt:    time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC),
		EndedAt:      time.Date(2026, 10, 8, 10, 5, 0, 0, time.UTC),
		Passed:       false,
		Rows: []Row{
			{Cluster: "<script>alert(1)</script>", Provider: "github", Passed: true, Stage: "completed",
				Links: []Link{{Label: "run.log", Target: "./c1/run.log"}}},
			{Cluster: "c2", Provider: "gitlab", Passed: false, Stage: "authentication", Error: "a & b <denied>"},
		},
	}
}

var _ = Describe("RenderHTML", func() {
	When("the document is valid", func() {
		It("should return a self-contained document", func() {
			html, err := RenderHTML(htmlDocument())
			Expect(err).ShouldNot(HaveOccurred())
			Expect(html).Should(ContainSubstring("<!DOCTYPE html>"))
			Expect(html).Should(ContainSubstring("<title>LKMABT aggregate run run-1</title>"))
			Expect(html).Should(ContainSubstring("<table>"))
		})
	})

	When("a value contains markup", func() {
		It("should escape it", func() {
			html, err := RenderHTML(htmlDocument())
			Expect(err).ShouldNot(HaveOccurred())
			Expect(html).Should(ContainSubstring("&lt;script&gt;alert(1)&lt;/script&gt;"))
			Expect(html).ShouldNot(ContainSubstring("<script"))
		})

		It("should escape ampersands and angle brackets in error text", func() {
			html, err := RenderHTML(htmlDocument())
			Expect(err).ShouldNot(HaveOccurred())
			Expect(html).Should(ContainSubstring("a &amp; b &lt;denied&gt;"))
		})
	})

	When("a row has an artifact link", func() {
		It("should render relative artifact links", func() {
			html, err := RenderHTML(htmlDocument())
			Expect(err).ShouldNot(HaveOccurred())
			Expect(html).Should(ContainSubstring(`<a href="./c1/run.log">run.log</a>`))
		})
	})

	When("rows passed and failed", func() {
		It("should mark the aggregate and row results with their classes", func() {
			html, err := RenderHTML(htmlDocument())
			Expect(err).ShouldNot(HaveOccurred())
			Expect(html).Should(ContainSubstring(`<span class="failed">failed</span>`))
			Expect(html).Should(ContainSubstring(`<td class="passed">passed</td>`))
			Expect(html).Should(ContainSubstring(`<td class="failed">failed</td>`))
		})
	})

	When("a row has no links", func() {
		It("should render none", func() {
			html, err := RenderHTML(htmlDocument())
			Expect(err).ShouldNot(HaveOccurred())
			Expect(html).Should(ContainSubstring("<td>none</td>"))
		})
	})

	It("should not reference scripts or external assets", func() {
		html, err := RenderHTML(htmlDocument())
		Expect(err).ShouldNot(HaveOccurred())
		Expect(html).ShouldNot(ContainSubstring("javascript:"))
		Expect(html).ShouldNot(ContainSubstring("<script"))
		Expect(html).ShouldNot(ContainSubstring(`src="http`))
		Expect(html).ShouldNot(ContainSubstring("<link"))
	})
})

var _ = Describe("RenderHTML and RenderMarkdown parity", func() {
	It("should carry the same run identity, cluster, provider, stage, and result values", func() {
		document := htmlDocument()
		html, err := RenderHTML(document)
		Expect(err).ShouldNot(HaveOccurred())
		markdown := RenderMarkdown(document)

		for _, value := range []string{"run-1", "c1", "c2", "github", "gitlab", "completed", "authentication", "passed", "failed"} {
			Expect(html).Should(ContainSubstring(value), "html should contain %s", value)
			Expect(markdown).Should(ContainSubstring(value), "markdown should contain %s", value)
		}
	})
})
