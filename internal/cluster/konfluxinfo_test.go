package cluster

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// konfluxInfoConfigMap builds the cluster ConfigMap that publishes Konflux integration info.
func konfluxInfoConfigMap(info string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: konfluxInfoNamespace, Name: konfluxPublicInfoConfigMap},
		Data:       map[string]string{"info.json": info},
	}
}

var _ = Describe("ReadGitHubAppSlug", func() {
	When("the cluster publishes a GitHub Application URL", func() {
		It("should return the application slug", func() {
			client := fake.NewSimpleClientset(konfluxInfoConfigMap(`{"integrations":{"github":{"application_url":"https://github.com/apps/konflux-kflux-ocp-p01"}}}`))

			slug, err := ReadGitHubAppSlug(context.Background(), client)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(slug).Should(Equal("konflux-kflux-ocp-p01"))
		})
	})

	When("the config map is missing", func() {
		It("should return an error naming the config map", func() {
			_, err := ReadGitHubAppSlug(context.Background(), fake.NewSimpleClientset())
			Expect(err).Should(MatchError(ContainSubstring("konflux-info/konflux-public-info")))
		})
	})

	When("info.json is malformed", func() {
		It("should return a parse error", func() {
			client := fake.NewSimpleClientset(konfluxInfoConfigMap(`{"integrations":`))

			_, err := ReadGitHubAppSlug(context.Background(), client)
			Expect(err).Should(MatchError(ContainSubstring("parse konflux-info/konflux-public-info info.json")))
		})
	})

	When("the GitHub Application URL is absent", func() {
		It("should return an error", func() {
			client := fake.NewSimpleClientset(konfluxInfoConfigMap(`{"integrations":{}}`))

			_, err := ReadGitHubAppSlug(context.Background(), client)
			Expect(err).Should(MatchError(ContainSubstring("not a github.com/apps URL")))
		})
	})

	When("the Kubernetes client is missing", func() {
		It("should return an error", func() {
			_, err := ReadGitHubAppSlug(context.Background(), nil)
			Expect(err).Should(MatchError("kubernetes client is required"))
		})
	})
})

var _ = Describe("githubAppSlugFromURL", func() {
	DescribeTable("app URL parsing",
		func(raw, want, wantErr string) {
			slug, err := githubAppSlugFromURL(raw)
			if wantErr != "" {
				Expect(err).Should(MatchError(ContainSubstring(wantErr)))
				return
			}
			Expect(err).ShouldNot(HaveOccurred())
			Expect(slug).Should(Equal(want))
		},
		Entry("github app URL", "https://github.com/apps/konflux-kflux-ocp-p01", "konflux-kflux-ocp-p01", ""),
		Entry("trailing slash", "https://github.com/apps/konflux-kflux-ocp-p01/", "konflux-kflux-ocp-p01", ""),
		Entry("non-github host", "https://gitlab.com/apps/konflux-kflux-ocp-p01", "", "not a github.com/apps URL"),
		Entry("missing apps path", "https://github.com/konflux-kflux-ocp-p01", "", "not a github.com/apps URL"),
		Entry("nested path", "https://github.com/apps/konflux/kflux", "", "no valid app slug"),
		Entry("empty URL", "", "", "not a github.com/apps URL"),
	)
})
