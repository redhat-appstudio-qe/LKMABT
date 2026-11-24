package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// countingPrompt blocks inside Confirm until release is closed.
type countingPrompt struct {
	approved bool
	calls    int
	entered  chan struct{}
	release  chan struct{}
}

// Confirm signals entry, blocks on release, then records the call and returns the decision.
func (p *countingPrompt) Confirm(context.Context, string) (bool, error) {
	p.calls++
	if p.entered != nil {
		p.entered <- struct{}{}
		<-p.release
	}
	return p.approved, nil
}

// recordingPrompt records confirmation requests and returns the configured decision.
type recordingPrompt struct {
	approved bool
	calls    int
	messages []string
}

// Confirm records the message and returns the configured decision.
func (p *recordingPrompt) Confirm(_ context.Context, message string) (bool, error) {
	p.calls++
	p.messages = append(p.messages, message)
	return p.approved, nil
}

// overlapPrompt tracks whether Confirm calls ever run concurrently.
type overlapPrompt struct {
	inFlight *atomic.Int32
	overlaps *atomic.Int32
}

// Confirm records concurrent entries, holds the prompt briefly, then returns approval.
func (p *overlapPrompt) Confirm(context.Context, string) (bool, error) {
	if previous := p.inFlight.Add(1) - 1; previous != 0 {
		p.overlaps.Add(1)
	}
	time.Sleep(10 * time.Millisecond)
	p.inFlight.Add(-1)
	return true, nil
}

// cancelingPrompt blocks until released, then reports the context outcome.
type cancelingPrompt struct {
	entered chan struct{}
	release chan struct{}
}

// Confirm signals entry, waits for release, and returns the context error.
func (p *cancelingPrompt) Confirm(ctx context.Context, _ string) (bool, error) {
	close(p.entered)
	<-p.release
	return false, ctx.Err()
}

var _ = Describe("Console.Printf", func() {
	When("a writer is configured", func() {
		It("should write one terminated line", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out}
			console.Printf("cluster %s: started", "c1")
			Expect(out.String()).Should(Equal("cluster c1: started\n"))
		})
	})

	When("the console is nil", func() {
		It("should do nothing", func() {
			var console *Console
			Expect(func() { console.Printf("x") }).ShouldNot(Panic())
		})
	})

	When("Out is nil", func() {
		It("should do nothing", func() {
			Expect(func() { (&Console{}).Printf("x") }).ShouldNot(Panic())
		})
	})

	When("several goroutines print", func() {
		It("should keep every line whole", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out}
			var group sync.WaitGroup
			for range 8 {
				group.Add(2)
				go func() {
					defer group.Done()
					for range 25 {
						console.Printf("%s", strings.Repeat("A", 200))
					}
				}()
				go func() {
					defer group.Done()
					for range 25 {
						console.Printf("%s", strings.Repeat("B", 200))
					}
				}()
			}
			group.Wait()
			lines := strings.Split(out.String(), "\n")
			nonEmpty := 0
			for _, line := range lines {
				if line == "" {
					continue
				}
				nonEmpty++
				Expect(line).Should(SatisfyAny(Equal(strings.Repeat("A", 200)), Equal(strings.Repeat("B", 200))))
			}
			Expect(nonEmpty).Should(Equal(400))
		})
	})
})

var _ = Describe("Console.Confirm", func() {
	When("no prompt is configured", func() {
		It("should report misconfiguration", func() {
			console := &Console{Out: io.Discard}
			_, err := console.Confirm(context.Background(), "msg")
			Expect(err).Should(MatchError(ContainSubstring("interactive confirmation is not configured")))
		})
	})

	When("a prompt is configured", func() {
		It("should delegate the message and return its decision", func() {
			prompt := &recordingPrompt{approved: true}
			console := &Console{Out: io.Discard, Prompt: prompt}
			approved, err := console.Confirm(context.Background(), "delete tenant")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(approved).Should(BeTrue())
			Expect(prompt.calls).Should(Equal(1))
			Expect(prompt.messages).Should(ConsistOf("delete tenant"))
		})
	})

	When("the console is its own prompt", func() {
		It("should delegate to the configured prompt instead of recursing", func() {
			inner := &recordingPrompt{approved: true}
			console := &Console{Out: io.Discard, Prompt: inner}

			approved, err := console.confirm(context.Background(), console, "delete tenant")

			Expect(err).ShouldNot(HaveOccurred())
			Expect(approved).Should(BeTrue())
			Expect(inner.calls).Should(Equal(1))
		})

		It("should reject a recursively configured prompt", func() {
			console := &Console{Out: io.Discard}
			console.Prompt = console

			_, err := console.Confirm(context.Background(), "delete tenant")

			Expect(err).Should(MatchError(ContainSubstring("configured recursively")))
		})
	})

	When("one prompt is open", func() {
		It("should buffer status lines until the prompt completes", func() {
			prompt := &countingPrompt{approved: true, entered: make(chan struct{}), release: make(chan struct{})}
			out := &bytes.Buffer{}
			console := &Console{Out: out, Prompt: prompt}

			confirmDone := make(chan struct{})
			go func() {
				defer close(confirmDone)
				_, _ = console.Confirm(context.Background(), "delete tenant")
			}()
			<-prompt.entered

			printDone := make(chan struct{})
			go func() {
				defer close(printDone)
				console.Printf("cluster c1: started")
			}()
			Eventually(printDone, 100*time.Millisecond).Should(BeClosed())
			Expect(out.String()).ShouldNot(ContainSubstring("cluster c1: started"))

			close(prompt.release)
			Eventually(confirmDone).Should(BeClosed())
			Expect(out.String()).Should(ContainSubstring("cluster c1: started"))
		})

		It("should buffer direct writes until the prompt completes", func() {
			prompt := &countingPrompt{approved: true, entered: make(chan struct{}), release: make(chan struct{})}
			out := &bytes.Buffer{}
			console := &Console{Out: out, Prompt: prompt}

			confirmDone := make(chan struct{})
			go func() {
				defer close(confirmDone)
				_, _ = console.Confirm(context.Background(), "delete tenant")
			}()
			<-prompt.entered

			writeDone := make(chan struct{})
			go func() {
				defer close(writeDone)
				_, _ = console.Write([]byte("provider progress\n"))
			}()
			Eventually(writeDone, 100*time.Millisecond).Should(BeClosed())
			Expect(out.String()).ShouldNot(ContainSubstring("provider progress"))

			close(prompt.release)
			Eventually(confirmDone).Should(BeClosed())
			Expect(out.String()).Should(ContainSubstring("provider progress"))
		})
	})

	When("several prompts run concurrently", func() {
		It("should serialize them", func() {
			prompt := &overlapPrompt{inFlight: &atomic.Int32{}, overlaps: &atomic.Int32{}}
			console := &Console{Out: io.Discard, Prompt: prompt}
			var group sync.WaitGroup
			results := make([]bool, 8)
			for index := range 8 {
				group.Add(1)
				go func() {
					defer group.Done()
					approved, err := console.Confirm(context.Background(), "delete tenant")
					Expect(err).ShouldNot(HaveOccurred())
					results[index] = approved
				}()
			}
			group.Wait()
			Expect(prompt.overlaps.Load()).Should(BeZero())
			Expect(results).Should(ConsistOf(true, true, true, true, true, true, true, true))
		})
	})
})

var _ = Describe("Console.Alert", func() {
	const (
		alertKey   = "architecture:kflux-osp-p01:linux/s390x,linux/ppc64le->:linux/amd64,linux/arm64"
		alertTitle = "kflux-osp-p01: architecture support narrowed"
	)

	var alertLines []string

	BeforeEach(func() {
		alertLines = []string{
			"not supported: linux/s390x, linux/ppc64le",
			"testing: linux/amd64, linux/arm64",
		}
	})

	When("color is disabled", func() {
		It("should write the pinned plain block when color is disabled", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out}

			console.Alert(alertKey, alertTitle, alertLines...)

			Expect(out.String()).Should(Equal(" ! " + alertTitle + "\n" +
				"   not supported: linux/s390x, linux/ppc64le\n" +
				"   testing: linux/amd64, linux/arm64\n"))
			Expect(out.String()).ShouldNot(ContainSubstring("\x1b["))
		})
	})

	When("color is enabled", func() {
		It("should mark the title in amber and leave detail lines uncolored", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out, animate: true, color: true, frameInterval: time.Hour}

			console.Alert(alertKey, alertTitle, alertLines...)

			Expect(out.String()).Should(Equal(" \x1b[33m!\x1b[0m \x1b[33m" + alertTitle + "\x1b[0m\n" +
				"   not supported: linux/s390x, linux/ppc64le\n" +
				"   testing: linux/amd64, linux/arm64\n"))
		})
	})

	When("the same key repeats", func() {
		It("should write one block for a repeated key", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out}

			console.Alert(alertKey, alertTitle, alertLines...)
			console.Alert(alertKey, alertTitle, alertLines...)

			Expect(strings.Count(out.String(), alertTitle)).Should(Equal(1))
			Expect(strings.Count(out.String(), "not supported: linux/s390x, linux/ppc64le")).Should(Equal(1))
			Expect(strings.Count(out.String(), "testing: linux/amd64, linux/arm64")).Should(Equal(1))
		})
	})

	When("every key is distinct", func() {
		It("should write one block per distinct key", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out}

			console.Alert("k1", alertTitle, alertLines...)
			console.Alert("k2", alertTitle, alertLines...)

			Expect(strings.Count(out.String(), alertTitle)).Should(Equal(2))
			Expect(strings.Count(out.String(), "not supported: linux/s390x, linux/ppc64le")).Should(Equal(2))
			Expect(strings.Count(out.String(), "testing: linux/amd64, linux/arm64")).Should(Equal(2))
		})
	})

	When("goroutines share a key", func() {
		It("should write exactly one whole block when goroutines share a key", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out}

			var group sync.WaitGroup
			for range 8 {
				group.Add(1)
				go func() {
					defer group.Done()
					console.Alert(alertKey, alertTitle, alertLines...)
				}()
			}
			group.Wait()

			Expect(out.String()).Should(Equal(" ! " + alertTitle + "\n" +
				"   not supported: linux/s390x, linux/ppc64le\n" +
				"   testing: linux/amd64, linux/arm64\n"))
		})
	})

	When("an animated step is active", func() {
		It("should keep the alert block above the redrawn live region", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out, animate: true, frameInterval: time.Hour}
			step := console.StartStep("Preflight cluster (github) 🔍")

			console.Alert(alertKey, alertTitle, alertLines...)
			console.Printf("after alert")
			step.End(true)

			rendered := out.String()
			Expect(rendered).Should(ContainSubstring(" ! " + alertTitle + "\n" +
				"   not supported: linux/s390x, linux/ppc64le\n" +
				"   testing: linux/amd64, linux/arm64\n"))
			lastBlock := strings.LastIndex(rendered, "   testing: linux/amd64, linux/arm64")
			afterAlert := strings.Index(rendered, "after alert")
			Expect(strings.Index(rendered, " ! "+alertTitle)).Should(BeNumerically(">=", 0))
			Expect(afterAlert).Should(BeNumerically(">", lastBlock))
			Expect(strings.LastIndex(rendered[:afterAlert], "Preflight cluster (github) 🔍")).Should(BeNumerically(">", lastBlock))
		})
	})

	When("a prompt owns the terminal", func() {
		It("should buffer the block until a prompt completes", func() {
			prompt := &countingPrompt{approved: true, entered: make(chan struct{}), release: make(chan struct{})}
			out := &bytes.Buffer{}
			console := &Console{Out: out, Prompt: prompt}

			confirmDone := make(chan struct{})
			go func() {
				defer close(confirmDone)
				_, _ = console.Confirm(context.Background(), "delete tenant")
			}()
			<-prompt.entered

			alertDone := make(chan struct{})
			go func() {
				defer close(alertDone)
				console.Alert(alertKey, alertTitle, alertLines...)
			}()
			Eventually(alertDone, 100*time.Millisecond).Should(BeClosed())
			Expect(out.String()).ShouldNot(ContainSubstring(alertTitle))

			close(prompt.release)
			Eventually(confirmDone).Should(BeClosed())
			Expect(out.String()).Should(ContainSubstring(" ! " + alertTitle))
			Expect(out.String()).Should(ContainSubstring("   not supported: linux/s390x, linux/ppc64le"))
			Expect(out.String()).Should(ContainSubstring("   testing: linux/amd64, linux/arm64"))

			console.Printf("after prompt")
			Expect(strings.Index(out.String(), "after prompt")).Should(BeNumerically(">", strings.LastIndex(out.String(), "   testing: linux/amd64, linux/arm64")))
		})
	})

	When("the console is nil", func() {
		It("should do nothing on a nil console", func() {
			var console *Console
			Expect(func() { console.Alert(alertKey, alertTitle, alertLines...) }).ShouldNot(Panic())
		})
	})

	When("Out is nil", func() {
		It("should do nothing when Out is nil", func() {
			console := &Console{}
			Expect(func() { console.Alert(alertKey, alertTitle, alertLines...) }).ShouldNot(Panic())
			Expect(console.seenAlerts).Should(BeNil())
		})
	})

	When("another console already showed the key", func() {
		It("should not share deduplication between consoles", func() {
			out1 := &bytes.Buffer{}
			out2 := &bytes.Buffer{}
			console1 := &Console{Out: out1}
			console2 := &Console{Out: out2}

			console1.Alert(alertKey, alertTitle, alertLines...)
			console2.Alert(alertKey, alertTitle, alertLines...)

			block := " ! " + alertTitle + "\n" +
				"   not supported: linux/s390x, linux/ppc64le\n" +
				"   testing: linux/amd64, linux/arm64\n"
			Expect(out1.String()).Should(Equal(block))
			Expect(out2.String()).Should(Equal(block))
		})
	})
})

var _ = Describe("Console.Close", func() {
	When("an animated step is active", func() {
		It("should erase the live region and drop buffered output", func() {
			out := &bytes.Buffer{}
			console := &Console{Out: out, animate: true, frameInterval: time.Hour}
			console.StartStep("Verify builds (github) 🏗️")
			fmt.Fprint(&console.pending, "stale pending\n")
			console.partial = []byte("stale partial")
			Expect(console.renderedLines).Should(Equal(1))
			Expect(console.spinning).Should(BeTrue())

			console.Close()

			Expect(console.spinning).Should(BeFalse())
			Expect(console.renderedLines).Should(BeZero())
			Expect(console.pending.Len()).Should(BeZero())
			Expect(console.partial).Should(BeNil())
			Expect(out.String()).ShouldNot(ContainSubstring("stale"))
			Expect(out.String()).Should(HaveSuffix("\x1b[1A\r\x1b[2K\n\x1b[1A"))
		})
	})

	When("the console is nil", func() {
		It("should do nothing", func() {
			var console *Console
			Expect(func() { console.Close() }).ShouldNot(Panic())
		})
	})
})
