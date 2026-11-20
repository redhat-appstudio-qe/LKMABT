package cluster

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"
)

type fakeCluster struct {
	server      string
	checks      []CheckResult
	scheduling  SchedulingResult
	accessCalls int
}

type fakeGitHubAppChecker struct {
	slug string
	err  error
}

// VerifyGitHubAppInstallation records the slug and returns the configured error.
func (f *fakeGitHubAppChecker) VerifyGitHubAppInstallation(_ context.Context, slug string) error {
	f.slug = slug
	return f.err
}

// Server returns the fake cluster's configured API server URL.
func (f *fakeCluster) Server(context.Context) (string, error) { return f.server, nil }

// CheckAccess records the call and returns configured fake access results.
func (f *fakeCluster) CheckAccess(context.Context, []AccessCheck) []CheckResult {
	f.accessCalls++
	return f.checks
}

// CheckScheduling returns the fake cluster's configured scheduling result.
func (f *fakeCluster) CheckScheduling(context.Context, SchedulingSpec) SchedulingResult {
	return f.scheduling
}

var _ = Describe("RunPreflight", func() {
	It("rejects wrong server without access check", func() {
		fake := &fakeCluster{server: "https://actual.example"}
		_, err := RunPreflight(context.Background(), fake, PreflightSpec{ExpectedServer: "https://expected.example"})
		Expect(err).Should(HaveOccurred())
		Expect(fake.accessCalls).Should(Equal(0))
	})

	It("rejects failed controller scheduling", func() {
		fake := &fakeCluster{
			server:     "https://api.example",
			scheduling: SchedulingResult{Checks: []CheckResult{{Name: "multi-platform-controller", Details: "controller deployment is not ready"}}},
		}
		_, err := RunPreflight(context.Background(), fake, PreflightSpec{ExpectedServer: "https://api.example", Scheduling: SchedulingSpec{Namespace: "multi-platform-controller", Platforms: []string{"linux/arm64"}}})
		Expect(err).Should(HaveOccurred())
		Expect(err.Error()).Should(ContainSubstring("scheduling check \"multi-platform-controller\" failed"))
	})

	It("surfaces a failed access check with guidance", func() {
		fake := &fakeCluster{server: "https://api.example", checks: []CheckResult{{
			Name:    "gitlab SCM Secret",
			Details: "create secrets in namespace tenant denied: rbac denied; grant Secret access in the tenant namespace",
		}}}

		_, err := RunPreflight(context.Background(), fake, PreflightSpec{ExpectedServer: "https://api.example"})

		Expect(err).Should(HaveOccurred())
		Expect(err.Error()).Should(ContainSubstring("preflight check \"gitlab SCM Secret\" failed"))
		Expect(err.Error()).Should(ContainSubstring("grant Secret access in the tenant namespace"))
	})

	It("accepts readiness for a supported platform", func() {
		fake := &fakeCluster{
			server: "https://api.example",
			checks: []CheckResult{{Name: "pipelines", Passed: true}},
			scheduling: SchedulingResult{
				Checks:    []CheckResult{{Name: "multi-platform-controller", Passed: true}},
				Platforms: []PlatformSupport{{Platform: "linux/arm64", Configured: true, Supported: true, Details: "platform is configured via static host arm64-dev"}},
			},
		}
		result, err := RunPreflight(context.Background(), fake, PreflightSpec{
			ExpectedServer: "https://api.example",
			Scheduling:     SchedulingSpec{Namespace: "multi-platform-controller", Platforms: []string{"linux/arm64"}},
		})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(result.Scheduling).Should(HaveLen(1))
		Expect(result.Scheduling[0].Name).Should(Equal("multi-platform-controller"))
		Expect(result.Supported()).Should(Equal([]string{"linux/arm64"}))
		Expect(result.Unsupported()).Should(BeEmpty())
	})

	When("a requested platform is undeclared", func() {
		It("does not fail preflight and reports it as unsupported", func() {
			fake := &fakeCluster{
				server: "https://api.example",
				scheduling: SchedulingResult{
					Checks: []CheckResult{{Name: "multi-platform-controller", Passed: true}},
					Platforms: []PlatformSupport{
						{Platform: "linux/amd64", Configured: true, Supported: true, Details: "platform is configured via dynamic host scheduling"},
						{Platform: "linux/arm64", Details: "platform is not configured in host-config"},
					},
				},
			}
			result, err := RunPreflight(context.Background(), fake, PreflightSpec{
				ExpectedServer: "https://api.example",
				Scheduling:     SchedulingSpec{Namespace: "multi-platform-controller", Platforms: []string{"linux/amd64", "linux/arm64"}},
			})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(result.Supported()).Should(Equal([]string{"linux/amd64"}))
			Expect(result.Unsupported()).Should(Equal([]string{"linux/arm64"}))
		})
	})

	When("a platform is declared but unusable", func() {
		It("fails preflight for an incomplete static host", func() {
			fake := &fakeCluster{
				server: "https://api.example",
				scheduling: SchedulingResult{
					Checks:    []CheckResult{{Name: "multi-platform-controller", Passed: true}},
					Platforms: []PlatformSupport{{Platform: "linux/s390x", Configured: true, Details: "no usable static host for linux/s390x: s390x-dev (missing user, secret)"}},
				},
			}
			result, err := RunPreflight(context.Background(), fake, PreflightSpec{
				ExpectedServer: "https://api.example",
				Scheduling:     SchedulingSpec{Namespace: "multi-platform-controller", Platforms: []string{"linux/s390x"}},
			})
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).Should(ContainSubstring("no usable static host"))
			Expect(result.Unsupported()).Should(BeEmpty())
		})
	})

	When("every requested platform is undeclared", func() {
		It("fails with a message naming the selection", func() {
			fake := &fakeCluster{
				server: "https://api.example",
				scheduling: SchedulingResult{
					Checks: []CheckResult{{Name: "multi-platform-controller", Passed: true}},
					Platforms: []PlatformSupport{
						{Platform: "linux/arm64", Details: "platform is not configured in host-config"},
						{Platform: "linux/s390x", Details: "platform is not configured in host-config"},
					},
				},
			}
			result, err := RunPreflight(context.Background(), fake, PreflightSpec{
				ExpectedServer: "https://api.example",
				Scheduling:     SchedulingSpec{Namespace: "multi-platform-controller", Platforms: []string{"linux/arm64", "linux/s390x"}},
			})
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).Should(ContainSubstring("cluster does not support any requested platform linux/arm64, linux/s390x"))
			Expect(result.Unsupported()).Should(Equal([]string{"linux/arm64", "linux/s390x"}))
		})
	})
})

var _ = Describe("VerifyGitHubAppOnboarding", func() {
	When("the checker accepts the cluster's published app", func() {
		It("should return the app slug", func() {
			checker := &fakeGitHubAppChecker{}
			client := fake.NewSimpleClientset(konfluxInfoConfigMap(`{"integrations":{"github":{"application_url":"https://github.com/apps/konflux-kflux-ocp-p01"}}}`))

			slug, err := VerifyGitHubAppOnboarding(context.Background(), client, checker)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(slug).Should(Equal("konflux-kflux-ocp-p01"))
			Expect(checker.slug).Should(Equal("konflux-kflux-ocp-p01"))
		})
	})

	When("the checker rejects the installation", func() {
		It("should return the checker's error", func() {
			checker := &fakeGitHubAppChecker{err: errors.New("GitHub App cluster-app is not installed")}
			client := fake.NewSimpleClientset(konfluxInfoConfigMap(`{"integrations":{"github":{"application_url":"https://github.com/apps/cluster-app"}}}`))

			_, err := VerifyGitHubAppOnboarding(context.Background(), client, checker)
			Expect(err).Should(MatchError("GitHub App cluster-app is not installed"))
		})
	})
})
