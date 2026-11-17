package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redhat-appstudio/lkmabt/internal/evidence"
	"github.com/redhat-appstudio/lkmabt/internal/model"
)

const TriggerCommentFormat = "# lkmabt trigger %s %d"

type Confirmation interface {
	// Confirm requests approval for the supplied action description.
	Confirm(context.Context, string) (bool, error)
}

type Stage interface {
	// Preflight validates cluster identity, access, and scheduling prerequisites.
	Preflight(context.Context, *model.RunManifest) error
	// EnsureFixture validates and prepares the run's tenant fixture resources.
	EnsureFixture(context.Context, *model.RunManifest) error
	// TriggerComponents updates fixture files to trigger builds and records commits.
	TriggerComponents(context.Context, *model.RunManifest) error
	// VerifyBuilds waits for successful builds and records their PipelineRuns.
	VerifyBuilds(context.Context, *model.RunManifest) error
	// VerifyBuildOutputs records evidence for the expected build architectures.
	VerifyBuildOutputs(context.Context, *model.RunManifest) error
}

type Options struct {
	RunID             string
	Provider          string
	ClusterServer     string
	OnManifestCreated func(model.RunManifest)
	CreatedAt         time.Time // zero means time.Now()
	StateDir          string
	FixtureNamespace  string
	Application       string
	Timeouts          PhaseTimeouts
}

type PhaseTimeouts struct {
	Preflight time.Duration
	Build     time.Duration
}

type Runner struct {
	Store evidence.ManifestStore
	Stage Stage
	Now   func() time.Time
}

// Run creates a manifest and advances it through each workflow stage.
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
		if shouldSkip(manifest.Phase, step.phase, index, steps) {
			continue
		}
		stepContext, cancel := contextWithTimeout(ctx, step.timeout)
		err := step.run(stepContext, &manifest)
		cancel()
		if err != nil {
			manifest.Failure = &model.Failure{Phase: step.phase, Reason: failureReason(step.phase), Message: err.Error()}
			_ = r.Store.Transition(&manifest, model.PhaseFailed)
			return manifest, err
		}
		if step.phase != manifest.Phase {
			if err := r.Store.Transition(&manifest, step.phase); err != nil {
				return manifest, err
			}
		}
	}
	return manifest, nil
}

// shouldSkip reports whether a step precedes the current phase or the run is terminal.
func shouldSkip(current, target model.Phase, index int, _ []struct {
	phase   model.Phase
	timeout time.Duration
	run     func(context.Context, *model.RunManifest) error
}) bool {
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
	return index < start[current] || target == ""
}

// contextWithTimeout applies a deadline when timeout is positive.
func contextWithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

// failureReason derives the failure code for a workflow phase.
func failureReason(phase model.Phase) string {
	return strings.TrimSuffix(string(phase), "-wait") + "-failed"
}
