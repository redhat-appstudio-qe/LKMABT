package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/workflow"
)

// defaultFrameInterval is how often a running step redraws its animation frame.
const defaultFrameInterval = 100 * time.Millisecond

// Console serializes status output, animated steps, and interactive prompts
// across concurrent provider runs. Out receives terminal output and Prompt asks
// the operator for confirmation.
//
// One writer lock orders every terminal write. In animated mode the live region
// is erased before a durable line is written and redrawn below it, so durable
// output and live rows never overwrite each other; writePermanentLocked is the
// usual path. A separate prompt lock guarantees at most one confirmation owns the
// terminal and buffers durable output until the prompt completes; Suspend pauses
// the animation but does not buffer.
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
	// pending holds durable lines buffered while a prompt owns the terminal.
	// Transient heartbeats are never buffered.
	pending bytes.Buffer
	partial []byte
	width   int

	active        []*Step
	frame         int
	spinning      bool
	frameDone     chan struct{}
	renderedLines int
	// frameInterval overrides the redraw period in tests.
	frameInterval time.Duration
}

// NewConsole returns a console bound to standard output and the interactive
// confirmation prompt. Steps animate only when standard output is a terminal,
// and color is used only in that animated mode and when NO_COLOR is unset.
func NewConsole() *Console {
	prompt := NewInteractiveConfirmationPrompt()
	animate := smartTerminal(os.Stdout)
	return &Console{
		Out:         os.Stdout,
		Prompt:      prompt,
		interactive: prompt.Interactive,
		animate:     animate,
		color:       animate && os.Getenv("NO_COLOR") == "",
		width:       terminalWidth(os.Stdout),
	}
}

// Interactive reports whether prompts and browser logins can be presented.
func (c *Console) Interactive() bool {
	return c != nil && c.interactive
}

// Close releases the live region when the run ends. Buffered and partial output
// is dropped and the animation goroutine is stopped so a canceled or failed run
// never replays stale progress after the run summary. It is safe to call on a
// nil console.
func (c *Console) Close() {
	if c == nil {
		return
	}
	c.outMu.Lock()
	defer c.outMu.Unlock()
	c.partial = nil
	c.pending.Reset()
	c.eraseLiveLocked()
	c.haltSpinnerLocked()
}

// Printf writes one serialized status line. Running steps are cleared and
// redrawn below the line. While a prompt owns the terminal the line is buffered
// and flushed after the prompt completes.
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
	line := fmt.Sprintf(format, args...)
	c.writePermanentLocked(strings.Count(line, "\n")+1, func() {
		fmt.Fprintln(c.Out, line)
	})
}

// Write serializes progress output from concurrent provider stages. Only
// complete lines reach the terminal; a trailing unterminated fragment is held
// until more data completes it, emitted by Suspend, or dropped by Close. While a
// prompt owns the terminal, output is buffered instead. It satisfies io.Writer.
func (c *Console) Write(p []byte) (int, error) {
	if c == nil || c.Out == nil {
		return len(p), nil
	}
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if c.promptActive {
		return c.pending.Write(p)
	}
	c.partial = append(c.partial, p...)
	index := bytes.LastIndexByte(c.partial, '\n')
	if index < 0 {
		return len(p), nil
	}
	complete := append([]byte(nil), c.partial[:index+1]...)
	c.partial = append(c.partial[:0], c.partial[index+1:]...)
	var writeErr error
	c.writePermanentLocked(permanentLines(complete), func() {
		_, writeErr = c.Out.Write(complete)
	})
	if writeErr != nil {
		return 0, writeErr
	}
	return len(p), nil
}

// StartStep begins one status step and returns nil on a nil console. In a
// terminal it draws an animated spinner; otherwise it emits a plain start line
// and no animation. While animated, a prompt or suspension defers drawing the
// step until output resumes.
func (c *Console) StartStep(description string) *Step {
	return c.startStep("", description)
}

// startStep begins a step with an optional provider label used by plain
// heartbeat lines.
func (c *Console) startStep(provider, description string) *Step {
	if c == nil || c.Out == nil {
		return nil
	}
	c.outMu.Lock()
	defer c.outMu.Unlock()
	step := &Step{console: c, description: description, provider: provider}
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
	c.drawLiveLocked()
	return step
}

// Result writes a completed step line without an animated start. It is used for
// work that already finished, such as per-cluster authentication failures, and
// is buffered while a prompt owns the terminal.
func (c *Console) Result(success bool, description string) {
	if c == nil || c.Out == nil {
		return
	}
	c.outMu.Lock()
	defer c.outMu.Unlock()
	line := resultLine(c.color, success, description)
	if c.promptActive {
		fmt.Fprintln(&c.pending, line)
		if len(c.active) == 0 {
			c.haltSpinnerLocked()
		}
		return
	}
	c.writePermanentLocked(1, func() {
		fmt.Fprintln(c.Out, line)
	})
}

// Suspend hides the live region while an external command writes directly to the
// terminal, such as a browser or device login. It completes any partial line and
// stops animation; concurrent progress still reaches the terminal but no live
// rows are drawn. The returned function restores the live region when the
// external command returns and is a no-op on a nil console.
func (c *Console) Suspend() func() {
	if c == nil {
		return func() {}
	}
	c.outMu.Lock()
	c.suspended = true
	c.flushPartialLocked()
	c.eraseLiveLocked()
	c.haltSpinnerLocked()
	c.outMu.Unlock()
	return func() {
		c.outMu.Lock()
		defer c.outMu.Unlock()
		c.suspended = false
		if len(c.active) > 0 && !c.promptActive {
			if !c.spinning {
				c.startSpinnerLocked()
			}
			c.drawLiveLocked()
		}
	}
}

// Confirm serializes prompts and buffers durable status lines until the prompt
// completes; heartbeat updates are dropped instead. It returns an error when no
// confirmation prompt is configured.
func (c *Console) Confirm(ctx context.Context, message string) (bool, error) {
	if c == nil || c.Prompt == nil {
		return false, fmt.Errorf("interactive confirmation is not configured")
	}
	return c.confirm(ctx, c.Prompt, message)
}

// confirm runs prompt while the live region is cleared. Durable output is
// buffered until the prompt completes and the live region is redrawn afterwards.
func (c *Console) confirm(ctx context.Context, prompt workflow.Confirmation, message string) (bool, error) {
	if c == nil || prompt == nil {
		return false, fmt.Errorf("interactive confirmation is not configured")
	}
	// A console passed as its own prompt would recurse into promptMu.
	if promptConsole, ok := prompt.(*Console); ok && promptConsole == c {
		prompt = c.Prompt
		if prompt == nil {
			return false, fmt.Errorf("interactive confirmation is not configured")
		}
		if nested, ok := prompt.(*Console); ok && nested == c {
			return false, fmt.Errorf("interactive confirmation is configured recursively")
		}
	}
	c.promptMu.Lock()
	defer c.promptMu.Unlock()
	c.outMu.Lock()
	c.flushPartialLocked()
	c.promptActive = true
	c.eraseLiveLocked()
	c.outMu.Unlock()
	approved, err := prompt.Confirm(ctx, message)
	c.outMu.Lock()
	c.promptActive = false
	if c.Out != nil && c.pending.Len() > 0 {
		if c.pending.Bytes()[c.pending.Len()-1] != '\n' {
			c.pending.WriteByte('\n')
		}
		_, _ = c.pending.WriteTo(c.Out)
	}
	if len(c.active) > 0 && !c.suspended {
		if !c.spinning {
			c.startSpinnerLocked()
		}
		c.drawLiveLocked()
	}
	c.outMu.Unlock()
	return approved, err
}

// writePermanentLocked completes any buffered partial line, clears the live
// region, writes durable output, and redraws the remaining steps. lines is the
// number of terminal rows write occupies, used to clear rows the shrunken live
// region leaves behind. Callers hold outMu.
func (c *Console) writePermanentLocked(lines int, write func()) {
	if len(c.partial) > 0 {
		partial := append([]byte(nil), c.partial...)
		c.partial = c.partial[:0]
		original := write
		lines++
		write = func() {
			_, _ = c.Out.Write(partial)
			fmt.Fprintln(c.Out)
			original()
		}
	}
	erased := c.eraseLiveLocked()
	write()
	if !c.spinning || len(c.active) == 0 {
		c.clearBelowLocked(erased - lines)
		return
	}
	c.drawLiveLocked()
	c.clearBelowLocked(erased - lines - len(c.active))
}

// flushPartialLocked completes a buffered unterminated line before a prompt or
// external command takes over the terminal. Callers hold outMu.
func (c *Console) flushPartialLocked() {
	if len(c.partial) == 0 {
		return
	}
	partial := append([]byte(nil), c.partial...)
	c.partial = c.partial[:0]
	c.writePermanentLocked(1, func() {
		_, _ = c.Out.Write(partial)
		fmt.Fprintln(c.Out)
	})
}

// permanentLines counts the terminal rows p occupies, treating a final
// unterminated fragment as one row.
func permanentLines(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	lines := bytes.Count(p, []byte{'\n'})
	if p[len(p)-1] != '\n' {
		lines++
	}
	return lines
}

// clearBelowLocked clears lines left below the live region after it shrinks and
// restores the cursor to the end of the visible block. Non-positive counts do
// nothing. Callers hold outMu.
func (c *Console) clearBelowLocked(lines int) {
	if lines <= 0 {
		return
	}
	for range lines {
		fmt.Fprint(c.Out, "\r\x1b[2K\n")
	}
	fmt.Fprintf(c.Out, "\x1b[%dA", lines)
}

// startSpinnerLocked starts the frame goroutine when animation is available:
// the console animates, is not suspended, and at least one step is active.
// Callers must not start it while a prompt owns the terminal. The goroutine
// redraws the live region every frame interval under outMu. Callers hold outMu.
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
					c.drawLiveLocked()
				}
				c.outMu.Unlock()
			}
		}
	}()
}

// haltSpinnerLocked stops the frame goroutine. The live region is left on screen
// for the caller to clear. Callers hold outMu.
func (c *Console) haltSpinnerLocked() {
	if !c.spinning {
		return
	}
	c.spinning = false
	close(c.frameDone)
	c.frameDone = nil
}

// drawLiveLocked advances the animation frame and renders every active step,
// one line per step, leaving the cursor on the line after the block. It is a
// no-op unless the spinner runs, a step is active, and no prompt or suspension
// is active. Autowrap is disabled for the whole block so a line wider than the
// terminal cannot consume an extra row that later erasing would miss. Callers
// hold outMu.
func (c *Console) drawLiveLocked() {
	if !c.spinning || len(c.active) == 0 || c.promptActive || c.suspended {
		return
	}
	previous := c.renderedLines
	if previous > 0 {
		fmt.Fprintf(c.Out, "\x1b[%dA", previous)
	}
	lines := len(c.active)
	maxLines := previous
	if lines > maxLines {
		maxLines = lines
	}
	frame := spinnerFrames[c.frame%len(spinnerFrames)]
	c.frame++
	if c.color {
		frame = "\x1b[36m" + frame + "\x1b[0m"
	}
	fmt.Fprint(c.Out, "\x1b[?7l")
	for index := range maxLines {
		fmt.Fprint(c.Out, "\r\x1b[2K")
		if index < lines {
			fmt.Fprintf(c.Out, spinnerLineFormat, frame, c.active[index].render())
		}
		fmt.Fprint(c.Out, "\n")
	}
	fmt.Fprint(c.Out, "\x1b[?7h")
	if maxLines > lines {
		fmt.Fprintf(c.Out, "\x1b[%dA", maxLines-lines)
	}
	c.renderedLines = lines
}

// eraseLiveLocked clears the live region, leaves the cursor at its first line,
// and returns the number of cleared lines. It assumes the cursor sits on the line
// after the block, where drawLiveLocked leaves it. Callers hold outMu.
func (c *Console) eraseLiveLocked() int {
	erased := c.renderedLines
	if erased == 0 {
		return 0
	}
	fmt.Fprintf(c.Out, "\x1b[%dA", erased)
	for range erased {
		fmt.Fprint(c.Out, "\r\x1b[2K\n")
	}
	fmt.Fprintf(c.Out, "\x1b[%dA", erased)
	c.renderedLines = 0
	return erased
}
