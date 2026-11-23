package workflow

import (
	"io"
	"strings"
)

// AlertSink receives durable, human-formatted alert blocks. key deduplicates
// repeats for the lifetime of the sink; title and lines are already written
// for a human and carry no structure the sink must interpret.
type AlertSink interface {
	Alert(key, title string, lines ...string)
}

// AlertLines renders one alert block: the marker and title on the first
// line, then one indented detail line each. marker and title may already
// carry ANSI from a colored caller; the layout does not change with color.
func AlertLines(marker, title string, lines ...string) []string {
	block := make([]string, 0, len(lines)+1)
	block = append(block, " "+marker+" "+title)
	for _, line := range lines {
		block = append(block, "   "+line)
	}
	return block
}

// writeAlertBlock writes the whole block in one Write call, so a writer that
// serializes Write calls cannot interleave another line inside it. A nil
// writer is a no-op.
func writeAlertBlock(out io.Writer, block []string) {
	if out == nil {
		return
	}
	_, _ = io.WriteString(out, strings.Join(block, "\n")+"\n")
}

// alert emits one durable informational block. A console that implements
// AlertSink renders and deduplicates it; any other writer receives the whole
// plain block in one Write, so provider stages sharing a writer cannot splice
// their lines into it. The run log always receives the plain copy. alert never
// prompts, blocks, or changes the run result.
func (s *LiveStage) alert(key, title string, lines ...string) {
	block := AlertLines("!", title, lines...)
	// Rendering and deduplication belong to the sink; the run log is the
	// durable record, so it always receives the plain copy.
	if sink, ok := s.Out.(AlertSink); ok {
		sink.Alert(key, title, lines...)
	} else {
		writeAlertBlock(s.Out, block)
	}
	writeAlertBlock(s.Log, block)
}
