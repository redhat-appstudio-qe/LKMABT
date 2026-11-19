package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/redhat-appstudio/lkmabt/internal/platform"
	"github.com/redhat-appstudio/lkmabt/internal/providers"
	"sigs.k8s.io/yaml"
)

// platformUpdate is one validated fixture PipelineRun replacement held until
// every manifest has been read and rewritten.
type platformUpdate struct {
	component string
	path      string
	content   string
	sha       string
}

// configureBuildPlatforms commits the selected platforms to each fixture
// PipelineRun and returns the configuration commit per component. All
// manifests are read and validated before the first commit, so a malformed
// fixture cannot leave partial configuration behind.
func (s *LiveStage) configureBuildPlatforms(ctx context.Context, components []string) (map[string]string, error) {
	if len(s.Request.TektonPaths) == 0 {
		return nil, nil
	}
	if len(s.Request.TektonPaths) != len(components) {
		return nil, fmt.Errorf("fixture has %d components but %d Tekton pipeline paths", len(components), len(s.Request.TektonPaths))
	}
	platforms := s.platforms()
	manifestPlatforms := platform.ManifestNames(platforms)
	s.notice("configuring fixture builds for %s", strings.Join(platforms, ", "))
	updates := make([]platformUpdate, 0, len(components))
	for index, component := range components {
		path := s.Request.TektonPaths[index]
		file, err := s.Provider.ReadFile(ctx, s.Fixture, path, s.Request.SourceBranch)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		content, err := setBuildPlatforms(file.Content, manifestPlatforms)
		if err != nil {
			return nil, fmt.Errorf("configure %s: %w", path, err)
		}
		if content == file.Content {
			continue
		}
		updates = append(updates, platformUpdate{component: component, path: path, content: content, sha: file.SHA})
	}
	prerequisites := make(map[string]string, len(updates))
	committed := make([]string, 0, len(updates))
	for _, update := range updates {
		commit, err := s.Provider.UpdateFile(ctx, s.Fixture, update.path, s.Request.SourceBranch, update.content, update.sha, providers.CommitMessageConfigure)
		if err != nil {
			return prerequisites, fmt.Errorf("configure %s: %w (%s)", update.path, err, committedSummary(committed))
		}
		prerequisites[update.component] = commit.SHA
		committed = append(committed, update.path)
	}
	return prerequisites, nil
}

// committedSummary reports fixture configuration already committed when a later
// provider update fails.
func committedSummary(paths []string) string {
	if len(paths) == 0 {
		return "no fixture configuration was committed"
	}
	return "fixture configuration already committed: " + strings.Join(paths, ", ")
}

// setBuildPlatforms rewrites the top-level spec.params build-platforms value in
// a fixture PipelineRun while leaving the rest of the manifest unchanged. The
// result is parsed before it is returned, so a formatting mistake cannot reach
// the fixture branch.
func setBuildPlatforms(contents string, platforms []string) (string, error) {
	if len(platforms) == 0 {
		return "", fmt.Errorf("at least one build platform is required")
	}
	lines := strings.Split(contents, "\n")
	specLine := -1
	for index, line := range lines {
		if strings.TrimRight(line, " \t\r") == "spec:" {
			specLine = index
			break
		}
	}
	if specLine < 0 {
		return "", fmt.Errorf("manifest has no top-level spec")
	}
	specChildIndent := -1
	for index := specLine + 1; index < len(lines); index++ {
		line := lines[index]
		trimmed := strings.TrimLeft(line, " ")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		specChildIndent = indentWidth(line)
		break
	}
	if specChildIndent <= 0 {
		return "", fmt.Errorf("manifest has no spec.params")
	}
	paramsLine := -1
	for index := specLine + 1; index < len(lines); index++ {
		line := lines[index]
		trimmed := strings.TrimLeft(line, " ")
		indent := indentWidth(line)
		if line != "" && indent < specChildIndent {
			break
		}
		if indent == specChildIndent && strings.TrimRight(trimmed, " \t\r") == "params:" {
			paramsLine = index
			break
		}
	}
	if paramsLine < 0 {
		return "", fmt.Errorf("manifest has no spec.params")
	}
	paramsIndent := indentWidth(lines[paramsLine])
	entryLine := -1
	for index := paramsLine + 1; index < len(lines); index++ {
		line := lines[index]
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if line != "" && indent <= paramsIndent && !strings.HasPrefix(trimmed, "- ") {
			break
		}
		if strings.TrimRight(trimmed, " \t\r") == "- name: build-platforms" {
			entryLine = index
			break
		}
	}
	if entryLine < 0 {
		return "", fmt.Errorf("spec.params does not declare build-platforms; update the fixture pipeline to accept it")
	}
	entryIndent := indentWidth(lines[entryLine])
	valueLine := -1
	valueIndent := 0
	for index := entryLine + 1; index < len(lines); index++ {
		line := lines[index]
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if strings.TrimSpace(line) == "" {
			continue
		}
		if indent < entryIndent || (indent == entryIndent && strings.HasPrefix(trimmed, "- ")) {
			break
		}
		if strings.HasPrefix(trimmed, "value:") {
			valueLine, valueIndent = index, indent
			break
		}
	}
	if valueLine < 0 {
		return "", fmt.Errorf("spec.params build-platforms has no value")
	}
	itemIndent := valueIndent
	for next := valueLine + 1; next < len(lines); next++ {
		trimmed := strings.TrimLeft(lines[next], " ")
		if strings.TrimSpace(lines[next]) == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if indent := len(lines[next]) - len(trimmed); indent >= valueIndent && strings.HasPrefix(trimmed, "- ") {
			itemIndent = indent
		}
		break
	}
	itemPrefix := strings.Repeat(" ", itemIndent) + "- "
	endLine := valueLine + 1
	for endLine < len(lines) {
		if strings.HasPrefix(lines[endLine], itemPrefix) {
			endLine++
			continue
		}
		// Keep trailing blank lines and comments intact; consume separators only
		// when another list item follows.
		next := endLine
		for next < len(lines) {
			trimmed := strings.TrimLeft(lines[next], " ")
			if strings.TrimSpace(lines[next]) != "" && !(strings.HasPrefix(trimmed, "#") && indentWidth(lines[next]) >= itemIndent) {
				break
			}
			next++
		}
		if next >= len(lines) || !strings.HasPrefix(lines[next], itemPrefix) {
			break
		}
		endLine = next
	}
	replacement := make([]string, 0, len(platforms)+1)
	replacement = append(replacement, strings.Repeat(" ", valueIndent)+"value:")
	for _, buildPlatform := range platforms {
		replacement = append(replacement, itemPrefix+buildPlatform)
	}
	updated := make([]string, 0, len(lines)-(endLine-valueLine)+len(replacement))
	updated = append(updated, lines[:valueLine]...)
	updated = append(updated, replacement...)
	updated = append(updated, lines[endLine:]...)
	result := strings.Join(updated, "\n")
	if err := validateBuildPlatforms(result, platforms); err != nil {
		return "", err
	}
	return result, nil
}

// indentWidth returns the number of leading spaces in a YAML line.
func indentWidth(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// validateBuildPlatforms parses a rewritten manifest and confirms that its
// top-level build-platforms value is exactly the requested array.
func validateBuildPlatforms(contents string, platforms []string) error {
	var manifest map[string]any
	if err := yaml.Unmarshal([]byte(contents), &manifest); err != nil {
		return fmt.Errorf("rewritten manifest is not valid YAML: %w", err)
	}
	spec, _ := manifest["spec"].(map[string]any)
	params, _ := spec["params"].([]any)
	for _, raw := range params {
		param, ok := raw.(map[string]any)
		if !ok || param["name"] != "build-platforms" {
			continue
		}
		values, ok := param["value"].([]any)
		if !ok {
			return fmt.Errorf("rewritten build-platforms value is not a list")
		}
		actual := make([]string, 0, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("rewritten build-platforms value contains a non-string")
			}
			actual = append(actual, text)
		}
		if !slices.Equal(actual, platforms) {
			return fmt.Errorf("rewritten build-platforms value %v does not match the selection %v", actual, platforms)
		}
		return nil
	}
	return fmt.Errorf("rewritten manifest has no spec.params build-platforms")
}
