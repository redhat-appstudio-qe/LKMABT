package workflow

import (
	"bytes"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("LiveStage progress output", func() {
	It("routes heartbeats to the progress callback and logs the full line", func() {
		out := &bytes.Buffer{}
		log := &bytes.Buffer{}
		var lines []string
		stage := LiveStage{
			Out: out,
			Log: log,
			Progress: func(line string) {
				lines = append(lines, line)
			},
		}

		stage.progress("builds: web: buildah 0/2")

		Expect(out.String()).Should(BeEmpty())
		Expect(lines).Should(ConsistOf("builds: web: buildah 0/2"))
		Expect(log.String()).Should(Equal("builds: web: buildah 0/2\n"))
	})

	It("prints plain progress when no callback is configured", func() {
		out := &bytes.Buffer{}
		log := &bytes.Buffer{}
		stage := LiveStage{Out: out, Log: log, NoticePrefix: "github run - "}

		stage.progress("builds: web: buildah 0/2")

		Expect(out.String()).Should(Equal("github run - builds: web: buildah 0/2\n"))
		Expect(log.String()).Should(Equal("builds: web: buildah 0/2\n"))
	})

	It("prefixes durable notices but leaves the run log unprefixed", func() {
		out := &bytes.Buffer{}
		log := &bytes.Buffer{}
		stage := LiveStage{Out: out, Log: log, NoticePrefix: "gitlab run - "}

		stage.notice("warning: %d", 7)

		Expect(out.String()).Should(Equal("gitlab run - warning: 7\n"))
		Expect(log.String()).Should(Equal("warning: 7\n"))
	})
})
