package workflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/cleanup"
	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	"github.com/redhat-appstudio/lkmabt/internal/collector"
	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/platform"
	"github.com/redhat-appstudio/lkmabt/internal/providers"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

type confirmationStub struct {
	approved  bool
	calls     int
	messages  []string
	onConfirm func()
}

// syncBuffer is a goroutine-safe bytes.Buffer for progress assertions.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p under the buffer mutex.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns the accumulated text under the buffer mutex.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// alertCall records one AlertSink invocation.
type alertCall struct {
	key   string
	title string
	lines []string
}

// recordingAlertSink implements both AlertSink and io.Writer so tests can
// assert the durable alert block and the plain fallback separately.
type recordingAlertSink struct {
	calls   []alertCall
	written bytes.Buffer
}

// Alert records one call with a copy of its detail lines.
func (s *recordingAlertSink) Alert(key, title string, lines ...string) {
	s.calls = append(s.calls, alertCall{key: key, title: title, lines: append([]string(nil), lines...)})
}

// Write records a plain writer copy of a block.
func (s *recordingAlertSink) Write(p []byte) (int, error) {
	return s.written.Write(p)
}

// reachableEgressProbe reports that every probe target is reachable.
func reachableEgressProbe(context.Context, string, string) (cluster.EgressResult, error) {
	return cluster.EgressResult{Reachable: true}, nil
}

// fakePreflightCluster satisfies cluster.Cluster for Preflight tests.
type fakePreflightCluster struct {
	server       string
	accessChecks *[]cluster.AccessCheck
	scheduling   *cluster.SchedulingSpec
	platforms    []cluster.PlatformSupport
}

// Server returns the configured API server.
func (f fakePreflightCluster) Server(context.Context) (string, error) { return f.server, nil }

// CheckAccess records the requested checks and passes them.
func (f fakePreflightCluster) CheckAccess(_ context.Context, checks []cluster.AccessCheck) []cluster.CheckResult {
	if f.accessChecks != nil {
		*f.accessChecks = checks
	}
	return nil
}

// CheckScheduling records the requested platforms and passes every check.
func (f fakePreflightCluster) CheckScheduling(_ context.Context, spec cluster.SchedulingSpec) cluster.SchedulingResult {
	if f.scheduling != nil {
		*f.scheduling = spec
	}
	if f.platforms != nil {
		return cluster.SchedulingResult{Platforms: f.platforms}
	}
	support := make([]cluster.PlatformSupport, 0, len(spec.Platforms))
	for _, name := range spec.Platforms {
		support = append(support, cluster.PlatformSupport{Platform: name, Supported: true, Configured: true})
	}
	return cluster.SchedulingResult{Platforms: support}
}

// contextCheckingLister fails when the namespace listing context is already done.
type contextCheckingLister struct {
	names []string
}

// List returns the configured names unless the context has ended.
func (l contextCheckingLister) List(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]string(nil), l.names...), nil
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
	mu      sync.Mutex
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

func (p *githubAppProviderStub) UpdateFile(context.Context, providers.FixtureRepository, string, string, string, string, string) (providers.Commit, error) {
	return providers.Commit{}, nil
}

// VerifyGitHubAppInstallation records the slug and returns the next queued
// error, then the configured error. Checks run in background goroutines, so
// the stub state is mutex-guarded.
func (p *githubAppProviderStub) VerifyGitHubAppInstallation(_ context.Context, slug string) error {
	p.mu.Lock()
	p.slug = slug
	p.calls++
	call := p.calls
	verify := p.verify
	var err error
	if verify == nil {
		if len(p.errs) > 0 {
			err = p.errs[0]
			p.errs = p.errs[1:]
		} else {
			err = p.err
		}
	}
	p.mu.Unlock()
	if verify != nil {
		return verify(call)
	}
	return err
}

// callCount returns the number of installation checks performed.
func (p *githubAppProviderStub) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// lastSlug returns the last checked app slug.
func (p *githubAppProviderStub) lastSlug() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.slug
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
	Describe("LiveStage platform preflight", func() {
		It("checks the selected build platforms", func() {
			var scheduling cluster.SchedulingSpec
			stage := &LiveStage{
				Request: config.Request{
					ClusterServer:   "https://api.example",
					TenantNamespace: "tenant",
					Platforms:       []string{platform.LinuxS390X, platform.LinuxPPC64LE},
				},
				PreflightCluster: fakePreflightCluster{server: "https://api.example", scheduling: &scheduling},
				Namespace:        cleanup.NamespaceService{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), NameLister: testNameLister{}},
			}

			Expect(stage.Preflight(context.Background(), &model.RunManifest{RunID: "run-1"})).Should(Succeed())
			Expect(scheduling.Platforms).Should(Equal([]string{platform.LinuxS390X, platform.LinuxPPC64LE}))
		})

		It("should alert the unsupported and effective platforms", func() {
			var scheduling cluster.SchedulingSpec
			sink := &recordingAlertSink{}
			log := &bytes.Buffer{}
			stage := &LiveStage{
				Request: config.Request{
					ClusterServer:   "https://api.example",
					ClusterName:     "osp-p01",
					TenantNamespace: "tenant",
					Platforms:       []string{platform.LinuxAMD64, platform.LinuxS390X},
				},
				PreflightCluster: fakePreflightCluster{
					server:     "https://api.example",
					scheduling: &scheduling,
					platforms: []cluster.PlatformSupport{
						{Platform: platform.LinuxAMD64, Configured: true, Supported: true},
						{Platform: platform.LinuxS390X, Details: "platform is not configured in host-config"},
					},
				},
				Namespace: cleanup.NamespaceService{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), NameLister: testNameLister{}},
				Out:       sink,
				Log:       log,
			}

			Expect(stage.Preflight(context.Background(), &model.RunManifest{RunID: "run-1"})).Should(Succeed())
			Expect(scheduling.Platforms).Should(Equal([]string{platform.LinuxAMD64, platform.LinuxS390X}))
			Expect(stage.Request.Platforms).Should(Equal([]string{platform.LinuxAMD64}))
			Expect(sink.calls).Should(HaveLen(1))
			Expect(sink.calls[0].key).Should(Equal("architecture:osp-p01:linux/s390x->:linux/amd64"))
			Expect(sink.calls[0].title).Should(Equal("osp-p01: architecture support narrowed"))
			Expect(sink.calls[0].lines).Should(Equal([]string{"not supported: linux/s390x", "testing: linux/amd64"}))
			Expect(sink.written.String()).Should(BeEmpty())
			Expect(log.String()).Should(Equal(" ! osp-p01: architecture support narrowed\n   not supported: linux/s390x\n   testing: linux/amd64\n"))
			Expect(log.String()).ShouldNot(ContainSubstring("does not declare"))
			Expect(log.String()).ShouldNot(ContainSubstring("warning:"))
			Expect(log.String()).ShouldNot(ContainSubstring("error:"))
		})

		It("should not alert when preflight rejects an entirely unsupported selection", func() {
			sink := &recordingAlertSink{}
			log := &bytes.Buffer{}
			stage := &LiveStage{
				Request: config.Request{
					ClusterServer:   "https://api.example",
					ClusterName:     "osp-p01",
					TenantNamespace: "tenant",
					Platforms:       []string{platform.LinuxAMD64, platform.LinuxS390X},
				},
				PreflightCluster: fakePreflightCluster{
					server: "https://api.example",
					platforms: []cluster.PlatformSupport{
						{Platform: platform.LinuxAMD64, Configured: false, Supported: false},
						{Platform: platform.LinuxS390X, Configured: false, Supported: false},
					},
				},
				Namespace: cleanup.NamespaceService{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), NameLister: testNameLister{}},
				Out:       sink,
				Log:       log,
			}

			err := stage.Preflight(context.Background(), &model.RunManifest{RunID: "run-1"})

			Expect(err).Should(MatchError(ContainSubstring("does not support any requested platform")))
			Expect(sink.calls).Should(BeEmpty())
			Expect(log.String()).Should(BeEmpty())
			Expect(stage.Request.Platforms).Should(Equal([]string{platform.LinuxAMD64, platform.LinuxS390X}))
		})

		It("should use a new alert key when the support split changes", func() {
			sink := &recordingAlertSink{}
			stage := &LiveStage{
				Request: config.Request{ClusterName: "osp-p01", Platforms: []string{platform.LinuxAMD64, platform.LinuxS390X}},
				Out:     sink,
				Log:     io.Discard,
			}
			amd64Supported := cluster.PreflightResult{Platforms: []cluster.PlatformSupport{
				{Platform: platform.LinuxAMD64, Configured: true, Supported: true},
				{Platform: platform.LinuxS390X, Configured: false, Supported: false},
			}}

			stage.limitToSupportedPlatforms(amd64Supported)

			stage.Request.Platforms = []string{platform.LinuxAMD64, platform.LinuxS390X}
			s390xSupported := cluster.PreflightResult{Platforms: []cluster.PlatformSupport{
				{Platform: platform.LinuxAMD64, Configured: false, Supported: false},
				{Platform: platform.LinuxS390X, Configured: true, Supported: true},
			}}
			stage.limitToSupportedPlatforms(s390xSupported)

			Expect(sink.calls).Should(HaveLen(2))
			Expect(sink.calls[0].key).Should(Equal("architecture:osp-p01:linux/s390x->:linux/amd64"))
			Expect(sink.calls[1].key).Should(Equal("architecture:osp-p01:linux/amd64->:linux/s390x"))
		})

		It("should key the alert as cluster when the request has no cluster name", func() {
			sink := &recordingAlertSink{}
			stage := &LiveStage{
				Request: config.Request{Platforms: []string{platform.LinuxAMD64, platform.LinuxS390X}},
				Out:     sink,
				Log:     io.Discard,
			}
			result := cluster.PreflightResult{Platforms: []cluster.PlatformSupport{
				{Platform: platform.LinuxAMD64, Configured: true, Supported: true},
				{Platform: platform.LinuxS390X, Configured: false, Supported: false},
			}}

			stage.limitToSupportedPlatforms(result)

			Expect(sink.calls).Should(HaveLen(1))
			Expect(sink.calls[0].key).Should(Equal("architecture:cluster:linux/s390x->:linux/amd64"))
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

				verification, err := stage.verifyGitHubApp(context.Background())
				Expect(err).ShouldNot(HaveOccurred())
				Expect(verification.Verified).Should(BeTrue())
				Expect(verification.Slug).Should(Equal("konflux-kflux-ocp-p01"))
				Expect(provider.lastSlug()).Should(Equal("konflux-kflux-ocp-p01"))
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

				_, err := stage.verifyGitHubApp(context.Background())
				Expect(err).ShouldNot(HaveOccurred())
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

				_, err := stage.verifyGitHubApp(context.Background())
				Expect(err).Should(MatchError("GitHub App konflux-kflux-ocp-p01 is not installed"))
			})
		})

		When("the cluster clients are missing", func() {
			It("should fail before reading the cluster info", func() {
				stage := &LiveStage{Provider: &githubAppProviderStub{}, Log: &bytes.Buffer{}}

				_, err := stage.verifyGitHubApp(context.Background())
				Expect(err).Should(MatchError("cluster clients are not initialized"))
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

				verification, err := stage.verifyGitHubApp(context.Background())
				Expect(err).ShouldNot(HaveOccurred())
				Expect(opened).Should(Equal([]string{required.InstallURL}))
				Expect(provider.callCount()).Should(Equal(3))
				Expect(out.String()).Should(ContainSubstring(required.InstallURL))
				Expect(out.String()).Should(ContainSubstring("LKMABT is opening the installation page in your browser"))
				Expect(out.String()).Should(ContainSubstring("LKMABT has already acquired its GitHub API token"))
				Expect(out.String()).Should(ContainSubstring("Approve it in a browser session signed in as an organization owner of redhat-appstudio-qe"))
				Expect(out.String()).Should(ContainSubstring("Select the redhat-appstudio-qe organization"))
				Expect(out.String()).Should(ContainSubstring("Grant access to redhat-appstudio-qe/dr_test_mathwizz, or to all repositories"))
				Expect(out.String()).Should(ContainSubstring("Return to this terminal"))
				Expect(verification.Verified).Should(BeTrue())
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

				_, err := stage.verifyGitHubApp(context.Background())
				Expect(errors.As(err, &required)).Should(BeTrue())
				Expect(err).Should(MatchError(ContainSubstring("GitHub App cluster-app is not installed for organization redhat-appstudio-qe")))
				Expect(opened).Should(BeFalse())
				Expect(provider.callCount()).Should(Equal(1))
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

				_, err := stage.verifyGitHubApp(ctx)
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

				_, err := stage.verifyGitHubApp(context.Background())
				Expect(err).ShouldNot(HaveOccurred())
				Expect(provider.callCount()).Should(Equal(3))
				Expect(out.String()).ShouldNot(ContainSubstring("502 Bad Gateway"))
				Expect(out.String()).ShouldNot(ContainSubstring("Still waiting"))
			})
		})

		When("installation checks keep failing", func() {
			It("should report the latest failure at the status interval", func() {
				required := appInstallationRequired()
				provider := &githubAppProviderStub{errs: []error{required}, err: errors.New("502 Bad Gateway")}
				out := &syncBuffer{}
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
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					_, err := stage.verifyGitHubApp(ctx)
					done <- err
				}()

				Eventually(out.String, 2*time.Second, 5*time.Millisecond).Should(ContainSubstring("Still waiting for GitHub App cluster-app installation"))
				Eventually(out.String, 2*time.Second, 5*time.Millisecond).Should(ContainSubstring("last check: 502 Bad Gateway"))
				cancel()
				var err error
				Eventually(func() bool {
					select {
					case err = <-done:
						return true
					default:
						return false
					}
				}, time.Second, 5*time.Millisecond).Should(BeTrue())
				Expect(errors.Is(err, context.Canceled)).Should(BeTrue())
			})
		})

		When("a check outlives the status interval", func() {
			It("should report progress while the check is still running", func() {
				required := appInstallationRequired()
				release := make(chan struct{})
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
				}()
				provider := &githubAppProviderStub{verify: func(call int) error {
					if call == 1 {
						return required
					}
					<-release
					return nil
				}}
				out := &syncBuffer{}
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
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() {
					_, err := stage.verifyGitHubApp(ctx)
					done <- err
				}()

				Eventually(out.String, 2*time.Second, 5*time.Millisecond).Should(ContainSubstring("Still waiting for GitHub App cluster-app installation"))
				close(release)
				Eventually(done, 2*time.Second).Should(Receive(BeNil()))
			})
		})

		When("the installation excludes the fixture repository", func() {
			It("should fail without retrying the check", func() {
				required := appInstallationRequired()
				selection := &providers.AppRepositorySelectionError{
					AppSlug:     "cluster-app",
					Owner:       "redhat-appstudio-qe",
					Repository:  "redhat-appstudio-qe/dr_test_mathwizz",
					SettingsURL: "https://github.com/organizations/redhat-appstudio-qe/settings/installations/7",
				}
				provider := &githubAppProviderStub{verify: func(call int) error {
					if call == 1 {
						return required
					}
					return selection
				}}
				stage := &LiveStage{
					Provider:      provider,
					Clients:       &cluster.ClientSet{Kubernetes: kubernetesfake.NewSimpleClientset(konfluxInfoObject("https://github.com/apps/cluster-app"))},
					Interactive:   true,
					ParentContext: context.Background(),
					Out:           &bytes.Buffer{},
					OpenURL:       func(string) error { return nil },
					Log:           &bytes.Buffer{},
				}
				previous := githubAppPollInterval
				githubAppPollInterval = time.Millisecond
				defer func() { githubAppPollInterval = previous }()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				_, err := stage.verifyGitHubApp(ctx)
				var got *providers.AppRepositorySelectionError
				Expect(errors.As(err, &got)).Should(BeTrue())
				Expect(err).Should(MatchError(ContainSubstring("not for redhat-appstudio-qe/dr_test_mathwizz")))
				Expect(provider.callCount()).Should(Equal(2))
			})
		})

		When("the installation appears after the preflight deadline", func() {
			It("should keep waiting when approved", func() {
				required := appInstallationRequired()
				var installed atomic.Bool
				provider := &githubAppProviderStub{verify: func(int) error {
					if installed.Load() {
						return nil
					}
					return required
				}}
				prompt := &confirmationStub{approved: true, onConfirm: func() { installed.Store(true) }}
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

				verification, err := stage.verifyGitHubApp(ctx)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(verification.Verified).Should(BeTrue())
				Expect(verification.Context).Should(Equal(stage.ParentContext))
				Expect(prompt.calls).Should(Equal(1))
				Expect(prompt.messages[0]).Should(ContainSubstring("GitHub App cluster-app is still not installed. Keep waiting?"))
				Expect(out.String()).Should(ContainSubstring("continuing to wait for GitHub App cluster-app installation"))
			})
		})

		When("the installation appears after the preflight deadline but the wait is declined", func() {
			It("should fail with the deadline error and installation URL", func() {
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

				_, err := stage.verifyGitHubApp(ctx)
				Expect(errors.Is(err, context.DeadlineExceeded)).Should(BeTrue())
				Expect(err).Should(MatchError(ContainSubstring(required.InstallURL)))
				Expect(prompt.calls).Should(Equal(1))
			})
		})

		When("the installation appears but its repository selection cannot be verified", func() {
			It("should warn without claiming the installation was verified", func() {
				required := appInstallationRequired()
				provider := &githubAppProviderStub{verify: func(call int) error {
					if call == 1 {
						return required
					}
					return fmt.Errorf("%w: selected repositories", providers.ErrUnverifiable)
				}}
				out := &bytes.Buffer{}
				stage := &LiveStage{
					Provider:      provider,
					Clients:       &cluster.ClientSet{Kubernetes: kubernetesfake.NewSimpleClientset(konfluxInfoObject("https://github.com/apps/cluster-app"))},
					Interactive:   true,
					ParentContext: context.Background(),
					Out:           out,
					OpenURL:       func(string) error { return nil },
					Log:           &bytes.Buffer{},
				}
				previous := githubAppPollInterval
				githubAppPollInterval = time.Millisecond
				defer func() { githubAppPollInterval = previous }()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				verification, err := stage.verifyGitHubApp(ctx)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(verification.Verified).Should(BeFalse())
				Expect(out.String()).Should(ContainSubstring("warning:"))
				Expect(out.String()).ShouldNot(ContainSubstring("installation verified"))
			})
		})

		When("the installation is approved after the preflight deadline", func() {
			It("should continue preflight through namespace discovery on the live context", func() {
				required := appInstallationRequired()
				var installed atomic.Bool
				provider := &githubAppProviderStub{verify: func(int) error {
					if installed.Load() {
						return nil
					}
					return required
				}}
				prompt := &confirmationStub{approved: true, onConfirm: func() { installed.Store(true) }}
				dynamic := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
				out := &bytes.Buffer{}
				stage := &LiveStage{
					Request: config.Request{
						Provider:        config.ProviderGitHub,
						ClusterServer:   "https://api.example",
						TenantNamespace: "tenant",
					},
					Provider:         provider,
					Clients:          &cluster.ClientSet{Kubernetes: kubernetesfake.NewSimpleClientset(konfluxInfoObject("https://github.com/apps/cluster-app")), Dynamic: dynamic},
					PreflightCluster: fakePreflightCluster{server: "https://api.example"},
					Interactive:      true,
					Prompt:           prompt,
					ParentContext:    context.Background(),
					Namespace:        cleanup.NamespaceService{Dynamic: dynamic, NameLister: contextCheckingLister{}},
					Out:              out,
					OpenURL:          func(string) error { return nil },
					Log:              &bytes.Buffer{},
				}
				previous := githubAppPollInterval
				githubAppPollInterval = time.Millisecond
				defer func() { githubAppPollInterval = previous }()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				manifest := &model.RunManifest{RunID: "run-1"}

				Expect(stage.Preflight(ctx, manifest)).Should(Succeed())
				Expect(prompt.calls).Should(Equal(1))
				Expect(out.String()).Should(ContainSubstring("GitHub App cluster-app installation verified"))
				Expect(manifest.TargetClusterServer).Should(Equal("https://api.example"))
			})
		})
	})

	It("does not wait for the browser opener to exit", func() {
		directory := GinkgoT().TempDir()
		for _, name := range []string{"xdg-open", "open"} {
			script := filepath.Join(directory, name)
			Expect(os.WriteFile(script, []byte("#!/bin/sh\nsleep 3\n"), 0o755)).Should(Succeed())
		}
		GinkgoT().Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))

		start := time.Now()
		Expect(OpenBrowser("https://github.com/apps/cluster-app/installations/new")).Should(Succeed())
		Expect(time.Since(start)).Should(BeNumerically("<", time.Second))
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
		dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "tenant"},
		}}, wrongComponent)
		kubernetes := kubernetesfake.NewSimpleClientset()
		kubernetes.PrependReactor("create", "selfsubjectaccessreviews", func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
		})
		fixture := providers.FixtureRepository{Owner: "konflux-qe", Name: "dr_test_mathwizz_gl", URL: "https://gitlab.com/konflux-qe/dr_test_mathwizz_gl"}
		names, paths, _ := config.FullComponents()
		stage := &LiveStage{
			Request: config.Request{
				Provider: "gitlab", TenantNamespace: "tenant", ApplicationName: "app",
				SourceBranch: "main", ComponentNames: names, ComponentPaths: paths,
				Credentials: config.Credentials{GitLabToken: "token", GitLabTokenEphemeral: true},
			},
			Clients:     &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetes},
			ProbeEgress: reachableEgressProbe,
			Provider:    &githubAppProviderStub{fixture: fixture},
			Fixture:     fixture,
			Log:         io.Discard,
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

	When("a smoke run selects one component", func() {
		It("should create the single component a smoke run selects", func() {
			fixture := providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"}
			dynamic := namespaceDynamicClient(namespaceObject("tenant", "run-1"))
			stage := &LiveStage{
				Request: config.Request{
					Provider:        config.ProviderGitHub,
					TenantNamespace: "tenant",
					ApplicationName: "app",
					SourceBranch:    "smoke-branch",
					Smoke:           true,
					ComponentNames:  []string{"mathwizz-history-worker"},
					ComponentPaths:  []string{"history-worker/Dockerfile"},
					TektonPaths:     []string{".tekton/konflux-test-mathwizz-history-worker-push.yaml"},
				},
				Clients:  &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetesfake.NewSimpleClientset()},
				Provider: &githubAppProviderStub{fixture: fixture},
				Fixture:  fixture,
				Log:      io.Discard,
			}
			manifest := &model.RunManifest{RunID: "run-1"}

			Expect(stage.EnsureFixture(context.Background(), manifest)).Should(Succeed())
			Expect(manifest.Fixture.Components).Should(Equal([]string{"mathwizz-history-worker"}))
			Expect(manifest.Fixture.Branch).Should(Equal("smoke-branch"))
			object, err := dynamic.Resource(cluster.ComponentGVR).Namespace("tenant").Get(context.Background(), "mathwizz-history-worker", metav1.GetOptions{})
			Expect(err).ShouldNot(HaveOccurred())
			git, found, getErr := unstructured.NestedMap(object.Object, "spec", "source", "git")
			Expect(getErr).ShouldNot(HaveOccurred())
			Expect(found).Should(BeTrue())
			Expect(git).Should(Equal(map[string]any{
				"url":           fixture.URL,
				"revision":      "smoke-branch",
				"context":       "history-worker",
				"dockerfileUrl": "Dockerfile",
			}))
			for _, name := range []string{"mathwizz-web-server", "mathwizz-frontend"} {
				_, getErr := dynamic.Resource(cluster.ComponentGVR).Namespace("tenant").Get(context.Background(), name, metav1.GetOptions{})
				Expect(apierrors.IsNotFound(getErr)).Should(BeTrue(), name)
			}
		})
	})

	When("a full run selects the fixture", func() {
		It("should create all three components in request order", func() {
			fixture := providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"}
			dynamic := namespaceDynamicClient(namespaceObject("tenant", "run-1"))
			stage := &LiveStage{
				Request: config.Request{
					Provider:        config.ProviderGitHub,
					TenantNamespace: "tenant",
					ApplicationName: "app",
					SourceBranch:    "main",
					ComponentNames:  []string{"mathwizz-web-server", "mathwizz-history-worker", "mathwizz-frontend"},
					ComponentPaths:  []string{"web-server/Dockerfile", "history-worker/Dockerfile", "frontend/Dockerfile"},
					TektonPaths: []string{
						".tekton/konflux-test-mathwizz-web-server-push.yaml",
						".tekton/konflux-test-mathwizz-history-worker-push.yaml",
						".tekton/konflux-test-mathwizz-frontend-push.yaml",
					},
				},
				Clients:     &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetesfake.NewSimpleClientset()},
				ProbeEgress: reachableEgressProbe,
				Provider:    &githubAppProviderStub{fixture: fixture},
				Fixture:     fixture,
				Log:         io.Discard,
			}
			manifest := &model.RunManifest{RunID: "run-1"}

			Expect(stage.EnsureFixture(context.Background(), manifest)).Should(Succeed())
			Expect(manifest.Fixture.Components).Should(Equal([]string{"mathwizz-web-server", "mathwizz-history-worker", "mathwizz-frontend"}))
		})
	})

	When("the selection is invalid", func() {
		It("should reject a selection whose names and paths disagree", func() {
			for _, test := range []struct {
				names []string
				paths []string
				want  string
			}{
				{
					names: []string{"mathwizz-web-server", "mathwizz-history-worker"},
					paths: []string{"web-server/Dockerfile"},
					want:  "run selects 2 components but 1 Dockerfile paths",
				},
				{
					names: []string{"mathwizz-web-server"},
					paths: []string{"web-server/Dockerfile", "history-worker/Dockerfile"},
					want:  "run selects 1 components but 2 Dockerfile paths",
				},
				{
					names: nil,
					paths: nil,
					want:  "run selects 0 components but 0 Dockerfile paths",
				},
			} {
				fixture := providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"}
				stage := &LiveStage{
					Request: config.Request{
						Provider:        config.ProviderGitHub,
						TenantNamespace: "tenant",
						ApplicationName: "app",
						SourceBranch:    "main",
						ComponentNames:  test.names,
						ComponentPaths:  test.paths,
					},
					Clients:  &cluster.ClientSet{Dynamic: namespaceDynamicClient(), Kubernetes: kubernetesfake.NewSimpleClientset()},
					Provider: &githubAppProviderStub{fixture: fixture},
					Fixture:  fixture,
					Log:      io.Discard,
				}
				Expect(stage.EnsureFixture(context.Background(), &model.RunManifest{RunID: "run-1"})).Should(MatchError(test.want), "%v", test.names)
			}
		})

		It("should reject a selection that disagrees with the expected component count", func() {
			fixture := providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"}
			dynamic := namespaceDynamicClient()
			stage := &LiveStage{
				Request: config.Request{
					Provider:        config.ProviderGitHub,
					TenantNamespace: "tenant",
					ApplicationName: "app",
					SourceBranch:    "main",
					ComponentNames:  []string{"mathwizz-history-worker"},
					ComponentPaths:  []string{"history-worker/Dockerfile"},
				},
				Clients:  &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetesfake.NewSimpleClientset()},
				Provider: &githubAppProviderStub{fixture: fixture},
				Fixture:  fixture,
				Log:      io.Discard,
			}

			err := stage.EnsureFixture(context.Background(), &model.RunManifest{RunID: "run-1"})

			Expect(err).Should(MatchError("fixture expects 3 components but got 1"))
			Expect(dynamic.Actions()).Should(BeEmpty())
		})
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

	It("rejects a later failed PipelineRun while an earlier component is still pending", func() {
		commits := []model.TriggerCommit{
			{Component: "mathwizz-web-server", SHA: "sha-web"},
			{Component: "mathwizz-frontend", SHA: "sha-frontend"},
		}
		pipelineRuns := []unstructured.Unstructured{
			pipelineRun("run-frontend", "mathwizz-frontend", "sha-frontend", "uid-frontend", "False"),
		}

		_, pending, err := matchBuildTaskRuns(pipelineRuns, nil, commits, nil)

		var failed *PipelineRunFailedError
		Expect(errors.As(err, &failed)).Should(BeTrue())
		Expect(failed.Name).Should(Equal("run-frontend"))
		Expect(pending).Should(BeFalse())
	})

	It("fails with component, namespace, trigger SHA, and observed count when no PipelineRun appears", func() {
		commit := model.TriggerCommit{Component: "mathwizz-web-server", SHA: "trigger-sha", CreatedAt: time.Now().UTC().Add(-time.Second)}
		_, err := waitForBuildTaskRuns(context.Background(), 10*time.Millisecond, time.Second, time.Millisecond, "mathwizz-test", []model.TriggerCommit{commit}, func(context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, error) {
			return nil, nil, nil
		}, nil, nil, buildEvidenceCache{})
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
		build := buildTaskRunForWorkflow("build-task", "uid-fallback", "True", "buildah-oci-ta")
		identities, pending, err := matchBuildTaskRuns(
			[]unstructured.Unstructured{object},
			[]unstructured.Unstructured{build},
			[]model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "trigger-sha", CreatedAt: triggeredAt}},
			nil,
		)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(pending).Should(BeFalse())
		Expect(identities).Should(HaveLen(1))
		Expect(identities[0].SourceSHA).Should(Equal("observed-sha"))
	})

	It("rejects stale and ambiguous fallback PipelineRuns", func() {
		triggeredAt := time.Now().UTC()
		stale := pipelineRun("run-stale", "mathwizz-web-server", "stale-sha", "uid-stale", "True")
		stale.SetCreationTimestamp(metav1.NewTime(triggeredAt.Add(-time.Second)))
		_, pending, err := matchBuildTaskRuns([]unstructured.Unstructured{stale}, nil, []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "trigger-sha", CreatedAt: triggeredAt}}, nil)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(pending).Should(BeTrue())

		first := pipelineRun("run-fallback-1", "mathwizz-web-server", "observed-sha-1", "uid-fallback-1", "Unknown")
		first.SetCreationTimestamp(metav1.NewTime(triggeredAt.Add(time.Second)))
		second := pipelineRun("run-fallback-2", "mathwizz-web-server", "observed-sha-2", "uid-fallback-2", "Unknown")
		second.SetCreationTimestamp(metav1.NewTime(triggeredAt.Add(2 * time.Second)))
		_, _, err = matchBuildTaskRuns([]unstructured.Unstructured{first, second}, nil, []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "trigger-sha", CreatedAt: triggeredAt}}, nil)
		Expect(err).Should(MatchError(ContainSubstring("ambiguous")))
	})

	It("should name every ambiguous fallback candidate and its source SHA", func() {
		trigger := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
		commit := model.TriggerCommit{Component: "mathwizz-web-server", SHA: "sha-trigger", CreatedAt: trigger}
		pipelines := []unstructured.Unstructured{
			pipelineRunAt("run-fallback-a", "mathwizz-web-server", "sha-a", "uid-a", "True", trigger.Add(time.Minute)),
			pipelineRunAt("run-fallback-b", "mathwizz-web-server", "sha-b", "uid-b", "True", trigger.Add(2*time.Minute)),
		}

		_, _, err := matchBuildTaskRuns(pipelines, nil, []model.TriggerCommit{commit}, nil)

		Expect(err).Should(MatchError("ambiguous PipelineRuns for component mathwizz-web-server after trigger sha-trigger; candidates: mathwizz-test/run-fallback-a (sha sha-a), mathwizz-test/run-fallback-b (sha sha-b)"))
	})

	It("should render a candidate without a source SHA as unknown", func() {
		object := pipelineRun("run-nosha", "mathwizz-web-server", "", "uid-nosha", "True")

		Expect(pipelineRunCandidates([]*unstructured.Unstructured{&object})).Should(Equal("mathwizz-test/run-nosha (sha unknown)"))
	})

	It("accepts successful Buildah TaskRuns while the PipelineRun is still running", func() {
		commit := model.TriggerCommit{Component: "mathwizz-web-server", SHA: "observed-sha", CreatedAt: time.Now().UTC()}
		pipeline := pipelineRun("run-build", "mathwizz-web-server", "observed-sha", "uid-build", "Unknown")
		build := buildTaskRunForWorkflow("build-task", "uid-build", "True", "buildah-oci-ta")
		identities, pending, err := matchBuildTaskRuns([]unstructured.Unstructured{pipeline}, []unstructured.Unstructured{build}, []model.TriggerCommit{commit}, nil)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(pending).Should(BeFalse())
		Expect(identities).Should(HaveLen(1))
		Expect(identities[0].Name).Should(Equal("run-build"))
	})

	It("waits for every distinct matrix platform before accepting a component", func() {
		commit := model.TriggerCommit{Component: "mathwizz-web-server", SHA: "observed-sha", CreatedAt: time.Now().UTC()}
		pipeline := pipelineRun("run-build", "mathwizz-web-server", "observed-sha", "uid-build", "Unknown")
		amd64 := buildTaskRunForWorkflow("build-task-amd64", "uid-build", "True", "buildah-oci-ta")
		duplicateAMD64 := buildTaskRunForWorkflow("build-task-amd64-2", "uid-build", "True", "buildah-oci-ta")
		arm64 := buildTaskRunForWorkflowOnPlatform("build-task-arm64", "uid-build", "True", "buildah-oci-ta", platform.LinuxARM64)
		expected := []string{platform.LinuxAMD64, platform.LinuxARM64}

		identities, pending, err := matchBuildTaskRunsWithExpected([]unstructured.Unstructured{pipeline}, []unstructured.Unstructured{amd64}, []model.TriggerCommit{commit}, expected, nil)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(pending).Should(BeTrue())
		Expect(identities).Should(BeEmpty())

		identities, pending, err = matchBuildTaskRunsWithExpected([]unstructured.Unstructured{pipeline}, []unstructured.Unstructured{amd64, duplicateAMD64}, []model.TriggerCommit{commit}, expected, nil)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(pending).Should(BeTrue())
		Expect(identities).Should(BeEmpty())

		identities, pending, err = matchBuildTaskRunsWithExpected([]unstructured.Unstructured{pipeline}, []unstructured.Unstructured{amd64, arm64}, []model.TriggerCommit{commit}, expected, nil)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(pending).Should(BeFalse())
		Expect(identities).Should(HaveLen(1))
	})

	It("waits for a running Buildah TaskRun to publish its platform", func() {
		commit := model.TriggerCommit{Component: "mathwizz-web-server", SHA: "observed-sha", CreatedAt: time.Now().UTC()}
		pipeline := pipelineRun("run-build", "mathwizz-web-server", "observed-sha", "uid-build", "Unknown")
		running := buildTaskRunForWorkflow("build-task", "uid-build", "Unknown", "buildah-oci-ta")
		unstructured.RemoveNestedField(running.Object, "status", "results")
		succeeded := buildTaskRunForWorkflowOnPlatform("build-task", "uid-build", "True", "buildah-oci-ta", platform.LinuxAMD64)
		expected := []string{platform.LinuxAMD64}

		identities, pending, err := matchBuildTaskRunsWithExpected([]unstructured.Unstructured{pipeline}, []unstructured.Unstructured{running}, []model.TriggerCommit{commit}, expected, nil)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(pending).Should(BeTrue())
		Expect(identities).Should(BeEmpty())

		identities, pending, err = matchBuildTaskRunsWithExpected([]unstructured.Unstructured{pipeline}, []unstructured.Unstructured{succeeded}, []model.TriggerCommit{commit}, expected, nil)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(pending).Should(BeFalse())
		Expect(identities).Should(HaveLen(1))
	})

	It("rejects a succeeded PipelineRun that is missing a matrix platform", func() {
		commit := model.TriggerCommit{Component: "mathwizz-web-server", SHA: "observed-sha", CreatedAt: time.Now().UTC()}
		pipeline := pipelineRun("run-build", "mathwizz-web-server", "observed-sha", "uid-build", "True")
		amd64 := buildTaskRunForWorkflow("build-task-amd64", "uid-build", "True", "buildah-oci-ta")

		_, _, err := matchBuildTaskRunsWithExpected([]unstructured.Unstructured{pipeline}, []unstructured.Unstructured{amd64}, []model.TriggerCommit{commit}, []string{platform.LinuxAMD64, platform.LinuxARM64}, nil)
		Expect(err).Should(MatchError(ContainSubstring("missing Buildah TaskRuns for " + platform.LinuxARM64)))
	})

	It("accepts a post-build PipelineRun failure when its Buildah TaskRuns succeeded", func() {
		commit := model.TriggerCommit{Component: "mathwizz-web-server", SHA: "observed-sha", CreatedAt: time.Now().UTC()}
		pipeline := pipelineRun("run-build", "mathwizz-web-server", "observed-sha", "uid-build", "False")
		build := buildTaskRunForWorkflow("build-task", "uid-build", "True", "buildah-oci-ta")
		identities, pending, err := matchBuildTaskRuns([]unstructured.Unstructured{pipeline}, []unstructured.Unstructured{build}, []model.TriggerCommit{commit}, nil)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(pending).Should(BeFalse())
		Expect(identities).Should(HaveLen(1))
		Expect(identities[0].Succeeded).Should(BeTrue())
	})

	It("fails a failed PipelineRun when only some Buildah TaskRuns succeeded", func() {
		commit := model.TriggerCommit{Component: "mathwizz-web-server", SHA: "observed-sha", CreatedAt: time.Now().UTC()}
		pipeline := pipelineRun("run-build", "mathwizz-web-server", "observed-sha", "uid-build", "False")
		build := buildTaskRunForWorkflow("build-task", "uid-build", "True", "buildah-oci-ta")
		unfinished := buildTaskRunForWorkflow("build-task-2", "uid-build", "Unknown", "buildah-oci-ta")
		_, pending, err := matchBuildTaskRuns([]unstructured.Unstructured{pipeline}, []unstructured.Unstructured{build, unfinished}, []model.TriggerCommit{commit}, nil)
		var failure *PipelineRunFailedError
		Expect(errors.As(err, &failure)).Should(BeTrue())
		Expect(pending).Should(BeFalse())
	})

	It("fails a build TaskRun while the PipelineRun is still pending", func() {
		commit := model.TriggerCommit{Component: "mathwizz-web-server", SHA: "observed-sha", CreatedAt: time.Now().UTC()}
		pipeline := pipelineRun("run-build", "mathwizz-web-server", "observed-sha", "uid-build", "Unknown")
		build := buildTaskRunForWorkflow("build-task", "uid-build", "False", "buildah-oci-ta")
		_, pending, err := matchBuildTaskRuns([]unstructured.Unstructured{pipeline}, []unstructured.Unstructured{build}, []model.TriggerCommit{commit}, nil)
		var failure *PipelineRunFailedError
		Expect(errors.As(err, &failure)).Should(BeTrue())
		Expect(pending).Should(BeFalse())
	})

	It("ignores non-Buildah TaskRuns after the PipelineRun succeeds", func() {
		commit := model.TriggerCommit{Component: "mathwizz-web-server", SHA: "observed-sha", CreatedAt: time.Now().UTC()}
		pipeline := pipelineRun("run-build", "mathwizz-web-server", "observed-sha", "uid-build", "True")
		build := buildTaskRunForWorkflow("build-task", "uid-build", "True", "buildah-oci-ta")
		validation := buildTaskRunForWorkflow("validation-task", "uid-build", "False", "sast-shell-check")
		identities, err := waitForBuildTaskRuns(context.Background(), time.Second, time.Second, time.Millisecond, "mathwizz-test", []model.TriggerCommit{commit}, func(context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, error) {
			return []unstructured.Unstructured{pipeline}, []unstructured.Unstructured{build, validation}, nil
		}, nil, nil, buildEvidenceCache{})
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

	It("accepts successful Buildah TaskRuns while the PipelineRun is still running", func() {
		pipeline := pipelineRun("run-1", "mathwizz-web-server", "sha-1", "uid-1", "Unknown")
		build := buildTaskRunForWorkflow("build-task", "uid-1", "True", "buildah-oci-ta")
		dynamic := namespaceDynamicClient(&pipeline, &build)
		stage := &LiveStage{
			Request: config.Request{
				StateDir:  GinkgoT().TempDir(),
				Platforms: []string{platform.LinuxAMD64},
				Timeouts:  config.Timeouts{Trigger: time.Second, Build: time.Second},
			},
			Clients: &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetesfake.NewSimpleClientset()},
		}
		manifest := &model.RunManifest{
			ArtifactDirectory: "run-1",
			Fixture:           model.FixtureIdentity{TenantNamespace: "mathwizz-test"},
			TriggerCommits:    []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "sha-1"}},
		}

		Expect(stage.VerifyBuilds(context.Background(), manifest)).Should(Succeed())
		Expect(manifest.PipelineRuns).Should(HaveLen(1))
		Expect(manifest.BuildOutputs).Should(HaveLen(1))
	})

	It("accepts a post-build PipelineRun failure when Buildah succeeded", func() {
		pipeline := pipelineRun("run-1", "mathwizz-web-server", "sha-1", "uid-1", "False")
		build := buildTaskRunForWorkflow("build-task", "uid-1", "True", "buildah-oci-ta")
		dynamic := namespaceDynamicClient(&pipeline, &build)
		stage := &LiveStage{
			Request: config.Request{
				StateDir:  GinkgoT().TempDir(),
				Platforms: []string{platform.LinuxAMD64},
				Timeouts:  config.Timeouts{Trigger: time.Second, Build: time.Second},
			},
			Clients: &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetesfake.NewSimpleClientset()},
		}
		manifest := &model.RunManifest{
			ArtifactDirectory: "run-1",
			Fixture:           model.FixtureIdentity{TenantNamespace: "mathwizz-test"},
			TriggerCommits:    []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "sha-1"}},
		}

		Expect(stage.VerifyBuilds(context.Background(), manifest)).Should(Succeed())
		Expect(manifest.PipelineRuns).Should(HaveLen(1))
		Expect(manifest.BuildOutputs).Should(HaveLen(1))
		Expect(manifest.FailedBuildLogs).Should(BeEmpty())
	})

	It("keeps completed component identities in a deferred manifest", func() {
		previousThreshold := admissionPendingThreshold
		admissionPendingThreshold = time.Millisecond
		DeferCleanup(func() { admissionPendingThreshold = previousThreshold })
		previousPoll := buildPollInterval
		buildPollInterval = time.Millisecond
		DeferCleanup(func() { buildPollInterval = previousPoll })

		doneCommit := model.TriggerCommit{Component: "mathwizz-web-server", SHA: "sha-web"}
		pendingCommit := model.TriggerCommit{Component: "mathwizz-history-worker", SHA: "sha-worker"}
		doneRun := pipelineRun("run-web", "mathwizz-web-server", "sha-web", "uid-web", "Unknown")
		doneTask := buildTaskRunForWorkflow("task-web", "uid-web", "True", "buildah-oci-ta")
		pendingRun := pendingPipelineRun("run-worker", "mathwizz-history-worker", "sha-worker", "uid-worker", time.Now().Add(-time.Hour), pipelineRunPendingStatus, pipelineRunPendingStatus, "")
		stage := &LiveStage{
			Request: config.Request{
				Platforms: []string{platform.LinuxAMD64},
				Timeouts:  config.Timeouts{Trigger: time.Second, Build: time.Second},
			},
			Clients: &cluster.ClientSet{Dynamic: namespaceDynamicClient(&doneRun, &doneTask, &pendingRun), Kubernetes: kubernetesfake.NewSimpleClientset()},
		}
		manifest := &model.RunManifest{
			Fixture:        model.FixtureIdentity{TenantNamespace: "mathwizz-test"},
			TriggerCommits: []model.TriggerCommit{doneCommit, pendingCommit},
		}

		err := stage.VerifyBuilds(context.Background(), manifest)

		var admission *AdmissionPendingError
		Expect(errors.As(err, &admission)).Should(BeTrue())
		Expect(manifest.BuildOutputs).Should(HaveLen(1))
		Expect(manifest.BuildOutputs[0].Component).Should(Equal("mathwizz-web-server"))
		Expect(manifest.PipelineRuns).Should(HaveLen(1))
		Expect(manifest.PipelineRuns[0].Name).Should(Equal("run-web"))
	})

	It("rejects PipelineRun overshoot for an exact trigger SHA", func() {
		commits := []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "sha-1"}}
		_, _, err := matchBuildTaskRuns([]unstructured.Unstructured{
			pipelineRun("run-1", "mathwizz-web-server", "sha-1", "uid-1", "True"),
			pipelineRun("run-2", "mathwizz-web-server", "sha-1", "uid-2", "True"),
		}, nil, commits, nil)
		Expect(err).Should(MatchError("PipelineRun overshoot for component mathwizz-web-server: 2 succeeded for commit sha-1, expected exactly 1; candidates: mathwizz-test/run-1 (sha sha-1), mathwizz-test/run-2 (sha sha-1)"))
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
		}, nil)
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

	It("should ignore an unrelated component's PaC onboarding failure while triggering", func() {
		dynamic := namespaceDynamicClient()
		dynamic.PrependReactor("list", "components", func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
				componentWithPaCStatus("mathwizz-web-server", "error"),
				componentWithPaCStatus("mathwizz-history-worker", "enabled"),
			}}, nil
		})
		provider := &recordingProvider{files: map[string]string{
			"history-worker/Dockerfile":                              "FROM base\n",
			".tekton/konflux-test-mathwizz-history-worker-push.yaml": "spec:\n  params:\n  - name: build-platforms\n    value:\n    - linux/x86_64\n",
		}}
		fixture := providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"}
		stage := &LiveStage{
			Request: config.Request{
				Provider:        config.ProviderGitHub,
				TenantNamespace: "tenant",
				ApplicationName: "app",
				SourceBranch:    "smoke-branch",
				Smoke:           true,
				ComponentNames:  []string{"mathwizz-history-worker"},
				ComponentPaths:  []string{"history-worker/Dockerfile"},
				TektonPaths:     []string{".tekton/konflux-test-mathwizz-history-worker-push.yaml"},
				Platforms:       []string{platform.LinuxAMD64},
				Timeouts:        config.Timeouts{Trigger: time.Second, Build: time.Second},
			},
			Out:      io.Discard,
			Log:      io.Discard,
			Fixture:  fixture,
			Clients:  &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetesfake.NewSimpleClientset()},
			Provider: provider,
		}
		manifest := &model.RunManifest{
			RunID:   "run-1",
			Fixture: model.FixtureIdentity{TenantNamespace: "tenant", Components: []string{"mathwizz-history-worker"}},
		}

		Expect(stage.TriggerComponents(context.Background(), manifest)).Should(Succeed())
		Expect(manifest.TriggerCommits).Should(HaveLen(1))
		Expect(manifest.TriggerCommits[0].Component).Should(Equal("mathwizz-history-worker"))
		Expect(provider.updated).Should(HaveKey("history-worker/Dockerfile"))
	})

	It("should fail triggering when the selected component's PaC onboarding failed", func() {
		dynamic := namespaceDynamicClient()
		dynamic.PrependReactor("list", "components", func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
				componentWithPaCStatus("mathwizz-history-worker", "error"),
				componentWithPaCStatus("mathwizz-web-server", "enabled"),
			}}, nil
		})
		provider := &recordingProvider{files: map[string]string{
			"history-worker/Dockerfile":                              "FROM base\n",
			".tekton/konflux-test-mathwizz-history-worker-push.yaml": "spec:\n  params:\n  - name: build-platforms\n    value:\n    - linux/x86_64\n",
		}}
		fixture := providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"}
		stage := &LiveStage{
			Request: config.Request{
				Provider:        config.ProviderGitHub,
				TenantNamespace: "tenant",
				ApplicationName: "app",
				SourceBranch:    "smoke-branch",
				Smoke:           true,
				ComponentNames:  []string{"mathwizz-history-worker"},
				ComponentPaths:  []string{"history-worker/Dockerfile"},
				TektonPaths:     []string{".tekton/konflux-test-mathwizz-history-worker-push.yaml"},
				Platforms:       []string{platform.LinuxAMD64},
				Timeouts:        config.Timeouts{Trigger: time.Second, Build: time.Second},
			},
			Out:      io.Discard,
			Log:      io.Discard,
			Fixture:  fixture,
			Clients:  &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetesfake.NewSimpleClientset()},
			Provider: provider,
		}
		manifest := &model.RunManifest{
			RunID:   "run-1",
			Fixture: model.FixtureIdentity{TenantNamespace: "tenant", Components: []string{"mathwizz-history-worker"}},
		}

		err := stage.TriggerComponents(context.Background(), manifest)

		Expect(err).Should(MatchError(ContainSubstring("wait for PaC onboarding: component mathwizz-history-worker PaC onboarding failed")))
		Expect(manifest.TriggerCommits).Should(BeEmpty())
		Expect(provider.updated).Should(BeEmpty())
	})

	Describe("LiveStage build progress", func() {
		When("PaC onboarding is pending", func() {
			It("should report the entry line and heartbeats with enabled counts and pending names", func() {
				previous := buildStatusInterval
				buildStatusInterval = 10 * time.Millisecond
				DeferCleanup(func() { buildStatusInterval = previous })
				out := &syncBuffer{}
				stage := &LiveStage{Out: out}
				names := []string{"mathwizz-web-server", "mathwizz-frontend"}
				started := time.Now()
				err := waitForPaCEnabled(context.Background(), 5*time.Second, 2*time.Millisecond, names, func(context.Context) ([]unstructured.Unstructured, error) {
					state := "pending"
					if time.Since(started) > 100*time.Millisecond {
						state = "enabled"
					}
					objects := make([]unstructured.Unstructured, 0, len(names))
					for _, name := range names {
						objects = append(objects, componentWithPaCStatus(name, state))
					}
					return objects, nil
				}, stage.notice)
				Expect(err).ShouldNot(HaveOccurred())
				text := out.String()
				Expect(text).Should(ContainSubstring("waiting for PaC onboarding of 2 components"))
				Expect(text).Should(ContainSubstring("PaC onboarding: 0/2 enabled; pending mathwizz-web-server, mathwizz-frontend"))
			})
		})

		When("PipelineRuns and Buildah TaskRuns are pending", func() {
			It("should report the wait entry, the Buildah transition, and per-component heartbeats", func() {
				previous := buildStatusInterval
				buildStatusInterval = 10 * time.Millisecond
				DeferCleanup(func() { buildStatusInterval = previous })
				out := &syncBuffer{}
				stage := &LiveStage{Out: out}
				started := time.Now()
				commit := model.TriggerCommit{Component: "mathwizz-web-server", SHA: "sha-1"}
				pipeline := pipelineRunAt("run-build", "mathwizz-web-server", "sha-1", "uid-build", "Unknown", started.Add(-2*time.Minute))
				succeededPipeline := pipelineRunAt("run-build", "mathwizz-web-server", "sha-1", "uid-build", "True", started.Add(-2*time.Minute))
				pendingTask := buildTaskRunForWorkflow("build-task", "uid-build", "Unknown", "buildah-oci-ta")
				succeededTask := buildTaskRunForWorkflow("build-task", "uid-build", "True", "buildah-oci-ta")
				_, err := waitForBuildTaskRuns(context.Background(), 5*time.Second, 5*time.Second, 2*time.Millisecond, "mathwizz-test", []model.TriggerCommit{commit}, func(context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, error) {
					switch elapsed := time.Since(started); {
					case elapsed < 100*time.Millisecond:
						return nil, nil, nil
					case elapsed < 250*time.Millisecond:
						return []unstructured.Unstructured{pipeline}, []unstructured.Unstructured{pendingTask}, nil
					default:
						return []unstructured.Unstructured{succeededPipeline}, []unstructured.Unstructured{succeededTask}, nil
					}
				}, stage.notice, nil, buildEvidenceCache{})
				Expect(err).ShouldNot(HaveOccurred())
				text := out.String()
				Expect(text).Should(ContainSubstring("waiting for PipelineRuns for 1 triggered components"))
				Expect(text).Should(ContainSubstring("mathwizz-web-server: no PipelineRun"))
				Expect(text).Should(ContainSubstring("PipelineRuns appeared for all 1 components; waiting for Buildah TaskRuns"))
				Expect(text).Should(ContainSubstring("mathwizz-web-server: running for"))
				Expect(text).Should(ContainSubstring("buildah 0/1 succeeded"))
			})
		})

		When("a PipelineRun failed before creating Buildah TaskRuns", func() {
			It("should fail immediately instead of waiting", func() {
				calls := 0
				_, err := waitForBuildTaskRuns(context.Background(), 5*time.Second, 5*time.Second, time.Millisecond, "mathwizz-test", []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "sha-1"}}, func(context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, error) {
					calls++
					return []unstructured.Unstructured{pipelineRun("run-build", "mathwizz-web-server", "sha-1", "uid-build", "False")}, nil, nil
				}, nil, nil, buildEvidenceCache{})
				var failure *PipelineRunFailedError
				Expect(errors.As(err, &failure)).Should(BeTrue())
				Expect(failure.Component).Should(Equal("mathwizz-web-server"))
				Expect(failure.Name).Should(Equal("run-build"))
				Expect(calls).Should(Equal(1))
			})
		})

		When("a stale owned tenant namespace is deleted", func() {
			It("should report the deletion to the console", func() {
				dynamic := namespaceDynamicClient(namespaceObject("tenant", "run-1"))
				out := &bytes.Buffer{}
				stage := &LiveStage{
					Request:   config.Request{TenantNamespace: "tenant"},
					Out:       out,
					Log:       io.Discard,
					Namespace: cleanup.NamespaceService{Dynamic: dynamic, PollInterval: time.Millisecond, NameLister: testNameLister{names: []string{"tenant"}}},
				}
				Expect(stage.ensureTenantNamespaceAvailable(context.Background(), &model.RunManifest{RunID: "run-2"})).Should(Succeed())
				Expect(out.String()).Should(ContainSubstring("deleting stale owned tenant namespace tenant from run run-1"))
			})
		})

		Describe("VerifyBuilds PaC Secret inspection", func() {
			It("should fail with a typed error when a pending build's annotated Secret is missing", func() {
				pipeline := annotatedPipelineRun("run-build", "mathwizz-web-server", "sha-1", "uid-build", "Unknown", "git-auth-secret", time.Now().Add(-5*time.Minute))
				stage := &LiveStage{
					Request: config.Request{Timeouts: config.Timeouts{Trigger: time.Second, Build: time.Second}},
					Clients: &cluster.ClientSet{Dynamic: namespaceDynamicClient(&pipeline), Kubernetes: kubernetesfake.NewSimpleClientset()},
					Out:     &bytes.Buffer{},
				}
				manifest := &model.RunManifest{Fixture: model.FixtureIdentity{TenantNamespace: "mathwizz-test"}, TriggerCommits: []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "sha-1"}}}

				err := stage.VerifyBuilds(context.Background(), manifest)
				var missing *cluster.MissingPacAuthSecretError
				Expect(errors.As(err, &missing)).Should(BeTrue())
				Expect(missing.PipelineRun).Should(Equal("run-build"))
				Expect(missing.Component).Should(Equal("mathwizz-web-server"))
				Expect(missing.Secret).Should(Equal("git-auth-secret"))
				Expect(missing.Pending).Should(BeNumerically(">", cluster.PacAuthSecretGracePeriod))
			})

			It("should defer an admission-pending build before failing its missing Secret", func() {
				previousThreshold := admissionPendingThreshold
				admissionPendingThreshold = time.Millisecond
				DeferCleanup(func() { admissionPendingThreshold = previousThreshold })
				previousPoll := buildPollInterval
				buildPollInterval = time.Millisecond
				DeferCleanup(func() { buildPollInterval = previousPoll })

				pipeline := pendingPipelineRun("run-pending", "mathwizz-web-server", "sha-1", "uid-pending", time.Now().Add(-time.Hour), pipelineRunPendingStatus, pipelineRunPendingStatus, "")
				pipeline.SetAnnotations(map[string]string{cluster.PacGitAuthSecretAnnotation: "git-auth-secret"})
				stage := &LiveStage{
					Request: config.Request{Platforms: []string{platform.LinuxAMD64}, Timeouts: config.Timeouts{Trigger: time.Second, Build: time.Second}},
					Clients: &cluster.ClientSet{Dynamic: namespaceDynamicClient(&pipeline), Kubernetes: kubernetesfake.NewSimpleClientset()},
					Out:     &bytes.Buffer{},
				}
				manifest := &model.RunManifest{Fixture: model.FixtureIdentity{TenantNamespace: "mathwizz-test"}, TriggerCommits: []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "sha-1"}}}

				err := stage.VerifyBuilds(context.Background(), manifest)

				var admission *AdmissionPendingError
				Expect(errors.As(err, &admission)).Should(BeTrue())
				var missing *cluster.MissingPacAuthSecretError
				Expect(errors.As(err, &missing)).Should(BeFalse())
			})

			It("should report a matched failed PipelineRun before a missing Secret", func() {
				failed := pipelineRun("run-failed", "mathwizz-web-server", "sha-1", "uid-failed", "False")
				pending := annotatedPipelineRun("run-pending", "mathwizz-history-worker", "sha-2", "uid-pending", "Unknown", "git-auth-secret", time.Now().Add(-5*time.Minute))
				stage := &LiveStage{
					Request: config.Request{Timeouts: config.Timeouts{Trigger: time.Second, Build: time.Second}},
					Clients: &cluster.ClientSet{Dynamic: namespaceDynamicClient(&failed, &pending), Kubernetes: kubernetesfake.NewSimpleClientset()},
					Out:     &bytes.Buffer{},
				}
				manifest := &model.RunManifest{Fixture: model.FixtureIdentity{TenantNamespace: "mathwizz-test"}, TriggerCommits: []model.TriggerCommit{
					{Component: "mathwizz-web-server", SHA: "sha-1"},
					{Component: "mathwizz-history-worker", SHA: "sha-2"},
				}}

				err := stage.VerifyBuilds(context.Background(), manifest)

				var failure *PipelineRunFailedError
				Expect(errors.As(err, &failure)).Should(BeTrue())
				Expect(failure.Name).Should(Equal("run-failed"))
				var missing *cluster.MissingPacAuthSecretError
				Expect(errors.As(err, &missing)).Should(BeFalse())
			})

			It("should succeed when the annotated Secret exists", func() {
				pipeline := annotatedPipelineRun("run-build", "mathwizz-web-server", "sha-1", "uid-build", "True", "git-auth-secret", time.Now().Add(-5*time.Minute))
				build := buildTaskRunForWorkflow("build-task", "uid-build", "True", "buildah-oci-ta")
				stage := &LiveStage{
					Request: config.Request{Platforms: []string{platform.LinuxAMD64}, Timeouts: config.Timeouts{Trigger: time.Second, Build: time.Second}},
					Clients: &cluster.ClientSet{
						Dynamic:    namespaceDynamicClient(&pipeline, &build),
						Kubernetes: kubernetesfake.NewSimpleClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "git-auth-secret", Namespace: "mathwizz-test"}}),
					},
					Out: &bytes.Buffer{},
				}
				manifest := &model.RunManifest{Fixture: model.FixtureIdentity{TenantNamespace: "mathwizz-test"}, TriggerCommits: []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "sha-1"}}}

				Expect(stage.VerifyBuilds(context.Background(), manifest)).Should(Succeed())
				Expect(manifest.PipelineRuns).Should(HaveLen(1))
				Expect(manifest.PipelineRuns[0].Name).Should(Equal("run-build"))
			})

			It("should warn once and keep waiting when the Secret read is forbidden", func() {
				pipeline := annotatedPipelineRun("run-build", "mathwizz-web-server", "sha-1", "uid-build", "Unknown", "git-auth-secret", time.Now().Add(-5*time.Minute))
				dynamic := namespaceDynamicClient(&pipeline)
				var taskRunLists, pipelineRunLists atomic.Int32
				dynamic.PrependReactor("list", "pipelineruns", func(kubetesting.Action) (bool, runtime.Object, error) {
					current := pipeline.DeepCopy()
					if pipelineRunLists.Add(1) > 1 {
						current.Object["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Succeeded", "status": "True"}}
					}
					return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*current}}, nil
				})
				dynamic.PrependReactor("list", "taskruns", func(kubetesting.Action) (bool, runtime.Object, error) {
					if taskRunLists.Add(1) == 1 {
						return true, &unstructured.UnstructuredList{}, nil
					}
					build := buildTaskRunForWorkflow("build-task", "uid-build", "True", "buildah-oci-ta")
					return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{build}}, nil
				})
				kube := kubernetesfake.NewSimpleClientset()
				kube.PrependReactor("get", "secrets", func(kubetesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "git-auth-secret", errors.New("forbidden"))
				})
				out := &bytes.Buffer{}
				stage := &LiveStage{
					Request: config.Request{Platforms: []string{platform.LinuxAMD64}, Timeouts: config.Timeouts{Trigger: time.Second, Build: time.Second}},
					Clients: &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kube},
					Out:     out,
				}
				manifest := &model.RunManifest{Fixture: model.FixtureIdentity{TenantNamespace: "mathwizz-test"}, TriggerCommits: []model.TriggerCommit{{Component: "mathwizz-web-server", SHA: "sha-1"}}}

				Expect(stage.VerifyBuilds(context.Background(), manifest)).Should(Succeed())
				Expect(manifest.PipelineRuns).Should(HaveLen(1))
				Expect(strings.Count(out.String(), "warning: cannot inspect PaC git authentication Secrets")).Should(Equal(1))
			})
		})
	})
})

var _ = Describe("build evidence capture", func() {
	It("captures each component's evidence before PaC prunes it", func() {
		previous := buildPollInterval
		buildPollInterval = time.Millisecond
		DeferCleanup(func() { buildPollInterval = previous })

		commitA := model.TriggerCommit{Component: "component-a", SHA: "sha-a"}
		commitB := model.TriggerCommit{Component: "component-b", SHA: "sha-b"}
		runA := pipelineRun("run-a", "component-a", "sha-a", "uid-a", "True")
		pendingB := pipelineRun("run-b", "component-b", "sha-b", "uid-b", "Unknown")
		doneB := pipelineRun("run-b", "component-b", "sha-b", "uid-b", "True")
		taskA := buildTaskRunForWorkflow("task-a", "uid-a", "True", "buildah-oci-ta")
		taskB := buildTaskRunForWorkflow("task-b", "uid-b", "True", "buildah-oci-ta")

		var polls atomic.Int32
		dynamic := namespaceDynamicClient()
		dynamic.PrependReactor("list", "pipelineruns", func(kubetesting.Action) (bool, runtime.Object, error) {
			poll := polls.Add(1)
			if poll < 3 {
				return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{runA, pendingB}}, nil
			}
			return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{doneB}}, nil
		})
		dynamic.PrependReactor("list", "taskruns", func(kubetesting.Action) (bool, runtime.Object, error) {
			if polls.Load() < 3 {
				return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{taskA}}, nil
			}
			return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{taskB}}, nil
		})

		stage := &LiveStage{
			Request: config.Request{
				Platforms: []string{platform.LinuxAMD64},
				Timeouts:  config.Timeouts{Trigger: time.Second, Build: 5 * time.Second},
			},
			Clients: &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetesfake.NewSimpleClientset()},
		}
		manifest := &model.RunManifest{
			Fixture:        model.FixtureIdentity{TenantNamespace: "mathwizz-test", Components: []string{"component-a", "component-b"}},
			TriggerCommits: []model.TriggerCommit{commitA, commitB},
		}

		Expect(stage.VerifyBuilds(context.Background(), manifest)).Should(Succeed())
		Expect(manifest.BuildOutputs).Should(HaveLen(2))
		Expect(manifest.BuildOutputs[0].Component).Should(Equal("component-a"))
		Expect(manifest.BuildOutputs[0].CreatedOutputs).Should(HaveKeyWithValue(platform.LinuxAMD64, "quay.io/example/image"))
		Expect(manifest.BuildOutputs[1].Component).Should(Equal("component-b"))
		Expect(manifest.PipelineRuns).Should(HaveLen(2))
		Expect(manifest.PipelineRuns[0].Name).Should(Equal("run-a"))
		Expect(manifest.PipelineRuns[1].Name).Should(Equal("run-b"))

		var gets atomic.Int32
		dynamic.PrependReactor("get", "pipelineruns", func(kubetesting.Action) (bool, runtime.Object, error) {
			gets.Add(1)
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "tekton.dev", Resource: "pipelineruns"}, "run-a")
		})
		Expect(stage.VerifyBuildOutputs(context.Background(), manifest)).Should(Succeed())
		Expect(gets.Load()).Should(BeZero())
		Expect(manifest.BuildOutputs).Should(HaveLen(2))
	})

	It("fails fast when an observed PipelineRun disappears", func() {
		commit := model.TriggerCommit{Component: "component-a", SHA: "sha-a"}
		calls := 0
		_, err := waitForBuildTaskRuns(context.Background(), time.Second, time.Second, time.Millisecond, "mathwizz-test", []model.TriggerCommit{commit}, func(context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, error) {
			calls++
			if calls == 1 {
				pending := pipelineRun("run-a", "component-a", "sha-a", "uid-a", "Unknown")
				return []unstructured.Unstructured{pending}, nil, nil
			}
			return nil, nil, nil
		}, nil, nil, buildEvidenceCache{})

		var disappeared *PipelineRunDisappearedError
		Expect(errors.As(err, &disappeared)).Should(BeTrue())
		Expect(disappeared.Component).Should(Equal("component-a"))
		Expect(disappeared.Name).Should(Equal("run-a"))
		Expect(err.Error()).Should(ContainSubstring("check PaC max-keep-runs or other pruning"))
		Expect(calls).Should(Equal(2))
		Expect(errors.Is(err, context.DeadlineExceeded)).Should(BeFalse())
	})

	It("does not fail when a captured component's PipelineRun disappears", func() {
		commitA := model.TriggerCommit{Component: "component-a", SHA: "sha-a"}
		commitB := model.TriggerCommit{Component: "component-b", SHA: "sha-b"}
		runA := pipelineRun("run-a", "component-a", "sha-a", "uid-a", "True")
		pendingB := pipelineRun("run-b", "component-b", "sha-b", "uid-b", "Unknown")
		doneB := pipelineRun("run-b", "component-b", "sha-b", "uid-b", "True")
		taskA := buildTaskRunForWorkflow("task-a", "uid-a", "True", "buildah-oci-ta")
		taskB := buildTaskRunForWorkflow("task-b", "uid-b", "True", "buildah-oci-ta")

		captured := make(map[string]model.PipelineRunIdentity)
		evidence := buildEvidenceCache{
			Captured: func(component string) (model.PipelineRunIdentity, bool) {
				identity, ok := captured[component]
				return identity, ok
			},
			Capture: func(identity model.PipelineRunIdentity, _ *unstructured.Unstructured, _ []unstructured.Unstructured) error {
				captured[identity.Component] = identity
				return nil
			},
		}
		calls := 0
		identities, err := waitForBuildTaskRuns(context.Background(), time.Second, time.Second, time.Millisecond, "mathwizz-test", []model.TriggerCommit{commitA, commitB}, func(context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, error) {
			calls++
			if calls == 1 {
				return []unstructured.Unstructured{runA, pendingB}, []unstructured.Unstructured{taskA}, nil
			}
			return []unstructured.Unstructured{doneB}, []unstructured.Unstructured{taskB}, nil
		}, nil, nil, evidence)

		Expect(err).ShouldNot(HaveOccurred())
		Expect(identities).Should(HaveLen(2))
		Expect(identities[0].Name).Should(Equal("run-a"))
		Expect(identities[1].Name).Should(Equal("run-b"))
	})

	It("captures evidence on the first poll that observes Buildah success", func() {
		commit := model.TriggerCommit{Component: "component-a", SHA: "sha-a"}
		running := pipelineRun("run-a", "component-a", "sha-a", "uid-a", "Unknown")
		build := buildTaskRunForWorkflow("task-a", "uid-a", "True", "buildah-oci-ta")

		var polls, captureCalls, capturedAt atomic.Int32
		evidence := buildEvidenceCache{
			Capture: func(model.PipelineRunIdentity, *unstructured.Unstructured, []unstructured.Unstructured) error {
				captureCalls.Add(1)
				capturedAt.Store(polls.Load())
				return nil
			},
		}
		_, err := waitForBuildTaskRuns(context.Background(), time.Second, time.Second, time.Millisecond, "mathwizz-test", []model.TriggerCommit{commit}, func(context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, error) {
			polls.Add(1)
			return []unstructured.Unstructured{running}, []unstructured.Unstructured{build}, nil
		}, nil, nil, evidence)

		Expect(err).ShouldNot(HaveOccurred())
		Expect(captureCalls.Load()).Should(Equal(int32(1)))
		Expect(capturedAt.Load()).Should(Equal(int32(1)))
	})

	It("replaces stale evidence and drops duplicates", func() {
		current := model.PipelineRunIdentity{Namespace: "mathwizz-test", Name: "run-current", UID: "uid-new", Component: "component-a"}
		stale := model.BuildOutputEvidence{
			Component:      "component-a",
			PipelineRun:    model.PipelineRunIdentity{Namespace: "mathwizz-test", Name: "run-old", UID: "uid-old", Component: "component-a"},
			Platforms:      []string{platform.LinuxAMD64},
			CreatedOutputs: map[string]string{platform.LinuxAMD64: "quay.io/example/old"},
			OutputDigests:  map[string]string{platform.LinuxAMD64: "sha256:old"},
		}
		pipeline := pipelineRun("run-current", "component-a", "sha-a", "uid-new", "True")
		build := buildTaskRunForWorkflow("task-current", "uid-new", "True", "buildah-oci-ta")
		stage := &LiveStage{
			Request: config.Request{Platforms: []string{platform.LinuxAMD64}},
			Clients: &cluster.ClientSet{Dynamic: namespaceDynamicClient(&pipeline, &build), Kubernetes: kubernetesfake.NewSimpleClientset()},
		}
		manifest := &model.RunManifest{
			PipelineRuns: []model.PipelineRunIdentity{current, current},
			BuildOutputs: []model.BuildOutputEvidence{stale, stale},
		}

		Expect(stage.VerifyBuildOutputs(context.Background(), manifest)).Should(Succeed())
		Expect(manifest.PipelineRuns).Should(HaveLen(1))
		Expect(manifest.BuildOutputs).Should(HaveLen(1))
		Expect(string(manifest.BuildOutputs[0].PipelineRun.UID)).Should(Equal("uid-new"))
		Expect(manifest.BuildOutputs[0].CreatedOutputs).Should(HaveKeyWithValue(platform.LinuxAMD64, "quay.io/example/image"))
	})

	It("captures completed evidence before a component PaC onboarding error", func() {
		commit := model.TriggerCommit{Component: "component-a", SHA: "sha-a"}
		runA := pipelineRun("run-a", "component-a", "sha-a", "uid-a", "True")
		taskA := buildTaskRunForWorkflow("task-a", "uid-a", "True", "buildah-oci-ta")
		dynamic := namespaceDynamicClient()
		dynamic.PrependReactor("list", "components", func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{componentWithPaCStatus("component-b", "error")}}, nil
		})
		dynamic.PrependReactor("list", "pipelineruns", func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{runA}}, nil
		})
		dynamic.PrependReactor("list", "taskruns", func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{taskA}}, nil
		})
		stage := &LiveStage{
			Request: config.Request{
				Platforms: []string{platform.LinuxAMD64},
				Timeouts:  config.Timeouts{Trigger: time.Second, Build: time.Second},
			},
			Clients: &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetesfake.NewSimpleClientset()},
		}
		manifest := &model.RunManifest{
			Fixture:        model.FixtureIdentity{TenantNamespace: "mathwizz-test", Components: []string{"component-b"}},
			TriggerCommits: []model.TriggerCommit{commit},
		}

		err := stage.VerifyBuilds(context.Background(), manifest)

		Expect(err).Should(MatchError(ContainSubstring("component component-b PaC onboarding failed")))
		Expect(manifest.BuildOutputs).Should(HaveLen(1))
		Expect(manifest.BuildOutputs[0].Component).Should(Equal("component-a"))
	})

	It("should ignore an unrelated component's PaC onboarding failure while verifying builds", func() {
		runA := pipelineRun("run-a", "component-a", "sha-a", "uid-a", "True")
		taskA := buildTaskRunForWorkflow("task-a", "uid-a", "True", "buildah-oci-ta")
		dynamic := namespaceDynamicClient()
		dynamic.PrependReactor("list", "components", func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{componentWithPaCStatus("component-b", "error")}}, nil
		})
		dynamic.PrependReactor("list", "pipelineruns", func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{runA}}, nil
		})
		dynamic.PrependReactor("list", "taskruns", func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{taskA}}, nil
		})
		stage := &LiveStage{
			Request: config.Request{Platforms: []string{platform.LinuxAMD64}, Timeouts: config.Timeouts{Trigger: time.Second, Build: time.Second}},
			Clients: &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetesfake.NewSimpleClientset()},
		}
		manifest := &model.RunManifest{
			Fixture:        model.FixtureIdentity{TenantNamespace: "mathwizz-test", Components: []string{"component-a"}},
			TriggerCommits: []model.TriggerCommit{{Component: "component-a", SHA: "sha-a"}},
		}

		Expect(stage.VerifyBuilds(context.Background(), manifest)).Should(Succeed())
		Expect(manifest.BuildOutputs).Should(HaveLen(1))
		Expect(manifest.BuildOutputs[0].Component).Should(Equal("component-a"))
		Expect(manifest.PipelineRuns).Should(HaveLen(1))
	})

	It("keeps captured evidence across a PaC retrigger", func() {
		const (
			componentA = "component-a"
			componentB = "component-b"
		)
		runA := pipelineRun("run-a", componentA, "sha-a", "uid-a", "True")
		pendingB := annotatedPipelineRun("run-b", componentB, "sha-b", "uid-b", "Unknown", "git-auth-secret", time.Now().Add(-5*time.Minute))
		newB := pipelineRun("run-b-new", componentB, "setup-1", "uid-b-new", "True")
		taskA := buildTaskRunForWorkflow("task-a", "uid-a", "True", "buildah-oci-ta")
		taskB := buildTaskRunForWorkflow("task-b", "uid-b-new", "True", "buildah-oci-ta")

		var retriggered atomic.Bool
		var polls atomic.Int32
		dynamic := namespaceDynamicClient()
		dynamic.PrependReactor("list", "pipelineruns", func(kubetesting.Action) (bool, runtime.Object, error) {
			if polls.Add(1) < 2 {
				return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{runA, pendingB}}, nil
			}
			return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{newB}}, nil
		})
		dynamic.PrependReactor("list", "taskruns", func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{taskA, taskB}}, nil
		})
		provider := &retriggeringProvider{
			recordingProvider: &recordingProvider{files: map[string]string{"a/Dockerfile": "FROM base\n", "b/Dockerfile": "FROM base\n"}},
			retriggered:       &retriggered,
		}
		stage := &LiveStage{
			Request: config.Request{
				ComponentPaths: []string{"a/Dockerfile", "b/Dockerfile"},
				SourceBranch:   "main",
				Platforms:      []string{platform.LinuxAMD64},
				Timeouts:       config.Timeouts{Trigger: time.Second, Build: time.Second},
			},
			Fixture:  providers.FixtureRepository{Owner: "owner", Name: "repo", URL: "https://example.com/owner/repo"},
			Clients:  &cluster.ClientSet{Dynamic: dynamic, Kubernetes: kubernetesfake.NewSimpleClientset()},
			Provider: provider,
		}
		manifest := &model.RunManifest{
			RunID: "run-1",
			Fixture: model.FixtureIdentity{
				TenantNamespace: "mathwizz-test",
				Components:      []string{componentA, componentB},
			},
			TriggerCommits: []model.TriggerCommit{
				{Component: componentA, SHA: "sha-a"},
				{Component: componentB, SHA: "sha-b"},
			},
		}

		Expect(stage.VerifyBuilds(context.Background(), manifest)).Should(Succeed())
		Expect(retriggered.Load()).Should(BeTrue())
		Expect(manifest.TriggerCommits[1].SHA).Should(Equal("setup-1"))
		Expect(manifest.BuildOutputs).Should(HaveLen(2))
		Expect(manifest.BuildOutputs[0].PipelineRun.Name).Should(Equal("run-a"))
		Expect(manifest.BuildOutputs[1].PipelineRun.Name).Should(Equal("run-b-new"))
		Expect(manifest.PipelineRuns).Should(HaveLen(2))
	})

	It("still rejects duplicate PipelineRuns after evidence is captured", func() {
		commit := model.TriggerCommit{Component: "component-a", SHA: "sha-a"}
		pipelines := []unstructured.Unstructured{
			pipelineRun("run-a", "component-a", "sha-a", "uid-a", "True"),
			pipelineRun("run-a-duplicate", "component-a", "sha-a", "uid-a-duplicate", "True"),
		}
		captured := buildEvidenceCache{Captured: func(string) (model.PipelineRunIdentity, bool) {
			return model.PipelineRunIdentity{Component: "component-a", UID: "uid-a"}, true
		}}

		_, _, err := matchBuildTaskRuns(pipelines, nil, []model.TriggerCommit{commit}, captured.isCaptured)

		Expect(err).Should(MatchError(ContainSubstring("PipelineRun overshoot")))
	})

	It("rejects output verification without PipelineRuns", func() {
		stage := &LiveStage{Clients: &cluster.ClientSet{Dynamic: namespaceDynamicClient()}}

		err := stage.VerifyBuildOutputs(context.Background(), &model.RunManifest{})

		Expect(err).Should(MatchError(ContainSubstring("no PipelineRuns to verify build outputs")))
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

// pipelineRunAt creates a PipelineRun fixture with component, SHA, identity, status, and creation time.
func pipelineRunAt(name, component, sha, uid, status string, created time.Time) unstructured.Unstructured {
	object := pipelineRun(name, component, sha, uid, status)
	object.SetCreationTimestamp(metav1.NewTime(created))
	return object
}

// annotatedPipelineRun creates a PaC-annotated PipelineRun fixture with a creation time.
func annotatedPipelineRun(name, component, sha, uid, status, secret string, created time.Time) unstructured.Unstructured {
	object := pipelineRunAt(name, component, sha, uid, status, created)
	object.SetAnnotations(map[string]string{cluster.PacGitAuthSecretAnnotation: secret})
	return object
}

// buildTaskRunForWorkflow creates a PipelineRun-owned TaskRun fixture for a build or validation task.
func buildTaskRunForWorkflow(name, ownerUID, status, task string) unstructured.Unstructured {
	pipelineTask := "build-container"
	if task == "sast-shell-check" {
		pipelineTask = task
	}
	taskStatus := map[string]any{"conditions": []any{map[string]any{"type": "Succeeded", "status": status}}}
	if strings.Contains(strings.ToLower(task), "buildah") {
		taskStatus["results"] = []any{
			map[string]any{"name": "PLATFORM", "value": platform.LinuxAMD64},
			map[string]any{"name": "IMAGE_URL", "value": "quay.io/example/image"},
			map[string]any{"name": "IMAGE_DIGEST", "value": "sha256:image"},
		}
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
		"status": taskStatus,
	}}
}

// buildTaskRunForWorkflowOnPlatform creates a Buildah TaskRun whose PLATFORM
// result names the requested platform.
func buildTaskRunForWorkflowOnPlatform(name, ownerUID, status, task, taskPlatform string) unstructured.Unstructured {
	taskRun := buildTaskRunForWorkflow(name, ownerUID, status, task)
	results, found, err := unstructured.NestedSlice(taskRun.Object, "status", "results")
	if err != nil || !found {
		return taskRun
	}
	for index := range results {
		result, ok := results[index].(map[string]any)
		if !ok || result["name"] != "PLATFORM" {
			continue
		}
		result["value"] = taskPlatform
	}
	_ = unstructured.SetNestedSlice(taskRun.Object, results, "status", "results")
	return taskRun
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
		{Group: "tekton.dev", Version: "v1", Resource: "taskruns"}:                          "TaskRunList",
		cluster.KueueWorkloadGVR: "WorkloadList",
	}, objects...)
}

var _ = Describe("LiveStage preflight access checks", func() {
	It("checks the four pipeline resources and no GitLab Secret", func() {
		stage := &LiveStage{Request: config.Request{
			Provider:        config.ProviderGitLab,
			TenantNamespace: "mathwizz-test-gitlab",
			Credentials:     config.Credentials{GitLabTokenEphemeral: true},
		}}

		checks := stage.accessChecks()
		Expect(checks).Should(HaveLen(4))
		resources := make([]schema.GroupVersionResource, 0, len(checks))
		for _, check := range checks {
			Expect(check.Namespace).Should(Equal("mathwizz-test-gitlab"))
			Expect(check.Verbs).Should(BeEmpty())
			resources = append(resources, check.Resource)
		}
		Expect(resources).Should(ConsistOf(PipelineRunGVR, cluster.TaskRunGVR, cluster.ApplicationGVR, cluster.ComponentGVR))
		Expect(resources).ShouldNot(ContainElement(cluster.SecretGVR))
	})

	It("sends only the four API checks to preflight", func() {
		var checks []cluster.AccessCheck
		stage := &LiveStage{
			Request:          config.Request{Provider: config.ProviderGitLab, ClusterServer: "https://api.example", TenantNamespace: "tenant", Credentials: config.Credentials{GitLabTokenEphemeral: true}},
			PreflightCluster: fakePreflightCluster{server: "https://api.example", accessChecks: &checks},
			Namespace:        cleanup.NamespaceService{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), NameLister: testNameLister{}},
		}

		Expect(stage.Preflight(context.Background(), &model.RunManifest{})).Should(Succeed())
		Expect(checks).Should(HaveLen(4))
		for _, check := range checks {
			Expect(check.Resource).ShouldNot(Equal(cluster.SecretGVR))
		}
	})
})
