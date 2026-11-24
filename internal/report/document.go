// Package report builds the aggregate Markdown and HTML run reports and the
// artifact links they reference. Renderers escape markup but do not redact, so
// callers must keep secrets out of Document values.
package report

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StageAuthentication labels rows that never reached a cluster because the auth ladder failed.
const StageAuthentication = "authentication"

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
	// Stage is the last workflow phase reached, or StageAuthentication for rows
	// that never reached a cluster.
	Stage string
	// Error is the failure text and is empty for a passed row. It is rendered
	// verbatim (escaped for the output format), so callers must not put secrets
	// in it.
	Error string
	// Note carries operational context that is not a failure, such as a first
	// attempt deferred by scheduler admission before a successful retry.
	Note string
	// Platforms is the architecture selection this provider exercised after
	// preflight narrowing; empty when the run stopped before narrowing.
	Platforms []string
	Links     []Link
}

// Document is the aggregate report content for one LKMABT invocation.
type Document struct {
	// RunID appears in report headings; report file names come from the caller.
	RunID string
	// ClustersFile is the cluster file path shown in the report.
	ClustersFile string
	// Mode is "smoke" or "full".
	Mode string
	// Fixtures is the selected fixture: "github", "gitlab", or "both".
	Fixtures string
	// Architectures holds the canonical platforms requested for the run. A
	// cluster row reports any per-cluster subset after host-config narrowing.
	Architectures []string
	StartedAt     time.Time
	EndedAt       time.Time
	// AggregateLog links the run-wide log that carries authentication and
	// scheduling diagnostics not repeated in a cluster's run.log.
	AggregateLog Link
	// Warnings holds run-level failures that belong to no cluster, such as
	// credential cleanup.
	Warnings []string
	// Rows holds one row per cluster-provider pair; renderers group them by
	// cluster.
	Rows []Row
}

// ClusterView groups one cluster's provider rows for the run-results table and
// its run-logs section.
type ClusterView struct {
	Name string
	Rows []Row
}

// Result returns the row outcome as "passed" or "failed".
func (r Row) Result() string {
	if r.Passed {
		return "passed"
	}
	return "failed"
}

// Clusters groups rows by cluster in first-seen order.
func (d Document) Clusters() []ClusterView {
	clusters := make([]ClusterView, 0, len(d.Rows))
	index := make(map[string]int, len(d.Rows))
	for _, row := range d.Rows {
		position, ok := index[row.Cluster]
		if !ok {
			position = len(clusters)
			index[row.Cluster] = position
			clusters = append(clusters, ClusterView{Name: row.Cluster})
		}
		clusters[position].Rows = append(clusters[position].Rows, row)
	}
	return clusters
}

// Anchor returns the stable fragment identifier for this cluster's run-logs
// section. A cluster-file name is already safe and maps to cluster-<name>. Any
// other name is sanitized and suffixed with a hash, so two hand-built names
// cannot share one section identifier.
func (c ClusterView) Anchor() string {
	builder := &strings.Builder{}
	builder.WriteString("cluster-")
	safe := true
	for _, char := range c.Name {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '-', char == '_', char == '.':
			builder.WriteRune(char)
		default:
			safe = false
			builder.WriteByte('-')
		}
	}
	if safe {
		return builder.String()
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(c.Name))
	return fmt.Sprintf("%s-%08x", builder.String(), hash.Sum32())
}

// Result returns the cluster outcome as "passed" or "failed".
func (c ClusterView) Result() string {
	if c.Passed() {
		return "passed"
	}
	return "failed"
}

// Passed reports whether every provider row for the cluster passed.
func (c ClusterView) Passed() bool {
	for _, row := range c.Rows {
		if !row.Passed {
			return false
		}
	}
	return true
}

// Failures returns one failure line per failed provider row, naming the
// provider when the cluster ran more than one. An error every provider reported
// identically is emitted once without a provider name.
func (c ClusterView) Failures() []string {
	failed := make([]Row, 0, len(c.Rows))
	for _, row := range c.Rows {
		if !row.Passed {
			failed = append(failed, row)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	shared := len(c.Rows) > 1 && len(failed) == len(c.Rows) && failed[0].Error != ""
	for _, row := range failed[1:] {
		if row.Error != failed[0].Error {
			shared = false
			break
		}
	}
	failures := make([]string, 0, len(failed))
	for _, row := range failed {
		text := row.Error
		if text == "" {
			text = "failed"
			if row.Stage != "" {
				text += " at " + row.Stage
			}
		}
		if len(c.Rows) > 1 && !shared {
			text = row.Provider + ": " + text
		}
		failures = append(failures, text)
	}
	if shared {
		return failures[:1]
	}
	return failures
}

// RunArchitecturesText renders the architectures this cluster exercised. It
// names every requested architecture the cluster skipped so a narrowed
// selection never reads as a full one.
func (c ClusterView) RunArchitecturesText(requested []string) string {
	tested := make([]string, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, row := range c.Rows {
		for _, architecture := range row.Platforms {
			if _, ok := seen[architecture]; ok {
				continue
			}
			seen[architecture] = struct{}{}
			tested = append(tested, architecture)
		}
	}
	if len(tested) == 0 {
		return "not determined"
	}
	text := strings.Join(tested, ", ")
	missing := make([]string, 0, len(requested))
	for _, architecture := range requested {
		if _, ok := seen[architecture]; !ok {
			missing = append(missing, architecture)
		}
	}
	if len(missing) > 0 {
		text += "; missing " + strings.Join(missing, ", ")
	}
	return text
}

// Notes returns the distinct operational notes recorded by the cluster's rows.
func (c ClusterView) Notes() []string {
	notes := make([]string, 0, len(c.Rows))
	seen := make(map[string]struct{}, len(c.Rows))
	for _, row := range c.Rows {
		if row.Note == "" {
			continue
		}
		if _, ok := seen[row.Note]; ok {
			continue
		}
		seen[row.Note] = struct{}{}
		notes = append(notes, row.Note)
	}
	return notes
}

// Links returns the cluster's artifact links, deduplicated by target and kept
// in row order so the cluster run.log is listed first.
func (c ClusterView) Links() []Link {
	links := make([]Link, 0, len(c.Rows)*8)
	seen := make(map[string]struct{}, len(c.Rows)*8)
	for _, row := range c.Rows {
		for _, link := range row.Links {
			if _, ok := seen[link.Target]; ok {
				continue
			}
			seen[link.Target] = struct{}{}
			links = append(links, link)
		}
	}
	return links
}

// LogLinks returns the cluster's log-file links.
func (c ClusterView) LogLinks() []Link {
	return filterLinks(c.Links(), true)
}

// ArtifactLinks returns the cluster's non-log evidence links.
func (c ClusterView) ArtifactLinks() []Link {
	return filterLinks(c.Links(), false)
}

// filterLinks splits links by whether their target is a log file.
func filterLinks(links []Link, logs bool) []Link {
	filtered := make([]Link, 0, len(links))
	for _, link := range links {
		if strings.HasSuffix(link.Target, ".log") == logs {
			filtered = append(filtered, link)
		}
	}
	return filtered
}

// ArchitecturesText renders the requested architectures as a comma-separated
// list, or "none" when the document has none.
func (d Document) ArchitecturesText() string {
	if len(d.Architectures) == 0 {
		return "none"
	}
	return strings.Join(d.Architectures, ", ")
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
