package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing/iotest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ConfirmationPrompt", func() {
	It("accepts only explicit approval", func() {
		for _, answer := range []string{"y\n", "yes\n", "Y\n", "YES\n"} {
			prompt := NewConfirmationPrompt(strings.NewReader(answer), io.Discard, true)
			approved, err := prompt.Confirm(context.Background(), "delete tenant")
			Expect(err).NotTo(HaveOccurred())
			Expect(approved).To(BeTrue(), "answer %q", answer)
		}
	})

	It("refuses non-affirmative input", func() {
		for _, answer := range []string{"n\n", "no\n", "\n", "maybe\n"} {
			prompt := NewConfirmationPrompt(strings.NewReader(answer), io.Discard, true)
			approved, err := prompt.Confirm(context.Background(), "delete tenant")
			Expect(err).NotTo(HaveOccurred())
			Expect(approved).To(BeFalse(), "answer %q", answer)
		}
	})

	It("reports a non-interactive prompt as unavailable without printing", func() {
		var output bytes.Buffer
		prompt := NewConfirmationPrompt(strings.NewReader("y\n"), &output, false)

		approved, err := prompt.Confirm(context.Background(), "delete tenant")

		Expect(approved).To(BeFalse())
		Expect(errors.Is(err, ErrPromptUnavailable)).To(BeTrue())
		Expect(output.Len()).To(BeZero())
	})

	It("reports a nil input stream as unavailable without printing", func() {
		var output bytes.Buffer
		prompt := NewConfirmationPrompt(nil, &output, true)

		approved, err := prompt.Confirm(context.Background(), "delete tenant")

		Expect(approved).To(BeFalse())
		Expect(errors.Is(err, ErrPromptUnavailable)).To(BeTrue())
		Expect(output.Len()).To(BeZero())
	})

	It("reports EOF as unavailable and terminates the prompt line", func() {
		var output bytes.Buffer
		prompt := NewConfirmationPrompt(strings.NewReader(""), &output, true)

		approved, err := prompt.Confirm(context.Background(), "delete tenant")

		Expect(approved).To(BeFalse())
		Expect(errors.Is(err, ErrPromptUnavailable)).To(BeTrue())
		Expect(output.String()).To(ContainSubstring("delete tenant [y/N]: "))
		Expect(output.String()).To(HaveSuffix("\n"))
	})

	It("reports a read error as unavailable and terminates the prompt line", func() {
		var output bytes.Buffer
		prompt := NewConfirmationPrompt(iotest.ErrReader(errors.New("read failed")), &output, true)

		approved, err := prompt.Confirm(context.Background(), "delete tenant")

		Expect(approved).To(BeFalse())
		Expect(errors.Is(err, ErrPromptUnavailable)).To(BeTrue())
		Expect(output.String()).To(HaveSuffix("\n"))
	})

	It("reports the context error before prompting", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var output bytes.Buffer
		prompt := NewConfirmationPrompt(strings.NewReader("y\n"), &output, true)

		approved, err := prompt.Confirm(ctx, "delete tenant")

		Expect(approved).To(BeFalse())
		Expect(err).Should(MatchError(context.Canceled))
		Expect(output.Len()).To(BeZero())
	})
})
