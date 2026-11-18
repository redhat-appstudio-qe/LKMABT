package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/workflow"
)

// defaultFrameInterval is how often a running step redraws its animation frame.
const defaultFrameInterval = 100 * time.Millisecond

// Console serializes status output, animated steps, and interactive prompts
// across concurrent provider runs.
type Console struct {
	outMu    sync.Mutex
	promptMu sync.Mutex

	Out         io.Writer
	Prompt      workflow.Confirmation
	interactive bool
	animate     bool
	color       bool

	promptActive bool
	suspended    bool
	pending      bytes.Buffer

	active    []*Step
	frame     int
	spinning  bool
	frameDone chan struct{}
	// frameInterval overrides the redraw period in tests.
	frameInterval time.Duration
}

// NewConsole returns a console bound to standard output and the interactive
// confirmation prompt. Steps animate only when standard output is a terminal.
func NewConsole() *Console {
	prompt := NewInteractiveConfirmationPrompt()
	animate := smartTerminal(os.Stdout)
	return &Console{
		Out:         os.Stdout,
		Prompt:      prompt,
		interactive: prompt.Interactive,
		animate:     animate,
		color:       animate && os.Getenv("NO_COLOR") == "",
	}
}

// Interactive reports whether prompts and browser logins can be presented.
func (c *Console) Interactive() bool {
	return c != nil && c.interactive
}

// Printf writes one serialized status line. A running spinner is interrupted
// and redrawn below the line.
func (c *Console) Printf(format string, args ...any) {
	if c == nil || c.Out == nil {
		return
	}
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if c.promptActive {
		fmt.Fprintf(&c.pending, format+"\n", args...)
		return
	}
	c.writeLineLocked(format, args...)
}

// Write serializes progress output from concurrent provider stages.
func (c *Console) Write(p []byte) (int, error) {
	if c == nil || c.Out == nil {
		return len(p), nil
	}
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if c.promptActive {
		return c.pending.Write(p)
	}
	if c.spinning {
		c.clearLineLocked()
	}
	written, err := c.Out.Write(p)
	if c.spinning {
		c.drawFrameLocked()
	}
	return written, err
}

// StartStep begins one status step. In a terminal it draws an animated spinner;
// otherwise it emits a plain start line and no animation.
func (c *Console) StartStep(description string) *Step {
	if c == nil || c.Out == nil {
		return nil
	}
	c.outMu.Lock()
	defer c.outMu.Unlock()
	step := &Step{console: c, description: description}
	c.active = append(c.active, step)
	if !c.animate {
		line := fmt.Sprintf(" • %s  ...", description)
		if c.promptActive {
			fmt.Fprintln(&c.pending, line)
		} else {
			fmt.Fprintln(c.Out, line)
		}
		return step
	}
	if c.promptActive || c.suspended {
		return step
	}
	if !c.spinning {
		c.startSpinnerLocked()
	}
	c.drawFrameLocked()
	return step
}

// Result writes a completed step line without an animated start. It is used for
// work that already finished, such as per-cluster authentication failures.
func (c *Console) Result(success bool, description string) {
	if c == nil || c.Out == nil {
		return
	}
	c.outMu.Lock()
	defer c.outMu.Unlock()
	line := resultLine(c.color, success, description)
	if c.promptActive {
		fmt.Fprintln(&c.pending, line)
		return
	}
	if c.spinning {
		c.clearLineLocked()
	}
	fmt.Fprintln(c.Out, line)
	if c.spinning {
		c.drawFrameLocked()
	}
}

// Suspend hides the spinner while an external command writes directly to the
// terminal, such as a browser or device login. The returned function restores
// progress output when the command returns.
func (c *Console) Suspend() func() {
	if c == nil {
		return func() {}
	}
	c.outMu.Lock()
	c.suspended = true
	if c.spinning {
		c.clearLineLocked()
		c.haltSpinnerLocked()
	}
	c.outMu.Unlock()
	return func() {
		c.outMu.Lock()
		defer c.outMu.Unlock()
		c.suspended = false
		if len(c.active) > 0 && !c.promptActive {
			if !c.spinning {
				c.startSpinnerLocked()
			}
			c.drawFrameLocked()
		}
	}
}

// Confirm serializes an approval through the console's configured prompt.
func (c *Console) Confirm(ctx context.Context, message string) (bool, error) {
	if c == nil || c.Prompt == nil {
		return false, fmt.Errorf("interactive confirmation is not configured")
	}
	return c.confirm(ctx, c.Prompt, message)
}

// confirm runs prompt while the spinner is cleared and concurrent progress is
// buffered until the prompt completes.
func (c *Console) confirm(ctx context.Context, prompt workflow.Confirmation, message string) (bool, error) {
	if c == nil || prompt == nil {
		return false, fmt.Errorf("interactive confirmation is not configured")
	}
	c.promptMu.Lock()
	defer c.promptMu.Unlock()
	c.outMu.Lock()
	c.promptActive = true
	if c.spinning {
		c.clearLineLocked()
	}
	c.outMu.Unlock()
	approved, err := prompt.Confirm(ctx, message)
	c.outMu.Lock()
	c.promptActive = false
	if c.Out != nil && c.pending.Len() > 0 {
		_, _ = c.pending.WriteTo(c.Out)
	}
	if len(c.active) > 0 && !c.suspended {
		if !c.spinning {
			c.startSpinnerLocked()
		}
		c.drawFrameLocked()
	}
	c.outMu.Unlock()
	return approved, err
}

// writeLineLocked writes one terminated line, interrupting the spinner first.
// Callers hold outMu.
func (c *Console) writeLineLocked(format string, args ...any) {
	if c.spinning {
		c.clearLineLocked()
	}
	fmt.Fprintf(c.Out, format+"\n", args...)
	if c.spinning {
		c.drawFrameLocked()
	}
}

// startSpinnerLocked starts the frame goroutine when animation is available.
// Callers hold outMu.
func (c *Console) startSpinnerLocked() {
	if !c.animate || c.spinning || c.suspended || len(c.active) == 0 || c.Out == nil {
		return
	}
	c.spinning = true
	done := make(chan struct{})
	c.frameDone = done
	interval := c.frameInterval
	if interval <= 0 {
		interval = defaultFrameInterval
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				c.outMu.Lock()
				if c.spinning && !c.promptActive && !c.suspended {
					c.drawFrameLocked()
				}
				c.outMu.Unlock()
			}
		}
	}()
}

// haltSpinnerLocked stops the frame goroutine. The current line is left for the
// caller to clear or overwrite. Callers hold outMu.
func (c *Console) haltSpinnerLocked() {
	if !c.spinning {
		return
	}
	c.spinning = false
	close(c.frameDone)
	c.frameDone = nil
}

// drawFrameLocked renders the newest active step over the current line.
// Callers hold outMu.
func (c *Console) drawFrameLocked() {
	if !c.spinning || len(c.active) == 0 {
		return
	}
	frame := spinnerFrames[c.frame%len(spinnerFrames)]
	c.frame++
	if c.color {
		frame = "\x1b[36m" + frame + "\x1b[0m"
	}
	fmt.Fprintf(c.Out, spinnerFormat, frame, c.active[len(c.active)-1].description)
}

// clearLineLocked erases the spinner line. Callers hold outMu.
func (c *Console) clearLineLocked() {
	fmt.Fprint(c.Out, "\r\x1b[2K")
}
