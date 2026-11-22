package workflow

import (
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redhat-appstudio/lkmabt/internal/evidence"
	"github.com/redhat-appstudio/lkmabt/internal/model"
)

type fakeStage struct {
	calls     []string
	deadlines map[string]bool
	fail      string
	// deferAt names the step that returns deferErr to simulate a deferral.
	deferAt  string
	deferErr error
	// before runs at the start of a named stage.
	before func(string)
}

// call records a fake stage invocation and optionally returns a canceled-context error.
func (s *fakeStage) call(ctx context.Context, name string) error {
	s.calls = append(s.calls, name)
	if s.before != nil {
		s.before(name)
	}
	if _, ok := ctx.Deadline(); ok {
		if s.deadlines == nil {
			s.deadlines = map[string]bool{}
		}
		s.deadlines[name] = true
	}
	if s.fail == name {
		return context.Canceled
	}
	if s.deferAt == name {
		return s.deferErr
	}
	return nil
}

// Preflight records invocation of the fake preflight stage.
func (s *fakeStage) Preflight(ctx context.Context, _ *model.RunManifest) error {
	return s.call(ctx, "preflight")
}

// EnsureFixture records invocation of the fake fixture stage.
func (s *fakeStage) EnsureFixture(ctx context.Context, _ *model.RunManifest) error {
	return s.call(ctx, "fixture")
}

// TriggerComponents records invocation of the fake trigger stage.
func (s *fakeStage) TriggerComponents(ctx context.Context, _ *model.RunManifest) error {
	return s.call(ctx, "trigger")
}

// VerifyBuilds records invocation of the fake build stage.
func (s *fakeStage) VerifyBuilds(ctx context.Context, _ *model.RunManifest) error {
	return s.call(ctx, "builds")
}

// VerifyBuildOutputs records invocation of the fake output-verification stage.
func (s *fakeStage) VerifyBuildOutputs(ctx context.Context, _ *model.RunManifest) error {
	return s.call(ctx, "build-outputs")
}

// fixedClock returns a clock that always reports at.
func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

var _ = Describe("Runner", func() {
	It("fails instead of deferring when the deferral cannot be persisted", func() {
		root := GinkgoT().TempDir()
		stage := &fakeStage{
			deferAt:  "builds",
			deferErr: &AdmissionPendingError{Namespace: "tenant", Pending: []AdmissionPending{{Component: "web", PipelineRun: "run-web"}}},
			before: func(name string) {
				if name != "builds" {
					return
				}
				Expect(os.RemoveAll(root)).Should(Succeed())
				Expect(os.WriteFile(root, []byte("not a directory"), 0o600)).Should(Succeed())
			},
		}
		runner := Runner{Store: evidence.NewManifestStore(root), Stage: stage, Now: fixedClock(time.Unix(100, 0))}

		manifest, err := runner.Run(context.Background(), Options{RunID: "run-1", Provider: "github", ClusterServer: "https://api.example", FixtureNamespace: "tenant", Application: "app"})

		Expect(err).Should(HaveOccurred())
		Expect(IsDeferred(err)).Should(BeFalse())
		Expect(manifest.Deferred).Should(BeNil())
		Expect(manifest.Failure).ShouldNot(BeNil())
		Expect(manifest.Failure.Message).Should(ContainSubstring("record deferred admission wait"))
	})

	It("persists successful phase sequence", func() {
		stage := &fakeStage{}
		runner := Runner{Store: evidence.NewManifestStore(GinkgoT().TempDir()), Stage: stage, Now: fixedClock(time.Unix(100, 0))}
		manifest, err := runner.Run(context.Background(), Options{RunID: "run-1", Provider: "github", ClusterServer: "https://api.example", FixtureNamespace: "tenant", Application: "app"})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(manifest.Phase).Should(Equal(model.PhaseCompleted))
		Expect(stage.calls).Should(Equal([]string{"preflight", "fixture", "trigger", "builds", "build-outputs"}))
	})

	It("persists failure", func() {
		stage := &fakeStage{fail: "build-outputs"}
		runner := Runner{Store: evidence.NewManifestStore(GinkgoT().TempDir()), Stage: stage}
		manifest, err := runner.Run(context.Background(), Options{RunID: "run-2", Provider: "github", ClusterServer: "https://api.example"})
		Expect(err).Should(HaveOccurred())
		Expect(manifest.Phase).Should(Equal(model.PhaseFailed))
		Expect(manifest.Failure).ShouldNot(BeNil())
	})

	It("applies phase timeouts", func() {
		stage := &fakeStage{}
		runner := Runner{Store: evidence.NewManifestStore(GinkgoT().TempDir()), Stage: stage}
		_, err := runner.Run(context.Background(), Options{RunID: "run-timeout", Provider: "github", ClusterServer: "https://api.example", Timeouts: PhaseTimeouts{Preflight: time.Minute}})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(stage.deadlines["preflight"]).Should(BeTrue())
	})

	It("reports every executed phase and its result", func() {
		stage := &fakeStage{}
		runner := Runner{Store: evidence.NewManifestStore(GinkgoT().TempDir()), Stage: stage}
		var started, ended []model.Phase
		var endErrors []error

		_, err := runner.Run(context.Background(), Options{
			RunID: "run-progress", Provider: "github", ClusterServer: "https://api.example",
			OnPhaseStart: func(phase model.Phase) { started = append(started, phase) },
			OnPhaseEnd: func(phase model.Phase, phaseErr error) {
				ended = append(ended, phase)
				endErrors = append(endErrors, phaseErr)
			},
		})

		Expect(err).ShouldNot(HaveOccurred())
		Expect(started).Should(Equal([]model.Phase{
			model.PhasePreflight, model.PhaseFixtureReady, model.PhaseTriggered,
			model.PhaseBuildsVerified, model.PhaseBuildOutputsVerified, model.PhaseCompleted,
		}))
		Expect(ended).Should(Equal(started))
		Expect(endErrors).Should(ConsistOf(nil, nil, nil, nil, nil, nil))
	})

	It("reports the failing phase and does not start later phases", func() {
		stage := &fakeStage{fail: "builds"}
		runner := Runner{Store: evidence.NewManifestStore(GinkgoT().TempDir()), Stage: stage}
		var started, ended []model.Phase
		var lastErr error

		_, err := runner.Run(context.Background(), Options{
			RunID: "run-progress-fail", Provider: "github", ClusterServer: "https://api.example",
			OnPhaseStart: func(phase model.Phase) { started = append(started, phase) },
			OnPhaseEnd: func(phase model.Phase, phaseErr error) {
				ended = append(ended, phase)
				lastErr = phaseErr
			},
		})

		Expect(err).Should(HaveOccurred())
		Expect(started).Should(Equal([]model.Phase{
			model.PhasePreflight, model.PhaseFixtureReady, model.PhaseTriggered, model.PhaseBuildsVerified,
		}))
		Expect(ended).Should(Equal(started))
		Expect(lastErr).Should(HaveOccurred())
	})

	It("reports a phase transition failure instead of success", func() {
		store := evidence.NewManifestStore(GinkgoT().TempDir())
		stage := &fakeStage{}
		runner := Runner{Store: store, Stage: stage}
		var started, ended []model.Phase
		var endErrors []error

		_, err := runner.Run(context.Background(), Options{
			RunID: "run-transition-fail", Provider: "github", ClusterServer: "https://api.example",
			OnPhaseStart: func(phase model.Phase) { started = append(started, phase) },
			OnPhaseEnd: func(phase model.Phase, phaseErr error) {
				ended = append(ended, phase)
				endErrors = append(endErrors, phaseErr)
			},
			OnManifestCreated: func(created model.RunManifest) {
				// A directory where manifest.json belongs makes the next save fail.
				manifestPath := filepath.Join(store.RunDirFor(created), "manifest.json")
				Expect(os.Remove(manifestPath)).Should(Succeed())
				Expect(os.Mkdir(manifestPath, 0o750)).Should(Succeed())
			},
		})

		Expect(err).Should(HaveOccurred())
		// Preflight's transition is a no-op, so the failure follows the fixture step.
		Expect(started).Should(Equal([]model.Phase{model.PhasePreflight, model.PhaseFixtureReady}))
		Expect(ended).Should(Equal(started))
		Expect(endErrors[0]).Should(Succeed())
		Expect(endErrors[1]).Should(HaveOccurred())
	})

	When("a manifest is created", func() {
		It("should call OnManifestCreated with the persisted manifest", func() {
			store := evidence.NewManifestStore(GinkgoT().TempDir())
			var created model.RunManifest
			calls := 0
			hook := func(manifest model.RunManifest) {
				calls++
				created = manifest
				// manifest.json exists on disk by the time the hook runs.
				_, statErr := os.Stat(filepath.Join(store.Root, created.ArtifactDirectory, "manifest.json"))
				Expect(statErr).ShouldNot(HaveOccurred())
			}
			runner := Runner{Store: store, Stage: &fakeStage{}, Now: fixedClock(time.Date(2026, 10, 5, 14, 23, 0, 0, time.UTC))}

			_, err := runner.Run(context.Background(), Options{RunID: "run-1", Provider: "github", ClusterServer: "https://api.example", FixtureNamespace: "tenant", Application: "app", OnManifestCreated: hook})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(calls).Should(Equal(1))
			Expect(created.RunID).Should(Equal("run-1"))
			Expect(created.ArtifactDirectory).Should(Equal("github-run-14:23_5.10.2026_run-1"))
			Expect(created.Phase).Should(Equal(model.PhasePreflight))
		})

		It("should not call OnManifestCreated when the manifest cannot be created", func() {
			store := evidence.NewManifestStore(GinkgoT().TempDir())
			calls := 0
			hook := func(model.RunManifest) { calls++ }
			// A regular file where the run directory should go makes Create fail.
			Expect(os.WriteFile(filepath.Join(store.Root, "github-run-14:23_5.10.2026_run-1"), nil, 0o640)).Should(Succeed())
			runner := Runner{Store: store, Stage: &fakeStage{}, Now: fixedClock(time.Date(2026, 10, 5, 14, 23, 0, 0, time.UTC))}

			_, err := runner.Run(context.Background(), Options{RunID: "run-1", Provider: "github", ClusterServer: "https://api.example", OnManifestCreated: hook})
			Expect(err).Should(MatchError(ContainSubstring("already exists")))
			Expect(calls).Should(Equal(0))
		})
	})

	When("the run ID already exists", func() {
		It("should reject the second run instead of resuming", func() {
			store := evidence.NewManifestStore(GinkgoT().TempDir())
			runner := Runner{Store: store, Stage: &fakeStage{}, Now: fixedClock(time.Date(2026, 10, 5, 14, 23, 0, 0, time.UTC))}
			options := Options{RunID: "run-1", Provider: "github", ClusterServer: "https://api.example", FixtureNamespace: "tenant", Application: "app"}

			_, err := runner.Run(context.Background(), options)
			Expect(err).ShouldNot(HaveOccurred())
			manifest, err := runner.Run(context.Background(), options)
			Expect(err).Should(MatchError(ContainSubstring("already exists")))
			Expect(manifest.RunID).Should(Equal("run-1"))
			Expect(manifest.Phase).Should(Equal(model.PhasePreflight))
		})
	})

	When("Options.CreatedAt is set", func() {
		It("should use it for the manifest and directory", func() {
			store := evidence.NewManifestStore(GinkgoT().TempDir())
			runner := Runner{Store: store, Stage: &fakeStage{}, Now: fixedClock(time.Date(2026, 10, 9, 23, 0, 0, 0, time.UTC))}

			manifest, err := runner.Run(context.Background(), Options{RunID: "run-1", Provider: "github", ClusterServer: "https://api.example",
				CreatedAt: time.Date(2026, 10, 5, 14, 23, 0, 0, time.FixedZone("IDT", 3*3600))})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(manifest.CreatedAt).Should(Equal(time.Date(2026, 10, 5, 14, 23, 0, 0, time.FixedZone("IDT", 3*3600))))
			Expect(manifest.ArtifactDirectory).Should(Equal("github-run-14:23_5.10.2026_run-1"))
		})
	})

	When("Options.CreatedAt is zero", func() {
		It("should use the runner clock", func() {
			store := evidence.NewManifestStore(GinkgoT().TempDir())
			runner := Runner{Store: store, Stage: &fakeStage{}, Now: fixedClock(time.Date(2026, 10, 5, 14, 23, 0, 0, time.UTC))}

			manifest, err := runner.Run(context.Background(), Options{RunID: "run-1", Provider: "github", ClusterServer: "https://api.example"})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(manifest.CreatedAt).Should(Equal(time.Date(2026, 10, 5, 14, 23, 0, 0, time.UTC)))
			Expect(manifest.ArtifactDirectory).Should(Equal("github-run-14:23_5.10.2026_run-1"))
		})
	})

	When("the run fails", func() {
		It("should still publish the created manifest", func() {
			store := evidence.NewManifestStore(GinkgoT().TempDir())
			calls := 0
			hook := func(model.RunManifest) { calls++ }
			runner := Runner{Store: store, Stage: &fakeStage{fail: "build-outputs"}}

			manifest, err := runner.Run(context.Background(), Options{RunID: "run-1", Provider: "github", ClusterServer: "https://api.example", OnManifestCreated: hook})
			Expect(err).Should(HaveOccurred())
			Expect(calls).Should(Equal(1))
			Expect(manifest.Phase).Should(Equal(model.PhaseFailed))
		})
	})
})
