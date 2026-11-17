package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/redhat-appstudio/lkmabt/internal/workflow"
)

// Console serializes status output and interactive prompts across concurrent provider runs.
type Console struct {
	mu          sync.Mutex
	Out         io.Writer
	Prompt      workflow.Confirmation
	interactive bool
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
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.Out, format+"\n", args...)
}

// Confirm serializes an interactive approval against status output and other clusters.
func (c *Console) Confirm(ctx context.Context, message string) (bool, error) {
	if c == nil || c.Prompt == nil {
		return false, fmt.Errorf("interactive confirmation is not configured")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Prompt.Confirm(ctx, message)
}
