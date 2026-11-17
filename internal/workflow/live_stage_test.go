package workflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/cleanup"
	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	"github.com/redhat-appstudio/lkmabt/internal/collector"
	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/providers"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

type confirmationStub struct {
	approved  bool
	calls     int
	messages  []string
	onConfirm func()
}

// Confirm records the prompt message and returns the stub's configured approval.
func (s *confirmationStub) Confirm(_ context.Context, message string) (bool, error) {
	s.calls++
	s.messages = append(s.messages, message)
	if s.onConfirm != nil {
		s.onConfirm()
	}
	return s.approved, nil
}

type testNameLister struct {
	names []string
}

// List returns a copy of the fake namespace names.
func (l testNameLister) List(context.Context) ([]string, error) {
	return append([]string(nil), l.names...), nil
}

type githubAppProviderStub struct {
	slug    string
	err     error
	errs    []error
	verify  func(call int) error
	calls   int
	fixture providers.FixtureRepository
}

func (p *githubAppProviderStub) ValidateFixture(context.Context, providers.FixtureRepository) (providers.FixtureRepository, error) {
	return p.fixture, nil
}

func (p *githubAppProviderStub) ReadFile(context.Context, providers.FixtureRepository, string, string) (providers.FileVersion, error) {
	return providers.FileVersion{}, nil
}

func (p *githubAppProviderStub) UpdateFile(context.Context, providers.FixtureRepository, string, string, string, string) (providers.Commit, error) {
	return providers.Commit{}, nil
}

// VerifyGitHubAppInstallation records the slug and returns the next queued
// error, then the configured error.
func (p *githubAppProviderStub) VerifyGitHubAppInstallation(_ context.Context, slug string) error {
	p.slug = slug
	p.calls++
	if p.verify != nil {
		return p.verify(p.calls)
	}
	if len(p.errs) > 0 {
		err := p.errs[0]
		p.errs = p.errs[1:]
		return err
	}
	return p.err
}

// appInstallationRequired builds the provider error for an uninstalled cluster App.
func appInstallationRequired() *providers.AppInstallationRequiredError {
	return &providers.AppInstallationRequiredError{
		AppSlug:    "cluster-app",
		Owner:      "redhat-appstudio-qe",
		Repository: "redhat-appstudio-qe/dr_test_mathwizz",
		InstallURL: "https://github.com/apps/cluster-app/installations/new",
	}
}

// konfluxInfoObject builds the ConfigMap that publishes the cluster's GitHub App URL.
func konfluxInfoObject(appURL string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "konflux-info", Name: "konflux-public-info"},
		Data:       map[string]string{"info.json": fmt.Sprintf(`{"integrations":{"github":{"application_url":%q}}}`, appURL)},
	}
}

var _ = Describe("LiveStage", func() {
	Describe("LiveStage tenant namespace preflight", func() {
		When("namespaces are owned", func() {
			It("should delete an owned stale namespace without prompting", func() {
				dynamic := namespaceDynamicClient(namespaceObject("tenant", "run-1"))
				prompt := &confirmationStub{approved: true}
				log := &bytes.Buffer{}
				stage := &LiveStage{
					Request:   config.Request{TenantNamespace: "tenant"},
					Prompt:    prompt,
					Log:       log,
					Namespace: cleanup.NamespaceService{Dynamic: dynamic, PollInterval: time.Millisecond, NameLister: testNameLister{names: []string{"tenant"}}},
				}
				Expect(stage.ensureTenantNamespaceAvailable(context.Background(), &model.RunManifest{RunID: "run-2"})).Should(Succeed())
				Expect(prompt.calls).Should(Equal(0))
				_, err := dynamic.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant", metav1.GetOptions{})
				Expect(apierrors.IsNotFound(err)).Should(BeTrue())
				Expect(log.String()).Should(ContainSubstring("deleting stale owned tenant namespace tenant from run run-1"))
			})

			It("should delete owned exact and prefixed namespaces without prompting", func() {
				dynamic := namespaceDynamicClient(
					namespaceObject("tenant", "run-1"),
					namespaceObject("tenant-20261005b", "run-2"),
				)
				prompt := &confirmationStub{approved: true}
				stage := &LiveStage{
					Request: config.Request{TenantNamespace: "tenant"},
					Prompt:  prompt,
					Log:     &bytes.Buffer{},
					Namespace: cleanup.NamespaceService{
						Dynamic:      dynamic,
						PollInterval: time.Millisecond,
						NameLister:   testNameLister{names: []string{"tenant-20261005b", "tenant"}},
					},
				}

				Expect(stage.ensureTenantNamespaceAvailable(context.Background(), &model.RunManifest{RunID: "run-3"})).Should(Succeed())
				Expect(prompt.calls).Should(Equal(0))
				for _, name := range []string{"tenant", "tenant-20261005b"} {
					_, err := dynamic.Resource(cleanup.NamespaceGVR).Get(context.Background(), name, metav1.GetOptions{})
					Expect(apierrors.IsNotFound(err)).Should(BeTrue(), name)
				}
			})
		})

		When("namespaces are unowned", func() {
			It("should retain an unowned namespace when the prompt is declined", func() {
				dynamic := namespaceDynamicClient(namespaceObject("tenant", "run-1"))
				object, err := dynamic.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant", metav1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())
				object.SetLabels(map[string]string{cleanup.RunIDLabel: "run-1"})
				_, err = dynamic.Resource(cleanup.NamespaceGVR).Update(context.Background(), object, metav1.UpdateOptions{})
				Expect(err).ShouldNot(HaveOccurred())
				prompt := &confirmationStub{}
				stage := &LiveStage{
					Request:   config.Request{TenantNamespace: "tenant"},
					Prompt:    prompt,
					Log:       &bytes.Buffer{},
					Namespace: cleanup.NamespaceService{Dynamic: dynamic, NameLister: testNameLister{names: []string{"tenant"}}},
				}
				err = stage.ensureTenantNamespaceAvailable(context.Background(), &model.RunManifest{RunID: "run-2"})
				Expect(err).Should(MatchError(ContainSubstring("tenant namespace tenant retained")))
				Expect(prompt.calls).Should(Equal(1))
				_, getErr := dynamic.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant", metav1.GetOptions{})
				Expect(getErr).ShouldNot(HaveOccurred())
			})

			It("should delete an approved unowned namespace", func() {
				dynamic := namespaceDynamicClient(namespaceObject("tenant", "run-1"))
				object, err := dynamic.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant", metav1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())
				object.SetLabels(map[string]string{cleanup.RunIDLabel: "run-1"})
				_, err = dynamic.Resource(cleanup.NamespaceGVR).Update(context.Background(), object, metav1.UpdateOptions{})
				Expect(err).ShouldNot(HaveOccurred())
				prompt := &confirmationStub{approved: true}
				stage := &LiveStage{
					Request:   config.Request{TenantNamespace: "tenant"},
					Prompt:    prompt,
					Log:       &bytes.Buffer{},
					Namespace: cleanup.NamespaceService{Dynamic: dynamic, PollInterval: time.Millisecond, NameLister: testNameLister{names: []string{"tenant"}}},
				}
				Expect(stage.ensureTenantNamespaceAvailable(context.Background(), &model.RunManifest{RunID: "run-2"})).Should(Succeed())
				_, getErr := dynamic.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant", metav1.GetOptions{})
				Expect(apierrors.IsNotFound(getErr)).Should(BeTrue())
			})

			It("should prompt and delete after approval when the manager label differs", func() {
				dynamic := namespaceDynamicClient(namespaceObject("tenant", "run-1"))
				object, err := dynamic.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant", metav1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())
				object.SetLabels(map[string]string{cleanup.ManagedByLabel: "other-tool", cleanup.RunIDLabel: "run-1"})
				_, err = dynamic.Resource(cleanup.NamespaceGVR).Update(context.Background(), object, metav1.UpdateOptions{})
				Expect(err).ShouldNot(HaveOccurred())
				prompt := &confirmationStub{approved: true}
				stage := &LiveStage{
					Request:   config.Request{TenantNamespace: "tenant"},
					Prompt:    prompt,
					Log:       &bytes.Buffer{},
					Namespace: cleanup.NamespaceService{Dynamic: dynamic, PollInterval: time.Millisecond, NameLister: testNameLister{names: []string{"tenant"}}},
				}
				Expect(stage.ensureTenantNamespaceAvailable(context.Background(), &model.RunManifest{RunID: "run-2"})).Should(Succeed())
				Expect(prompt.calls).Should(Equal(1))
				Expect(prompt.messages).Should(HaveLen(1))
				Expect(prompt.messages[0]).Should(ContainSubstring("tenant"))
				Expect(prompt.messages[0]).Should(ContainSubstring("ownership labels"))
				_, getErr := dynamic.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant", metav1.GetOptions{})
				Expect(apierrors.IsNotFound(getErr)).Should(BeTrue())
			})

			It("should auto-delete the owned one and prompt once for the unowned one when owned and unowned namespaces are mixed", func() {
				dynamic := namespaceDynamicClient(
					namespaceObject("tenant", "run-1"),
					namespaceObject("tenant-20261005b", "run-2"),
				)
				object, err := dynamic.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant-20261005b", metav1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())
				object.SetLabels(map[string]string{cleanup.RunIDLabel: "run-2"})
				_, err = dynamic.Resource(cleanup.NamespaceGVR).Update(context.Background(), object, metav1.UpdateOptions{})
				Expect(err).ShouldNot(HaveOccurred())
				prompt := &confirmationStub{approved: true}
				stage := &LiveStage{
					Request: config.Request{TenantNamespace: "tenant"},
					Prompt:  prompt,
					Log:     &bytes.Buffer{},
					Namespace: cleanup.NamespaceService{
						Dynamic:      dynamic,
						PollInterval: time.Millisecond,
						NameLister:   testNameLister{names: []string{"tenant", "tenant-20261005b"}},
					},
				}
				Expect(stage.ensureTenantNamespaceAvailable(context.Background(), &model.RunManifest{RunID: "run-3"})).Should(Succeed())
				Expect(prompt.calls).Should(Equal(1))
				for _, name := range []string{"tenant", "tenant-20261005b"} {
					_, getErr := dynamic.Resource(cleanup.NamespaceGVR).Get(context.Background(), name, metav1.GetOptions{})
					Expect(apierrors.IsNotFound(getErr)).Should(BeTrue(), name)
				}
			})
		})

		When("ownership labels are absent and no prompt is configured", func() {
			It("should refuse to continue", func() {
				dynamic := namespaceDynamicClient(namespaceObject("tenant", "run-1"))
				object, err := dynamic.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant", metav1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())
				object.SetLabels(map[string]string{cleanup.RunIDLabel: "run-1"})
				_, err = dynamic.Resource(cleanup.NamespaceGVR).Update(context.Background(), object, metav1.UpdateOptions{})
				Expect(err).ShouldNot(HaveOccurred())
				stage := &LiveStage{
					Request:   config.Request{TenantNamespace: "tenant"},
					Namespace: cleanup.NamespaceService{Dynamic: dynamic, NameLister: testNameLister{names: []string{"tenant"}}},
				}
				err = stage.ensureTenantNamespaceAvailable(context.Background(), &model.RunManifest{RunID: "run-2"})
				Expect(err).Should(MatchError(ContainSubstring("ownership labels")))
				Expect(err).Should(MatchError(ContainSubstring("requires interactive approval")))
				_, getErr := dynamic.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant", metav1.GetOptions{})
				Expect(getErr).ShouldNot(HaveOccurred())
			})
		})

		When("namespaces are out of scope", func() {
			It("should leave them untouched", func() {
				dynamic := namespaceDynamicClient(
					namespaceObject("other", "run-1"),
					namespaceObject("tenantx", "run-2"),
				)
				prompt := &confirmationStub{approved: true}
				stage := &LiveStage{
					Request: config.Request{TenantNamespace: "tenant"},
					Prompt:  prompt,
					Log:     &bytes.Buffer{},
					Namespace: cleanup.NamespaceService{
						Dynamic:      dynamic,
						PollInterval: time.Millisecond,
						NameLister:   testNameLister{names: []string{"other", "tenantx"}},
					},
				}
				Expect(stage.ensureTenantNamespaceAvailable(context.Background(), &model.RunManifest{RunID: "run-3"})).Should(Succeed())
				Expect(prompt.calls).Should(Equal(0))
				for _, name := range []string{"other", "tenantx"} {
					_, getErr := dynamic.Resource(cleanup.NamespaceGVR).Get(context.Background(), name, metav1.GetOptions{})
					Expect(getErr).ShouldNot(HaveOccurred(), name)
				}
			})

			It("allows missing tenant namespace", func() {
				stage := &LiveStage{
					Request:   config.Request{TenantNamespace: "tenant"},
					Log:       &bytes.Buffer{},
					Namespace: cleanup.NamespaceService{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), NameLister: testNameLister{names: nil}},
				}
				Expect(stage.ensureTenantNamespaceAvailable(context.Background(), &model.RunManifest{RunID: "run-1"})).Should(Succeed())
			})
		})

		When("the lister returns no scoped names", func() {
			It("should not prompt", func() {
				dynamic := namespaceDynamicClient(namespaceObject("other", "run-1"))
				prompt := &confirmationStub{approved: true}
				stage := &LiveStage{
					Request: config.Request{TenantNamespace: "tenant"},
					Prompt:  prompt,
					Log:     &bytes.Buffer{},
					Namespace: cleanup.NamespaceService{
						Dynamic:      dynamic,
						PollInterval: time.Millisecond,
						NameLister:   testNameLister{names: []string{"other"}},
					},
				}
				Expect(stage.ensureTenantNamespaceAvailable(context.Background(), &model.RunManifest{RunID: "run-2"})).Should(Succeed())
				Expect(prompt.calls).Should(Equal(0))
				_, getErr := dynamic.Resource(cleanup.NamespaceGVR).Get(context.Background(), "other", metav1.GetOptions{})
				Expect(getErr).ShouldNot(HaveOccurred())
			})
		})
	})
	Describe("LiveStage GitHub App preflight", func() {
		When("the cluster publishes an installed app", func() {
			It("should verify the app against the fixture", func() {
				provider := &githubAppProviderStub{}
				log := &bytes.Buffer{}
				stage := &LiveStage{
					Provider: provider,
					Clients:  &cluster.ClientSet{Kubernetes: kubernetesfake.NewSimpleClientset(konfluxInfoObject("https://github.com/apps/konflux-kflux-ocp-p01"))},
					Log:      log,
				}

				Expect(stage.verifyGitHubApp(context.Background())).Should(Succeed())
				Expect(provider.slug).Should(Equal("konflux-kflux-ocp-p01"))
				Expect(log.String()).Should(ContainSubstring("GitHub App konflux-kflux-ocp-p01 installation verified"))
			})
		})

		When("the provider cannot verify selected repositories", func() {
			It("should warn and continue", func() {
				provider := &githubAppProviderStub{err: fmt.Errorf("%w: selected repositories", providers.ErrUnverifiable)}
				log := &bytes.Buffer{}
				stage := &LiveStage{
					Provider: provider,
					Clients:  &cluster.ClientSet{Kubernetes: kubernetesfake.NewSimpleClientset(konfluxInfoObject("https://github.com/apps/konflux-kflux-ocp-p01"))},
					Log:      log,
				}

				Expect(stage.verifyGitHubApp(context.Background())).Should(Succeed())
				Expect(log.String()).Should(ContainSubstring("warning:"))
				Expect(log.String()).Should(ContainSubstring("selected repositories"))
			})
		})

		When("the provider rejects the installation", func() {
			It("should return the provider error", func() {
				provider := &githubAppProviderStub{err: errors.New("GitHub App konflux-kflux-ocp-p01 is not installed")}
				stage := &LiveStage{
					Provider: provider,
					Clients:  &cluster.ClientSet{Kubernetes: kubernetesfake.NewSimpleClientset(konfluxInfoObject("https://github.com/apps/konflux-kflux-ocp-p01"))},
					Log:      &bytes.Buffer{},
				}

				Expect(stage.verifyGitHubApp(context.Background())).Should(MatchError("GitHub App konflux-kflux-ocp-p01 is not installed"))
			})
		})

		When("the cluster clients are missing", func() {
			It("should fail before reading the cluster info", func() {
				stage := &LiveStage{Provider: &githubAppProviderStub{}, Log: &bytes.Buffer{}}

				Expect(stage.verifyGitHubApp(context.Background())).Should(MatchError("cluster clients are not initialized"))
			})
		})

		When("the app is not installed in an interactive session", func() {
			It("should open the installation page and wait for the installation", func() {
				required := appInstallationRequired()
				provider := &githubAppProviderStub{errs: []error{required, required}}
				out := &bytes.Buffer{}
				var opened []string
				stage := &LiveStage{
					Provider:    provider,
					Clients:     &cluster.ClientSet{Kubernetes: kubernetesfake.NewSimpleClientset(konfluxInfoObject("https://github.com/apps/cluster-app"))},
					Interactive: true,
					Out:         out,
					OpenURL: func(rawURL string) error {
						opened = append(opened, rawURL)
						return nil
					},
					Log: &bytes.Buffer{},
				}
				previous := githubAppPollInterval
				githubAppPollInterval = time.Millisecond
				defer func() { githubAppPollInterval = previous }()

				Expect(stage.verifyGitHubApp(context.Background())).Should(Succeed())
				Expect(opened).Should(Equal([]string{required.InstallURL}))
				Expect(provider.calls).Should(Equal(3))
				Expect(out.String()).Should(ContainSubstring(required.InstallURL))
				Expect(out.String()).Should(ContainSubstring("LKMABT is opening the installation page in your browser"))
				Expect(out.String()).Should(ContainSubstring("LKMABT has already acquired its GitHub API token"))
				Expect(out.String()).Should(ContainSubstring("Approve it in a browser session signed in as an organization owner of redhat-appstudio-qe"))
				Expect(out.String()).Should(ContainSubstring("Select the redhat-appstudio-qe organization"))
				Expect(out.String()).Should(ContainSubstring("Grant access to redhat-appstudio-qe/dr_test_mathwizz, or to all repositories"))
				Expect(out.String()).Should(ContainSubstring("Return to this terminal"))
				Expect(out.String()).Should(ContainSubstring("GitHub App cluster-app installation verified"))
			})
		})

		When("the app is not installed in a non-interactive session", func() {
			It("should fail with the typed installation error without opening a browser", func() {
				required := appInstallationRequired()
				provider := &githubAppProviderStub{err: required}
				opened := false
				stage := &LiveStage{
					Provider: provider,
					Clients:  &cluster.ClientSet{Kubernetes: kubernetesfake.NewSimpleClientset(konfluxInfoObject("https://github.com/apps/cluster-app"))},
					OpenURL: func(string) error {
						opened = true
						return nil
					},
					Log: &bytes.Buffer{},
				}

				err := stage.verifyGitHubApp(context.Background())
				Expect(errors.As(err, &required)).Should(BeTrue())
				Expect(err).Should(MatchError(ContainSubstring("GitHub App cluster-app is not installed for organization redhat-appstudio-qe")))
				Expect(opened).Should(BeFalse())
				Expect(provider.calls).Should(Equal(1))
			})
		})

		When("the installation does not arrive before the deadline without an interactive prompt", func() {
			It("should fail when the preflight context expires", func() {
				required := appInstallationRequired()
				provider := &githubAppProviderStub{err: required}
				stage := &LiveStage{
					Provider:    provider,
					Clients:     &cluster.ClientSet{Kubernetes: kubernetesfake.NewSimpleClientset(konfluxInfoObject("https://github.com/apps/cluster-app"))},
					Interactive: true,
					Out:         &bytes.Buffer{},
					OpenURL:     func(string) error { return nil },
					Log:         &bytes.Buffer{},
				}
				previous := githubAppPollInterval
				githubAppPollInterval = time.Millisecond
				defer func() { githubAppPollInterval = previous }()
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()

				err := stage.verifyGitHubApp(ctx)
				Expect(errors.Is(err, context.DeadlineExceeded)).Should(BeTrue())
				Expect(err).Should(MatchError(ContainSubstring("waiting for GitHub App cluster-app installation")))
			})
		})

		When("an installation check fails transiently", func() {
			It("should retry without warning before the status interval", func() {
				required := appInstallationRequired()
				provider := &githubAppProviderStub{errs: []error{required, errors.New("502 Bad Gateway")}}
				out := &bytes.Buffer{}
				stage := &LiveStage{
					Provider:    provider,
					Clients:     &cluster.ClientSet{Kubernetes: kubernetesfake.NewSimpleClientset(konfluxInfoObject("https://github.com/apps/cluster-app"))},
					Interactive: true,
					Out:         out,
					OpenURL:     func(string) error { return nil },
					Log:         &bytes.Buffer{},
				}
				previous := githubAppPollInterval
				githubAppPollInterval = time.Millisecond
				defer func() { githubAppPollInterval = previous }()

				Expect(stage.verifyGitHubApp(context.Background())).Should(Succeed())
				Expect(provider.calls).Should(Equal(3))
				Expect(out.String()).ShouldNot(ContainSubstring("502 Bad Gateway"))
				Expect(out.String()).ShouldNot(ContainSubstring("Still waiting"))
			})
		})

		When("installation checks keep failing", func() {
			It("should report the latest failure at the status interval", func() {
				required := appInstallationRequired()
				provider := &githubAppProviderStub{errs: []error{required}, err: errors.New("502 Bad Gateway")}
				out := &bytes.Buffer{}
				stage := &LiveStage{
					Provider:    provider,
					Clients:     &cluster.ClientSet{Kubernetes: kubernetesfake.NewSimpleClientset(konfluxInfoObject("https://github.com/apps/cluster-app"))},
					Interactive: true,
					Out:         out,
					OpenURL:     func(string) error { return nil },
					Log:         &bytes.Buffer{},
				}
				previousPoll, previousStatus := githubAppPollInterval, githubAppStatusInterval
				githubAppPollInterval = time.Millisecond
				githubAppStatusInterval = 5 * time.Millisecond
				defer func() {
					githubAppPollInterval = previousPoll
					githubAppStatusInterval = previousStatus
				}()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
				defer cancel()

				err := stage.verifyGitHubApp(ctx)
				Expect(errors.Is(err, context.DeadlineExceeded)).Should(BeTrue())
				Expect(err).Should(MatchError(ContainSubstring("last check: 502 Bad Gateway")))
				Expect(out.String()).Should(ContainSubstring("Still waiting for GitHub App cluster-app installation"))
				Expect(out.String()).Should(ContainSubstring("last check: 502 Bad Gateway"))
			})
		})

		When("the installation appears after the preflight deadline", func() {
			It("should keep waiting when approved", func() {
				required := appInstallationRequired()
				installed := false
				provider := &githubAppProviderStub{verify: func(int) error {
					if installed {
						return nil
					}
					return required
				}}
				prompt := &confirmationStub{approved: true, onConfirm: func() { installed = true }}
				out := &bytes.Buffer{}
				stage := &LiveStage{
					Provider:      provider,
					Clients:       &cluster.ClientSet{Kubernetes: kubernetesfake.NewSimpleClientset(konfluxInfoObject("https://github.com/apps/cluster-app"))},
					Interactive:   true,
					Prompt:        prompt,
					ParentContext: context.Background(),
					Out:           out,
					OpenURL:       func(string) error { return nil },
					Log:           &bytes.Buffer{},
				}
				previous := githubAppPollInterval
				githubAppPollInterval = time.Millisecond
				defer func() { githubAppPollInterval = previous }()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()

				Expect(stage.verifyGitHubApp(ctx)).Should(Succeed())
				Expect(prompt.calls).Should(Equal(1))
				Expect(prompt.messages[0]).Should(ContainSubstring("GitHub App cluster-app is still not installed. Keep waiting?"))
				Expect(out.String()).Should(ContainSubstring("continuing to wait for GitHub App cluster-app installation"))
				Expect(out.String()).Should(ContainSubstring("GitHub App cluster-app installation verified"))
			})
		})

		When("the installation appears after the preflight deadline but the wait is declined", func() {
			It("should fail with the deadline error", func() {
				required := appInstallationRequired()
				provider := &githubAppProviderStub{err: required}
				prompt := &confirmationStub{}
				stage := &LiveStage{
					Provider:      provider,
					Clients:       &cluster.ClientSet{Kubernetes: kubernetesfake.NewSimpleClientset(konfluxInfoObject("https://github.com/apps/cluster-app"))},
					Interactive:   true,
					Prompt:        prompt,
					ParentContext: context.Background(),
					Out:           &bytes.Buffer{},
					OpenURL:       func(string) error { return nil },
					Log:           &bytes.Buffer{},
				}
				previous := githubAppPollInterval
				githubAppPollInterval = time.Millisecond
				defer func() { githubAppPollInterval = previous }()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()

				err := stage.verifyGitHubApp(ctx)
				Expect(errors.Is(err, context.DeadlineExceeded)).Should(BeTrue())
				Expect(prompt.calls).Should(Equal(1))
			})
		})
	})

	It("persists the tenant namespace and ephemeral flag when fixture setup fails", func() {
		listKinds := map[schema.GroupVersionResource]string{
			cluster.NamespaceGVR:   "NamespaceList",
			cluster.ApplicationGVR: "ApplicationList",
			cluster.ComponentGVR:   "ComponentList",
			cluster.SecretGVR:      "SecretList",
		}
		wrongComponent := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "appstudio.redhat.com/v1alpha1", "kind": "Component",
			"metadata": map[string]any{"name": "mathwizz-web-server", "namespace": "tenant"},
			"spec": map[string]any{"application": "app", "source": map[string]any{"git": map[string]any{
				"url": "https://gitlab.com/other/repo", "revision": "main", "context": "web-server", "dockerfileUrl": "Dockerfile",
			}}},
		}}
		dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, wrongComponent)
		fixture := providers.FixtureRepository{Owner: "konflux-qe", Name: "dr_test_mathwizz_gl", URL: "https://gitlab.com/konflux-qe/dr_test_mathwizz_gl"}
		stage := &LiveStage{
			Request: config.Request{
				Provider: "gitlab", TenantNamespace: "tenant", ApplicationName: "app",
				SourceBranch: "main", ComponentPaths: config.DefaultComponentPaths,
				Credentials: config.Credentials{GitLabToken: "token", GitLabTokenEphemeral: true},
			},
			Clients:  &cluster.ClientSet{Dynamic: dynamic},
			Provider: &githubAppProviderStub{fixture: fixture},
			Fixture:  fixture,
			Log:      io.Discard,
		}
		manifest := &model.RunManifest{RunID: "run-1"}

		err := stage.EnsureFixture(context.Background(), manifest)
		Expect(err).Should(HaveOccurred())
		Expect(manifest.Fixture.TenantNamespace).Should(Equal("tenant"))
		Expect(manifest.Fixture.EphemeralGitLabAuth).Should(BeTrue())

		secret, getErr := dynamic.Resource(cluster.SecretGVR).Namespace("tenant").Get(context.Background(), cluster.GitLabCredentialSecretName, metav1.GetOptions{})
		Expect(getErr).ShouldNot(HaveOccurred())
		Expect(secret.GetAnnotations()).Should(HaveKeyWithValue("appstudio.redhat.com/scm.repository", "konflux-qe/dr_test_mathwizz_gl"))
	})

	It("restores recorded canonical repository from manifest", func() {
		got := fixtureFromManifest(model.FixtureIdentity{
			RepositoryOwner: "redhat-appstudio-qe",
			RepositoryName:  "dr_test_mathwizz",
			Repository:      "https://github.com/redhat-appstudio-qe/dr_test_mathwizz",
		})
		want := providers.FixtureRepository{Owner: "redhat-appstudio-qe", Name: "dr_test_mathwizz", URL: "https://github.com/redhat-appstudio-qe/dr_test_mathwizz"}
		Expect(got).Should(Equal(want))
	})

	It("derives component context and Dockerfile from configured paths", func() {
		for _, test := range []struct {
			path        string
			wantContext string
			wantFile    string
		}{
			{path: "web-server/Dockerfile", wantContext: "web-server", wantFile: "Dockerfile"},
			{path: "history-worker/Dockerfile", wantContext: "history-worker", wantFile: "Dockerfile"},
			{path: "frontend/Dockerfile", wantContext: "frontend", wantFile: "Dockerfile"},
			{path: "Dockerfile", wantContext: ".", wantFile: "Dockerfile"},
		} {
			got := componentSpec("component", "https://github.com/example/repository", "main", test.path)
			Expect(got.Context).Should(Equal(test.wantContext), test.path)
			Expect(got.Dockerfile).Should(Equal(test.wantFile), test.path)
		}
	})

	It("polls until pipeline runs succeed", func() {
		commits := []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "sha-1"}}
		calls := 0
		identities, err := waitForBuildMatches(context.Background(), time.Second, time.Second, time.Millisecond, "mathwizz-test", commits, func(context.Context) ([]unstructured.Unstructured, error) {
			calls++
			if calls == 1 {
				return nil, nil
			}
			return []unstructured.Unstructured{pipelineRun("run-1", "mathwizz-web-server", "sha-1", "uid-1", "True")}, nil
		})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(calls).Should(BeNumerically(">=", 2))
		Expect(identities).Should(HaveLen(1))
		Expect(identities[0].Name).Should(Equal("run-1"))
		Expect(identities[0].Succeeded).Should(BeTrue())
	})

	It("rejects failed PipelineRun", func() {
		commits := []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "sha-1"}}
		_, err := waitForBuildMatches(context.Background(), time.Second, time.Second, time.Millisecond, "mathwizz-test", commits, func(context.Context) ([]unstructured.Unstructured, error) {
			return []unstructured.Unstructured{pipelineRun("run-1", "mathwizz-web-server", "sha-1", "uid-1", "False")}, nil
		})
		Expect(err).Should(HaveOccurred())
	})

	It("fails with component, namespace, trigger SHA, and observed count when no PipelineRun appears", func() {
		commit := model.TriggerCommit{Component: "mathwizz-web-server", SHA: "trigger-sha", CreatedAt: time.Now().UTC().Add(-time.Second)}
		_, err := waitForBuildMatches(context.Background(), 10*time.Millisecond, time.Second, time.Millisecond, "mathwizz-test", []model.TriggerCommit{commit}, func(context.Context) ([]unstructured.Unstructured, error) {
			return nil, nil
		})
		Expect(err).Should(MatchError(And(
			ContainSubstring("mathwizz-web-server"),
			ContainSubstring("mathwizz-test"),
			ContainSubstring("trigger-sha"),
			ContainSubstring("observed 0 PipelineRuns"),
		)))
	})

	It("accepts one post-trigger PipelineRun when source SHA differs", func() {
		triggeredAt := time.Now().UTC()
		object := pipelineRun("run-fallback", "mathwizz-web-server", "observed-sha", "uid-fallback", "True")
		object.SetCreationTimestamp(metav1.NewTime(triggeredAt.Add(time.Second)))
		identities, pending, err := matchBuildsWithObjects([]unstructured.Unstructured{object}, []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "trigger-sha", CreatedAt: triggeredAt}})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(pending).Should(BeFalse())
		Expect(identities).Should(HaveLen(1))
		Expect(identities[0].SourceSHA).Should(Equal("observed-sha"))
	})

	It("rejects stale and ambiguous fallback PipelineRuns", func() {
		triggeredAt := time.Now().UTC()
		stale := pipelineRun("run-stale", "mathwizz-web-server", "stale-sha", "uid-stale", "True")
		stale.SetCreationTimestamp(metav1.NewTime(triggeredAt.Add(-time.Second)))
		_, pending, err := matchBuildsWithObjects([]unstructured.Unstructured{stale}, []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "trigger-sha", CreatedAt: triggeredAt}})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(pending).Should(BeTrue())

		first := pipelineRun("run-fallback-1", "mathwizz-web-server", "observed-sha-1", "uid-fallback-1", "Unknown")
		first.SetCreationTimestamp(metav1.NewTime(triggeredAt.Add(time.Second)))
		second := pipelineRun("run-fallback-2", "mathwizz-web-server", "observed-sha-2", "uid-fallback-2", "Unknown")
		second.SetCreationTimestamp(metav1.NewTime(triggeredAt.Add(2 * time.Second)))
		_, _, err = matchBuildsWithObjects([]unstructured.Unstructured{first, second}, []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "trigger-sha", CreatedAt: triggeredAt}})
		Expect(err).Should(MatchError(ContainSubstring("ambiguous")))
	})

	It("waits for Buildah TaskRuns without waiting for validation TaskRuns", func() {
		commit := model.TriggerCommit{Component: "mathwizz-web-server", SHA: "observed-sha", CreatedAt: time.Now().UTC()}
		pipeline := pipelineRun("run-build", "mathwizz-web-server", "observed-sha", "uid-build", "Unknown")
		build := buildTaskRunForWorkflow("build-task", "uid-build", "True", "buildah-oci-ta")
		validation := buildTaskRunForWorkflow("validation-task", "uid-build", "False", "sast-shell-check")
		identities, err := waitForBuildTaskRuns(context.Background(), time.Second, time.Second, time.Millisecond, "mathwizz-test", []model.TriggerCommit{commit}, func(context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, error) {
			return []unstructured.Unstructured{pipeline}, []unstructured.Unstructured{build, validation}, nil
		})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(identities).Should(HaveLen(1))
		Expect(identities[0].Name).Should(Equal("run-build"))
	})

	It("collects failed container logs", func() {
		pipeline := pipelineRun("run-1", "mathwizz-web-server", "sha-1", "uid-1", "False")
		pipeline.SetNamespace("tenant")
		taskRun := failedTaskRunForWorkflow()
		dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "components"}: "ComponentList",
			{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}:               "PipelineRunList",
			{Group: "tekton.dev", Version: "v1", Resource: "taskruns"}:                   "TaskRunList",
		}, &pipeline, taskRun)
		kube := kubernetesfake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "build-pod", Namespace: "tenant"}})
		stateDir := GinkgoT().TempDir()
		stage := &LiveStage{
			Request:              config.Request{StateDir: stateDir, Timeouts: config.Timeouts{Trigger: time.Second, Build: time.Second}},
			Clients:              &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kube},
			FailedBuildCollector: &collector.FailedBuildCollector{Dynamic: dynamic, Kubernetes: kube},
		}
		manifest := &model.RunManifest{
			ArtifactDirectory: "run-1",
			Fixture:           model.FixtureIdentity{TenantNamespace: "tenant"},
			TriggerCommits:    []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "sha-1"}},
		}
		err := stage.VerifyBuilds(context.Background(), manifest)
		var pipelineRunError *PipelineRunFailedError
		Expect(errors.As(err, &pipelineRunError)).Should(BeTrue())
		Expect(manifest.FailedBuildLogs).Should(HaveLen(1))
		Expect(manifest.FailedBuildLogs[0].LogPath).ShouldNot(BeEmpty())
		Expect(manifest.FailedBuildLogs[0].LogPath).Should(Equal(filepath.Join("failed-builds", "build-task_step-build.log")))
		_, err = os.Stat(filepath.Join(stateDir, manifest.ArtifactDirectory, manifest.FailedBuildLogs[0].LogPath))
		Expect(err).ShouldNot(HaveOccurred())
	})

	It("counts baseline PipelineRuns", func() {
		objects := []unstructured.Unstructured{
			pipelineRun("existing-1", "mathwizz-web-server", "old-sha", "uid-old-1", "True"),
			pipelineRun("existing-2", "mathwizz-web-server", "old-sha-2", "uid-old-2", "False"),
			pipelineRun("existing-3", "mathwizz-web-server", "old-sha-3", "uid-old-3", "Unknown"),
		}
		succeeded, total := countBaselinePRs(objects)
		Expect(succeeded).Should(Equal(1))
		Expect(total).Should(Equal(3))

		succeeded, total = countBaselinePRs(nil)
		Expect(succeeded).Should(BeZero())
		Expect(total).Should(BeZero())
	})

	It("detects build overshoot", func() {
		commits := []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "sha-1"}}
		_, err := waitForBuildMatches(context.Background(), time.Second, time.Second, time.Millisecond, "mathwizz-test", commits, func(context.Context) ([]unstructured.Unstructured, error) {
			return []unstructured.Unstructured{
				pipelineRun("run-1", "mathwizz-web-server", "sha-1", "uid-1", "True"),
				pipelineRun("run-2", "mathwizz-web-server", "sha-1", "uid-2", "True"),
			}, nil
		})
		Expect(err).Should(MatchError(ContainSubstring("overshoot")))
	})

	It("polls PaC before triggering commits", func() {
		components := []string{"mathwizz-web-server", "mathwizz-history-worker", "mathwizz-frontend"}
		calls := 0
		err := waitForPaCEnabled(context.Background(), time.Second, time.Millisecond, components, func(context.Context) ([]unstructured.Unstructured, error) {
			calls++
			state := "pending"
			if calls > 1 {
				state = "enabled"
			}
			objects := make([]unstructured.Unstructured, 0, len(components))
			for _, component := range components {
				objects = append(objects, componentWithPaCStatus(component, state))
			}
			return objects, nil
		})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(calls).Should(BeNumerically(">=", 2))
	})

	It("detects repository onboarding failure in PaC status", func() {
		object := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{
				"name": "mathwizz-web-server",
				"annotations": map[string]any{
					"build.appstudio.openshift.io/status": `{"pac":{"state":"error","error-message":"repository is restricted"}}`,
				},
			},
		}}
		Expect(paCStatusError(&object)).Should(HaveOccurred())
	})
})

// failedTaskRunForWorkflow creates a failed Buildah TaskRun with a pod and step container.
func failedTaskRunForWorkflow() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tekton.dev/v1", "kind": "TaskRun",
		"metadata": map[string]any{
			"name": "build-task", "namespace": "tenant",
			"labels": map[string]any{
				"tekton.dev/pipelineRun":  "run-1",
				"tekton.dev/pipelineTask": "build-container",
				"tekton.dev/task":         "buildah-oci-ta",
			},
		},
		"status": map[string]any{
			"podName":    "build-pod",
			"conditions": []any{map[string]any{"type": "Succeeded", "status": "False", "message": "step failed"}},
			"steps":      []any{map[string]any{"name": "build", "container": "step-build", "terminated": map[string]any{"exitCode": int64(1)}}},
		},
	}}
}

// pipelineRun creates a PipelineRun fixture with component, SHA, identity, and status metadata.
func pipelineRun(name, component, sha, uid, status string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tekton.dev/v1",
		"kind":       "PipelineRun",
		"metadata": map[string]any{
			"name":      name,
			"namespace": "mathwizz-test",
			"uid":       uid,
			"labels": map[string]any{
				"appstudio.openshift.io/component": component,
				"pipelinesascode.tekton.dev/sha":   sha,
			},
		},
		"status": map[string]any{
			"conditions": []any{map[string]any{
				"type":               "Succeeded",
				"status":             status,
				"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
			}},
		},
	}}
}

// buildTaskRunForWorkflow creates a PipelineRun-owned TaskRun fixture for a build or validation task.
func buildTaskRunForWorkflow(name, ownerUID, status, task string) unstructured.Unstructured {
	pipelineTask := "build-container"
	if task == "sast-shell-check" {
		pipelineTask = task
	}
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tekton.dev/v1",
		"kind":       "TaskRun",
		"metadata": map[string]any{
			"name":      name,
			"namespace": "mathwizz-test",
			"labels": map[string]any{
				"tekton.dev/pipelineTask": pipelineTask,
				"tekton.dev/task":         task,
			},
			"ownerReferences": []any{map[string]any{
				"apiVersion": "tekton.dev/v1",
				"kind":       "PipelineRun",
				"name":       "run-build",
				"uid":        ownerUID,
			}},
		},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Succeeded", "status": status}}},
	}}
}

// componentWithPaCStatus creates a component fixture with the requested PaC state.
func componentWithPaCStatus(name, state string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name": name,
			"annotations": map[string]any{
				"build.appstudio.openshift.io/status": fmt.Sprintf(`{"pac":{"state":%q}}`, state),
			},
		},
	}}
}

// namespaceObject creates an owned tenant namespace fixture for a run.
func namespaceObject(name, runID string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": name,
			"labels": map[string]any{
				cleanup.ManagedByLabel: cleanup.ManagedByValue,
				cleanup.RunIDLabel:     runID,
			},
		},
	}}
}

// namespaceDynamicClient creates a fake dynamic client with list kinds for workflow resources.
func namespaceDynamicClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		cleanup.NamespaceGVR: "NamespaceList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "applications"}:      "ApplicationList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "components"}:        "ComponentList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "imagerepositories"}: "ImageRepositoryList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "releases"}:          "ReleaseList",
		{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}:                      "PipelineRunList",
	}, objects...)
}
