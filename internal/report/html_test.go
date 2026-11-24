package report

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("RenderHTML", func() {
	When("the document is valid", func() {
		It("should return a self-contained document with the run data", func() {
			html, err := RenderHTML(reportDocument())
			Expect(err).ShouldNot(HaveOccurred())
			Expect(html).Should(ContainSubstring("<!DOCTYPE html>"))
			Expect(html).Should(ContainSubstring("<title>LKMABT aggregate run run-1</title>"))
			Expect(html).Should(ContainSubstring("<li>Requested architectures: linux/amd64, linux/arm64</li>"))
			Expect(html).Should(ContainSubstring("<li>Mode: full</li>"))
			Expect(html).Should(ContainSubstring("<li>Fixtures: both</li>"))
			Expect(html).Should(ContainSubstring(`<li>Run log: <a href="./run.log">aggregate run.log</a></li>`))
			Expect(html).ShouldNot(ContainSubstring("Result:"))
			Expect(html).Should(ContainSubstring("<table>"))
			Expect(html).Should(ContainSubstring("<th>Cluster name</th><th>Pass/Fail</th><th>Failure reason</th><th>Run architectures</th>"))
		})

		It("should link a cluster to its run-logs section and render its architectures", func() {
			html, err := RenderHTML(reportDocument())
			Expect(err).ShouldNot(HaveOccurred())
			Expect(html).Should(ContainSubstring(`<td><a href="#cluster-c1">c1</a></td>`))
			Expect(html).Should(ContainSubstring(`<td>linux/amd64, linux/arm64</td>`))
		})

		It("should render a failed cluster's reason and unknown architecture selection", func() {
			html, err := RenderHTML(reportDocument())
			Expect(err).ShouldNot(HaveOccurred())
			Expect(html).Should(ContainSubstring("authentication failed: no usable kubeconfig at /home/u/.kube/config-c2"))
			Expect(html).Should(ContainSubstring(`<td>not determined</td>`))
		})

		It("should render one run-logs section per cluster with run.log first", func() {
			html, err := RenderHTML(reportDocument())
			Expect(err).ShouldNot(HaveOccurred())
			Expect(html).Should(ContainSubstring(`<section id="cluster-c1">`))
			Expect(html).Should(ContainSubstring(`<h3>c1 run logs</h3>`))
			Expect(html).Should(ContainSubstring(`<li><a href="./c1/run.log">run.log</a></li>`))
			Expect(html).Should(ContainSubstring(`<li><a href="./c1/github/run.log">c1/github/run.log</a></li>`))
			Expect(html).Should(ContainSubstring(`<p>none</p>`))
		})
	})

	When("a cluster has a log and evidence", func() {
		It("should list logs and keep evidence under an artifacts list", func() {
			document := Document{Rows: []Row{{Cluster: "c1", Provider: "github", Passed: true, Stage: "completed",
				Links: []Link{{Label: "run.log", Target: "./c1/run.log"}, {Label: "manifest.json", Target: "./c1/manifest.json"}}}}}

			html, err := RenderHTML(document)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(html).Should(ContainSubstring(`<li><a href="./c1/run.log">run.log</a></li>`))
			Expect(html).Should(ContainSubstring(`<p><strong>Artifacts:</strong></p>`))
			Expect(html).Should(ContainSubstring(`<li><a href="./c1/manifest.json">manifest.json</a></li>`))
		})
	})

	When("a cluster records a first-attempt note", func() {
		It("should render the note under its run-logs heading", func() {
			document := Document{Rows: []Row{{Cluster: "c1", Provider: "github", Passed: true, Stage: "completed", Note: "first attempt deferred: admission pending"}}}

			html, err := RenderHTML(document)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(html).Should(ContainSubstring(`<p class="note">Note: first attempt deferred: admission pending</p>`))
		})
	})

	When("the document records a run-level warning", func() {
		It("should render it in the run data", func() {
			document := Document{Warnings: []string{"credential cleanup failed: still present"}}

			html, err := RenderHTML(document)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(html).Should(ContainSubstring(`<li class="warning">Warning: credential cleanup failed: still present</li>`))
		})
	})

	When("a value contains markup", func() {
		It("should escape cluster names and error text", func() {
			document := Document{Rows: []Row{{Cluster: "<script>alert(1)</script>", Provider: "github", Passed: false, Error: "a & b <denied>"}}}

			html, err := RenderHTML(document)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(html).Should(ContainSubstring("&lt;script&gt;alert(1)&lt;/script&gt;"))
			Expect(html).Should(ContainSubstring("a &amp; b &lt;denied&gt;"))
			Expect(html).ShouldNot(ContainSubstring("<script"))
		})
	})

	When("clusters passed and failed", func() {
		It("should mark each result with its class", func() {
			html, err := RenderHTML(reportDocument())
			Expect(err).ShouldNot(HaveOccurred())
			Expect(html).Should(ContainSubstring(`<td class="passed">passed</td>`))
			Expect(html).Should(ContainSubstring(`<td class="failed">failed</td>`))
		})
	})

	It("should not reference scripts or external assets", func() {
		html, err := RenderHTML(reportDocument())
		Expect(err).ShouldNot(HaveOccurred())
		Expect(html).ShouldNot(ContainSubstring("javascript:"))
		Expect(html).ShouldNot(ContainSubstring("<script"))
		Expect(html).ShouldNot(ContainSubstring(`src="http`))
		Expect(html).ShouldNot(ContainSubstring("<link"))
	})
})

var _ = Describe("RenderHTML and RenderMarkdown parity", func() {
	It("should carry the same run identity, cluster, stage, and result values", func() {
		document := reportDocument()
		html, err := RenderHTML(document)
		Expect(err).ShouldNot(HaveOccurred())
		markdown := RenderMarkdown(document)

		for _, value := range []string{"run-1", "c1", "c2", "authentication", "passed", "failed", "linux/amd64", "linux/arm64", "not determined"} {
			Expect(html).Should(ContainSubstring(value), "html should contain %s", value)
			Expect(markdown).Should(ContainSubstring(value), "markdown should contain %s", value)
		}
	})
})
