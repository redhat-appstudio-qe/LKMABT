package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/workflow"
)

var _ = Describe("Console steps", func() {
	When("output is not a terminal", func() {
		It("should print plain start and success lines", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out}

			step := console.StartStep("Preflight cluster (github) 🔍")
			step.End(true)

			Expect(out.String()).Should(Equal(
				" • Preflight cluster (github) 🔍  ...\n" +
					" ✓ Preflight cluster (github) 🔍\n"))
		})

		It("should print a failure line", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out}

			step := console.StartStep("Verify builds (gitlab) 🏗️")
			step.End(false)

			Expect(out.String()).Should(ContainSubstring(" ✗ Verify builds (gitlab) 🏗️\n"))
			Expect(out.String()).ShouldNot(ContainSubstring("\x1b["))
		})

		It("should keep concurrent steps on separate lines", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out}

			first := console.StartStep("Trigger components (github) 🚀")
			second := console.StartStep("Trigger components (gitlab) 🚀")
			first.End(true)
			second.End(true)

			Expect(out.String()).Should(ContainSubstring(" ✓ Trigger components (github) 🚀\n"))
			Expect(out.String()).Should(ContainSubstring(" ✓ Trigger components (gitlab) 🚀\n"))
			Expect(strings.Count(out.String(), "\r")).Should(BeZero())
		})

		It("should print heartbeat detail as one plain line", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out}

			step := console.startStep("github", "Verify builds (github) 🏗️")
			step.Update("builds: web: buildah 0/2")

			Expect(out.String()).Should(ContainSubstring("github run - builds: web: buildah 0/2\n"))
			Expect(out.String()).ShouldNot(ContainSubstring("\x1b["))
			step.End(true)
		})

		It("should hold a partial write until its newline arrives", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out}

			step := console.StartStep("Verify builds 🏗️")
			written, err := console.Write([]byte("partial"))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(written).Should(Equal(len("partial")))
			Expect(out.String()).ShouldNot(ContainSubstring("partial"))

			_, err = console.Write([]byte(" line\n"))
			Expect(err).ShouldNot(HaveOccurred())
			Expect(out.String()).Should(ContainSubstring("partial line\n"))
			step.End(true)
		})
	})

	When("output is a terminal", func() {
		It("should animate in place and replace the line with a green checkmark", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out, animate: true, color: true, frameInterval: time.Hour}

			step := console.StartStep("Verify architectures (github) 🧪")
			Expect(out.String()).Should(ContainSubstring("\r\x1b[2K"))
			Expect(out.String()).Should(ContainSubstring("Verify architectures (github) 🧪"))

			step.End(true)

			Expect(out.String()).Should(ContainSubstring("\r\x1b[2K"))
			Expect(out.String()).Should(ContainSubstring(" \x1b[32m✓\x1b[0m Verify architectures (github) 🧪\n"))
			Expect(console.spinning).Should(BeFalse())
		})

		It("should redraw the remaining active step when one finishes", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out, animate: true, frameInterval: time.Hour}

			first := console.StartStep("Preflight cluster (github) 🔍")
			second := console.StartStep("Preflight cluster (gitlab) 🔍")
			second.End(true)

			text := out.String()
			Expect(text).Should(ContainSubstring(" ✓ Preflight cluster (gitlab) 🔍\n"))
			Expect(strings.LastIndex(text, "Preflight cluster (github) 🔍")).Should(
				BeNumerically(">", strings.Index(text, "✓ Preflight cluster (gitlab) 🔍")))

			first.End(true)
			Expect(console.spinning).Should(BeFalse())
		})

		It("should interrupt the live region for log lines and redraw it", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out, animate: true, frameInterval: time.Hour}

			step := console.StartStep("Verify builds (github) 🏗️")
			console.Printf("waiting for component graph")

			text := out.String()
			Expect(text).Should(ContainSubstring("\r\x1b[2K"))
			Expect(text).Should(ContainSubstring("waiting for component graph\n"))
			Expect(strings.LastIndex(text, "Verify builds (github) 🏗️")).Should(
				BeNumerically(">", strings.Index(text, "waiting for component graph")))
			step.End(true)
		})

		It("should print an overlong heartbeat durably and clip the live line", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out, animate: true, width: 50, frameInterval: time.Hour}

			step := console.StartStep("Verify builds (github) 🏗️")
			detail := strings.TrimSpace(strings.Repeat("web: buildah 0/2; ", 8))
			step.Update(detail)

			text := out.String()
			Expect(text).Should(ContainSubstring(detail))
			Expect(text).Should(ContainSubstring("…"))
			step.End(true)
		})

		It("should keep newline-bearing detail on one live row", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out, animate: true, frameInterval: time.Hour}

			step := console.StartStep("Preflight cluster (github) 🔍")
			step.Update("last check failed\nretrying")

			text := out.String()
			Expect(text).Should(ContainSubstring("last check failed retrying"))
			Expect(text).ShouldNot(ContainSubstring("last check failed\nretrying"))
			step.End(true)
		})

		It("should not replay overlong heartbeat history after a prompt", func() {
			prompt := &countingPrompt{approved: true, entered: make(chan struct{}), release: make(chan struct{})}
			out := &bytes.Buffer{}
			console := &Console{Out: out, Prompt: prompt, animate: true, width: 60, frameInterval: time.Hour}
			step := console.StartStep("Verify builds (github) 🏗️")

			confirmDone := make(chan struct{})
			go func() {
				defer close(confirmDone)
				_, _ = console.Confirm(context.Background(), "delete tenant")
			}()
			<-prompt.entered

			padding := strings.TrimSpace(strings.Repeat("web: buildah 0/2; ", 6))
			for attempt := range 4 {
				step.Update(fmt.Sprintf("attempt-%d %s", attempt, padding))
			}
			Expect(out.String()).ShouldNot(ContainSubstring("attempt-"))

			close(prompt.release)
			Eventually(confirmDone).Should(BeClosed())

			text := out.String()
			for _, stale := range []string{"attempt-0", "attempt-1", "attempt-2"} {
				Expect(text).ShouldNot(ContainSubstring(stale))
			}
			Expect(strings.Count(text, "attempt-")).Should(Equal(1))
			Expect(text).Should(ContainSubstring("attempt-3"))

			step.End(true)
		})

		It("should print a durable block above all active steps", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out, animate: true, frameInterval: time.Hour}

			first := console.StartStep("Verify builds (github) 🏗️")
			second := console.StartStep("Verify builds (gitlab) 🏗️")
			console.Printf("GitHub App is not installed.\nTo complete the installation:\n  1. Approve it.")

			text := out.String()
			blockEnd := strings.Index(text, "  1. Approve it.")
			Expect(blockEnd).Should(BeNumerically(">=", 0))
			Expect(strings.LastIndex(text, "Verify builds (github) 🏗️")).Should(BeNumerically(">", blockEnd))
			Expect(strings.LastIndex(text, "Verify builds (gitlab) 🏗️")).Should(BeNumerically(">", blockEnd))

			first.End(true)
			second.End(true)
		})

		It("should update one active line without adding another", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out, animate: true, frameInterval: time.Hour}

			first := console.StartStep("Verify builds (github) 🏗️")
			second := console.StartStep("Verify builds (gitlab) 🏗️")
			first.Update("web: buildah 0/2")
			second.Update("web: running 1m13s")

			text := out.String()
			Expect(text).Should(ContainSubstring("Verify builds (github) 🏗️ · web: buildah 0/2"))
			Expect(text).Should(ContainSubstring("Verify builds (gitlab) 🏗️ · web: running 1m13s"))
			Expect(strings.Count(text, " ✓ ")).Should(BeZero())

			first.End(true)
			second.End(true)
		})

		It("should pause animation while an external command owns the terminal", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out, animate: true, frameInterval: time.Hour}

			step := console.StartStep("Authenticate provider credentials 🔑")
			resume := console.Suspend()
			Expect(console.spinning).Should(BeFalse())

			fmt.Fprint(out, "browser login output\n")
			resume()
			Expect(console.spinning).Should(BeTrue())

			step.End(true)
			Expect(console.spinning).Should(BeFalse())
		})

		It("should not animate a step started while suspended", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out, animate: true, frameInterval: time.Hour}

			resume := console.Suspend()
			step := console.StartStep("Authenticate provider credentials 🔑")
			Expect(console.spinning).Should(BeFalse())

			resume()
			Expect(console.spinning).Should(BeTrue())
			step.End(true)
		})
	})

	When("a step ends deferred", func() {
		It("should use the pause marker instead of the failure marker", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out}

			step := console.StartStep("Verify builds (github) 🏗️")
			step.EndDeferred("admission pending")

			Expect(out.String()).Should(ContainSubstring(" ⏸ Verify builds (github) 🏗️ - deferred (admission pending)\n"))
			Expect(out.String()).ShouldNot(ContainSubstring("✗"))
			Expect(out.String()).ShouldNot(ContainSubstring("\x1b["))
		})
	})

	When("a prompt is open", func() {
		It("should buffer plain step lines until the prompt completes", func() {
			prompt := &countingPrompt{approved: true, entered: make(chan struct{}), release: make(chan struct{})}
			out := &bytes.Buffer{}
			console := &Console{Out: out, Prompt: prompt}

			confirmDone := make(chan struct{})
			go func() {
				defer close(confirmDone)
				_, _ = console.Confirm(context.Background(), "delete tenant")
			}()
			<-prompt.entered

			step := console.StartStep("Prepare fixture (github) 🧰")
			step.End(true)
			Expect(out.String()).ShouldNot(ContainSubstring("Prepare fixture (github) 🧰"))

			close(prompt.release)
			Eventually(confirmDone).Should(BeClosed())
			Expect(out.String()).Should(ContainSubstring(" • Prepare fixture (github) 🧰  ..."))
			Expect(out.String()).Should(ContainSubstring(" ✓ Prepare fixture (github) 🧰"))
		})

		It("should buffer an animated step result until the prompt completes", func() {
			prompt := &countingPrompt{approved: true, entered: make(chan struct{}), release: make(chan struct{})}
			out := &bytes.Buffer{}
			console := &Console{Out: out, Prompt: prompt, animate: true, frameInterval: time.Hour}

			step := console.StartStep("Preflight cluster (gitlab) 🔍")
			confirmDone := make(chan struct{})
			go func() {
				defer close(confirmDone)
				_, _ = console.Confirm(context.Background(), "delete tenant")
			}()
			<-prompt.entered
			step.End(true)
			Expect(out.String()).ShouldNot(ContainSubstring("✓ Preflight cluster (gitlab) 🔍"))

			close(prompt.release)
			Eventually(confirmDone).Should(BeClosed())
			Expect(out.String()).Should(ContainSubstring("✓ Preflight cluster (gitlab) 🔍"))
		})

		It("should not replay plain heartbeats after the prompt", func() {
			prompt := &countingPrompt{approved: true, entered: make(chan struct{}), release: make(chan struct{})}
			out := &bytes.Buffer{}
			console := &Console{Out: out, Prompt: prompt}
			step := console.StartStep("Verify builds (github) 🏗️")

			confirmDone := make(chan struct{})
			go func() {
				defer close(confirmDone)
				_, _ = console.Confirm(context.Background(), "delete tenant")
			}()
			<-prompt.entered

			for attempt := range 3 {
				step.Update(fmt.Sprintf("attempt-%d", attempt))
			}
			console.Printf("durable line")
			Expect(out.String()).ShouldNot(ContainSubstring("attempt-"))
			Expect(out.String()).ShouldNot(ContainSubstring("durable line"))

			close(prompt.release)
			Eventually(confirmDone).Should(BeClosed())

			text := out.String()
			Expect(text).ShouldNot(ContainSubstring("attempt-"))
			Expect(text).Should(ContainSubstring("durable line\n"))
			step.End(true)
		})

		It("should drop heartbeats when a canceled prompt releases", func() {
			ctx, cancel := context.WithCancel(context.Background())
			prompt := &cancelingPrompt{entered: make(chan struct{}), release: make(chan struct{})}
			out := &bytes.Buffer{}
			console := &Console{Out: out, Prompt: prompt}
			step := console.StartStep("Prepare fixture (github) 🧰")

			confirmDone := make(chan struct{})
			go func() {
				defer close(confirmDone)
				_, _ = console.Confirm(ctx, "delete tenant")
			}()
			<-prompt.entered

			for attempt := range 3 {
				step.Update(fmt.Sprintf("attempt-%d", attempt))
			}
			console.Printf("durable line")
			Expect(out.String()).ShouldNot(ContainSubstring("attempt-"))

			cancel()
			close(prompt.release)
			Eventually(confirmDone).Should(BeClosed())

			step.End(false)

			text := out.String()
			Expect(text).ShouldNot(ContainSubstring("attempt-"))
			Expect(text).Should(ContainSubstring("durable line\n"))
			Expect(strings.Count(text, "✗ Prepare fixture (github) 🧰")).Should(Equal(1))
		})
	})

	When("a step ends twice", func() {
		It("should write one result line", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out}
			step := console.StartStep("Finish run 🎉")

			step.End(true)
			step.End(false)

			Expect(strings.Count(out.String(), "✓ Finish run 🎉")).Should(Equal(1))
			Expect(out.String()).ShouldNot(ContainSubstring("✗"))
		})
	})
})

var _ = Describe("phaseDescription", func() {
	DescribeTable("describing workflow phases",
		func(phase model.Phase, provider, expected string) {
			Expect(phaseDescription(phase, provider)).Should(Equal(expected))
		},
		Entry("preflight", model.PhasePreflight, "github", "Preflight cluster (github) 🔍"),
		Entry("preflight single", model.PhasePreflight, "", "Preflight cluster 🔍"),
		Entry("fixture", model.PhaseFixtureReady, "gitlab", "Prepare fixture (gitlab) 🧰"),
		Entry("trigger", model.PhaseTriggered, "github", "Trigger components (github) 🚀"),
		Entry("builds", model.PhaseBuildsVerified, "github", "Verify builds (github) 🏗️"),
		Entry("outputs", model.PhaseBuildOutputsVerified, "gitlab", "Verify architectures (gitlab) 🧪"),
		Entry("completed", model.PhaseCompleted, "", "Finish run 🎉"),
	)
})

var _ = Describe("newPhaseObserver", func() {
	It("should emit steps when the writer is a console", func() {
		out := &bytes.Buffer{}
		observer := newPhaseObserver(&Console{Out: out}, "github")

		observer.start(model.PhasePreflight)
		observer.end(model.PhasePreflight, nil)

		Expect(out.String()).Should(ContainSubstring(" • Preflight cluster (github) 🔍  ..."))
		Expect(out.String()).Should(ContainSubstring(" ✓ Preflight cluster (github) 🔍"))
	})

	It("should be a no-op for other writers", func() {
		Expect(newPhaseObserver(io.Discard, "github")).Should(BeNil())
	})

	It("should end a deferred phase with the pause marker", func() {
		out := &bytes.Buffer{}
		observer := newPhaseObserver(&Console{Out: out}, "github")

		observer.start(model.PhaseBuildsVerified)
		observer.end(model.PhaseBuildsVerified, &workflow.AdmissionPendingError{Namespace: "tenant"})

		Expect(out.String()).Should(ContainSubstring(" ⏸ Verify builds (github) 🏗️ - deferred (admission pending)\n"))
		Expect(out.String()).ShouldNot(ContainSubstring("✗"))
	})
})
