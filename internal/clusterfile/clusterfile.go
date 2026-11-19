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

// Cluster types accepted in the clusters file.
const (
	// TypeOCP is an OpenShift cluster reached through kflux or the oc login ladder.
	TypeOCP = "ocp"
	// TypeROKS is an IBM Cloud ROKS cluster whose kubeconfig comes from ibmcloud ks.
	TypeROKS = "roks"
)

// DefaultIBMCloudEndpoint is the IBM Cloud CLI endpoint type used when a ROKS
// entry omits one.
const DefaultIBMCloudEndpoint = "vpe"

// Entry is one cluster listed in the clusters file.
type Entry struct {
	// Name is the cluster name, unique in the file and safe as a directory name.
	Name string `json:"name"`
	// Type is TypeOCP or TypeROKS; empty means TypeOCP.
	Type string `json:"type,omitempty"`
	// IBMCloud is the IBM Cloud cluster name for a ROKS entry; empty means Name.
	IBMCloud string `json:"ibmcloud,omitempty"`
	// IBMCloudEndpoint is the ibmcloud CLI endpoint type for a ROKS entry; empty
	// means DefaultIBMCloudEndpoint.
	IBMCloudEndpoint string `json:"ibmcloud-endpoint,omitempty"`
	// Kubeconfig is the kubeconfig path; empty means DefaultKubeconfig. Relative
	// paths resolve against the clusters file directory.
	Kubeconfig string `json:"kubeconfig,omitempty"`
	// Server is the optional expected API server URL.
	Server string `json:"server,omitempty"`
	// Catalog overrides the kflux catalog name derived from Name. Authentication
	// fills it after resolving a real cluster host label (for example
	// stone-stage-p01) through `kflux cluster list`; it is not a YAML field.
	Catalog string `json:"-"`
}

// IsROKS reports whether the entry is an IBM Cloud ROKS cluster.
func (e Entry) IsROKS() bool {
	return e.Type == TypeROKS
}

// CatalogName returns the kflux catalog name for the entry. A runtime Catalog
// override (resolved from kflux's cluster list) wins; otherwise a "kflux-"
// prefix is stripped and names without the prefix are used unchanged.
func (e Entry) CatalogName() string {
	if e.Catalog != "" {
		return e.Catalog
	}
	return catalogName(e.Name)
}

func catalogName(name string) string {
	return strings.TrimPrefix(name, "kflux-")
}

// document is the strict YAML shape of the clusters file; unknown keys are rejected.
type document struct {
	Clusters []Entry `json:"clusters"`
}

// namePattern restricts cluster names to values that are safe as directory
// names and as kflux -c targets.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidName reports whether name is safe as a directory name and kflux -c
// target. It rejects empty names and the . and .. directory components.
func ValidName(name string) bool {
	// "." and ".." are not directory components: joining them escapes the cluster directory.
	return name != "." && name != ".." && namePattern.MatchString(name)
}

// DefaultKubeconfig returns home/.kube/config-<catalog>, the default kubeconfig
// path for a kflux catalog name.
func DefaultKubeconfig(home, catalog string) string {
	return filepath.Join(home, ".kube", "config-"+catalog)
}

// Find returns the entry named name, or an error if the file lists none.
func Find(entries []Entry, name string) (Entry, error) {
	for _, entry := range entries {
		if entry.Name == name {
			return entry, nil
		}
	}
	return Entry{}, fmt.Errorf("clusters file does not list cluster %q", name)
}

// Load parses and normalizes the clusters file at path. Parsing is strict:
// unknown fields are rejected and the document must list at least one cluster.
// Each entry is validated (unique safe name, known type, IBM Cloud fields only
// for ROKS, unique kubeconfig path, absolute http or https server) and its
// defaults are applied. A missing file and the removed "kflux" field get
// actionable errors.
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
		// The removed kflux field is a migration error, not a generic typo.
		if strings.Contains(err.Error(), "kflux") {
			return nil, fmt.Errorf("clusters file %q sets the removed \"kflux\" field; delete it and keep the kflux- prefix on the cluster name: %w", path, err)
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
		entryType := strings.TrimSpace(entry.Type)
		// An absent type keeps the kflux and oc login ladder.
		if entryType == "" {
			entryType = TypeOCP
		}
		if entryType != TypeOCP && entryType != TypeROKS {
			return nil, fmt.Errorf("clusters file %q cluster %q type %q must be %s or %s", path, name, entry.Type, TypeOCP, TypeROKS)
		}
		ibmCloud, endpoint, err := normalizeIBMCloud(entryType, entry.IBMCloud, entry.IBMCloudEndpoint, name, path)
		if err != nil {
			return nil, err
		}
		// The kflux catalog name is derived from the entry name; it must stay a
		// single safe directory and argv component after the prefix is removed.
		catalog := catalogName(name)
		if !ValidName(catalog) {
			return nil, fmt.Errorf("clusters file %q cluster name %q yields invalid kflux catalog name %q; keep a real catalog name after the kflux- prefix", path, name, catalog)
		}
		kubeconfig := resolveKubeconfig(entry.Kubeconfig, catalog, baseDir, home)
		if other, found := seenKubeconfigs[kubeconfig]; found {
			return nil, fmt.Errorf("clusters file %q clusters %q and %q resolve to the same kubeconfig path %s", path, other, name, kubeconfig)
		}
		seenKubeconfigs[kubeconfig] = name
		server, err := normalizeServer(entry.Server, name, path)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, Entry{
			Name:             name,
			Type:             entryType,
			IBMCloud:         ibmCloud,
			IBMCloudEndpoint: endpoint,
			Kubeconfig:       kubeconfig,
			Server:           server,
		})
	}
	return normalized, nil
}

// ibmcloudEndpoints are the endpoint types accepted by ibmcloud ks cluster config --endpoint.
var ibmcloudEndpoints = map[string]struct{}{"vpe": {}, "private": {}, "link": {}}

// normalizeIBMCloud validates and defaults the IBM Cloud fields of a ROKS entry.
func normalizeIBMCloud(entryType, cluster, endpoint, name, path string) (string, string, error) {
	cluster = strings.TrimSpace(cluster)
	endpoint = strings.TrimSpace(endpoint)
	// kflux and oc login own non-ROKS entries, so IBM Cloud fields would never be read.
	if entryType != TypeROKS {
		if cluster != "" || endpoint != "" {
			return "", "", fmt.Errorf("clusters file %q cluster %q sets ibmcloud fields but type is %q; set type: %s", path, name, entryType, TypeROKS)
		}
		return "", "", nil
	}
	if cluster == "" {
		cluster = name
	}
	if endpoint == "" {
		endpoint = DefaultIBMCloudEndpoint
	}
	if _, accepted := ibmcloudEndpoints[endpoint]; !accepted {
		return "", "", fmt.Errorf("clusters file %q cluster %q ibmcloud-endpoint %q must be vpe, private, or link", path, name, endpoint)
	}
	return cluster, endpoint, nil
}

// resolveKubeconfig applies the default path, ~ expansion, and relative-path anchoring.
func resolveKubeconfig(value, catalog, baseDir, home string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultKubeconfig(home, catalog)
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

// UsesDefaultKubeconfig reports whether the entry's normalized Kubeconfig equals
// DefaultKubeconfig(home, CatalogName()). The default path is kflux's own
// per-cluster path, so it follows the kflux catalog name.
func (e Entry) UsesDefaultKubeconfig(home string) bool {
	return e.Kubeconfig == DefaultKubeconfig(home, e.CatalogName())
}
