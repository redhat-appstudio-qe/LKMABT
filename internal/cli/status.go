package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/redhat-appstudio/lkmabt/internal/model"
	"golang.org/x/term"
)

// spinnerFrames is the braille animation kind uses for its loading spinner.
var spinnerFrames = []string{
	"⠈⠁", "⠈⠑", "⠈⠱", "⠈⡱", "⢀⡱", "⢄⡱", "⢄⡱", "⢆⡱",
	"⢎⡱", "⢎⡰", "⢎⡠", "⢎⡀", "⢎⠁", "⠎⠁", "⠊⠁",
}

// spinnerLineFormat draws one frame and its step description. Wrapping is
// disabled around the whole live block so a long line cannot create stale rows.
const spinnerLineFormat = "%s %s"

// Step is one status line that starts as an animated spinner and is replaced by
// a success or failure marker when it ends. Detail may be updated while the
// step is running to show a heartbeat without adding a new line.
type Step struct {
	console     *Console
	description string
	detail      string
	provider    string
	done        bool
}

// Update replaces the step's detail and redraws the live region. In plain mode
// it prints one progress line, prefixed with the provider in a both run.
func (s *Step) Update(detail string) {
	if s == nil || s.console == nil {
		return
	}
	c := s.console
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if s.done {
		return
	}
	s.detail = detail
	full := s.description
	if detail != "" {
		full += " · " + detail
	}
	if !c.animate {
		line := s.plainDetail()
		if line == "" {
			return
		}
		if c.promptActive {
			fmt.Fprintln(&c.pending, line)
		} else {
			fmt.Fprintln(c.Out, line)
		}
		return
	}
	if limit := c.renderWidth(); limit > 0 && displayWidth(full) > limit {
		c.writePermanentLocked(1, func() {
			fmt.Fprintln(c.Out, full)
		})
		return
	}
	if c.spinning && !c.promptActive && !c.suspended {
		c.drawLiveLocked()
	}
}

// End completes the step and replaces its live line with a result marker. It is
// safe to call more than once and on a nil step.
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
	c.writePermanentLocked(1, func() {
		fmt.Fprintln(c.Out, line)
	})
	if len(c.active) == 0 {
		c.haltSpinnerLocked()
	}
}

// render returns the live line text, including the current detail, clipped to
// the terminal width so disabled wrapping cannot drop the tail silently.
func (s *Step) render() string {
	line := s.description
	if s.detail != "" {
		line += " · " + s.detail
	}
	if s.console == nil {
		return line
	}
	return truncateLine(line, s.console.renderWidth())
}

// renderWidth returns the cells available after the spinner frame and space.
func (c *Console) renderWidth() int {
	if c.width <= 3 {
		return 0
	}
	return c.width - 3
}

// displayWidth estimates terminal cells, treating emoji as double-width.
func displayWidth(line string) int {
	width := 0
	for _, r := range line {
		width += runeWidth(r)
	}
	return width
}

// truncateLine clips line to width cells and appends an ellipsis.
func truncateLine(line string, width int) string {
	if width <= 0 || displayWidth(line) <= width {
		return line
	}
	if width == 1 {
		return "…"
	}
	var builder strings.Builder
	used := 0
	for _, r := range line {
		charWidth := runeWidth(r)
		if used+charWidth > width-1 {
			break
		}
		builder.WriteRune(r)
		used += charWidth
	}
	builder.WriteString("…")
	return builder.String()
}

// runeWidth estimates the terminal width of one rune.
func runeWidth(r rune) int {
	switch {
	case r == '\u200d' || r == '\ufe0f' || unicode.Is(unicode.Mn, r):
		return 0
	case r >= 0x1F000 && r <= 0x1FAFF, r >= 0x2600 && r <= 0x27BF:
		return 2
	default:
		return 1
	}
}

// plainDetail returns the provider-prefixed heartbeat line for non-TTY output.
func (s *Step) plainDetail() string {
	if s.detail == "" {
		return ""
	}
	if s.provider != "" {
		return s.provider + " run - " + s.detail
	}
	return s.detail
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

// terminalWidth returns w's column count, or 0 when it is not a terminal.
func terminalWidth(w io.Writer) int {
	file, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return 0
	}
	width, _, err := term.GetSize(int(file.Fd()))
	if err != nil {
		return 0
	}
	return width
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

// phaseDescription builds the status text for one provider phase. The provider
// label is empty for single-provider runs.
func phaseDescription(phase model.Phase, provider string) string {
	action, emoji := phaseAction(phase)
	if provider == "" {
		return action + " " + emoji
	}
	return fmt.Sprintf("%s (%s) %s", action, provider, emoji)
}

// phaseObserver turns workflow phase transitions and progress into console
// steps.
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
	o.step = o.console.startStep(o.provider, phaseDescription(phase, o.provider))
}

// progress updates the active phase with heartbeat detail.
func (o *phaseObserver) progress(detail string) {
	if o == nil || o.step == nil {
		return
	}
	o.step.Update(detail)
}

// end completes the open step with the phase result.
func (o *phaseObserver) end(_ model.Phase, err error) {
	if o == nil {
		return
	}
	o.step.End(err == nil)
	o.step = nil
}
