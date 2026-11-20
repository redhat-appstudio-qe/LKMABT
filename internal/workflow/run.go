package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redhat-appstudio/lkmabt/internal/evidence"
	"github.com/redhat-appstudio/lkmabt/internal/model"
)

// TriggerCommentFormat is appended as a comment line to a component's
// Dockerfile to force a fixture commit. The arguments are the run ID and a Unix
// timestamp; the timestamp keeps repeated triggers distinct.
const TriggerCommentFormat = "# lkmabt trigger %s %d"

// Confirmation asks an operator to approve a described action.
type Confirmation interface {
	// Confirm requests approval for the supplied action description. It reports
	// false without an error when the operator declines.
	Confirm(context.Context, string) (bool, error)
}

// Stage executes the workflow steps in order. Implementations mutate the
// manifest in place; Runner persists each phase transition after the step
// succeeds.
type Stage interface {
	// Preflight validates cluster identity, access, and scheduling prerequisites.
	Preflight(context.Context, *model.RunManifest) error
	// EnsureFixture validates and prepares the run's tenant fixture resources.
	EnsureFixture(context.Context, *model.RunManifest) error
	// TriggerComponents appends a trigger comment to each component Dockerfile,
	// pushes the commits, and records them in the manifest.
	TriggerComponents(context.Context, *model.RunManifest) error
	// VerifyBuilds waits for successful builds and records their PipelineRuns.
	VerifyBuilds(context.Context, *model.RunManifest) error
	// VerifyBuildOutputs records evidence for the expected build architectures.
	VerifyBuildOutputs(context.Context, *model.RunManifest) error
}

// Options configures a single Runner.Run invocation.
type Options struct {
	// RunID identifies the run; an empty value generates one.
	RunID         string
	Provider      string
	ClusterServer string
	// OnManifestCreated runs once after the manifest is persisted and before the
	// first phase, so callbacks may rely on the artifact directory existing.
	OnManifestCreated func(model.RunManifest)
	CreatedAt         time.Time // zero means time.Now()
	// FixtureNamespace and Application are recorded in the manifest fixture.
	FixtureNamespace string
	Application      string
	Timeouts         PhaseTimeouts
	// OnPhaseStart and OnPhaseEnd report each executed phase to a progress
	// view. OnPhaseEnd receives the phase error, nil on success.
	OnPhaseStart func(model.Phase)
	OnPhaseEnd   func(model.Phase, error)
}

// PhaseTimeouts bounds stage execution. A non-positive value disables the
// deadline. Preflight covers preflight and fixture preparation; Build covers
// triggering, build verification, and build-output verification.
type PhaseTimeouts struct {
	Preflight time.Duration
	Build     time.Duration
}

// Runner advances a run through the workflow stages. Store and Stage are
// required, and a Runner must not be used concurrently.
type Runner struct {
	Store evidence.ManifestStore
	Stage Stage
	// Now defaults to time.Now and stamps the manifest creation time when
	// Options.CreatedAt is zero.
	Now func() time.Time
}

// Run creates a manifest, then executes each stage in phase order until the run
// completes or a stage fails. Each stage receives a context bounded by its
// PhaseTimeouts value. On failure, Run records a Failure, attempts to persist
// PhaseFailed, and returns the stage error; a failed failure-transition is
// ignored so the caller still observes the stage error. A stage error wrapping
// ErrDeferred is recorded as a non-terminal Deferral instead: the manifest keeps
// its last completed phase and is saved, so a caller can retry the cluster as a
// fresh run without treating the scheduler wait as a build failure. Run always
// starts a new run and never resumes an existing manifest.
func (r Runner) Run(ctx context.Context, options Options) (model.RunManifest, error) {
	if r.Stage == nil {
		return model.RunManifest{}, fmt.Errorf("workflow stage is required")
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	runID := options.RunID
	if runID == "" {
		runID = "run-" + uuid.NewString()
	}
	var manifest model.RunManifest
	created := options.CreatedAt
	if created.IsZero() {
		created = now().UTC()
	}
	manifest = model.RunManifest{RunID: runID, Provider: options.Provider, TargetClusterServer: options.ClusterServer, Phase: model.PhasePreflight, CreatedAt: created, Fixture: model.FixtureIdentity{TenantNamespace: options.FixtureNamespace, Application: options.Application}}
	if err := r.Store.Create(&manifest); err != nil {
		return manifest, err
	}
	if options.OnManifestCreated != nil {
		options.OnManifestCreated(manifest)
	}
	steps := []struct {
		phase   model.Phase
		timeout time.Duration
		run     func(context.Context, *model.RunManifest) error
	}{
		{model.PhasePreflight, options.Timeouts.Preflight, r.Stage.Preflight},
		{model.PhaseFixtureReady, options.Timeouts.Preflight, r.Stage.EnsureFixture},
		{model.PhaseTriggered, options.Timeouts.Build, r.Stage.TriggerComponents},
		{model.PhaseBuildsVerified, options.Timeouts.Build, r.Stage.VerifyBuilds},
		{model.PhaseBuildOutputsVerified, options.Timeouts.Build, r.Stage.VerifyBuildOutputs},
		{model.PhaseCompleted, 0, func(context.Context, *model.RunManifest) error { return nil }},
	}
	for index, step := range steps {
		if shouldSkip(manifest.Phase, index) {
			continue
		}
		stepContext, cancel := contextWithTimeout(ctx, step.timeout)
		if options.OnPhaseStart != nil {
			options.OnPhaseStart(step.phase)
		}
		err := step.run(stepContext, &manifest)
		cancel()
		if err != nil {
			if IsDeferred(err) {
				deferral := deferralFor(err, step.phase, now())
				manifest.Deferred = &deferral
				if saveErr := r.Store.Save(&manifest); saveErr != nil {
					err = errors.Join(err, fmt.Errorf("save deferred run: %w", saveErr))
				}
				if options.OnPhaseEnd != nil {
					options.OnPhaseEnd(step.phase, err)
				}
				return manifest, err
			}
			if options.OnPhaseEnd != nil {
				options.OnPhaseEnd(step.phase, err)
			}
			manifest.Failure = &model.Failure{Phase: step.phase, Reason: failureReason(step.phase), Message: err.Error()}
			_ = r.Store.Transition(&manifest, model.PhaseFailed)
			return manifest, err
		}
		if step.phase != manifest.Phase {
			if transitionErr := r.Store.Transition(&manifest, step.phase); transitionErr != nil {
				if options.OnPhaseEnd != nil {
					options.OnPhaseEnd(step.phase, transitionErr)
				}
				return manifest, transitionErr
			}
		}
		if options.OnPhaseEnd != nil {
			options.OnPhaseEnd(step.phase, nil)
		}
	}
	return manifest, nil
}

// shouldSkip reports whether the step at index must be skipped. Terminal runs
// skip every step. A run whose current phase is later than the step skips it,
// while an unknown current phase starts at the first step.
func shouldSkip(current model.Phase, index int) bool {
	if current == model.PhaseFailed || current == model.PhaseCompleted {
		return true
	}
	start := map[model.Phase]int{
		model.PhasePreflight:            0,
		model.PhaseFixtureReady:         1,
		model.PhaseTriggered:            2,
		model.PhaseBuildsVerified:       3,
		model.PhaseBuildOutputsVerified: 4,
		model.PhaseCompleted:            5,
	}
	return index < start[current]
}

// contextWithTimeout applies a deadline when timeout is positive; otherwise it
// returns the parent context with a no-op cancel function.
func contextWithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

// failureReason derives the failure code for a workflow phase.
func failureReason(phase model.Phase) string {
	return string(phase) + "-failed"
}
