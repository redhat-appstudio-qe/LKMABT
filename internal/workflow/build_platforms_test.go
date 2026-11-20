package workflow

import (
	"context"
	"fmt"
	"io"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/platform"
	"github.com/redhat-appstudio/lkmabt/internal/providers"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type recordingProvider struct {
	files    map[string]string
	updated  map[string]string
	messages map[string]string
	failPath string
	commits  int
}

// ValidateFixture returns the fixture repository unchanged.
func (p *recordingProvider) ValidateFixture(_ context.Context, fixture providers.FixtureRepository) (providers.FixtureRepository, error) {
	return fixture, nil
}

// ReadFile returns the configured file content.
func (p *recordingProvider) ReadFile(_ context.Context, _ providers.FixtureRepository, path, branch string) (providers.FileVersion, error) {
	content, found := p.files[path]
	if !found {
		return providers.FileVersion{}, fmt.Errorf("file %s is not configured", path)
	}
	return providers.FileVersion{Path: path, Branch: branch, Content: content, SHA: "sha-" + path}, nil
}

// UpdateFile records the replacement content and returns a distinct commit.
func (p *recordingProvider) UpdateFile(_ context.Context, _ providers.FixtureRepository, path, _ string, content, _, message string) (providers.Commit, error) {
	if path == p.failPath {
		return providers.Commit{}, fmt.Errorf("provider rejected %s", path)
	}
	p.commits++
	if p.updated == nil {
		p.updated = make(map[string]string)
	}
	if p.messages == nil {
		p.messages = make(map[string]string)
	}
	p.updated[path] = content
	p.messages[path] = message
	return providers.Commit{SHA: fmt.Sprintf("setup-%d", p.commits), Message: message}, nil
}

var _ = Describe("build platform configuration", func() {
	When("the fixture PipelineRun declares build-platforms", func() {
		It("replaces the block list and preserves the other parameters", func() {
			contents := `spec:
  params:
  - name: git-url
    value: https://example.com/repo
  - name: build-platforms
    value:
    - linux/x86_64
    - linux/arm64
  - name: output-image
    value: quay.io/example/image
  pipelineSpec:
    params:
    - default:
      - linux/x86_64
      name: build-platforms
`
			updated, err := setBuildPlatforms(contents, []string{"linux/x86_64", "linux/arm64", "linux/s390x", "linux/ppc64le"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(updated).Should(ContainSubstring("  - name: build-platforms\n    value:\n    - linux/x86_64\n    - linux/arm64\n    - linux/s390x\n    - linux/ppc64le\n  - name: output-image"))
			Expect(updated).Should(ContainSubstring("  - name: git-url\n    value: https://example.com/repo"))
			Expect(updated).Should(ContainSubstring("    - default:\n      - linux/x86_64"))
		})

		It("ignores a nested pipelineSpec params block", func() {
			contents := "spec:\n  pipelineSpec:\n    params:\n    - name: build-platforms\n      value:\n      - linux/x86_64\n  params:\n  - name: git-url\n    value: https://example.com/repo\n  - name: build-platforms\n    value:\n    - linux/x86_64\n"
			updated, err := setBuildPlatforms(contents, []string{"linux/s390x"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(updated).Should(Equal("spec:\n  pipelineSpec:\n    params:\n    - name: build-platforms\n      value:\n      - linux/x86_64\n  params:\n  - name: git-url\n    value: https://example.com/repo\n  - name: build-platforms\n    value:\n    - linux/s390x\n"))
		})

		It("preserves a deeper list indentation", func() {
			contents := "spec:\n  params:\n  - name: build-platforms\n    value:\n      - linux/x86_64\n      - linux/arm64\n"
			updated, err := setBuildPlatforms(contents, []string{"linux/s390x"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(updated).Should(Equal("spec:\n  params:\n  - name: build-platforms\n    value:\n      - linux/s390x\n"))
		})

		It("replaces an inline value with a block list", func() {
			contents := "spec:\n  params:\n  - name: build-platforms\n    value: [linux/x86_64, linux/arm64]\n  - name: output-image\n    value: quay.io/example/image\n"
			updated, err := setBuildPlatforms(contents, []string{"linux/s390x"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(updated).Should(Equal("spec:\n  params:\n  - name: build-platforms\n    value:\n    - linux/s390x\n  - name: output-image\n    value: quay.io/example/image\n"))
		})
	})

	When("the fixture PipelineRun has no build-platforms value", func() {
		It("fails with an actionable error", func() {
			contents := "spec:\n  params:\n  - name: git-url\n    value: https://example.com/repo\n  pipelineSpec:\n    params:\n    - default:\n      - linux/x86_64\n      name: build-platforms\n"
			_, err := setBuildPlatforms(contents, []string{"linux/s390x"})
			Expect(err).Should(MatchError(ContainSubstring("does not declare build-platforms")))
			Expect(err).Should(MatchError(ContainSubstring("update the fixture pipeline")))
		})
	})

	When("the fixture has a build-platforms parameter", func() {
		It("commits the selected platforms to every PipelineRun", func() {
			manifest := "spec:\n  params:\n  - name: build-platforms\n    value:\n    - linux/x86_64\n    - linux/arm64\n"
			provider := &recordingProvider{files: map[string]string{
				".tekton/web.yaml":   manifest,
				".tekton/api.yaml":   manifest,
				".tekton/front.yaml": manifest,
			}}
			stage := &LiveStage{
				Request: config.Request{
					TektonPaths:  []string{".tekton/web.yaml", ".tekton/api.yaml", ".tekton/front.yaml"},
					Platforms:    []string{platform.LinuxS390X, platform.LinuxPPC64LE},
					SourceBranch: "main",
				},
				Fixture:  providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"},
				Provider: provider,
				Out:      io.Discard,
				Log:      io.Discard,
			}

			prerequisites, err := stage.configureBuildPlatforms(context.Background(), []string{"web", "api", "front"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(prerequisites).Should(HaveKeyWithValue("web", "setup-1"))
			Expect(prerequisites).Should(HaveKeyWithValue("api", "setup-2"))
			Expect(prerequisites).Should(HaveKeyWithValue("front", "setup-3"))
			Expect(provider.updated).Should(HaveLen(3))
			Expect(provider.updated[".tekton/web.yaml"]).Should(ContainSubstring("    - linux/s390x\n    - linux/ppc64le\n"))
			Expect(provider.messages[".tekton/web.yaml"]).Should(Equal(providers.CommitMessageConfigure))
			Expect(provider.messages[".tekton/web.yaml"]).Should(ContainSubstring("[skip ci]"))
		})

		It("does not commit when a later manifest is invalid", func() {
			provider := &recordingProvider{files: map[string]string{
				".tekton/web.yaml": "spec:\n  params:\n  - name: build-platforms\n    value:\n    - linux/x86_64\n",
				".tekton/api.yaml": "spec:\n  params:\n  - name: git-url\n    value: https://example.com/repo\n",
			}}
			stage := &LiveStage{
				Request: config.Request{
					TektonPaths:  []string{".tekton/web.yaml", ".tekton/api.yaml"},
					Platforms:    []string{platform.LinuxS390X},
					SourceBranch: "main",
				},
				Fixture:  providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"},
				Provider: provider,
			}

			_, err := stage.configureBuildPlatforms(context.Background(), []string{"web", "api"})
			Expect(err).Should(MatchError(ContainSubstring(".tekton/api.yaml")))
			Expect(err).Should(MatchError(ContainSubstring("does not declare build-platforms")))
			Expect(provider.updated).Should(BeEmpty())
		})

		It("names committed manifests when a later provider update fails", func() {
			manifest := "spec:\n  params:\n  - name: build-platforms\n    value:\n    - linux/x86_64\n"
			provider := &recordingProvider{
				files: map[string]string{
					".tekton/web.yaml": manifest,
					".tekton/api.yaml": manifest,
				},
				failPath: ".tekton/api.yaml",
			}
			stage := &LiveStage{
				Request: config.Request{
					TektonPaths:  []string{".tekton/web.yaml", ".tekton/api.yaml"},
					Platforms:    []string{platform.LinuxS390X},
					SourceBranch: "main",
				},
				Fixture:  providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"},
				Provider: provider,
			}

			prerequisites, err := stage.configureBuildPlatforms(context.Background(), []string{"web", "api"})
			Expect(err).Should(MatchError(ContainSubstring(".tekton/web.yaml")))
			Expect(prerequisites).Should(HaveKeyWithValue("web", "setup-1"))
		})

		It("skips a PipelineRun that already selects the platforms", func() {
			manifest := "spec:\n  params:\n  - name: build-platforms\n    value:\n    - linux/x86_64\n    - linux/arm64\n"
			provider := &recordingProvider{files: map[string]string{".tekton/web.yaml": manifest}}
			stage := &LiveStage{
				Request: config.Request{
					TektonPaths:  []string{".tekton/web.yaml"},
					Platforms:    []string{platform.LinuxAMD64, platform.LinuxARM64},
					SourceBranch: "main",
				},
				Fixture:  providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"},
				Provider: provider,
			}

			prerequisites, err := stage.configureBuildPlatforms(context.Background(), []string{"web"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(prerequisites).Should(BeEmpty())
			Expect(provider.updated).Should(BeEmpty())
		})
	})

	When("a prerequisite commit produced a PipelineRun", func() {
		It("does not use that run as trigger evidence", func() {
			setup := pipelineRun("setup-run", "mathwizz-web-server", "setup-sha", "uid-setup", "True")
			trigger := pipelineRun("trigger-run", "mathwizz-web-server", "trigger-sha", "uid-trigger", "True")
			commit := model.TriggerCommit{Component: "mathwizz-web-server", SHA: "trigger-sha", PrerequisiteSHAs: []string{"setup-sha"}}

			Expect(matchingPipelineRuns([]unstructured.Unstructured{setup}, commit)).Should(BeEmpty())
			matches := matchingPipelineRuns([]unstructured.Unstructured{setup, trigger}, commit)
			Expect(matches).Should(HaveLen(1))
			Expect(matches[0].GetName()).Should(Equal("trigger-run"))
		})
	})
})

var _ = Describe("build platform names", func() {
	It("uses the fixture spelling for each selected platform", func() {
		Expect(platform.ManifestNames(platform.All())).Should(Equal([]string{"linux/x86_64", "linux/arm64", "linux/s390x", "linux/ppc64le"}))
	})

	It("defaults a request without platforms to every supported platform", func() {
		stage := &LiveStage{}
		Expect(stage.platforms()).Should(Equal(platform.All()))
	})
})
