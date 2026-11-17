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
	scheduling  []CheckResult
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

// CheckScheduling returns the fake cluster's configured scheduling results.
func (f *fakeCluster) CheckScheduling(context.Context, SchedulingSpec) []CheckResult {
	return f.scheduling
}

var _ = Describe("RunPreflight", func() {
	It("rejects wrong server without access check", func() {
		fake := &fakeCluster{server: "https://actual.example"}
		_, err := RunPreflight(context.Background(), fake, PreflightSpec{ExpectedServer: "https://expected.example"})
		Expect(err).To(HaveOccurred())
		Expect(fake.accessCalls).To(Equal(0))
	})

	It("rejects failed controller scheduling", func() {
		fake := &fakeCluster{server: "https://api.example", scheduling: []CheckResult{{Name: "multi-platform-controller/linux/arm64", Details: "platform is not configured in host-config"}}}
		_, err := RunPreflight(context.Background(), fake, PreflightSpec{ExpectedServer: "https://api.example", Scheduling: SchedulingSpec{Namespace: "multi-platform-controller", Platforms: []string{"linux/arm64"}}})
		Expect(err).To(HaveOccurred())
	})

	It("requires controller scheduling readiness", func() {
		fake := &fakeCluster{
			server:     "https://api.example",
			checks:     []CheckResult{{Name: "pipelines", Passed: true}},
			scheduling: []CheckResult{{Name: "multi-platform-controller/linux/arm64", Passed: true}},
		}
		result, err := RunPreflight(context.Background(), fake, PreflightSpec{
			ExpectedServer: "https://api.example",
			Scheduling:     SchedulingSpec{Namespace: "multi-platform-controller", Platforms: []string{"linux/arm64"}},
		})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(result.Scheduling).To(HaveLen(1))
		Expect(result.Scheduling[0].Name).To(Equal("multi-platform-controller/linux/arm64"))
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
