package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// ErrPromptUnavailable reports that a confirmation prompt could not ask the
// operator: input is not interactive, or reading the answer failed. Callers
// must not treat it as a declined answer.
var ErrPromptUnavailable = errors.New("confirmation prompt is unavailable")

// ConfirmationPrompt asks for approval over In and Out. Interactive enables
// prompting; when it is false, Confirm refuses without writing to Out.
type ConfirmationPrompt struct {
	In          io.Reader
	Out         io.Writer
	Interactive bool
}

// NewConfirmationPrompt returns a confirmation prompt using the supplied streams and mode.
func NewConfirmationPrompt(in io.Reader, out io.Writer, interactive bool) ConfirmationPrompt {
	return ConfirmationPrompt{In: in, Out: out, Interactive: interactive}
}

// NewInteractiveConfirmationPrompt creates a prompt backed by standard input and output.
func NewInteractiveConfirmationPrompt() ConfirmationPrompt {
	return NewConfirmationPrompt(os.Stdin, os.Stdout, stdinIsTerminal())
}

// stdinIsTerminal reports whether standard input is attached to a terminal.
func stdinIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// Confirm writes message followed by a [y/N] suffix and accepts only y, yes, Y,
// or YES. It reports the context error if ctx is already done, and a failed
// prompt write is returned as an error. Non-interactive input, a nil In, a read
// failure, or an empty read reports ErrPromptUnavailable; an empty line or any
// non-affirmative answer reports false with no error. A cancellation during the
// read does not interrupt it.
func (p ConfirmationPrompt) Confirm(ctx context.Context, message string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !p.Interactive {
		return false, fmt.Errorf("%w: standard input is not an interactive terminal", ErrPromptUnavailable)
	}
	if p.In == nil {
		return false, fmt.Errorf("%w: no input stream", ErrPromptUnavailable)
	}
	wrotePrompt := false
	if p.Out != nil {
		if _, err := fmt.Fprintf(p.Out, "%s [y/N]: ", message); err != nil {
			return false, fmt.Errorf("write confirmation prompt: %w", err)
		}
		wrotePrompt = true
	}
	data, err := bufio.NewReader(p.In).ReadString('\n')
	if err != nil {
		if wrotePrompt {
			_, _ = fmt.Fprintln(p.Out)
		}
		return false, fmt.Errorf("%w: %v", ErrPromptUnavailable, err)
	}
	if len(data) == 0 {
		if wrotePrompt {
			_, _ = fmt.Fprintln(p.Out)
		}
		return false, fmt.Errorf("%w: no response", ErrPromptUnavailable)
	}
	answer := strings.TrimSpace(string(data))
	return answer == "y" || answer == "yes" || answer == "Y" || answer == "YES", nil
}
