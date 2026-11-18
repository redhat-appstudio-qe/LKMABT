package cli

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/redhat-appstudio/lkmabt/internal/model"
	"golang.org/x/term"
)

// spinnerFrames is the braille animation kind uses for its loading spinner.
var spinnerFrames = []string{
	"⠈⠁", "⠈⠑", "⠈⠱", "⠈⡱", "⢀⡱", "⢄⡱", "⢄⡱", "⢆⡱",
	"⢎⡱", "⢎⡰", "⢎⡠", "⢎⡀", "⢎⠁", "⠎⠁", "⠊⠁",
}

// spinnerFormat rewrites the spinner line in place. Wrapping is disabled while
// the frame is drawn so a line wider than the terminal cannot break \r update.
var spinnerFormat = "\x1b[?7l\r\x1b[2K%s %s\x1b[?7h"

func init() {
	// Old Windows consoles handle only the simplest escape sequences.
	if runtime.GOOS == "windows" {
		spinnerFormat = "\r\x1b[2K%s %s"
	}
}

// Step is one status line that starts as an animated spinner and is replaced by
// a success or failure marker when it ends.
type Step struct {
	console     *Console
	description string
	done        bool
}

// End completes the step and overwrites its spinner line. It is safe to call
// more than once and on a nil step.
func (s *Step) End(success bool) {
	if s == nil || s.console == nil {
		return
	}
	c := s.console
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if s.done {
		return
	}
	s.done = true
	for index, active := range c.active {
		if active == s {
			c.active = append(c.active[:index], c.active[index+1:]...)
			break
		}
	}
	line := resultLine(c.color, success, s.description)
	if c.promptActive {
		fmt.Fprintln(&c.pending, line)
		if len(c.active) == 0 {
			c.haltSpinnerLocked()
		}
		return
	}
	if !c.animate {
		fmt.Fprintln(c.Out, line)
		return
	}
	if c.spinning {
		c.clearLineLocked()
	}
	fmt.Fprintln(c.Out, line)
	if len(c.active) == 0 {
		c.haltSpinnerLocked()
		return
	}
	if !c.promptActive && !c.suspended {
		if !c.spinning {
			c.startSpinnerLocked()
		}
		c.drawFrameLocked()
	}
}

// resultLine renders one completed step with an optional colored marker.
func resultLine(color, success bool, description string) string {
	symbol := "✓"
	code := "32"
	if !success {
		symbol = "✗"
		code = "31"
	}
	if color {
		symbol = "\x1b[" + code + "m" + symbol + "\x1b[0m"
	}
	return " " + symbol + " " + description
}

// smartTerminal reports whether w can animate in place. Non-files, redirected
// output, and dumb terminals get the plain line-oriented log instead.
func smartTerminal(w io.Writer) bool {
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
}

// phaseAction describes a workflow phase as a short action and its emoji.
func phaseAction(phase model.Phase) (string, string) {
	switch phase {
	case model.PhasePreflight:
		return "Preflight cluster", "🔍"
	case model.PhaseFixtureReady:
		return "Prepare fixture", "🧰"
	case model.PhaseTriggered:
		return "Trigger components", "🚀"
	case model.PhaseBuildsVerified:
		return "Verify builds", "🏗️"
	case model.PhaseBuildOutputsVerified:
		return "Verify architectures", "🧪"
	case model.PhaseCompleted:
		return "Finish run", "🎉"
	default:
		return string(phase), "•"
	}
}

// phaseDescription builds the status text for one provider phase.
func phaseDescription(phase model.Phase, provider string) string {
	action, emoji := phaseAction(phase)
	if provider == "" {
		return action + " " + emoji
	}
	return fmt.Sprintf("%s (%s) %s", action, provider, emoji)
}

// phaseObserver turns workflow phase transitions into console steps.
type phaseObserver struct {
	console  *Console
	provider string
	step     *Step
}

// newPhaseObserver returns an observer for consoles and nil for other writers.
func newPhaseObserver(out io.Writer, provider string) *phaseObserver {
	console, ok := out.(*Console)
	if !ok || console == nil {
		return nil
	}
	return &phaseObserver{console: console, provider: provider}
}

// start opens the step for phase.
func (o *phaseObserver) start(phase model.Phase) {
	if o == nil {
		return
	}
	o.step = o.console.StartStep(phaseDescription(phase, o.provider))
}

// end completes the open step with the phase result.
func (o *phaseObserver) end(_ model.Phase, err error) {
	if o == nil {
		return
	}
	o.step.End(err == nil)
	o.step = nil
}
