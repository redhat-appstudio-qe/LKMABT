package report

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("RenderMarkdown", func() {
	When("the document has passed and failed clusters", func() {
		It("should render the run data without an aggregate result", func() {
			rendered := RenderMarkdown(reportDocument())
			Expect(rendered).Should(ContainSubstring("# LKMABT aggregate run run-1"))
			Expect(rendered).Should(ContainSubstring("- Started: 2026-10-08T10:00:00Z"))
			Expect(rendered).Should(ContainSubstring("- Ended: 2026-10-08T10:05:00Z"))
			Expect(rendered).Should(ContainSubstring("- Clusters file: `.lkmabt-clusters.yaml`"))
			Expect(rendered).Should(ContainSubstring("- Requested architectures: linux/amd64, linux/arm64"))
			Expect(rendered).Should(ContainSubstring("- Mode: full"))
			Expect(rendered).Should(ContainSubstring("- Fixtures: both"))
			Expect(rendered).Should(ContainSubstring("- Run log: [aggregate run.log](<./run.log>)"))
			Expect(rendered).ShouldNot(ContainSubstring("Result:"))
		})

		It("should render the run-results table", func() {
			rendered := RenderMarkdown(reportDocument())
			Expect(rendered).Should(ContainSubstring("## Run results\n\n| Cluster name | Pass/Fail | Failure reason | Run architectures |\n| --- | --- | --- | --- |\n"))
		})

		It("should link a passed cluster to its run-logs section with its architectures", func() {
			rendered := RenderMarkdown(reportDocument())
			Expect(rendered).Should(ContainSubstring("| [c1](#cluster-c1) | passed |  | linux/amd64, linux/arm64 |"))
		})

		It("should render a failed cluster's reason and unknown architecture selection", func() {
			rendered := RenderMarkdown(reportDocument())
			Expect(rendered).Should(ContainSubstring("| [c2](#cluster-c2) | failed | authentication failed: no usable kubeconfig at /home/u/.kube/config-c2 | not determined |"))
		})

		It("should render one run-logs section per cluster with run.log first", func() {
			rendered := RenderMarkdown(reportDocument())
			Expect(rendered).Should(ContainSubstring("<a id=\"cluster-c1\"></a>\n### c1 run logs\n\n- [run.log](<./c1/run.log>)\n- [c1/github/run.log](<./c1/github/run.log>)\n- [c1/gitlab/run.log](<./c1/gitlab/run.log>)"))
			Expect(rendered).Should(ContainSubstring("<a id=\"cluster-c2\"></a>\n### c2 run logs\n\n- none"))
			Expect(strings.Index(rendered, "### c1 run logs")).Should(BeNumerically("<", strings.Index(rendered, "### c2 run logs")))
		})
	})

	When("a cluster records a first-attempt note", func() {
		It("should render the note under the cluster's run-logs heading", func() {
			document := Document{Rows: []Row{{Cluster: "c1", Provider: "github", Passed: true, Stage: "completed", Note: "first attempt deferred: admission pending"}}}

			rendered := RenderMarkdown(document)

			Expect(rendered).Should(ContainSubstring("### c1 run logs\n\n**Note:** first attempt deferred: admission pending\n"))
		})
	})

	When("the document records a run-level warning", func() {
		It("should render it in the run data", func() {
			document := Document{Warnings: []string{"credential cleanup failed: still present"}}

			Expect(RenderMarkdown(document)).Should(ContainSubstring("- Warning: credential cleanup failed: still present"))
		})
	})

	When("a failure reason contains a pipe", func() {
		It("should escape the pipe", func() {
			document := Document{Rows: []Row{{Cluster: "c1", Provider: "github", Passed: false, Error: "build | failed"}}}

			Expect(RenderMarkdown(document)).Should(ContainSubstring(`| build \| failed |`))
		})
	})

	When("a failure reason spans lines", func() {
		It("should render line breaks as <br>", func() {
			document := Document{Rows: []Row{
				{Cluster: "c1", Provider: "github", Passed: false, Error: "github failed"},
				{Cluster: "c1", Provider: "gitlab", Passed: false, Error: "gitlab failed"},
			}}

			rendered := RenderMarkdown(document)

			Expect(rendered).Should(ContainSubstring("| github: github failed<br>gitlab: gitlab failed |"))
			Expect(rendered).ShouldNot(ContainSubstring("github failed\ngitlab failed"))
		})
	})

	When("a failure reason contains raw HTML", func() {
		It("should escape angle brackets and ampersands", func() {
			document := Document{Rows: []Row{{Cluster: "c1", Provider: "github", Passed: false, Error: "<img src=x onerror=alert(1)>"}}}

			rendered := RenderMarkdown(document)

			Expect(rendered).Should(ContainSubstring(`&lt;img src=x onerror=alert(1)&gt;`))
			Expect(rendered).ShouldNot(ContainSubstring("<img"))
		})
	})

	When("a failure reason contains Markdown delimiters", func() {
		It("should escape them", func() {
			document := Document{Rows: []Row{{Cluster: "c1", Provider: "github", Passed: false, Error: "[click](https://x) *bold* _u_"}}}

			rendered := RenderMarkdown(document)

			Expect(rendered).Should(ContainSubstring(`\[click\](https://x)`))
			Expect(rendered).Should(ContainSubstring(`\*bold\*`))
			Expect(rendered).Should(ContainSubstring(`\_u\_`))
		})
	})

	When("a link label contains Markdown", func() {
		It("should escape the label only", func() {
			document := Document{Rows: []Row{{Cluster: "c1", Provider: "github", Passed: true, Stage: "completed",
				Links: []Link{{Label: "[a](b)", Target: "./a.log"}}}}}

			rendered := RenderMarkdown(document)

			Expect(rendered).Should(ContainSubstring(`- [\[a\](b)](<./a.log>)`))
		})
	})

	When("a cluster has a log and evidence", func() {
		It("should list the log and keep the evidence on an artifacts line", func() {
			document := Document{Rows: []Row{{Cluster: "c1", Provider: "github", Passed: true, Stage: "completed",
				Links: []Link{{Label: "run.log", Target: "./c1/run.log"}, {Label: "manifest.json", Target: "./c1/manifest.json"}, {Label: "run directory", Target: "./c1/run/"}}}}}

			rendered := RenderMarkdown(document)

			Expect(rendered).Should(ContainSubstring("- [run.log](<./c1/run.log>)\n"))
			Expect(rendered).Should(ContainSubstring("**Artifacts:**\n\n- [manifest.json](<./c1/manifest.json>)\n- [run directory](<./c1/run/>)\n"))
		})
	})
})
