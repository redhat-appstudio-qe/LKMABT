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
body{font-family:sans-serif;margin:2rem}
table{border-collapse:collapse;width:100%}
th,td{border:1px solid #999;padding:0.4rem;text-align:left;vertical-align:top}
.passed{color:#0a7d00}
.failed{color:#a00000}
</style>
</head>
<body>
<h1>LKMABT aggregate run {{.RunID}}</h1>
<ul>
<li>Started: {{.StartedAtText}}</li>
<li>Ended: {{.EndedAtText}}</li>
<li>Clusters file: <code>{{.ClustersFile}}</code></li>
<li>Architectures: {{.ArchitecturesText}}</li>
<li>Result: <span class="{{.Result}}">{{.Result}}</span></li>
</ul>
<table>
<thead><tr><th>Cluster</th><th>Provider</th><th>Result</th><th>Stage</th><th>Note</th><th>Error</th><th>Artifacts</th></tr></thead>
<tbody>
{{range .Rows}}<tr>
<td>{{.Cluster}}</td>
<td>{{.Provider}}</td>
<td class="{{.Result}}">{{.Result}}</td>
<td>{{.Stage}}</td>
<td>{{.Note}}</td>
<td>{{.Error}}</td>
<td>{{if .Links}}{{range $index, $link := .Links}}{{if $index}}<br>{{end}}<a href="{{$link.Target}}">{{$link.Label}}</a>{{end}}{{else}}none{{end}}</td>
</tr>
{{end}}</tbody>
</table>
</body>
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
