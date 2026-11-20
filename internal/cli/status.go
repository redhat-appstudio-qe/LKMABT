package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/workflow"
	"golang.org/x/term"
)

// spinnerFrames holds the braille frames cycled by the live region.
var spinnerFrames = []string{
	"⠈⠁", "⠈⠑", "⠈⠱", "⠈⡱", "⢀⡱", "⢄⡱", "⢄⡱", "⢆⡱",
	"⢎⡱", "⢎⡰", "⢎⡠", "⢎⡀", "⢎⠁", "⠎⠁", "⠊⠁",
}

// spinnerLineFormat draws one frame and its step description. The live block is
// drawn with terminal autowrap disabled, and lines are clipped when the terminal
// width is known, so a long line cannot wrap into an extra row that later
// erasing would miss.
const spinnerLineFormat = "%s %s"

// Step is one status line that starts as an animated spinner and is replaced by
// a success or failure marker when it ends. Detail may be updated while the step
// runs: animated mode redraws the live row, while plain mode prints one line per
// update. Steps are created by Console.StartStep and serialize through their
// console, so Update and End are safe to call from concurrent goroutines and are
// no-ops after End.
type Step struct {
	console     *Console
	description string
	detail      string
	provider    string
	done        bool
}

// Update replaces the step's detail and redraws the live region. In plain mode
// it prints one progress line, prefixed with the provider label in a
// multi-provider run. In animated mode text wider than the terminal is written
// as a durable line above the live region while the live row keeps a clipped
// copy. Updates while a prompt owns the terminal are dropped: run.log keeps the
// full history, and in animated mode the live region redraws the latest detail
// when the prompt completes.
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
	s.detail = normalizeDetail(detail)
	// Heartbeats are transient: run.log already keeps the full history, so
	// replaying them after a prompt would duplicate stale progress. The live
	// region redraws the latest detail once the prompt completes.
	if c.promptActive {
		return
	}
	if !c.animate {
		line := s.plainDetail()
		if line == "" {
			return
		}
		fmt.Fprintln(c.Out, line)
		return
	}
	full := s.description
	if s.detail != "" {
		full += " · " + s.detail
	}
	if limit := c.renderWidth(); limit > 0 && displayWidth(full) > limit {
		c.writePermanentLocked(1, func() {
			fmt.Fprintln(c.Out, full)
		})
		return
	}
	if c.spinning && !c.suspended {
		c.drawLiveLocked()
	}
}

// normalizeDetail collapses API-provided heartbeat text to one terminal line.
func normalizeDetail(detail string) string {
	return strings.Join(strings.Fields(detail), " ")
}

// End removes the step from the live region and replaces its line with a result
// marker. It is idempotent and safe to call on a nil step. While a prompt owns
// the terminal the marker is buffered.
func (s *Step) End(success bool) {
	if s == nil || s.console == nil {
		return
	}
	s.endWith(resultLine(s.console.color, success, s.description))
}

// EndDeferred removes the step and reports a scheduler deferral, not a failure.
func (s *Step) EndDeferred(reason string) {
	if s == nil || s.console == nil {
		return
	}
	description := s.description + " - deferred"
	if reason != "" {
		description += " (" + reason + ")"
	}
	s.endWith(deferredLine(s.console.color, description))
}

// endWith removes the step from the active region and prints its final line.
func (s *Step) endWith(line string) {
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
// the terminal width. Clipping marks the cut with an ellipsis so text beyond the
// live row is visibly truncated rather than silently lost.
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

// renderWidth returns the cells available after the spinner frame and space, or
// 0 when the terminal width is unknown or too narrow to render.
func (c *Console) renderWidth() int {
	if c.width <= 3 {
		return 0
	}
	return c.width - 3
}

// displayWidth estimates terminal cells, treating emoji and symbol ranges as
// double-width and zero-width joiners, variation selectors, and combining marks
// as zero.
func displayWidth(line string) int {
	width := 0
	for _, r := range line {
		width += runeWidth(r)
	}
	return width
}

// truncateLine clips line to width cells and appends an ellipsis. A width of 0
// or less disables clipping; a width of 1 yields only the ellipsis.
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

// plainDetail returns the provider-prefixed heartbeat line for non-TTY output,
// or the empty string when the step has no detail.
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

// deferredLine renders a scheduler deferral with a pause marker, distinct from
// success and failure.
func deferredLine(color bool, description string) string {
	symbol := "⏸"
	if color {
		symbol = "\x1b[33m" + symbol + "\x1b[0m"
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

// terminalWidth returns w's column count, or 0 when it is not a terminal or its
// size cannot be read.
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
// steps. The workflow runner calls start and end in pairs; a start before the
// previous end would leave the earlier step in the live region.
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

// end completes the open step with the phase result. A deferral ends the step
// with a pause marker and a scheduler reason instead of a failure marker.
func (o *phaseObserver) end(_ model.Phase, err error) {
	if o == nil {
		return
	}
	if workflow.IsDeferred(err) {
		o.step.EndDeferred(workflow.DeferralReason(err))
	} else {
		o.step.End(err == nil)
	}
	o.step = nil
}
