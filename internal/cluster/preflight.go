package cluster

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/client-go/kubernetes"
)

// GitHubAppChecker verifies that a cluster's GitHub App can onboard the fixture repository.
type GitHubAppChecker interface {
	VerifyGitHubAppInstallation(context.Context, string) error
}

// PreflightSpec declares what RunPreflight must verify against the cluster. A
// non-empty Scheduling.Platforms enables the scheduling checks.
type PreflightSpec struct {
	ExpectedServer string
	AccessChecks   []AccessCheck
	Scheduling     SchedulingSpec
}

// PreflightResult records every access and scheduling check that ran, including
// failed ones. Scheduling stays empty when no platforms were requested.
// Platforms classifies each requested platform; an undeclared platform is
// reported here without failing preflight, while a declared but unusable
// platform still fails it.
type PreflightResult struct {
	Checks     []CheckResult     `json:"checks"`
	Scheduling []CheckResult     `json:"scheduling"`
	Platforms  []PlatformSupport `json:"platforms,omitempty"`
}

// Supported returns the requested platforms the cluster can schedule, in
// request order.
func (r PreflightResult) Supported() []string {
	return r.platformsMatching(true)
}

// Unsupported returns the requested platforms the cluster does not declare, in
// request order. A declared but unusable platform is not listed here because it
// fails preflight instead.
func (r PreflightResult) Unsupported() []string {
	return r.platformsMatching(false)
}

// platformsMatching filters the classified platforms by support. The
// unsupported list contains only platforms host-config does not declare;
// declared-but-unusable platforms fail preflight and are listed in neither.
func (r PreflightResult) platformsMatching(supported bool) []string {
	result := make([]string, 0, len(r.Platforms))
	for _, platform := range r.Platforms {
		if supported && platform.Supported {
			result = append(result, platform.Platform)
			continue
		}
		if !supported && !platform.Configured {
			result = append(result, platform.Platform)
		}
	}
	return result
}

// RunPreflight validates cluster identity, API access, and requested scheduling
// readiness. It requires cluster and a non-empty expected server, compares the
// cluster server ignoring trailing slashes, and fails on the first failed access
// or scheduling check. An architecture the cluster's host-config does not
// declare is reported in Platforms without failing preflight, so the caller can
// narrow the selection; a declared platform that cannot be scheduled still
// fails. When every requested platform is undeclared, preflight fails because
// the cluster cannot run the selection at all. On failure RunPreflight returns
// the checks collected so far alongside the error; scheduling checks run only
// when spec.Scheduling.Platforms is non-empty.
func RunPreflight(ctx context.Context, cluster Cluster, spec PreflightSpec) (PreflightResult, error) {
	if cluster == nil {
		return PreflightResult{}, fmt.Errorf("cluster is required")
	}
	if strings.TrimSpace(spec.ExpectedServer) == "" {
		return PreflightResult{}, fmt.Errorf("expected cluster server is required")
	}
	actual, err := cluster.Server(ctx)
	if err != nil {
		return PreflightResult{}, fmt.Errorf("read cluster server: %w", err)
	}
	if strings.TrimRight(actual, "/") != strings.TrimRight(spec.ExpectedServer, "/") {
		return PreflightResult{}, fmt.Errorf("cluster server mismatch: expected %q, got %q", spec.ExpectedServer, actual)
	}
	result := PreflightResult{}
	result.Checks = cluster.CheckAccess(ctx, spec.AccessChecks)
	for _, check := range result.Checks {
		if !check.Passed {
			return result, fmt.Errorf("preflight check %q failed: %s", check.Name, check.Details)
		}
	}
	if len(spec.Scheduling.Platforms) > 0 {
		scheduling := cluster.CheckScheduling(ctx, spec.Scheduling)
		result.Scheduling = scheduling.Checks
		result.Platforms = scheduling.Platforms
		for _, check := range result.Scheduling {
			if !check.Passed {
				return result, fmt.Errorf("scheduling check %q failed: %s", check.Name, check.Details)
			}
		}
		for _, support := range result.Platforms {
			if support.Configured && !support.Supported {
				return result, fmt.Errorf("scheduling check %q failed: %s", support.Platform, support.Details)
			}
		}
		if len(result.Supported()) == 0 {
			return result, fmt.Errorf("cluster does not support any requested platform %s; configure one in host-config or select another architecture", strings.Join(spec.Scheduling.Platforms, ", "))
		}
	}
	return result, nil
}

// VerifyGitHubAppOnboarding confirms the cluster's published GitHub App can
// onboard the fixture repository before the run creates or deletes cluster
// resources. It requires a non-nil checker, returns the app slug, and propagates
// ConfigMap read, parse, and installation-check errors.
func VerifyGitHubAppOnboarding(ctx context.Context, client kubernetes.Interface, checker GitHubAppChecker) (string, error) {
	if checker == nil {
		return "", fmt.Errorf("GitHub App checker is required")
	}
	slug, err := ReadGitHubAppSlug(ctx, client)
	if err != nil {
		return "", err
	}
	if err := checker.VerifyGitHubAppInstallation(ctx, slug); err != nil {
		return "", err
	}
	return slug, nil
}
