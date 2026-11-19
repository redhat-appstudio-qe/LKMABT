// Package report builds the aggregate Markdown and HTML run reports and the
// artifact links they reference. Renderers escape markup but do not redact, so
// callers must keep secrets out of Document values.
package report

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StageAuthentication labels rows that never reached a cluster because the auth ladder failed.
const StageAuthentication = "authentication"

// StageCleanup labels run-level credential cleanup failures.
const StageCleanup = "cleanup"

// Link is one artifact hyperlink in a report row.
type Link struct {
	// Label is the human-readable link text.
	Label string
	// Target is slash-separated, relative to the report file, and always
	// prefixed with "./". Directory targets end with "/".
	Target string
}

// Row reports the outcome of one cluster-provider pair.
type Row struct {
	Cluster  string
	Provider string
	// Passed is the row outcome; Result renders it as text.
	Passed bool
	// Stage is the last workflow phase reached, or StageAuthentication or
	// StageCleanup for rows that never ran the workflow.
	Stage string
	// Error is the failure text and is empty for a passed row. It is rendered
	// verbatim (escaped for the output format), so callers must not put secrets
	// in it.
	Error string
	Links []Link
}

// Document is the aggregate report content for one LKMABT invocation.
type Document struct {
	// RunID appears in report headings; report file names come from the caller.
	RunID string
	// ClustersFile is the cluster file path shown in the report.
	ClustersFile string
	// Architectures holds the canonical platforms exercised by the run.
	Architectures []string
	StartedAt     time.Time
	EndedAt       time.Time
	// Passed is the caller-computed aggregate outcome, not derived from Rows.
	Passed bool
	Rows   []Row
}

// ArchitecturesText renders the tested architectures as a comma-separated list,
// or "none" when the document has none.
func (d Document) ArchitecturesText() string {
	if len(d.Architectures) == 0 {
		return "none"
	}
	return strings.Join(d.Architectures, ", ")
}

// Result returns the row outcome as "passed" or "failed".
func (r Row) Result() string {
	if r.Passed {
		return "passed"
	}
	return "failed"
}

// Result returns the aggregate outcome as "passed" or "failed".
func (d Document) Result() string {
	if d.Passed {
		return "passed"
	}
	return "failed"
}

// StartedAtText returns the aggregate start time in RFC 3339 UTC.
func (d Document) StartedAtText() string {
	return d.StartedAt.UTC().Format(time.RFC3339)
}

// EndedAtText returns the aggregate end time in RFC 3339 UTC.
func (d Document) EndedAtText() string {
	return d.EndedAt.UTC().Format(time.RFC3339)
}

// Write renders the run-named Markdown and HTML reports into directory using
// mode 0640. The directory must already exist. The Markdown file is written
// first; if rendering or writing it fails, no HTML file is attempted. Write
// errors name the target file, and render errors name report.html.
func Write(directory string, document Document, runName string) error {
	markdownName, htmlName := runName+"-report.md", runName+"-report.html"
	if err := os.WriteFile(filepath.Join(directory, markdownName), []byte(RenderMarkdown(document)), 0o640); err != nil {
		return fmt.Errorf("write %s: %w", markdownName, err)
	}
	html, err := RenderHTML(document)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, htmlName), []byte(html), 0o640); err != nil {
		return fmt.Errorf("write %s: %w", htmlName, err)
	}
	return nil
}
