package cli

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/config"
)

// prefixRecordingRunner records the console prefixes runProviders applies.
type prefixRecordingRunner struct {
	mu       sync.Mutex
	prefixes []string
}

// Run reports a completed provider without touching cluster services.
func (r *prefixRecordingRunner) Run(context.Context, config.Request, RunLog) (RunOutcome, error) {
	return RunOutcome{}, nil
}

// withPrefix records the applied prefix and returns the same runner.
func (r *prefixRecordingRunner) withPrefix(prefix string) Runner {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prefixes = append(r.prefixes, prefix)
	return r
}

var _ = Describe("providerPrefixWriter", func() {
	It("should prefix every line in one write", func() {
		out := &bytes.Buffer{}
		writer := &providerPrefixWriter{prefix: "github run - ", out: out}

		written, err := writer.Write([]byte("first\nsecond\n"))

		Expect(err).ShouldNot(HaveOccurred())
		Expect(written).Should(Equal(len("first\nsecond\n")))
		Expect(out.String()).Should(Equal("github run - first\ngithub run - second\n"))
	})

	It("should hold a partial line until its newline arrives", func() {
		out := &bytes.Buffer{}
		writer := &providerPrefixWriter{prefix: "gitlab run - ", out: out}

		_, err := writer.Write([]byte("par"))
		Expect(err).ShouldNot(HaveOccurred())
		Expect(out.String()).Should(BeEmpty())

		_, err = writer.Write([]byte("tial\n"))
		Expect(err).ShouldNot(HaveOccurred())
		Expect(out.String()).Should(Equal("gitlab run - partial\n"))
	})

	It("should accept output when no console is configured", func() {
		writer := &providerPrefixWriter{prefix: "github run - "}

		written, err := writer.Write([]byte("line\n"))

		Expect(err).ShouldNot(HaveOccurred())
		Expect(written).Should(Equal(len("line\n")))
	})
})

var _ = Describe("LiveRunner.withPrefix", func() {
	It("should label console output while keeping the run log separate", func() {
		out := &bytes.Buffer{}
		runner := LiveRunner{Out: out}.withPrefix("github run - ").(LiveRunner)

		fmt.Fprintln(runner.Out, "waiting for PaC onboarding")

		Expect(out.String()).Should(Equal("github run - waiting for PaC onboarding\n"))
	})
})

var _ = Describe("runProviders console prefixes", func() {
	It("should prefix each provider in a both run", func() {
		runner := &prefixRecordingRunner{}
		request := config.Request{Provider: config.ProviderBoth, StateDir: GinkgoT().TempDir()}

		runProviders(context.Background(), request, runner)

		Expect(runner.prefixes).Should(ConsistOf("github run - ", "gitlab run - "))
	})

	It("should leave a single-provider run unprefixed", func() {
		runner := &prefixRecordingRunner{}
		request := config.Request{Provider: config.ProviderGitHub, StateDir: GinkgoT().TempDir()}

		runProviders(context.Background(), request, runner)

		Expect(runner.prefixes).Should(BeEmpty())
	})
})
