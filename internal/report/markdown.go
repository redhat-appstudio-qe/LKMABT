package report

import (
	"fmt"
	"strings"
)

// RenderMarkdown renders the aggregate report as Markdown: a header block and a
// six-column table with one row per Row. Cell values are escaped for table use.
// Markdown and HTML share the document model, so both reports agree on run
// identity, architectures, and every result value.
func RenderMarkdown(document Document) string {
	builder := &strings.Builder{}
	fmt.Fprintf(builder, "# LKMABT aggregate run %s\n\n", document.RunID)
	fmt.Fprintf(builder, "- Started: %s\n", document.StartedAtText())
	fmt.Fprintf(builder, "- Ended: %s\n", document.EndedAtText())
	fmt.Fprintf(builder, "- Clusters file: `%s`\n", document.ClustersFile)
	fmt.Fprintf(builder, "- Architectures: %s\n", document.ArchitecturesText())
	fmt.Fprintf(builder, "- Result: **%s**\n\n", document.Result())
	builder.WriteString("| Cluster | Provider | Result | Stage | Note | Error | Artifacts |\n")
	builder.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, row := range document.Rows {
		fmt.Fprintf(builder, "| %s | %s | %s | %s | %s | %s | %s |\n", cell(row.Cluster), cell(row.Provider), row.Result(), cell(row.Stage), cell(row.Note), cell(row.Error), markdownLinks(row.Links))
	}
	return builder.String()
}

// markdownLinks renders one row's artifact links as a comma-separated Markdown
// list, or "none" when the row has no links. Labels are escaped; targets must
// already be safe relative URLs, since they are used verbatim in angle brackets.
func markdownLinks(links []Link) string {
	if len(links) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(links))
	for _, link := range links {
		parts = append(parts, fmt.Sprintf("[%s](<%s>)", cell(link.Label), link.Target))
	}
	return strings.Join(parts, ", ")
}

// markdownCellEscapes lists characters that can open Markdown structure inside a table cell.
const markdownCellEscapes = "\\|[]()*_`"

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
