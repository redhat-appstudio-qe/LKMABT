package report

import (
	"fmt"
	"html/template"
	"strings"
)

// htmlTemplate parses once at startup; malformed template text is a programming error.
var htmlTemplate = template.Must(template.New("report").Parse(htmlTemplateText))

// htmlTemplateText is the self-contained report document; html/template escapes
// every value and link target.
const htmlTemplateText = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>LKMABT aggregate run {{.RunID}}</title>
<style>
body{font-family:sans-serif;margin:2rem;line-height:1.4}
table{border-collapse:collapse;width:100%}
th,td{border:1px solid #999;padding:0.4rem;text-align:left;vertical-align:top}
.passed{color:#0a7d00}
.failed{color:#a00000}
.warning{color:#a00000}
.note{color:#555}
section{margin-bottom:1rem}
</style>
</head>
<body>
<h1>LKMABT aggregate run {{.RunID}}</h1>
<ul>
<li>Started: {{.StartedAtText}}</li>
<li>Ended: {{.EndedAtText}}</li>
<li>Clusters file: <code>{{.ClustersFile}}</code></li>
<li>Requested architectures: {{.ArchitecturesText}}</li>
<li>Mode: {{.Mode}}</li>
<li>Fixtures: {{.Fixtures}}</li>
{{if .AggregateLog.Target}}<li>Run log: <a href="{{.AggregateLog.Target}}">{{.AggregateLog.Label}}</a></li>
{{end}}{{range .Warnings}}<li class="warning">Warning: {{.}}</li>
{{end}}</ul>
<h2>Run results</h2>
<table>
<thead><tr><th>Cluster name</th><th>Pass/Fail</th><th>Failure reason</th><th>Run architectures</th></tr></thead>
<tbody>
{{range .Clusters}}<tr>
<td><a href="#{{.Anchor}}">{{.Name}}</a></td>
<td class="{{.Result}}">{{.Result}}</td>
<td>{{range $index, $failure := .Failures}}{{if $index}}<br>{{end}}{{$failure}}{{end}}</td>
<td>{{.RunArchitecturesText $.Architectures}}</td>
</tr>
{{end}}</tbody>
</table>
<h2>Cluster run logs</h2>
{{range .Clusters}}<section id="{{.Anchor}}">
<h3>{{.Name}} run logs</h3>
{{range .Notes}}<p class="note">Note: {{.}}</p>
{{end}}{{if .LogLinks}}<ul>
{{range .LogLinks}}<li><a href="{{.Target}}">{{.Label}}</a></li>
{{end}}</ul>
{{else}}<p>none</p>
{{end}}{{if .ArtifactLinks}}<p><strong>Artifacts:</strong></p>
<ul>
{{range .ArtifactLinks}}<li><a href="{{.Target}}">{{.Label}}</a></li>
{{end}}</ul>
{{end}}</section>
{{end}}</body>
</html>
`

// RenderHTML renders the aggregate report as one self-contained HTML document
// with no scripts or external assets. html/template escapes every value and
// link target, so markup in report data stays inert. The only expected error is
// a template execution failure.
func RenderHTML(document Document) (string, error) {
	builder := &strings.Builder{}
	if err := htmlTemplate.Execute(builder, document); err != nil {
		return "", fmt.Errorf("render report.html: %w", err)
	}
	return builder.String(), nil
}
