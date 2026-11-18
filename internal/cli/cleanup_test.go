package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redhat-appstudio/lkmabt/internal/cleanup"
	"github.com/redhat-appstudio/lkmabt/internal/collector"
	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/evidence"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

var _ = Describe("cleanupRun", func() {
	It("deletes the manifest namespace only after confirming exact ownership", func() {
		store := cleanupTestStore()
		client := cleanupTestClient("run-1", cleanup.ManagedByValue)
		prompt := &cleanupTestPrompt{approved: true}

		Expect(cleanupRun(context.Background(), cleanupTestRequest("run-1", store.Root), store, cleanup.NamespaceService{Dynamic: client}, prompt, io.Discard)).Should(Succeed())
		Expect(prompt.calls).Should(Equal(1))
		Expect(prompt.message).Should(ContainSubstring("tenant-run-1"))
		Expect(prompt.message).Should(ContainSubstring("run-1"))
		_, err := client.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant-run-1", metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).Should(BeTrue())
	})

	It("refuses a namespace with a different run ID before prompting", func() {
		store := cleanupTestStore()
		client := cleanupTestClient("another-run", cleanup.ManagedByValue)
		prompt := &cleanupTestPrompt{approved: true}

		err := cleanupRun(context.Background(), cleanupTestRequest("run-1", store.Root), store, cleanup.NamespaceService{Dynamic: client}, prompt, io.Discard)
		Expect(err).Should(MatchError(ContainSubstring("exact LKMABT ownership")))
		Expect(prompt.calls).Should(BeZero())
		_, err = client.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant-run-1", metav1.GetOptions{})
		Expect(err).ShouldNot(HaveOccurred())
	})

	It("requires an explicit confirmation prompt", func() {
		store := cleanupTestStore()
		client := cleanupTestClient("run-1", cleanup.ManagedByValue)

		err := cleanupRun(context.Background(), cleanupTestRequest("run-1", store.Root), store, cleanup.NamespaceService{Dynamic: client}, nil, io.Discard)
		Expect(err).Should(MatchError(ContainSubstring("explicit confirmation")))
		_, err = client.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant-run-1", metav1.GetOptions{})
		Expect(err).ShouldNot(HaveOccurred())
	})

	It("retains the namespace when confirmation is declined", func() {
		store := cleanupTestStore()
		client := cleanupTestClient("run-1", cleanup.ManagedByValue)
		prompt := &cleanupTestPrompt{}

		Expect(cleanupRun(context.Background(), cleanupTestRequest("run-1", store.Root), store, cleanup.NamespaceService{Dynamic: client}, prompt, io.Discard)).Should(Succeed())
		_, err := client.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant-run-1", metav1.GetOptions{})
		Expect(err).ShouldNot(HaveOccurred())
	})

	It("refuses a run created for a different cluster", func() {
		store := cleanupTestStore()
		client := cleanupTestClient("run-1", cleanup.ManagedByValue)
		prompt := &cleanupTestPrompt{approved: true}
		request := cleanupTestRequest("run-1", store.Root)
		request.ClusterServer = "https://other.example"

		err := cleanupRun(context.Background(), request, store, cleanup.NamespaceService{Dynamic: client}, prompt, io.Discard)
		Expect(err).Should(MatchError(ContainSubstring("belongs to cluster")))
		Expect(prompt.calls).Should(BeZero())
	})

	It("refuses cleanup when a required saved artifact is missing", func() {
		store := cleanupTestStore()
		manifest, err := store.Load("run-1")
		Expect(err).ShouldNot(HaveOccurred())
		missingPath := filepath.Join(store.RunDirFor(manifest), "workload", "pods.json")
		Expect(os.Remove(missingPath)).Should(Succeed())
		client := cleanupTestClient("run-1", cleanup.ManagedByValue)
		prompt := &cleanupTestPrompt{approved: true}

		err = cleanupRun(context.Background(), cleanupTestRequest("run-1", store.Root), store, cleanup.NamespaceService{Dynamic: client}, prompt, io.Discard)
		Expect(err).Should(MatchError(ContainSubstring("workload/pods.json")))
		Expect(prompt.calls).Should(BeZero())
		_, err = client.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant-run-1", metav1.GetOptions{})
		Expect(err).ShouldNot(HaveOccurred())
	})

	It("refuses cleanup when the saved collection report marks a required artifact incomplete", func() {
		store := cleanupTestStore()
		manifest, err := store.Load("run-1")
		Expect(err).ShouldNot(HaveOccurred())
		reportPath := filepath.Join(store.RunDirFor(manifest), "collection-report.json")
		report, err := collector.ReadReport(reportPath)
		Expect(err).ShouldNot(HaveOccurred())
		for index := range report.Attempts {
			if report.Attempts[index].Path == "workload/components.json" {
				report.Attempts[index].Status = "error"
				report.Attempts[index].Error = "component snapshot failed"
			}
		}
		data, err := json.Marshal(report)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(os.WriteFile(reportPath, data, 0o640)).Should(Succeed())
		client := cleanupTestClient("run-1", cleanup.ManagedByValue)
		prompt := &cleanupTestPrompt{approved: true}

		err = cleanupRun(context.Background(), cleanupTestRequest("run-1", store.Root), store, cleanup.NamespaceService{Dynamic: client}, prompt, io.Discard)
		Expect(err).Should(MatchError(ContainSubstring("workload/components.json")))
		Expect(prompt.calls).Should(BeZero())
		_, err = client.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant-run-1", metav1.GetOptions{})
		Expect(err).ShouldNot(HaveOccurred())
	})

	It("refuses cleanup when a listed TaskRun YAML snapshot is missing", func() {
		store := cleanupTestStore()
		manifest, err := store.Load("run-1")
		Expect(err).ShouldNot(HaveOccurred())
		taskRunsPath := filepath.Join(store.RunDirFor(manifest), "workload", "taskruns.json")
		Expect(os.WriteFile(taskRunsPath, []byte("{\"items\":[{\"metadata\":{\"name\":\"build-task\"}}]}\n"), 0o640)).Should(Succeed())
		client := cleanupTestClient("run-1", cleanup.ManagedByValue)
		prompt := &cleanupTestPrompt{approved: true}

		err = cleanupRun(context.Background(), cleanupTestRequest("run-1", store.Root), store, cleanup.NamespaceService{Dynamic: client}, prompt, io.Discard)
		Expect(err).Should(MatchError(ContainSubstring("workload/taskruns/tenant-run-1--build-task.yaml")))
		Expect(prompt.calls).Should(BeZero())
		_, err = client.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant-run-1", metav1.GetOptions{})
		Expect(err).ShouldNot(HaveOccurred())
	})

	It("refuses cleanup when a saved TaskRun snapshot has invalid identity", func() {
		store := cleanupTestStore()
		manifest, err := store.Load("run-1")
		Expect(err).ShouldNot(HaveOccurred())
		runRoot := store.RunDirFor(manifest)
		taskRunsPath := filepath.Join(runRoot, "workload", "taskruns.json")
		Expect(os.WriteFile(taskRunsPath, []byte("{\"items\":[{\"metadata\":{\"name\":\"build-task\"}}]}\n"), 0o640)).Should(Succeed())
		snapshotPath := filepath.Join(runRoot, "workload", "taskruns", "tenant-run-1--build-task.yaml")
		Expect(os.WriteFile(snapshotPath, []byte("kind: TaskRun\nmetadata:\n  name: another-task\n  namespace: tenant-run-1\n"), 0o640)).Should(Succeed())
		client := cleanupTestClient("run-1", cleanup.ManagedByValue)
		prompt := &cleanupTestPrompt{approved: true}

		err = cleanupRun(context.Background(), cleanupTestRequest("run-1", store.Root), store, cleanup.NamespaceService{Dynamic: client}, prompt, io.Discard)
		Expect(err).Should(MatchError(ContainSubstring("TaskRun snapshot")))
		Expect(prompt.calls).Should(BeZero())
		_, err = client.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant-run-1", metav1.GetOptions{})
		Expect(err).ShouldNot(HaveOccurred())
	})

	It("allows cleanup after verifying retained failed-run artifacts", func() {
		store := cleanupTestFailedStore()
		manifest, err := store.Load("run-1")
		Expect(err).ShouldNot(HaveOccurred())
		report, err := collector.ReadReport(filepath.Join(store.RunDirFor(manifest), "collection-report.json"))
		Expect(err).ShouldNot(HaveOccurred())
		failedBuildsVerified := false
		for _, attempt := range report.Attempts {
			if attempt.Path == "failed-builds/" && attempt.Status == "success" {
				failedBuildsVerified = true
			}
		}
		Expect(failedBuildsVerified).Should(BeTrue())
		client := cleanupTestClient("run-1", cleanup.ManagedByValue)
		prompt := &cleanupTestPrompt{approved: true}

		Expect(cleanupRun(context.Background(), cleanupTestRequest("run-1", store.Root), store, cleanup.NamespaceService{Dynamic: client}, prompt, io.Discard)).Should(Succeed())
		Expect(prompt.calls).Should(Equal(1))
	})

	It("refuses cleanup when a recorded failed-build log is missing", func() {
		store := cleanupTestFailedStore()
		manifest, err := store.Load("run-1")
		Expect(err).ShouldNot(HaveOccurred())
		logPath := manifest.FailedBuildLogs[0].LogPath
		Expect(os.Remove(logPath)).Should(Succeed())
		client := cleanupTestClient("run-1", cleanup.ManagedByValue)
		prompt := &cleanupTestPrompt{approved: true}

		err = cleanupRun(context.Background(), cleanupTestRequest("run-1", store.Root), store, cleanup.NamespaceService{Dynamic: client}, prompt, io.Discard)
		Expect(err).Should(MatchError(ContainSubstring("failed-builds/build.log")))
		Expect(prompt.calls).Should(BeZero())
		_, err = client.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant-run-1", metav1.GetOptions{})
		Expect(err).ShouldNot(HaveOccurred())
	})
})

var _ = Describe("cleanupRun output", func() {
	It("should print verification and deletion steps to a console", func() {
		store := cleanupTestStore()
		client := cleanupTestClient("run-1", cleanup.ManagedByValue)
		out := &bytes.Buffer{}

		Expect(cleanupRun(context.Background(), cleanupTestRequest("run-1", store.Root), store, cleanup.NamespaceService{Dynamic: client}, &cleanupTestPrompt{approved: true}, &Console{Out: out})).Should(Succeed())

		Expect(out.String()).Should(ContainSubstring(" • Verify run run-1 🧾  ..."))
		Expect(out.String()).Should(ContainSubstring(" ✓ Verify run run-1 🧾"))
		Expect(out.String()).Should(ContainSubstring(" • Delete tenant namespace tenant-run-1 🧹  ..."))
		Expect(out.String()).Should(ContainSubstring(" ✓ Delete tenant namespace tenant-run-1 🧹"))
	})

	It("should report a failed verification step", func() {
		store := cleanupTestStore()
		client := cleanupTestClient("another-run", cleanup.ManagedByValue)
		out := &bytes.Buffer{}

		err := cleanupRun(context.Background(), cleanupTestRequest("run-1", store.Root), store, cleanup.NamespaceService{Dynamic: client}, &cleanupTestPrompt{}, &Console{Out: out})

		Expect(err).Should(HaveOccurred())
		Expect(out.String()).Should(ContainSubstring(" ✗ Verify run run-1 🧾"))
		Expect(out.String()).ShouldNot(ContainSubstring("Delete tenant namespace"))
	})
})

var _ = Describe("cleanupRun retention notice", func() {
	When("the confirmation is declined", func() {
		It("should write the retention notice to the visible writer", func() {
			store := cleanupTestStore()
			client := cleanupTestClient("run-1", cleanup.ManagedByValue)
			var notice bytes.Buffer

			Expect(cleanupRun(context.Background(), cleanupTestRequest("run-1", store.Root), store, cleanup.NamespaceService{Dynamic: client}, &cleanupTestPrompt{}, &notice)).Should(Succeed())
			Expect(notice.String()).Should(Equal("retained tenant namespace tenant-run-1\n"))
		})
	})

	When("the confirmation is approved", func() {
		It("should not write the retention notice", func() {
			store := cleanupTestStore()
			client := cleanupTestClient("run-1", cleanup.ManagedByValue)
			var notice bytes.Buffer

			Expect(cleanupRun(context.Background(), cleanupTestRequest("run-1", store.Root), store, cleanup.NamespaceService{Dynamic: client}, &cleanupTestPrompt{approved: true}, &notice)).Should(Succeed())
			Expect(notice.String()).ShouldNot(ContainSubstring("retained tenant namespace"))
			_, err := client.Resource(cleanup.NamespaceGVR).Get(context.Background(), "tenant-run-1", metav1.GetOptions{})
			Expect(apierrors.IsNotFound(err)).Should(BeTrue())
		})
	})
})

var _ = Describe("cleanupRun under the aggregate layout", func() {
	When("the run lives in an aggregate directory", func() {
		It("should locate the cluster root and verify its artifacts", func() {
			source := cleanupTestStore()
			stateDir := GinkgoT().TempDir()
			aggregate := "multi-run-10:46_8.10.2026_run-agg"
			clusterRoot := filepath.Join(stateDir, aggregate, "c1")
			// Copy the whole source tree so the run lives at a real aggregate path.
			Expect(filepath.WalkDir(source.Root, func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				relative, err := filepath.Rel(source.Root, path)
				if err != nil {
					return err
				}
				target := filepath.Join(clusterRoot, relative)
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if info.Mode()&fs.ModeSymlink != 0 {
					// The store's latest alias is a symlink; recreate it rather than read through it.
					linkTarget, err := os.Readlink(path)
					if err != nil {
						return err
					}
					return os.Symlink(linkTarget, target)
				}
				if entry.IsDir() {
					return os.MkdirAll(target, 0o750)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				return os.WriteFile(target, data, info.Mode().Perm())
			})).Should(Succeed())

			located, err := locateRunStateRoot(stateDir, "c1", "run-1")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(located).Should(Equal(clusterRoot))
			client := cleanupTestClient("run-1", cleanup.ManagedByValue)
			Expect(cleanupRun(context.Background(), cleanupTestRequest("run-1", located), evidence.NewManifestStore(located), cleanup.NamespaceService{Dynamic: client}, &cleanupTestPrompt{approved: true}, io.Discard)).Should(Succeed())
		})
	})
})

// writeRunManifest writes <stateDir>/<aggregate>/<cluster>/<providerRun>/manifest.json with the run ID.
func writeRunManifest(stateDir, aggregate, cluster, providerRun, runID string) {
	directory := filepath.Join(stateDir, aggregate, cluster, providerRun)
	Expect(os.MkdirAll(directory, 0o750)).Should(Succeed())
	Expect(os.WriteFile(filepath.Join(directory, "manifest.json"), []byte(`{"runID":"`+runID+`"}`), 0o640)).Should(Succeed())
}

var _ = Describe("locateRunStateRoot", func() {
	When("exactly one run directory holds the run", func() {
		It("should return the cluster state root", func() {
			stateDir := GinkgoT().TempDir()
			writeRunManifest(stateDir, "multi-run-10:46_8.10.2026_run-agg1", "c1", "github-run-x", "run-1")
			writeRunManifest(stateDir, "multi-run-10:46_8.10.2026_run-agg1", "c2", "github-run-y", "run-9")

			root, err := locateRunStateRoot(stateDir, "c1", "run-1")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(root).Should(Equal(filepath.Join(stateDir, "multi-run-10:46_8.10.2026_run-agg1", "c1")))
		})
	})

	When("no run directory holds the run", func() {
		It("should fail naming the run and cluster", func() {
			stateDir := GinkgoT().TempDir()
			writeRunManifest(stateDir, "multi-run-10:46_8.10.2026_run-agg1", "c1", "github-run-x", "run-2")

			_, err := locateRunStateRoot(stateDir, "c1", "run-1")
			Expect(err).Should(MatchError(ContainSubstring(`no run "run-1" for cluster "c1"`)))
		})
	})

	When("two aggregate directories hold the run", func() {
		It("should fail as ambiguous", func() {
			stateDir := GinkgoT().TempDir()
			writeRunManifest(stateDir, "multi-run-10:46_8.10.2026_run-agg1", "c1", "github-run-x", "run-1")
			writeRunManifest(stateDir, "multi-run-11:02_8.10.2026_run-agg2", "c1", "github-run-x", "run-1")

			_, err := locateRunStateRoot(stateDir, "c1", "run-1")
			Expect(err).Should(MatchError(ContainSubstring("matches 2 run directories")))
			Expect(err).Should(MatchError(ContainSubstring(stateDir)))
		})
	})

	When("the newest pointer also references the run", func() {
		It("should not double count the latest symlink", func() {
			stateDir := GinkgoT().TempDir()
			writeRunManifest(stateDir, "multi-run-10:46_8.10.2026_run-agg1", "c1", "github-run-x", "run-1")
			Expect(os.Symlink("multi-run-10:46_8.10.2026_run-agg1", filepath.Join(stateDir, "latest"))).Should(Succeed())

			root, err := locateRunStateRoot(stateDir, "c1", "run-1")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(root).Should(Equal(filepath.Join(stateDir, "multi-run-10:46_8.10.2026_run-agg1", "c1")))
		})
	})

	When("a manifest is not valid JSON", func() {
		It("should skip that run directory", func() {
			stateDir := GinkgoT().TempDir()
			writeRunManifest(stateDir, "multi-run-10:46_8.10.2026_run-agg1", "c1", "github-run-bad", "run-9")
			Expect(os.WriteFile(filepath.Join(stateDir, "multi-run-10:46_8.10.2026_run-agg1", "c1", "github-run-bad", "manifest.json"), []byte("{"), 0o640)).Should(Succeed())
			writeRunManifest(stateDir, "multi-run-10:46_8.10.2026_run-agg1", "c1", "github-run-good", "run-1")

			root, err := locateRunStateRoot(stateDir, "c1", "run-1")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(root).Should(Equal(filepath.Join(stateDir, "multi-run-10:46_8.10.2026_run-agg1", "c1")))
		})
	})

	When("a cluster directory is missing", func() {
		It("should skip it", func() {
			stateDir := GinkgoT().TempDir()
			writeRunManifest(stateDir, "multi-run-10:46_8.10.2026_run-agg1", "c9", "github-run-x", "run-1")

			_, err := locateRunStateRoot(stateDir, "c1", "run-1")
			Expect(err).Should(MatchError(ContainSubstring(`no run "run-1" for cluster "c1"`)))
		})
	})

	When("the state directory does not exist", func() {
		It("should report the read failure", func() {
			_, err := locateRunStateRoot(filepath.Join(GinkgoT().TempDir(), "absent"), "c1", "run-1")
			Expect(err).Should(MatchError(ContainSubstring("read state directory")))
		})
	})
})

type cleanupTestPrompt struct {
	approved bool
	calls    int
	message  string
}

// Confirm records the prompt message and returns the configured approval.
func (p *cleanupTestPrompt) Confirm(_ context.Context, message string) (bool, error) {
	p.calls++
	p.message = message
	return p.approved, nil
}

// cleanupTestRequest builds a cleanup request for runID and stateDir.
func cleanupTestRequest(runID, stateDir string) config.Request {
	return config.Request{Command: config.CommandCleanup, RunID: runID, ClusterServer: "https://api.example", StateDir: stateDir}
}

// cleanupTestStore creates a completed run with verified required artifacts.
func cleanupTestStore() evidence.ManifestStore {
	store := evidence.NewManifestStore(GinkgoT().TempDir())
	manifest := model.RunManifest{
		RunID:               "run-1",
		Provider:            config.ProviderGitHub,
		Phase:               model.PhaseCompleted,
		TargetClusterServer: "https://api.example/",
		Fixture:             model.FixtureIdentity{TenantNamespace: "tenant-run-1"},
	}
	Expect(store.Create(&manifest)).Should(Succeed())
	_, err := (collector.Collector{StateDir: store.Root}).CollectAndVerify(context.Background(), manifest.RunID, manifest, lifecycleTestSources())
	Expect(err).ShouldNot(HaveOccurred())
	return store
}

// cleanupTestFailedStore creates a failed run with a retained build log and verified artifacts.
func cleanupTestFailedStore() evidence.ManifestStore {
	store := evidence.NewManifestStore(GinkgoT().TempDir())
	manifest := model.RunManifest{
		RunID:               "run-1",
		ArtifactDirectory:   "run-1",
		Provider:            config.ProviderGitHub,
		Phase:               model.PhaseFailed,
		TargetClusterServer: "https://api.example/",
		Fixture:             model.FixtureIdentity{TenantNamespace: "tenant-run-1"},
		FailedBuildLogs:     []model.FailedBuildLog{{TaskRunName: "build-task", LogPath: filepath.Join(store.Root, "run-1", "failed-builds", "build.log")}},
	}
	Expect(store.Create(&manifest)).Should(Succeed())
	Expect(os.MkdirAll(filepath.Join(store.RunDirFor(manifest), "failed-builds"), 0o750)).Should(Succeed())
	Expect(os.WriteFile(manifest.FailedBuildLogs[0].LogPath, []byte("failure log"), 0o640)).Should(Succeed())
	_, err := (collector.Collector{StateDir: store.Root}).CollectAndVerify(context.Background(), manifest.RunID, manifest, lifecycleTestSources())
	Expect(err).ShouldNot(HaveOccurred())
	return store
}

// cleanupTestClient returns a fake client seeded with a tenant namespace and cleanup resource kinds.
func cleanupTestClient(runID, managedBy string) *fake.FakeDynamicClient {
	namespace := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": "tenant-run-1",
			"labels": map[string]any{
				cleanup.ManagedByLabel: managedBy,
				cleanup.RunIDLabel:     runID,
			},
		},
	}}
	listKinds := map[schema.GroupVersionResource]string{
		cleanup.NamespaceGVR: "NamespaceList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "applications"}:      "ApplicationList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "components"}:        "ComponentList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "imagerepositories"}: "ImageRepositoryList",
		{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "releases"}:          "ReleaseList",
		{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}:                      "PipelineRunList",
	}
	return fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, namespace)
}
