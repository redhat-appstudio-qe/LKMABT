// Package platform defines the image platforms LKMABT can build and the
// architecture arguments that select them.
package platform

import (
	"fmt"
	"strings"
)

const (
	LinuxAMD64   = "linux/amd64"
	LinuxARM64   = "linux/arm64"
	LinuxS390X   = "linux/s390x"
	LinuxPPC64LE = "linux/ppc64le"
)

// Architecture option names accepted after the fixture command.
const (
	ArchitectureARM64   = "arm64"
	ArchitectureAMD64   = "amd64"
	ArchitectureS390X   = "s390x"
	ArchitecturePPC64LE = "ppc64le"
	ArchitectureAWS     = "aws"
	ArchitectureIBM     = "ibm"
)

// Selection is one architecture argument and the canonical platforms it builds.
type Selection struct {
	Name      string
	Platforms []string
}

var selections = []Selection{
	{Name: ArchitectureARM64, Platforms: []string{LinuxARM64}},
	{Name: ArchitectureAMD64, Platforms: []string{LinuxAMD64}},
	{Name: ArchitectureS390X, Platforms: []string{LinuxS390X}},
	{Name: ArchitecturePPC64LE, Platforms: []string{LinuxPPC64LE}},
	{Name: ArchitectureAWS, Platforms: []string{LinuxAMD64, LinuxARM64}},
	{Name: ArchitectureIBM, Platforms: []string{LinuxS390X, LinuxPPC64LE}},
}

// Names returns the architecture argument names in help order.
func Names() []string {
	names := make([]string, len(selections))
	for index, selection := range selections {
		names[index] = selection.Name
	}
	return names
}

// Select resolves an architecture argument to its canonical platforms.
func Select(name string) ([]string, error) {
	for _, selection := range selections {
		if selection.Name == name {
			return append([]string(nil), selection.Platforms...), nil
		}
	}
	return nil, fmt.Errorf("unsupported architecture %q; choose one of %s", name, strings.Join(Names(), ", "))
}

// Combine resolves one or more architecture arguments to unique platforms in
// argument order, so repeated or overlapping groups expand once.
func Combine(names ...string) ([]string, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("at least one architecture is required")
	}
	result := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		platforms, err := Select(name)
		if err != nil {
			return nil, err
		}
		for _, buildPlatform := range platforms {
			if _, duplicate := seen[buildPlatform]; duplicate {
				continue
			}
			seen[buildPlatform] = struct{}{}
			result = append(result, buildPlatform)
		}
	}
	return result, nil
}

// All returns every supported platform in canonical order.
func All() []string {
	return []string{LinuxAMD64, LinuxARM64, LinuxS390X, LinuxPPC64LE}
}

// ManifestName returns the platform spelling fixture build pipelines use.
func ManifestName(canonical string) string {
	if canonical == LinuxAMD64 {
		return "linux/x86_64"
	}
	return canonical
}

// ManifestNames maps canonical platforms to fixture build pipeline spellings.
func ManifestNames(platforms []string) []string {
	result := make([]string, 0, len(platforms))
	for _, canonical := range platforms {
		result = append(result, ManifestName(canonical))
	}
	return result
}

// Normalize maps a supported platform alias to its canonical name, or "".
func Normalize(value string) string {
	switch strings.TrimSpace(value) {
	case "linux/amd64", "linux/x86_64", "linux-x86_64", "amd64", "x86_64":
		return LinuxAMD64
	case "linux/arm64", "linux/aarch64", "linux-aarch64", "arm64", "aarch64":
		return LinuxARM64
	case "linux/s390x", "linux-s390x", "s390x":
		return LinuxS390X
	case "linux/ppc64le", "linux-ppc64le", "ppc64le":
		return LinuxPPC64LE
	default:
		return ""
	}
}

// Supported reports whether value is already a canonical supported platform.
func Supported(value string) bool {
	return value != "" && Normalize(value) == value
}
