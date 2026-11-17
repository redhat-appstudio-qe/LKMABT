package clusterfile

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// DefaultPath is the clusters-file path used when --clusters-file is absent.
const DefaultPath = ".lkmabt-clusters.yaml"

// Entry is one cluster listed in the clusters file.
type Entry struct {
	Name       string `json:"name"`
	Kubeconfig string `json:"kubeconfig,omitempty"`
	Server     string `json:"server,omitempty"`
}

// document is the strict YAML shape of the clusters file.
type document struct {
	Clusters []Entry `json:"clusters"`
}

// namePattern restricts cluster names to values that are safe as directory
// names and as kflux -c targets.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidName reports whether name is safe as a directory name and kflux -c target.
func ValidName(name string) bool {
	// "." and ".." are not directory components: joining them escapes the cluster directory.
	return name != "." && name != ".." && namePattern.MatchString(name)
}

// DefaultKubeconfig returns the default kubeconfig path for a cluster named name.
func DefaultKubeconfig(home, name string) string {
	return filepath.Join(home, ".kube", "config-"+name)
}

// Find returns the entry named name.
func Find(entries []Entry, name string) (Entry, error) {
	for _, entry := range entries {
		if entry.Name == name {
			return entry, nil
		}
	}
	return Entry{}, fmt.Errorf("clusters file does not list cluster %q", name)
}

// Load parses and normalizes the clusters file at path.
func Load(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	// A missing file gets an actionable message naming the flag and the default path.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("clusters file %s not found; pass --clusters-file <path> or create %s in the working directory", path, DefaultPath)
	}
	if err != nil {
		return nil, fmt.Errorf("read clusters file %q: %w", path, err)
	}
	var doc document
	if err := yaml.UnmarshalStrict(data, &doc); err != nil {
		// A document without a clusters key reports the actionable empty-list
		// error; any other parse problem stays a parse error.
		var lenient document
		if yaml.Unmarshal(data, &lenient) == nil && len(lenient.Clusters) == 0 {
			return nil, fmt.Errorf("clusters file %q must list at least one cluster under \"clusters\"", path)
		}
		return nil, fmt.Errorf("parse clusters file %q: %w", path, err)
	}
	if len(doc.Clusters) == 0 {
		return nil, fmt.Errorf("clusters file %q must list at least one cluster under \"clusters\"", path)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve clusters file %q: %w", path, err)
	}
	// Relative kubeconfig paths anchor to the clusters file's directory.
	baseDir := filepath.Dir(absolute)
	return normalize(doc.Clusters, path, baseDir, home)
}

// normalize validates each entry and resolves its kubeconfig and server values.
func normalize(entries []Entry, path, baseDir, home string) ([]Entry, error) {
	seen := make(map[string]struct{}, len(entries))
	// Two entries sharing one kubeconfig would make successive logins write and share one file.
	seenKubeconfigs := make(map[string]string, len(entries))
	normalized := make([]Entry, 0, len(entries))
	for index, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			return nil, fmt.Errorf("clusters file %q entry %d requires a name", path, index+1)
		}
		if !ValidName(name) {
			return nil, fmt.Errorf("clusters file %q cluster name %q must match [A-Za-z0-9._-]+", path, name)
		}
		// Duplicate names would collide as cluster directories and kflux targets.
		if _, found := seen[name]; found {
			return nil, fmt.Errorf("clusters file %q lists duplicate cluster name %q", path, name)
		}
		seen[name] = struct{}{}
		kubeconfig := resolveKubeconfig(entry.Kubeconfig, name, baseDir, home)
		if other, found := seenKubeconfigs[kubeconfig]; found {
			return nil, fmt.Errorf("clusters file %q clusters %q and %q resolve to the same kubeconfig path %s", path, other, name, kubeconfig)
		}
		seenKubeconfigs[kubeconfig] = name
		server, err := normalizeServer(entry.Server, name, path)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, Entry{Name: name, Kubeconfig: kubeconfig, Server: server})
	}
	return normalized, nil
}

// resolveKubeconfig applies the default path, ~ expansion, and relative-path anchoring.
func resolveKubeconfig(value, name, baseDir, home string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultKubeconfig(home, name)
	}
	if value == "~" {
		return home
	}
	if strings.HasPrefix(value, "~/") {
		return filepath.Join(home, value[2:])
	}
	if !filepath.IsAbs(value) {
		return filepath.Join(baseDir, value)
	}
	return filepath.Clean(value)
}

// normalizeServer validates a configured API server URL and trims its trailing
// slashes. An empty value stays empty because the server is optional.
func normalizeServer(value, name, path string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	normalized := strings.TrimRight(value, "/")
	parsed, err := url.Parse(normalized)
	// A usable API server needs a host and an http or https scheme; without them
	// the value is neither a preflight expectation nor an oc login --server argument.
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("clusters file %q cluster %q server %q must be an absolute http or https URL", path, name, value)
	}
	return normalized, nil
}

// UsesDefaultKubeconfig reports whether the entry resolves to the default kubeconfig path.
func (e Entry) UsesDefaultKubeconfig(home string) bool {
	return e.Kubeconfig == DefaultKubeconfig(home, e.Name)
}
