package workflow

import (
	"bytes"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// chunkRecorder records every Write call as one chunk, so a spec can prove a
// complete alert block reaches a shared writer in one call.
type chunkRecorder struct {
	mu     sync.Mutex
	chunks []string
}

// Write records p as one chunk.
func (r *chunkRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chunks = append(r.chunks, string(p))
	return len(p), nil
}

// Chunks returns a copy of the recorded chunks.
func (r *chunkRecorder) Chunks() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.chunks...)
}

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

var _ = Describe("LiveStage alerts", func() {
	alertKey := "architecture:kflux-osp-p01:linux/s390x,linux/ppc64le->:linux/amd64,linux/arm64"
	alertTitle := "kflux-osp-p01: architecture support narrowed"
	alertLines := []string{
		"not supported: linux/s390x, linux/ppc64le",
		"testing: linux/amd64, linux/arm64",
	}
	alertBlock := " ! kflux-osp-p01: architecture support narrowed\n" +
		"   not supported: linux/s390x, linux/ppc64le\n" +
		"   testing: linux/amd64, linux/arm64\n"

	When("Out implements AlertSink", func() {
		It("should deliver one block to an alert sink and the plain block to the run log", func() {
			sink := &recordingAlertSink{}
			log := &bytes.Buffer{}
			stage := LiveStage{Out: sink, Log: log, NoticePrefix: "github run - "}
			lines := append([]string(nil), alertLines...)

			stage.alert(alertKey, alertTitle, lines...)
			lines[0] = "mutated after the call"

			Expect(sink.calls).Should(HaveLen(1))
			Expect(sink.calls[0].key).Should(Equal(alertKey))
			Expect(sink.calls[0].title).Should(Equal(alertTitle))
			Expect(sink.calls[0].lines).Should(Equal(alertLines))
			Expect(sink.written.String()).Should(BeEmpty())
			Expect(log.String()).Should(Equal(alertBlock))
		})
	})

	When("Out is a plain writer", func() {
		It("should write the plain block to a writer that is not an alert sink", func() {
			out := &bytes.Buffer{}
			log := &bytes.Buffer{}
			stage := LiveStage{Out: out, Log: log, NoticePrefix: "gitlab run - "}

			stage.alert(alertKey, alertTitle, alertLines...)

			Expect(out.String()).Should(Equal(alertBlock))
			Expect(log.String()).Should(Equal(alertBlock))
		})
	})

	When("two providers share a plain writer", func() {
		It("should keep each alert block intact", func() {
			out := &chunkRecorder{}
			firstLog := &bytes.Buffer{}
			secondLog := &bytes.Buffer{}
			first := LiveStage{Out: out, Log: firstLog}
			second := LiveStage{Out: out, Log: secondLog}
			firstTitle := "kflux-osp-p01: architecture support narrowed"
			secondTitle := "kflux-osp-p02: architecture support narrowed"
			firstBlock := " ! " + firstTitle + "\n" +
				"   not supported: linux/s390x\n" +
				"   testing: linux/amd64\n"
			secondBlock := " ! " + secondTitle + "\n" +
				"   not supported: linux/ppc64le\n" +
				"   testing: linux/arm64\n"

			var group sync.WaitGroup
			group.Add(2)
			go func() {
				defer group.Done()
				first.alert("architecture:kflux-osp-p01:linux/s390x->:linux/amd64", firstTitle,
					"not supported: linux/s390x", "testing: linux/amd64")
			}()
			go func() {
				defer group.Done()
				second.alert("architecture:kflux-osp-p02:linux/ppc64le->:linux/arm64", secondTitle,
					"not supported: linux/ppc64le", "testing: linux/arm64")
			}()
			group.Wait()

			Expect(out.Chunks()).Should(ConsistOf(firstBlock, secondBlock))
			Expect(firstLog.String()).Should(Equal(firstBlock))
			Expect(secondLog.String()).Should(Equal(secondBlock))
		})
	})

	When("Out is nil", func() {
		It("should keep the run log copy when Out is nil", func() {
			log := &bytes.Buffer{}
			stage := LiveStage{Log: log}

			stage.alert(alertKey, alertTitle, alertLines...)

			Expect(log.String()).Should(Equal(alertBlock))
		})
	})

	When("no writer is configured", func() {
		It("should do nothing when both writers are missing", func() {
			stage := LiveStage{}

			stage.alert(alertKey, alertTitle, alertLines...)
		})
	})
})
