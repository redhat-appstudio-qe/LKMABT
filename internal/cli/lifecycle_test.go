package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/cleanup"
	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	"github.com/redhat-appstudio/lkmabt/internal/collector"
	"github.com/redhat-appstudio/lkmabt/internal/evidence"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

type fakePrompt struct {
	approved bool
	calls    int
}

// Confirm counts prompt calls and returns the configured approval.
func (p *fakePrompt) Confirm(context.Context, string) (bool, error) {
	p.calls++
	return p.approved, nil
}

var _ = Describe("Lifecycle", func() {
	It("collects failure artifacts before prompt", func() {
		stateDir := GinkgoT().TempDir()
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(), namespaceObject("tenant", "run-1"))
		prompt := &fakePrompt{approved: false}
		manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseFailed, Fixture: model.FixtureIdentity{TenantNamespace: "tenant"}}
		sources := lifecycleTestSources()
		lifecycle := Lifecycle{Store: evidence.NewManifestStore(stateDir), Namespace: cleanup.NamespaceService{Dynamic: client}, Collector: collector.Collector{StateDir: stateDir}, Prompt: prompt, Sources: sources}
		if err := lifecycle.Finalize(context.Background(), manifest, errors.New("build failed")); err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(prompt.calls).To(Equal(1))
		loaded, err := lifecycle.Store.Load("run-1")
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(loaded.FailureArtifacts).NotTo(BeNil())
		Expect(loaded.FailureArtifacts.VerifiedAt).NotTo(BeZero())
		if _, err := lifecycle.Namespace.Inspect(context.Background(), "tenant"); err != nil {
			Fail("declined cleanup deleted namespace")
		}
	})

	It("suppresses prompt when artifact collection fails", func() {
		stateDir := GinkgoT().TempDir()
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(), namespaceObject("tenant", "run-1"))
		prompt := &fakePrompt{approved: true}
		manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseFailed, Fixture: model.FixtureIdentity{TenantNamespace: "tenant"}}
		lifecycle := Lifecycle{Store: evidence.NewManifestStore(stateDir), Namespace: cleanup.NamespaceService{Dynamic: client}, Collector: collector.Collector{StateDir: stateDir}, Prompt: prompt, Sources: []collector.Source{{Name: "applications", Path: "workload/applications.json", Collect: func(context.Context, string) error { return errors.New("api unavailable") }}}}
		err := lifecycle.Finalize(context.Background(), manifest, errors.New("build failed"))
		Expect(err).To(HaveOccurred())
		Expect(prompt.calls).To(Equal(0))
		if _, err := lifecycle.Namespace.Inspect(context.Background(), "tenant"); err != nil {
			Fail("collection failure deleted namespace")
		}
	})

	It("preserves result and namespace after declined successful cleanup", func() {
		stateDir := GinkgoT().TempDir()
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(), namespaceObject("tenant", "run-1"))
		prompt := &fakePrompt{approved: false}
		manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseCompleted, Fixture: model.FixtureIdentity{TenantNamespace: "tenant"}}
		store := evidence.NewManifestStore(stateDir)
		Expect(store.Create(&manifest)).Should(Succeed())
		lifecycle := Lifecycle{Store: store, Namespace: cleanup.NamespaceService{Dynamic: client}, Collector: collector.Collector{StateDir: stateDir}, Prompt: prompt, Sources: lifecycleTestSources()}
		if err := lifecycle.Finalize(context.Background(), manifest, nil); err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(prompt.calls).To(Equal(1))
		report, err := collector.ReadReport(filepath.Join(store.RunDirFor(manifest), "collection-report.json"))
		Expect(err).ShouldNot(HaveOccurred())
		Expect(report.RunID).To(Equal("run-1"))
		if _, err := lifecycle.Namespace.Inspect(context.Background(), "tenant"); err != nil {
			Fail("declined cleanup deleted namespace")
		}
	})

	It("suppresses successful cleanup when required artifact collection is incomplete", func() {
		stateDir := GinkgoT().TempDir()
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(), namespaceObject("tenant", "run-1"))
		prompt := &fakePrompt{approved: false}
		manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseCompleted, Fixture: model.FixtureIdentity{TenantNamespace: "tenant"}}
		store := evidence.NewManifestStore(stateDir)
		Expect(store.Create(&manifest)).Should(Succeed())
		sources := lifecycleTestSources()
		sources[0].Collect = func(context.Context, string) error { return errors.New("api unavailable") }
		lifecycle := Lifecycle{Store: store, Namespace: cleanup.NamespaceService{Dynamic: client}, Collector: collector.Collector{StateDir: stateDir}, Prompt: prompt, Sources: sources}

		err := lifecycle.Finalize(context.Background(), manifest, nil)
		Expect(err).Should(MatchError(ContainSubstring("workload/applications.json")))
		Expect(prompt.calls).To(Equal(0))
		if _, err := lifecycle.Namespace.Inspect(context.Background(), "tenant"); err != nil {
			Fail("incomplete successful-run artifacts deleted namespace")
		}
	})

	It("deletes the ephemeral GitLab SCM Secret before the namespace prompt", func() {
		stateDir := GinkgoT().TempDir()
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(), namespaceObject("tenant", "run-1"), gitLabSecretObject("tenant", "run-1"))
		prompt := &fakePrompt{approved: false}
		manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseCompleted, Fixture: model.FixtureIdentity{TenantNamespace: "tenant", EphemeralGitLabAuth: true}}
		store := evidence.NewManifestStore(stateDir)
		Expect(store.Create(&manifest)).Should(Succeed())
		lifecycle := Lifecycle{Store: store, Namespace: cleanup.NamespaceService{Dynamic: client}, Collector: collector.Collector{StateDir: stateDir}, Prompt: prompt, Sources: lifecycleTestSources()}

		Expect(lifecycle.Finalize(context.Background(), manifest, nil)).Should(Succeed())
		_, err := client.Resource(cluster.SecretGVR).Namespace("tenant").Get(context.Background(), cluster.GitLabCredentialSecretName, metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).Should(BeTrue())
		Expect(prompt.calls).Should(Equal(1))
	})

	It("deletes the ephemeral GitLab SCM Secret when the run context is canceled", func() {
		stateDir := GinkgoT().TempDir()
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(), namespaceObject("tenant", "run-1"), gitLabSecretObject("tenant", "run-1"))
		prompt := &fakePrompt{approved: false}
		manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseFailed, Fixture: model.FixtureIdentity{TenantNamespace: "tenant", EphemeralGitLabAuth: true}}
		store := evidence.NewManifestStore(stateDir)
		Expect(store.Create(&manifest)).Should(Succeed())
		lifecycle := Lifecycle{Store: store, Namespace: cleanup.NamespaceService{Dynamic: client}, Collector: collector.Collector{StateDir: stateDir}, Prompt: prompt, Sources: lifecycleTestSources()}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_ = lifecycle.Finalize(ctx, manifest, errors.New("interrupted"))
		_, err := client.Resource(cluster.SecretGVR).Namespace("tenant").Get(context.Background(), cluster.GitLabCredentialSecretName, metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).Should(BeTrue())
	})

	It("keeps failure evidence when the ephemeral GitLab SCM Secret cannot be deleted", func() {
		stateDir := GinkgoT().TempDir()
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(), namespaceObject("tenant", "run-1"), gitLabSecretObject("tenant", "run-1"))
		client.PrependReactor("delete", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("delete denied")
		})
		prompt := &fakePrompt{approved: false}
		manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseFailed, Fixture: model.FixtureIdentity{TenantNamespace: "tenant", EphemeralGitLabAuth: true}}
		store := evidence.NewManifestStore(stateDir)
		Expect(store.Create(&manifest)).Should(Succeed())
		lifecycle := Lifecycle{Store: store, Namespace: cleanup.NamespaceService{Dynamic: client}, Collector: collector.Collector{StateDir: stateDir}, Prompt: prompt, Sources: lifecycleTestSources()}

		err := lifecycle.Finalize(context.Background(), manifest, errors.New("build failed"))
		Expect(err).Should(MatchError(ContainSubstring("delete ephemeral GitLab SCM Secret")))
		Expect(prompt.calls).Should(Equal(1))
		loaded, loadErr := store.Load("run-1")
		Expect(loadErr).ShouldNot(HaveOccurred())
		Expect(loaded.FailureArtifacts).ShouldNot(BeNil())
	})

	It("accepts a missing ephemeral GitLab SCM Secret", func() {
		stateDir := GinkgoT().TempDir()
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(), namespaceObject("tenant", "run-1"))
		prompt := &fakePrompt{approved: false}
		manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseCompleted, Fixture: model.FixtureIdentity{TenantNamespace: "tenant", EphemeralGitLabAuth: true}}
		store := evidence.NewManifestStore(stateDir)
		Expect(store.Create(&manifest)).Should(Succeed())
		lifecycle := Lifecycle{Store: store, Namespace: cleanup.NamespaceService{Dynamic: client}, Collector: collector.Collector{StateDir: stateDir}, Prompt: prompt, Sources: lifecycleTestSources()}

		Expect(lifecycle.Finalize(context.Background(), manifest, nil)).Should(Succeed())
	})

	It("refuses to delete a GitLab SCM Secret owned by another run", func() {
		stateDir := GinkgoT().TempDir()
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(), namespaceObject("tenant", "run-1"), gitLabSecretObject("tenant", "run-2"))
		prompt := &fakePrompt{approved: false}
		manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseCompleted, Fixture: model.FixtureIdentity{TenantNamespace: "tenant", EphemeralGitLabAuth: true}}
		store := evidence.NewManifestStore(stateDir)
		Expect(store.Create(&manifest)).Should(Succeed())
		lifecycle := Lifecycle{Store: store, Namespace: cleanup.NamespaceService{Dynamic: client}, Collector: collector.Collector{StateDir: stateDir}, Prompt: prompt, Sources: lifecycleTestSources()}

		err := lifecycle.Finalize(context.Background(), manifest, nil)
		Expect(err).Should(MatchError(ContainSubstring("refusing to delete GitLab SCM Secret")))
		Expect(err.Error()).Should(ContainSubstring("run-2"))
		_, getErr := client.Resource(cluster.SecretGVR).Namespace("tenant").Get(context.Background(), cluster.GitLabCredentialSecretName, metav1.GetOptions{})
		Expect(getErr).ShouldNot(HaveOccurred())
	})

	It("refuses to delete an unlabeled GitLab SCM Secret", func() {
		stateDir := GinkgoT().TempDir()
		foreign := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": cluster.GitLabCredentialSecretName, "namespace": "tenant"}}}
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(), namespaceObject("tenant", "run-1"), foreign)
		prompt := &fakePrompt{approved: false}
		manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseCompleted, Fixture: model.FixtureIdentity{TenantNamespace: "tenant", EphemeralGitLabAuth: true}}
		store := evidence.NewManifestStore(stateDir)
		Expect(store.Create(&manifest)).Should(Succeed())
		lifecycle := Lifecycle{Store: store, Namespace: cleanup.NamespaceService{Dynamic: client}, Collector: collector.Collector{StateDir: stateDir}, Prompt: prompt, Sources: lifecycleTestSources()}

		err := lifecycle.Finalize(context.Background(), manifest, nil)
		Expect(err).Should(MatchError(ContainSubstring("refusing to delete GitLab SCM Secret")))
		_, getErr := client.Resource(cluster.SecretGVR).Namespace("tenant").Get(context.Background(), cluster.GitLabCredentialSecretName, metav1.GetOptions{})
		Expect(getErr).ShouldNot(HaveOccurred())
	})

	It("keeps failure evidence when the ephemeral GitLab SCM Secret cannot be read", func() {
		stateDir := GinkgoT().TempDir()
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(), namespaceObject("tenant", "run-1"), gitLabSecretObject("tenant", "run-1"))
		client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("get denied")
		})
		prompt := &fakePrompt{approved: false}
		manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseFailed, Fixture: model.FixtureIdentity{TenantNamespace: "tenant", EphemeralGitLabAuth: true}}
		store := evidence.NewManifestStore(stateDir)
		Expect(store.Create(&manifest)).Should(Succeed())
		lifecycle := Lifecycle{Store: store, Namespace: cleanup.NamespaceService{Dynamic: client}, Collector: collector.Collector{StateDir: stateDir}, Prompt: prompt, Sources: lifecycleTestSources()}

		err := lifecycle.Finalize(context.Background(), manifest, errors.New("build failed"))
		Expect(err).Should(MatchError(ContainSubstring("get ephemeral GitLab SCM Secret")))
		Expect(prompt.calls).Should(Equal(1))
		loaded, loadErr := store.Load("run-1")
		Expect(loadErr).ShouldNot(HaveOccurred())
		Expect(loaded.FailureArtifacts).ShouldNot(BeNil())
	})

	It("retains the GitLab SCM Secret for a configured token", func() {
		stateDir := GinkgoT().TempDir()
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(), namespaceObject("tenant", "run-1"), gitLabSecretObject("tenant", "run-1"))
		prompt := &fakePrompt{approved: false}
		manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseCompleted, Fixture: model.FixtureIdentity{TenantNamespace: "tenant"}}
		store := evidence.NewManifestStore(stateDir)
		Expect(store.Create(&manifest)).Should(Succeed())
		lifecycle := Lifecycle{Store: store, Namespace: cleanup.NamespaceService{Dynamic: client}, Collector: collector.Collector{StateDir: stateDir}, Prompt: prompt, Sources: lifecycleTestSources()}

		Expect(lifecycle.Finalize(context.Background(), manifest, nil)).Should(Succeed())
		_, err := client.Resource(cluster.SecretGVR).Namespace("tenant").Get(context.Background(), cluster.GitLabCredentialSecretName, metav1.GetOptions{})
		Expect(err).ShouldNot(HaveOccurred())
	})

	When("the tenant namespace is retained", func() {
		It("should write the retention line to the configured log", func() {
			stateDir := GinkgoT().TempDir()
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), namespaceObject("tenant", "run-1"))
			prompt := &fakePrompt{approved: false}
			log := &bytes.Buffer{}
			manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseCompleted, Fixture: model.FixtureIdentity{TenantNamespace: "tenant"}}
			store := evidence.NewManifestStore(stateDir)
			Expect(store.Create(&manifest)).Should(Succeed())
			lifecycle := Lifecycle{Store: store, Namespace: cleanup.NamespaceService{Dynamic: client}, Collector: collector.Collector{StateDir: stateDir}, Prompt: prompt, Sources: lifecycleTestSources(), Log: log}
			Expect(lifecycle.Finalize(context.Background(), manifest, nil)).Should(Succeed())
			Expect(log.String()).Should(ContainSubstring("retained tenant namespace tenant\n"))
			_, err := lifecycle.Namespace.Inspect(context.Background(), "tenant")
			Expect(err).ShouldNot(HaveOccurred())
		})
	})
})

// lifecycleTestSources returns artifact sources that write valid synthetic snapshots.
func lifecycleTestSources() []collector.Source {
	sources := collector.RequiredArtifactNames()
	result := make([]collector.Source, 0, len(sources)-2)
	for _, path := range sources[1 : len(sources)-1] {
		path := path
		result = append(result, collector.Source{Name: path, Path: path, Collect: func(_ context.Context, root string) error {
			if path == "workload/taskruns.json" {
				artifactPath := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(artifactPath), 0o750); err != nil {
					return err
				}
				return os.WriteFile(artifactPath, []byte("{\"items\":[]}\n"), 0o640)
			}
			return writeLifecycleArtifact(filepath.Join(root, path))
		}})
	}
	return result
}

// gitLabSecretObject creates the harness-owned GitLab SCM Secret for a run.
func gitLabSecretObject(namespace, runID string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": cluster.GitLabCredentialSecretName, "namespace": namespace,
			"labels": map[string]any{cleanup.ManagedByLabel: cleanup.ManagedByValue, cleanup.RunIDLabel: runID},
		},
	}}
}

// namespaceObject creates a namespace carrying the requested run ownership labels.
func namespaceObject(name, runID string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": name, "labels": map[string]any{cleanup.ManagedByLabel: cleanup.ManagedByValue, cleanup.RunIDLabel: runID}}}}
}

// writeLifecycleArtifact creates a directory or writes an empty JSON object at path.
func writeLifecycleArtifact(path string) error {
	if filepath.Ext(path) == "" {
		return os.MkdirAll(path, 0o750)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("{}\n"), 0o640)
}

var _ = Describe("Namespace deletion timeout", func() {
	It("fails instead of hanging when the namespace never disappears", func() {
		stateDir := GinkgoT().TempDir()
		client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			cleanup.NamespaceGVR:   "NamespaceList",
			cluster.ApplicationGVR: "ApplicationList",
			cluster.ComponentGVR:   "ComponentList",
			{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "imagerepositories"}: "ImageRepositoryList",
			{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "releases"}:          "ReleaseList",
			{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}:                      "PipelineRunList",
		}, namespaceObject("tenant", "run-1"))
		client.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, namespaceObject("tenant", "run-1"), nil
		})
		previous := namespaceDeletionTimeout
		namespaceDeletionTimeout = 50 * time.Millisecond
		DeferCleanup(func() { namespaceDeletionTimeout = previous })
		manifest := model.RunManifest{RunID: "run-1", Phase: model.PhaseCompleted, Fixture: model.FixtureIdentity{TenantNamespace: "tenant"}}
		lifecycle := Lifecycle{
			Store: evidence.NewManifestStore(stateDir), Namespace: cleanup.NamespaceService{Dynamic: client},
			Collector: collector.Collector{StateDir: stateDir}, Prompt: &fakePrompt{approved: true}, Sources: lifecycleTestSources(),
		}

		err := lifecycle.Finalize(context.Background(), manifest, nil)

		Expect(err).Should(MatchError(ContainSubstring("wait for namespace tenant deletion")))
		Expect(err.Error()).Should(ContainSubstring("deadline exceeded"))
	})
})
