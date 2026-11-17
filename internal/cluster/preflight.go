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

type PreflightSpec struct {
	ExpectedServer string
	AccessChecks   []AccessCheck
	Scheduling     SchedulingSpec
}

type PreflightResult struct {
	Checks     []CheckResult `json:"checks"`
	Scheduling []CheckResult `json:"scheduling"`
}

// RunPreflight validates cluster identity, API access, and requested scheduling readiness.
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
		result.Scheduling = cluster.CheckScheduling(ctx, spec.Scheduling)
		for _, check := range result.Scheduling {
			if !check.Passed {
				return result, fmt.Errorf("scheduling check %q failed: %s", check.Name, check.Details)
			}
		}
	}
	return result, nil
}

// VerifyGitHubAppOnboarding confirms the cluster's published GitHub App can onboard
// the fixture repository before the run creates or deletes cluster resources.
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
