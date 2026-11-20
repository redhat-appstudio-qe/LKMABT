package report

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// twoRowDocument builds the standard two-row markdown fixture.
func twoRowDocument() Document {
	return Document{
		RunID:         "run-1",
		ClustersFile:  ".lkmabt-clusters.yaml",
		Architectures: []string{"linux/amd64", "linux/arm64"},
		StartedAt:     time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC),
		EndedAt:       time.Date(2026, 10, 8, 10, 5, 0, 0, time.UTC),
		Passed:        false,
		Rows: []Row{
			{Cluster: "c1", Provider: "github", Passed: true, Stage: "completed", Links: []Link{{Label: "aggregate run.log", Target: "./run.log"}}},
			{Cluster: "c2", Provider: "gitlab", Passed: false, Stage: "authentication",
				Error: "authentication failed: no usable kubeconfig at /home/u/.kube/config-c2 after ibmcloud login"},
		},
	}
}

var _ = Describe("RenderMarkdown", func() {
	When("the document has passed and failed rows", func() {
		It("should render the header block", func() {
			rendered := RenderMarkdown(twoRowDocument())
			Expect(rendered).Should(ContainSubstring("# LKMABT aggregate run run-1"))
			Expect(rendered).Should(ContainSubstring("- Started: 2026-10-08T10:00:00Z"))
			Expect(rendered).Should(ContainSubstring("- Ended: 2026-10-08T10:05:00Z"))
			Expect(rendered).Should(ContainSubstring("- Clusters file: `.lkmabt-clusters.yaml`"))
			Expect(rendered).Should(ContainSubstring("- Architectures: linux/amd64, linux/arm64"))
			Expect(rendered).Should(ContainSubstring("- Result: **failed**"))
		})

		It("should render the table header", func() {
			rendered := RenderMarkdown(twoRowDocument())
			Expect(rendered).Should(ContainSubstring("| Cluster | Provider | Result | Stage | Error | Artifacts |\n| --- | --- | --- | --- | --- | --- |\n"))
		})

		It("should render a passed row with its artifact link", func() {
			rendered := RenderMarkdown(twoRowDocument())
			Expect(rendered).Should(ContainSubstring("| c1 | github | passed | completed |  | [aggregate run.log](<./run.log>) |"))
		})

		It("should render a failed row with stage and error text", func() {
			rendered := RenderMarkdown(twoRowDocument())
			Expect(rendered).Should(ContainSubstring("| c2 | gitlab | failed | authentication | authentication failed: no usable kubeconfig at /home/u/.kube/config-c2 after ibmcloud login | none |"))
		})
	})

	When("a cell contains a pipe", func() {
		It("should escape the pipe", func() {
			document := Document{Rows: []Row{{Cluster: "c|1", Provider: "github", Passed: true, Stage: "completed"}}}

			rendered := RenderMarkdown(document)

			Expect(rendered).Should(ContainSubstring(`| c\|1 | github | passed | completed |`))
		})
	})

	When("a cell spans lines", func() {
		It("should render line breaks as <br>", func() {
			document := Document{Rows: []Row{{Cluster: "c1", Provider: "github", Passed: true, Stage: "completed", Error: "line one\r\nline two"}}}

			rendered := RenderMarkdown(document)

			Expect(rendered).Should(ContainSubstring("| line one<br>line two |"))
			Expect(rendered).ShouldNot(ContainSubstring("line one\nline two"))
		})
	})

	When("a cell contains raw HTML", func() {
		It("should escape angle brackets and ampersands", func() {
			document := Document{Rows: []Row{{Cluster: "c1", Provider: "github", Passed: false, Stage: "builds-verified", Error: "<img src=x onerror=alert(1)>"}}}

			rendered := RenderMarkdown(document)

			Expect(rendered).Should(ContainSubstring(`&lt;img src=x onerror=alert\(1\)&gt;`))
			Expect(rendered).ShouldNot(ContainSubstring("<img"))
		})
	})

	When("a cell contains Markdown delimiters", func() {
		It("should escape them", func() {
			document := Document{Rows: []Row{{Cluster: "c1", Provider: "github", Passed: false, Stage: "builds-verified", Error: "[click](https://x) *bold* _u_"}}}

			rendered := RenderMarkdown(document)

			Expect(rendered).Should(ContainSubstring(`\[click\]\(https://x\)`))
			Expect(rendered).Should(ContainSubstring(`\*bold\*`))
			Expect(rendered).Should(ContainSubstring(`\_u\_`))
		})
	})

	When("a link label contains Markdown", func() {
		It("should escape the label only", func() {
			document := Document{Rows: []Row{{Cluster: "c1", Provider: "github", Passed: true, Stage: "completed",
				Links: []Link{{Label: "[a](b)", Target: "./a"}}}}}

			rendered := RenderMarkdown(document)

			Expect(rendered).Should(ContainSubstring(`[\[a\]\(b\)](<./a>)`))
		})
	})

	When("a row has several links", func() {
		It("should join them with commas", func() {
			document := Document{Rows: []Row{{Cluster: "c1", Provider: "github", Passed: true, Stage: "completed",
				Links: []Link{{Label: "a", Target: "./a"}, {Label: "b", Target: "./b/"}}}}}

			rendered := RenderMarkdown(document)

			Expect(rendered).Should(ContainSubstring("[a](<./a>), [b](<./b/>)"))
		})
	})

	When("every row passed", func() {
		It("should report a passed result", func() {
			document := Document{Passed: true, Rows: []Row{{Cluster: "c1", Provider: "github", Passed: true, Stage: "completed"}}}

			rendered := RenderMarkdown(document)

			Expect(rendered).Should(ContainSubstring("- Result: **passed**"))
		})
	})
})
