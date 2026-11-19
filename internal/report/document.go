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
	Label  string
	Target string // slash-separated, relative to the report file, always prefixed with "./"
}

// Row reports the outcome of one cluster-provider pair.
type Row struct {
	Cluster  string
	Provider string
	Passed   bool
	Stage    string
	Error    string
	Links    []Link
}

// Document is the aggregate report content for one LKMABT invocation.
type Document struct {
	RunID         string
	ClustersFile  string
	Architectures []string
	StartedAt     time.Time
	EndedAt       time.Time
	Passed        bool
	Rows          []Row
}

// ArchitecturesText renders the tested architectures for reports and summaries.
func (d Document) ArchitecturesText() string {
	if len(d.Architectures) == 0 {
		return "none"
	}
	return strings.Join(d.Architectures, ", ")
}

// Result renders the row outcome as "passed" or "failed".
func (r Row) Result() string {
	if r.Passed {
		return "passed"
	}
	return "failed"
}

// Result renders the aggregate outcome as "passed" or "failed".
func (d Document) Result() string {
	if d.Passed {
		return "passed"
	}
	return "failed"
}

// StartedAtText renders the aggregate start time in RFC 3339.
func (d Document) StartedAtText() string {
	return d.StartedAt.UTC().Format(time.RFC3339)
}

// EndedAtText renders the aggregate end time in RFC 3339.
func (d Document) EndedAtText() string {
	return d.EndedAt.UTC().Format(time.RFC3339)
}

// Write renders the run-named Markdown and HTML reports into directory.
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
