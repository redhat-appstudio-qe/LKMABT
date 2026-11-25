package report

import (
	"fmt"
	"strings"
)

// RenderMarkdown renders the aggregate report as Markdown: a run-data header, a
// per-cluster results table, and a run-logs section whose cluster headings the
// table links to. Cell values are escaped for table use. Markdown and HTML
// share the document model, so both reports agree on run identity, cluster
// outcomes, architectures, and every artifact link.
func RenderMarkdown(document Document) string {
	builder := &strings.Builder{}
	fmt.Fprintf(builder, "# LKMABT aggregate %s\n\n", document.RunID)
	fmt.Fprintf(builder, "- **Started:** %s\n", document.StartedAtText())
	fmt.Fprintf(builder, "- **Ended:** %s\n", document.EndedAtText())
	fmt.Fprintf(builder, "- **Clusters file:** `%s`\n", document.ClustersFile)
	fmt.Fprintf(builder, "- **Requested architectures:** %s\n", document.ArchitecturesText())
	fmt.Fprintf(builder, "- **Mode:** %s\n", document.Mode)
	fmt.Fprintf(builder, "- **Fixtures:** %s\n", document.Fixtures)
	if document.AggregateLog.Target != "" {
		fmt.Fprintf(builder, "- **Run log:** [%s](<%s>)\n", linkLabel(document.AggregateLog.Label), document.AggregateLog.Target)
	}
	for _, warning := range document.Warnings {
		fmt.Fprintf(builder, "- **Warning:** %s\n", cell(warning))
	}
	clusters := document.Clusters()
	builder.WriteString("\n## Run results\n\n")
	builder.WriteString("| Cluster name | Pass/Fail | Failure reason | Run architectures |\n")
	builder.WriteString("| --- | --- | --- | --- |\n")
	for _, cluster := range clusters {
		fmt.Fprintf(builder, "| [%s](#%s) | %s | %s | %s |\n",
			cell(cluster.Name), cluster.Anchor(), cluster.Result(),
			cell(strings.Join(cluster.Failures(), "\n")),
			cell(cluster.RunArchitecturesText(document.Architectures)))
	}
	builder.WriteString("\n## Cluster run logs\n")
	for _, cluster := range clusters {
		fmt.Fprintf(builder, "\n<a id=\"%s\"></a>\n### %s run logs\n\n", cluster.Anchor(), cell(cluster.Name))
		for _, note := range cluster.Notes() {
			fmt.Fprintf(builder, "**Note:** %s\n\n", cell(note))
		}
		logs := cluster.LogLinks()
		if len(logs) == 0 {
			builder.WriteString("- none\n")
		}
		for _, link := range logs {
			fmt.Fprintf(builder, "- [%s](<%s>)\n", linkLabel(link.Label), link.Target)
		}
		if artifacts := cluster.ArtifactLinks(); len(artifacts) > 0 {
			builder.WriteString("\n**Artifacts:**\n\n")
			for _, link := range artifacts {
				fmt.Fprintf(builder, "- [%s](<%s>)\n", linkLabel(link.Label), link.Target)
			}
		}
	}
	return builder.String()
}

// linkLabel escapes only the characters that can close or split a Markdown
// link label, so underscores and parentheses in artifact paths stay readable.
func linkLabel(value string) string {
	value = strings.ReplaceAll(value, "\r\n", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\\", "\\\\", "[", "\\[", "]", "\\]").Replace(value)
}

// markdownCellEscapes lists characters that can open Markdown structure inside a table cell.
const markdownCellEscapes = "\\|[]*_`"

// cell escapes a value for use inside one Markdown table cell. "&", "<", and
// ">" are entity-escaped and Markdown delimiters are backslash-escaped; CRLF
// and LF become <br>.
func cell(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(value)
	builder := &strings.Builder{}
	builder.Grow(len(value) * 2)
	for _, char := range value {
		// Backslash-escape any character that could open Markdown structure.
		if strings.ContainsRune(markdownCellEscapes, char) {
			builder.WriteByte('\\')
		}
		builder.WriteRune(char)
	}
	return strings.ReplaceAll(builder.String(), "\n", "<br>")
}
