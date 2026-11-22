package model

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// Phase identifies a point in a run's lifecycle. Non-terminal phases advance
// in declaration order; PhaseFailed may replace any phase except PhaseCompleted.
type Phase string

const (
	// PhasePreflight checks cluster identity, access, and scheduling.
	PhasePreflight Phase = "preflight"
	// PhaseFixtureReady marks the tenant fixture as prepared.
	PhaseFixtureReady Phase = "fixture-ready"
	// PhaseTriggered marks trigger commits pushed to the fixture branch.
	PhaseTriggered Phase = "triggered"
	// PhaseBuildsVerified marks every triggered component's PipelineRun as succeeded.
	PhaseBuildsVerified Phase = "builds-verified"
	// PhaseBuildOutputsVerified marks evidence recorded for each expected architecture.
	PhaseBuildOutputsVerified Phase = "build-outputs-verified"
	// PhaseCompleted is the successful terminal phase.
	PhaseCompleted Phase = "completed"
	// PhaseFailed is the terminal phase recorded with a Failure.
	PhaseFailed Phase = "failed"
)

var phaseOrder = []Phase{
	PhasePreflight, PhaseFixtureReady, PhaseTriggered, PhaseBuildsVerified,
	PhaseBuildOutputsVerified, PhaseCompleted,
}

// IsTerminalPhase reports whether phase marks a completed or failed run.
func IsTerminalPhase(phase Phase) bool {
	return phase == PhaseCompleted || phase == PhaseFailed
}

// ValidateTransition reports whether a run may move from from to to. Legal
// moves are an empty phase to PhasePreflight, each phase to its immediate
// successor, and any non-completed phase to PhaseFailed. PhaseCompleted rejects
// every target; PhaseFailed rejects every target except PhaseFailed itself, so
// recording a failure twice is idempotent.
func ValidateTransition(from, to Phase) error {
	if to == PhaseFailed {
		if from == PhaseCompleted {
			return fmt.Errorf("cannot fail a completed run")
		}
		return nil
	}
	if from == PhaseFailed || from == PhaseCompleted {
		return fmt.Errorf("cannot transition from terminal phase %q", from)
	}
	for index, phase := range phaseOrder {
		if phase == from {
			if index+1 < len(phaseOrder) && phaseOrder[index+1] == to {
				return nil
			}
			return fmt.Errorf("invalid transition %q -> %q", from, to)
		}
	}
	if from == "" && to == PhasePreflight {
		return nil
	}
	return fmt.Errorf("unknown phase transition %q -> %q", from, to)
}

// FixtureIdentity describes the tenant namespace and fixture repository that a
// run prepares and mutates.
type FixtureIdentity struct {
	TenantNamespace string   `json:"tenantNamespace"`
	Application     string   `json:"application"`
	Components      []string `json:"components"`
	RepositoryOwner string   `json:"repositoryOwner"`
	RepositoryName  string   `json:"repositoryName"`
	Repository      string   `json:"repository"`
	Branch          string   `json:"branch"`
	// EphemeralGitLabAuth marks a run-scoped GitLab token whose SCM Secret is
	// removed during finalization. The name avoids the evidence redactor's
	// credential-key fragments, which would replace a boolean with a string.
	EphemeralGitLabAuth bool `json:"ephemeralGitLabAuth,omitempty"`
}

// PipelineRunIdentity records a PipelineRun observed for a component.
type PipelineRunIdentity struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       types.UID `json:"uid"`
	Component string    `json:"component"`
	// SourceSHA is the trigger commit the PipelineRun built.
	SourceSHA   string    `json:"sourceSHA"`
	StartedAt   time.Time `json:"startedAt"`
	CompletedAt time.Time `json:"completedAt"`
	// Succeeded reports that the PipelineRun's build stage succeeded. LKMABT
	// does not wait for later image-index or scan tasks.
	Succeeded bool `json:"succeeded"`
}

// TriggerCommit records the fixture-branch commit pushed to trigger one
// component's build. Build evidence is matched to it by SHA.
type TriggerCommit struct {
	Component string    `json:"component"`
	SHA       string    `json:"sha"`
	URL       string    `json:"url,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	// PrerequisiteSHAs lists earlier fixture-configuration or superseded trigger
	// commits. PipelineRuns for those commits are not evidence that the current
	// trigger commit was built.
	PrerequisiteSHAs []string `json:"prerequisiteSHAs,omitempty"`
	// Retriggers counts automatic retriggers after PaC failed to reconcile a run.
	Retriggers int `json:"retriggers,omitempty"`
}

// BuildOutputEvidence records verified outputs for one component PipelineRun.
type BuildOutputEvidence struct {
	Component   string              `json:"component"`
	PipelineRun PipelineRunIdentity `json:"pipelineRun"`
	// Platforms lists the architectures the run was expected to build.
	Platforms []string `json:"platforms"`
	// CreatedOutputs maps each platform to the image reference built for it.
	CreatedOutputs map[string]string `json:"createdOutputs,omitempty"`
	// OutputDigests maps each platform to its verified image digest.
	OutputDigests map[string]string `json:"outputDigests,omitempty"`
	VerifiedAt    time.Time         `json:"verifiedAt"`
}

// FailedBuildLog records a failed build TaskRun step and, when collection
// succeeded, the stored tail of its container log.
type FailedBuildLog struct {
	PipelineRunName  string `json:"pipelineRunName"`
	TaskRunName      string `json:"taskRunName"`
	TaskName         string `json:"taskName"`
	StepContainer    string `json:"stepContainer"`
	ExitCode         int64  `json:"exitCode"`
	ConditionMessage string `json:"conditionMessage"`
	// LogPath is the stored log file; empty when the log could not be collected.
	LogPath string `json:"logPath,omitempty"`
}

// Failure describes why a run failed.
type Failure struct {
	// Phase is the phase that returned the error.
	Phase   Phase  `json:"phase"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// Deferral records a non-terminal attempt that stopped because the cluster
// could not admit its builds, not because a build failed. Phase is the phase
// whose step returned the deferral; the manifest keeps its last completed
// phase so the attempt can be retried as a fresh run without pretending it
// failed.
type Deferral struct {
	Phase      Phase     `json:"phase"`
	Reason     string    `json:"reason"`
	Message    string    `json:"message"`
	DeferredAt time.Time `json:"deferredAt"`
}

// FailureArtifactReport records which failure artifacts were collected and
// which required artifacts could not be saved.
type FailureArtifactReport struct {
	ArtifactPath          string    `json:"artifactPath"`
	RequiredArtifactNames []string  `json:"requiredArtifactNames"`
	SavedArtifactNames    []string  `json:"savedArtifactNames"`
	CollectionErrors      []string  `json:"collectionErrors,omitempty"`
	VerifiedAt            time.Time `json:"verifiedAt"`
}

// RunManifest is the persisted state of one run: the fixture it targets,
// commits pushed, PipelineRuns and build outputs observed, and any failure.
type RunManifest struct {
	RunID string `json:"runID"`
	// ArtifactDirectory is the run's directory name relative to the manifest
	// store root; Store.Create assigns it when empty.
	ArtifactDirectory   string                 `json:"artifactDirectory"`
	Provider            string                 `json:"provider"`
	TargetClusterServer string                 `json:"targetClusterServer"`
	CreatedAt           time.Time              `json:"createdAt"`
	UpdatedAt           time.Time              `json:"updatedAt"`
	Phase               Phase                  `json:"phase"`
	Fixture             FixtureIdentity        `json:"fixture"`
	TriggerCommits      []TriggerCommit        `json:"triggerCommits,omitempty"`
	PipelineRuns        []PipelineRunIdentity  `json:"pipelineRuns,omitempty"`
	BuildOutputs        []BuildOutputEvidence  `json:"buildOutputs,omitempty"`
	FailedBuildLogs     []FailedBuildLog       `json:"failedBuildLogs,omitempty"`
	Failure             *Failure               `json:"failure,omitempty"`
	Deferred            *Deferral              `json:"deferred,omitempty"`
	FailureArtifacts    *FailureArtifactReport `json:"failureArtifacts,omitempty"`
}
