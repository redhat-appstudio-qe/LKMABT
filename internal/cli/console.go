package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/redhat-appstudio/lkmabt/internal/workflow"
)

// Console serializes status output and interactive prompts across concurrent provider runs.
type Console struct {
	outMu        sync.Mutex
	promptMu     sync.Mutex
	Out          io.Writer
	Prompt       workflow.Confirmation
	interactive  bool
	promptActive bool
	pending      bytes.Buffer
}

// NewConsole returns a console bound to standard output and the interactive confirmation prompt.
func NewConsole() *Console {
	prompt := NewInteractiveConfirmationPrompt()
	return &Console{Out: os.Stdout, Prompt: prompt, interactive: prompt.Interactive}
}

// Interactive reports whether prompts and browser logins can be presented.
func (c *Console) Interactive() bool {
	return c != nil && c.interactive
}

// Printf writes one serialized status line.
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
	fmt.Fprintf(c.Out, format+"\n", args...)
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
	return c.Out.Write(p)
}

// Confirm serializes prompts and buffers progress output until the prompt completes.
func (c *Console) Confirm(ctx context.Context, message string) (bool, error) {
	if c == nil || c.Prompt == nil {
		return false, fmt.Errorf("interactive confirmation is not configured")
	}
	c.promptMu.Lock()
	defer c.promptMu.Unlock()
	c.outMu.Lock()
	c.promptActive = true
	c.outMu.Unlock()
	approved, err := c.Prompt.Confirm(ctx, message)
	c.outMu.Lock()
	c.promptActive = false
	if c.Out != nil && c.pending.Len() > 0 {
		_, _ = c.pending.WriteTo(c.Out)
	}
	c.outMu.Unlock()
	return approved, err
}
