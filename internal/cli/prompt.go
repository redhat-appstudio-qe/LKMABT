package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

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
// prompt write is returned as an error. A non-interactive prompt, a nil In, an
// empty response, or a read error reports false with no error; a cancellation
// during the read does not interrupt it.
func (p ConfirmationPrompt) Confirm(ctx context.Context, message string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !p.Interactive || p.In == nil {
		return false, nil
	}
	if p.Out != nil {
		if _, err := fmt.Fprintf(p.Out, "%s [y/N]: ", message); err != nil {
			return false, err
		}
	}
	data, err := bufio.NewReader(p.In).ReadString('\n')
	if err != nil || len(data) == 0 {
		return false, nil
	}
	answer := strings.TrimSpace(string(data))
	return answer == "y" || answer == "yes" || answer == "Y" || answer == "YES", nil
}
