package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/evidence"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/platform"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubetesting "k8s.io/client-go/testing"
)

// pendingPipelineRun creates an admission-pending PipelineRun fixture.
func pendingPipelineRun(name, component, sha, uid string, created time.Time, specStatus, reason, message string) unstructured.Unstructured {
	object := pipelineRunAt(name, component, sha, uid, "Unknown", created)
	if specStatus != "" {
		_ = unstructured.SetNestedField(object.Object, specStatus, "spec", "status")
	}
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	condition := conditions[0].(map[string]any)
	if reason != "" {
		condition["reason"] = reason
	}
	if message != "" {
		condition["message"] = message
	}
	_ = unstructured.SetNestedSlice(object.Object, conditions, "status", "conditions")
	return object
}

// withStartTime records a PipelineRun start time.
func withStartTime(object unstructured.Unstructured, started time.Time) unstructured.Unstructured {
	_ = unstructured.SetNestedField(object.Object, started.Format(time.RFC3339), "status", "startTime")
	return object
}

var _ = Describe("build admission", func() {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	Describe("admissionPending", func() {
		It("is pending when spec.status holds the PipelineRun", func() {
			object := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", now.Add(-time.Minute), pipelineRunPendingStatus, "", "")
			Expect(admissionPending(&object, nil)).Should(BeTrue())
		})

		It("is pending when the Succeeded condition reports PipelineRunPending", func() {
			object := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", now.Add(-time.Minute), "", pipelineRunPendingStatus, "")
			Expect(admissionPending(&object, nil)).Should(BeTrue())
		})

		It("is not pending once the PipelineRun started", func() {
			object := withStartTime(pendingPipelineRun("run-web", "web", "sha-web", "uid-web", now.Add(-time.Minute), pipelineRunPendingStatus, "", ""), now.Add(-30*time.Second))
			Expect(admissionPending(&object, nil)).Should(BeFalse())
		})

		It("is not pending once a child TaskRun exists", func() {
			object := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", now.Add(-time.Minute), pipelineRunPendingStatus, "", "")
			taskRuns := []unstructured.Unstructured{buildTaskRunForWorkflow("task-web", "uid-web", "Unknown", "buildah-oci-ta")}
			Expect(admissionPending(&object, taskRuns)).Should(BeFalse())
		})

		It("is not pending for a normal condition", func() {
			object := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", now.Add(-time.Minute), "", "", "")
			Expect(admissionPending(&object, nil)).Should(BeFalse())
		})
	})

	Describe("admissionTracker", func() {
		commit := model.TriggerCommit{Component: "web", SHA: "sha-web"}

		It("does not defer a PipelineRun below the threshold", func() {
			object := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", now.Add(-30*time.Second), pipelineRunPendingStatus, "", "")
			tracker := newAdmissionTracker(time.Minute, nil)

			Expect(tracker.deferred("tenant", now, []model.TriggerCommit{commit}, []unstructured.Unstructured{object}, nil, nil)).Should(BeNil())
		})

		It("defers an over-threshold PipelineRun with component, run, and reason", func() {
			object := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", now.Add(-2*time.Minute), pipelineRunPendingStatus, "", "quota exceeded")
			tracker := newAdmissionTracker(time.Minute, nil)

			err := tracker.deferred("tenant", now, []model.TriggerCommit{commit}, []unstructured.Unstructured{object}, nil, nil)
			Expect(err).ShouldNot(BeNil())
			Expect(err.Namespace).Should(Equal("tenant"))
			Expect(err.Pending).Should(HaveLen(1))
			Expect(err.Pending[0].Component).Should(Equal("web"))
			Expect(err.Pending[0].PipelineRun).Should(Equal("run-web"))
			Expect(err.Pending[0].Pending).Should(Equal(2 * time.Minute))
			Expect(err.Pending[0].Reason).Should(Equal("quota exceeded"))
			Expect(errors.Is(err, ErrDeferred)).Should(BeTrue())
		})

		It("measures an unstarted run from its first observation", func() {
			object := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", time.Time{}, pipelineRunPendingStatus, "", "")
			tracker := newAdmissionTracker(time.Minute, nil)
			runs := []unstructured.Unstructured{object}

			Expect(tracker.deferred("tenant", now, []model.TriggerCommit{commit}, runs, nil, nil)).Should(BeNil())
			err := tracker.deferred("tenant", now.Add(2*time.Minute), []model.TriggerCommit{commit}, runs, nil, nil)
			Expect(err).ShouldNot(BeNil())
			Expect(err.Pending[0].Pending).Should(Equal(2 * time.Minute))
		})

		It("forgets a component that stopped being pending", func() {
			object := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", time.Time{}, pipelineRunPendingStatus, "", "")
			tracker := newAdmissionTracker(time.Minute, nil)
			Expect(tracker.deferred("tenant", now, []model.TriggerCommit{commit}, []unstructured.Unstructured{object}, nil, nil)).Should(BeNil())
			Expect(tracker.since).Should(HaveKey("web"))

			started := withStartTime(object, now)
			Expect(tracker.deferred("tenant", now.Add(time.Minute), []model.TriggerCommit{commit}, []unstructured.Unstructured{started}, nil, nil)).Should(BeNil())
			Expect(tracker.since).Should(BeEmpty())
		})

		It("ignores a captured component", func() {
			object := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", now.Add(-time.Hour), pipelineRunPendingStatus, "", "")
			tracker := newAdmissionTracker(time.Minute, nil)

			Expect(tracker.deferred("tenant", now, []model.TriggerCommit{commit}, []unstructured.Unstructured{object}, nil, func(string) bool { return true })).Should(BeNil())
		})

		It("prefers the Kueue diagnostic reason", func() {
			object := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", now.Add(-2*time.Minute), pipelineRunPendingStatus, "", "condition message")
			tracker := newAdmissionTracker(time.Minute, func(*unstructured.Unstructured) string { return "kueue quota message" })

			err := tracker.deferred("tenant", now, []model.TriggerCommit{commit}, []unstructured.Unstructured{object}, nil, nil)
			Expect(err).ShouldNot(BeNil())
			Expect(err.Pending[0].Reason).Should(Equal("kueue quota message"))
		})

		It("falls back to the condition message when the diagnostic is empty", func() {
			object := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", now.Add(-2*time.Minute), pipelineRunPendingStatus, "", "condition message")
			tracker := newAdmissionTracker(time.Minute, func(*unstructured.Unstructured) string { return "" })

			err := tracker.deferred("tenant", now, []model.TriggerCommit{commit}, []unstructured.Unstructured{object}, nil, nil)
			Expect(err).ShouldNot(BeNil())
			Expect(err.Pending[0].Reason).Should(Equal("condition message"))
		})
	})

	Describe("admissionDiagnostic", func() {
		It("does not cache an empty Kueue result forever", func() {
			object := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", now.Add(-time.Minute), pipelineRunPendingStatus, "", "")
			var calls atomic.Int32
			dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
				cluster.KueueWorkloadGVR: "WorkloadList",
			})
			dynamic.PrependReactor("list", "workloads", func(kubetesting.Action) (bool, runtime.Object, error) {
				if calls.Add(1) == 1 {
					return true, &unstructured.UnstructuredList{}, nil
				}
				workload := &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "kueue.x-k8s.io/v1beta2",
					"kind":       "Workload",
					"metadata": map[string]any{
						"name":      "run-web-workload",
						"namespace": "tenant",
						"ownerReferences": []any{map[string]any{
							"apiVersion": "tekton.dev/v1",
							"kind":       "PipelineRun",
							"name":       "run-web",
							"uid":        "uid-web",
						}},
					},
					"status": map[string]any{"conditions": []any{map[string]any{
						"type": "QuotaReserved", "status": "False", "message": "quota arrived",
					}}},
				}}
				return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*workload}}, nil
			})
			stage := &LiveStage{Clients: &cluster.ClientSet{Dynamic: dynamic}}
			diagnose := stage.admissionDiagnostic(context.Background(), "tenant")

			Expect(diagnose(&object)).Should(BeEmpty())
			Expect(diagnose(&object)).Should(Equal("quota arrived"))
			Expect(calls.Load()).Should(Equal(int32(2)))
		})
	})

	Describe("admissionStatusDetail", func() {
		It("uses the Kueue diagnostic when present", func() {
			object := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", now.Add(-2*time.Minute), pipelineRunPendingStatus, "", "condition message")

			detail := admissionStatusDetail(&object, nil, now, func(*unstructured.Unstructured) string { return "kueue quota message" })
			Expect(detail).Should(Equal("PipelineRunPending for 2m0s (PipelineRun run-web; kueue quota message)"))
		})

		It("falls back to the condition message", func() {
			object := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", now.Add(-2*time.Minute), pipelineRunPendingStatus, "", "condition message")

			detail := admissionStatusDetail(&object, nil, now, func(*unstructured.Unstructured) string { return "" })
			Expect(detail).Should(Equal("PipelineRunPending for 2m0s (PipelineRun run-web; condition message)"))
		})
	})

	Describe("waitForBuildTaskRuns admission deferral", func() {
		It("returns the deferral after capturing a completed component", func() {
			previous := admissionPendingThreshold
			admissionPendingThreshold = time.Millisecond
			DeferCleanup(func() { admissionPendingThreshold = previous })

			pendingCommit := model.TriggerCommit{Component: "web", SHA: "sha-web"}
			doneCommit := model.TriggerCommit{Component: "worker", SHA: "sha-worker"}
			pendingRun := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", time.Now().Add(-time.Hour), pipelineRunPendingStatus, "", "quota exceeded")
			doneRun := pipelineRunAt("run-worker", "worker", "sha-worker", "uid-worker", "True", time.Now().Add(-time.Minute))
			doneTask := buildTaskRunForWorkflow("task-worker", "uid-worker", "True", "buildah-oci-ta")
			captured := make(map[string]model.PipelineRunIdentity)

			_, err := waitForBuildTaskRuns(context.Background(), 5*time.Second, 5*time.Second, time.Millisecond, "tenant",
				[]model.TriggerCommit{doneCommit, pendingCommit},
				func(context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, error) {
					return []unstructured.Unstructured{doneRun, pendingRun}, []unstructured.Unstructured{doneTask}, nil
				}, nil, nil, buildEvidenceCache{
					Capture: func(identity model.PipelineRunIdentity, _ *unstructured.Unstructured, _ []unstructured.Unstructured) error {
						captured[identity.Component] = identity
						return nil
					},
				})

			var admission *AdmissionPendingError
			Expect(errors.As(err, &admission)).Should(BeTrue())
			Expect(errors.Is(err, ErrDeferred)).Should(BeTrue())
			Expect(admission.Pending).Should(HaveLen(1))
			Expect(admission.Pending[0].Component).Should(Equal("web"))
			Expect(captured).Should(HaveKey("worker"))
		})
	})

	Describe("buildWaitStatus admission rendering", func() {
		It("renders PipelineRunPending plus the Kueue reason", func() {
			commit := model.TriggerCommit{Component: "web", SHA: "sha-web"}
			object := pendingPipelineRun("run-web", "web", "sha-web", "uid-web", now.Add(-2*time.Minute), pipelineRunPendingStatus, "", "condition message")

			status := buildWaitStatus([]model.TriggerCommit{commit}, []unstructured.Unstructured{object}, nil, now, nil,
				func(*unstructured.Unstructured) string { return "kueue quota message" }, nil)
			Expect(status).Should(Equal("builds: web: PipelineRunPending for 2m0s (PipelineRun run-web; kueue quota message)"))
		})
	})
})

var _ = Describe("Runner deferral", func() {
	It("persists a deferral without failing the run", func() {
		store := evidence.NewManifestStore(GinkgoT().TempDir())
		deferredErr := &AdmissionPendingError{
			Namespace: "tenant",
			Pending:   []AdmissionPending{{Component: "web", PipelineRun: "run-web", Pending: 2 * time.Minute, Reason: "quota exceeded"}},
		}
		stage := &fakeStage{deferAt: "builds", deferErr: deferredErr}
		runner := Runner{Store: store, Stage: stage, Now: fixedClock(nowForDeferral())}

		manifest, err := runner.Run(context.Background(), Options{RunID: "run-deferred", Provider: "github", ClusterServer: "https://api.example", FixtureNamespace: "tenant", Application: "app"})

		Expect(err).Should(HaveOccurred())
		Expect(errors.Is(err, ErrDeferred)).Should(BeTrue())
		Expect(manifest.Phase).Should(Equal(model.PhaseTriggered))
		Expect(manifest.Failure).Should(BeNil())
		Expect(manifest.Deferred).ShouldNot(BeNil())
		Expect(manifest.Deferred.Phase).Should(Equal(model.PhaseBuildsVerified))
		Expect(manifest.Deferred.Reason).Should(Equal("admission pending"))
		Expect(manifest.Deferred.Message).Should(ContainSubstring("quota exceeded"))

		data, readErr := os.ReadFile(filepath.Join(store.RunDirFor(manifest), "status.json"))
		Expect(readErr).ShouldNot(HaveOccurred())
		var status struct {
			RunID    string          `json:"runID"`
			Phase    model.Phase     `json:"phase"`
			Deferred *model.Deferral `json:"deferred"`
		}
		Expect(json.Unmarshal(data, &status)).Should(Succeed())
		Expect(status.RunID).Should(Equal("run-deferred"))
		Expect(status.Phase).Should(Equal(model.PhaseTriggered))
		Expect(status.Deferred).ShouldNot(BeNil())
		Expect(status.Deferred.Reason).Should(Equal("admission pending"))

		loaded, loadErr := store.Load("run-deferred")
		Expect(loadErr).ShouldNot(HaveOccurred())
		Expect(loaded.Deferred).ShouldNot(BeNil())
	})
})

// nowForDeferral returns a stable timestamp for deferral tests.
func nowForDeferral() time.Time {
	return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
}

var _ = Describe("LiveStage platform narrowing", func() {
	It("narrows to supported platforms and reports the rest", func() {
		log := &bytes.Buffer{}
		stage := &LiveStage{
			Request: config.Request{Platforms: []string{platform.LinuxAMD64, platform.LinuxS390X}},
			Log:     log,
		}
		result := cluster.PreflightResult{Platforms: []cluster.PlatformSupport{
			{Platform: platform.LinuxAMD64, Supported: true, Configured: true},
			{Platform: platform.LinuxS390X, Supported: false, Configured: false},
		}}

		stage.limitToSupportedPlatforms(result)

		Expect(stage.Request.Platforms).Should(Equal([]string{platform.LinuxAMD64}))
		Expect(log.String()).Should(Equal(" ! cluster: architecture support narrowed\n   not supported: " + platform.LinuxS390X + "\n   testing: " + platform.LinuxAMD64 + "\n"))
		Expect(log.String()).ShouldNot(ContainSubstring("warning:"))
		Expect(log.String()).ShouldNot(ContainSubstring("does not declare"))
	})

	It("does not notice when every requested platform is supported", func() {
		log := &bytes.Buffer{}
		stage := &LiveStage{
			Request: config.Request{Platforms: []string{platform.LinuxAMD64}},
			Log:     log,
		}
		result := cluster.PreflightResult{Platforms: []cluster.PlatformSupport{
			{Platform: platform.LinuxAMD64, Supported: true, Configured: true},
		}}

		stage.limitToSupportedPlatforms(result)

		Expect(stage.Request.Platforms).Should(Equal([]string{platform.LinuxAMD64}))
		Expect(log.String()).Should(BeEmpty())
	})
})
