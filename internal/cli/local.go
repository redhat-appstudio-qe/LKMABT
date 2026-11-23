package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/cleanup"
	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	"github.com/redhat-appstudio/lkmabt/internal/clusterauth"
	"github.com/redhat-appstudio/lkmabt/internal/clusterfile"
	"github.com/redhat-appstudio/lkmabt/internal/collector"
	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/evidence"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/providers"
	"github.com/redhat-appstudio/lkmabt/internal/providers/github"
	"github.com/redhat-appstudio/lkmabt/internal/providers/gitlab"
	"github.com/redhat-appstudio/lkmabt/internal/workflow"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// LiveRunner executes provider workflows and cleanup against live cluster services.
type LiveRunner struct {
	// Prompt decides whether finalization and cleanup may delete the tenant
	// namespace. A nil prompt never deletes during a run; during Cleanup it is
	// an error, because cleanup accepts no implicit approval.
	Prompt workflow.Confirmation
	// Out receives human-facing notices. When Out is a *Console, steps,
	// progress, and buffered prompts are routed through it.
	Out io.Writer
	// Interactive permits input prompts and browser logins.
	Interactive bool
}

// NewLiveRunner returns a Runner backed by the live cluster services. prompt
// approves finalization and cleanup deletions and stays nil when no deletion is
// allowed; out receives human-facing notices; interactive permits input prompts
// and browser logins.
func NewLiveRunner(prompt workflow.Confirmation, out io.Writer, interactive bool) Runner {
	return LiveRunner{Prompt: prompt, Out: out, Interactive: interactive}
}

// promptFor routes prompts through the console when it owns the output, so the
// spinner is cleared and concurrent progress is buffered until the prompt ends.
// The caller's confirmation policy is preserved: a nil prompt stays nil.
func promptFor(runner LiveRunner) workflow.Confirmation {
	console, ok := runner.Out.(*Console)
	if !ok || console == nil || runner.Prompt == nil {
		return runner.Prompt
	}
	// A console prompt already owns buffering; wrapping it would recurse.
	if promptConsole, ok := runner.Prompt.(*Console); ok && promptConsole == console {
		return runner.Prompt
	}
	return consolePrompt{console: console, prompt: runner.Prompt}
}

// consolePrompt runs the caller's prompt through the console's buffering.
type consolePrompt struct {
	console *Console
	prompt  workflow.Confirmation
}

// Confirm delegates to the caller's prompt while progress is buffered.
func (p consolePrompt) Confirm(ctx context.Context, message string) (bool, error) {
	return p.console.confirm(ctx, p.prompt, message)
}

// providerLabel returns the provider name for a request split across providers
// and "" for a single-provider request.
func providerLabel(request config.Request) string {
	if request.MultiProvider {
		return request.Provider
	}
	return ""
}

// stepFor starts a console step when out is a console and otherwise returns a
// nil step whose methods are no-ops.
func stepFor(out io.Writer, description string) *Step {
	if console, ok := out.(*Console); ok && console != nil {
		return console.StartStep(description)
	}
	return nil
}

// suspendOutput pauses console animation while a subprocess writes directly to
// the terminal. Writers that are not consoles need no suspension.
func suspendOutput(out io.Writer) func() {
	if console, ok := out.(*Console); ok && console != nil {
		return console.Suspend()
	}
	return func() {}
}

// Run executes one provider workflow against the request's cluster and then
// finalizes it.
//
// The tenant namespace is reserved across processes before the workflow starts,
// because a concurrent run must not delete resources this run still owns. The
// lock stays held through finalization, which runs even when the workflow fails:
// it collects failure artifacts or verifies saved ones, removes the run-scoped
// GitLab Secret when the run created one, and prompts for optional namespace
// deletion.
//
// Run may be called concurrently; the per-namespace lock serializes runs that
// share a tenant namespace. Failures before the workflow starts return an
// outcome carrying only the provider name. A workflow failure is wrapped with
// its ending phase, and any finalization failure is joined into the same error.
func (runner LiveRunner) Run(ctx context.Context, request config.Request, log RunLog) (RunOutcome, error) {
	clients, err := cluster.NewClientSet(request.Kubeconfig)
	if err != nil {
		return RunOutcome{Provider: request.Provider}, err
	}
	provider, err := providerFor(request)
	if err != nil {
		return RunOutcome{Provider: request.Provider}, err
	}
	stage, err := workflow.NewLiveStage(request, clients, provider)
	if err != nil {
		return RunOutcome{Provider: request.Provider}, err
	}
	stage.Prompt = promptFor(runner)
	stage.Interactive = runner.Interactive
	stage.Out = runner.Out
	stage.ParentContext = ctx
	stage.Log = log
	// Reserve the tenant namespace across processes before any artifact exists,
	// so a concurrent run cannot delete resources this run owns.
	releaseNamespace, err := acquireTenantNamespaceLock(tenantLockServer(request.ClusterServer, clients), request.TenantNamespace, tenantLockHolder(request.RunID, time.Now()))
	if err != nil {
		return RunOutcome{Provider: request.Provider}, err
	}
	defer releaseNamespace()
	store := evidence.NewManifestStore(request.StateDir)
	observer := newPhaseObserver(runner.Out, providerLabel(request))
	if observer != nil {
		stage.Progress = observer.progress
	}
	if label := providerLabel(request); label != "" {
		stage.NoticePrefix = label + " run - "
	}
	var logAttachErr error
	manifest, runErr := (workflow.Runner{Store: store, Stage: stage}).Run(ctx, workflow.Options{
		RunID:            request.RunID,
		Provider:         request.Provider,
		ClusterServer:    request.ClusterServer,
		FixtureNamespace: request.TenantNamespace,
		Application:      request.ApplicationName,
		Timeouts:         workflow.PhaseTimeouts{Preflight: request.Timeouts.Preflight, Build: request.Timeouts.Build},
		OnPhaseStart:     observer.start,
		OnPhaseEnd:       observer.end,
		OnManifestCreated: func(created model.RunManifest) {
			// The provider run directory only exists after Store.Create, so attach the log there.
			logAttachErr = log.Attach(filepath.Join(request.StateDir, created.ArtifactDirectory, "run.log"))
		},
	})
	outcome := outcomeFromManifest(request.Provider, manifest)
	if manifest.Phase == model.PhaseTriggered || manifest.Phase == model.PhaseBuildsVerified || manifest.Phase == model.PhaseBuildOutputsVerified || manifest.Phase == model.PhaseCompleted || workflow.IsDeferred(runErr) {
		// Record the selection after preflight narrowing. A preflight or fixture
		// failure has no tested architecture subset to report.
		outcome.Platforms = stage.Platforms()
	}
	if workflow.IsDeferred(runErr) {
		// A scheduler wait is not a failed run: release the owned tenant
		// namespace so its pending workloads stop holding queue positions, and
		// skip failure collection and the cleanup prompt.
		runErr = runErrorWithLogFailure(runErr, logAttachErr)
		outcome.DeferredReason = runErr.Error()
		if finalizeErr := runner.finalizeDeferredRun(ctx, stage, manifest, log); finalizeErr != nil {
			outcome.CleanupErr = finalizeErr
			runErr = fmt.Errorf("%w; deferred cleanup failed: %v", runErr, finalizeErr)
			outcome.DeferredReason = runErr.Error()
		}
		fmt.Fprintf(log, "deferred run %s at phase %s: %v\n", manifest.RunID, outcome.Stage, runErr)
		return outcome, runErr
	}
	finalizeErr := runner.finalizeRun(ctx, request, clients, stage, store, manifest, runErr, log)
	runErr = runErrorWithLogFailure(runErr, logAttachErr)
	if runErr != nil {
		if finalizeErr != nil {
			return outcome, fmt.Errorf("run %s failed at %s: %w; finalization failed: %v", manifest.RunID, outcome.Stage, runErr, finalizeErr)
		}
		return outcome, fmt.Errorf("run %s failed at %s: %w", manifest.RunID, outcome.Stage, runErr)
	}
	if finalizeErr != nil {
		return outcome, fmt.Errorf("run %s completed but finalization failed: %w", manifest.RunID, finalizeErr)
	}
	fmt.Fprintf(log, "completed run %s at phase %s\n", manifest.RunID, manifest.Phase)
	return outcome, nil
}

// Cleanup resolves the named cluster, locates the run under the state directory,
// and deletes the tenant namespace only after verification and explicit
// confirmation.
//
// The configured server must match the server the kubeconfig connects to.
// Cleanup acquires the latest-directory lock before searching, so a live run
// cannot rotate latest underneath it. If a live run already holds that lock, it
// searches only permanent aggregate directories and appends a retry hint when
// the run is not found there. A nil prompt is an error; a declined confirmation
// leaves the namespace in place and is not an error.
func (runner LiveRunner) Cleanup(ctx context.Context, request config.Request) error {
	if request.Command != config.CommandCleanup {
		return fmt.Errorf("unsupported cleanup command %q", request.Command)
	}
	entries, err := clusterfile.Load(request.ClustersFile)
	if err != nil {
		return err
	}
	entry, err := clusterfile.Find(entries, request.ClusterName)
	if err != nil {
		return err
	}
	authStep := stepFor(runner.Out, fmt.Sprintf("Authenticate cluster %s 🔐", entry.Name))
	resume := suspendOutput(runner.Out)
	resolved, failures := (clusterauth.Ladder{Runner: clusterauth.ExecRunner{Log: os.Stdout}, Interactive: runner.Interactive}).Authenticate(ctx, []clusterfile.Entry{entry})
	failure := failures[entry.Name]
	if failure == nil && len(resolved) > 0 {
		entry = resolved[0]
	}
	resume()
	authStep.End(failure == nil)
	if failure != nil {
		return failure
	}
	clients, err := cluster.NewClientSet(entry.Kubeconfig)
	if err != nil {
		return err
	}
	// Cleanup must verify the cluster it actually connected to, not only the configured value.
	connected := normalizeServer(clients.Config.Host)
	if configured := normalizeServer(entry.Server); configured != "" && configured != connected {
		return fmt.Errorf("cluster %s: kubeconfig server %s does not match configured server %s", entry.Name, connected, configured)
	}
	request.ClusterServer = connected
	locateStep := stepFor(runner.Out, fmt.Sprintf("Locate run %s 🔎", request.RunID))
	if _, err := os.Stat(request.StateDir); err != nil {
		locateStep.End(false)
		return fmt.Errorf("read state directory %s: %w", request.StateDir, err)
	}
	// Pin latest while cleaning a run from it; a live run owns the lock and can
	// only be cleaned from its permanent directory after it finishes.
	releaseLatest, lockErr := evidence.NewManifestStore(request.StateDir).LockLatest()
	if lockErr != nil && !errors.Is(lockErr, evidence.ErrLatestLocked) {
		locateStep.End(false)
		return fmt.Errorf("lock state directory %s: %w", request.StateDir, lockErr)
	}
	if releaseLatest != nil {
		defer releaseLatest()
	}
	locate := locateRunStateRoot
	if lockErr != nil {
		locate = locateNamedRunStateRoot
	}
	stateRoot, err := locate(request.StateDir, entry.Name, request.RunID)
	locateStep.End(err == nil)
	if err != nil {
		if lockErr != nil {
			return fmt.Errorf("%w (the latest directory belongs to a live run; retry after it finishes)", err)
		}
		return err
	}
	// Provider artifacts live under the located cluster root, so verification must resolve there.
	request.StateDir = stateRoot
	return cleanupRun(ctx, request, evidence.NewManifestStore(stateRoot), cleanup.NamespaceService{Dynamic: clients.Dynamic}, promptFor(runner), runner.Out)
}

// locateRunStateRoot finds the single <stateDir>/<aggregate>/<cluster> directory holding runID.
func locateRunStateRoot(stateDir, clusterName, runID string) (string, error) {
	return locateRunStateRootSkipping(stateDir, clusterName, runID, "")
}

// locateNamedRunStateRoot finds runID only in permanent aggregate directories,
// not in latest, which belongs to the live run holding the state-directory lock.
func locateNamedRunStateRoot(stateDir, clusterName, runID string) (string, error) {
	return locateRunStateRootSkipping(stateDir, clusterName, runID, "latest")
}

// locateRunStateRootSkipping returns the single <stateDir>/<aggregate>/<cluster>
// directory whose runs include runID, ignoring the aggregate named skip and
// hidden directories. Unreadable or malformed entries are skipped; the result is
// an error when no directory matches or when several do, because the caller
// cannot safely pick one.
func locateRunStateRootSkipping(stateDir, clusterName, runID, skip string) (string, error) {
	aggregates, err := os.ReadDir(stateDir)
	if err != nil {
		return "", fmt.Errorf("read state directory %s: %w", stateDir, err)
	}
	matches := []string{}
	for _, aggregate := range aggregates {
		if !aggregate.IsDir() || aggregate.Name() == skip || strings.HasPrefix(aggregate.Name(), ".") {
			continue
		}
		root := filepath.Join(stateDir, aggregate.Name(), clusterName)
		runDirs, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, runDir := range runDirs {
			if !runDir.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, runDir.Name(), "manifest.json"))
			if err != nil {
				continue
			}
			var manifest model.RunManifest
			if json.Unmarshal(data, &manifest) == nil && manifest.RunID == runID {
				matches = append(matches, root)
				break
			}
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no run %q for cluster %q under %s", runID, clusterName, stateDir)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("run %q for cluster %q matches %d run directories under %s; resolve manually", runID, clusterName, len(matches), stateDir)
	}
	return matches[0], nil
}

// cleanupRun deletes one run's tenant namespace after refusing unsafe requests:
// it requires an exact run ID, a manifest whose server matches the request, and
// verified saved artifacts, then confirms the namespace labels and run ID before
// locking it and prompting. A nil prompt is an error; an unavailable prompt is
// an error; a declined prompt writes a retention notice to out and returns nil.
func cleanupRun(ctx context.Context, request config.Request, store evidence.ManifestStore, namespaceService cleanup.NamespaceService, prompt workflow.Confirmation, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if strings.TrimSpace(request.RunID) == "" {
		return fmt.Errorf("cleanup requires an exact run ID")
	}
	verifyStep := stepFor(out, fmt.Sprintf("Verify run %s 🧾", request.RunID))
	verified := false
	defer func() {
		if !verified {
			verifyStep.End(false)
		}
	}()
	manifest, err := store.Load(request.RunID)
	if err != nil {
		return fmt.Errorf("load run %s: %w", request.RunID, err)
	}
	if manifest.RunID != request.RunID {
		return fmt.Errorf("run identity mismatch: requested %q, found %q", request.RunID, manifest.RunID)
	}
	if normalizeServer(manifest.TargetClusterServer) != normalizeServer(request.ClusterServer) {
		return fmt.Errorf("run %s belongs to cluster %s, not requested cluster %s", request.RunID, manifest.TargetClusterServer, request.ClusterServer)
	}
	if _, err := (collector.Collector{StateDir: request.StateDir}).VerifySavedArtifacts(request.RunID, manifest); err != nil {
		return fmt.Errorf("refusing cleanup without verified saved artifacts for run %s: %w", request.RunID, err)
	}
	namespace := manifest.Fixture.TenantNamespace
	if strings.TrimSpace(namespace) == "" {
		return fmt.Errorf("run %s has no tenant namespace", request.RunID)
	}
	info, err := namespaceService.Inspect(ctx, namespace)
	if err != nil {
		return fmt.Errorf("inspect namespace %s for run %s: %w", namespace, request.RunID, err)
	}
	if info.Name != namespace || info.Labels[cleanup.ManagedByLabel] != cleanup.ManagedByValue || info.RunID != request.RunID {
		return fmt.Errorf("refusing to clean namespace %s without exact LKMABT ownership for run %s", namespace, request.RunID)
	}
	verified = true
	verifyStep.End(true)
	// A live run holding this namespace is using the resources cleanup would delete.
	releaseNamespace, err := acquireTenantNamespaceLock(request.ClusterServer, namespace, tenantLockHolder(request.RunID, time.Now()))
	if err != nil {
		return err
	}
	defer releaseNamespace()
	if prompt == nil {
		return fmt.Errorf("cleanup requires explicit confirmation")
	}
	approved, err := prompt.Confirm(ctx, fmt.Sprintf("Delete tenant namespace %s owned by run %s", namespace, request.RunID))
	if errors.Is(err, ErrPromptUnavailable) {
		return fmt.Errorf("cleanup requires an interactive confirmation for tenant namespace %s: %w", namespace, err)
	}
	if err != nil {
		return err
	}
	if !approved {
		fmt.Fprintf(out, "retained tenant namespace %s\n", namespace)
		return nil
	}
	deleteStep := stepFor(out, fmt.Sprintf("Delete tenant namespace %s 🧹", namespace))
	uid, err := namespaceService.Delete(ctx, namespace, request.RunID)
	if err != nil {
		deleteStep.End(false)
		return err
	}
	if err := waitForNamespaceDeletion(ctx, namespaceService, namespace, uid); err != nil {
		deleteStep.End(false)
		return err
	}
	deleteStep.End(true)
	fmt.Fprintf(out, "deleted tenant namespace %s from run %s\n", namespace, request.RunID)
	return nil
}

// normalizeServer trims surrounding whitespace and trailing slashes from a server URL.
func normalizeServer(server string) string {
	return strings.TrimRight(strings.TrimSpace(server), "/")
}

// Lifecycle finalizes one run: it verifies or collects artifacts, deletes the
// run-scoped GitLab Secret, and optionally deletes the run-owned tenant
// namespace.
type Lifecycle struct {
	// Store persists failure-artifact reports when Root is set.
	Store     evidence.ManifestStore
	Namespace cleanup.NamespaceService
	// Collector verifies saved artifacts; an empty StateDir disables verification.
	Collector collector.Collector
	// Prompt must approve namespace deletion; a nil prompt never deletes.
	Prompt workflow.Confirmation
	// Sources overrides the collector sources otherwise derived from Namespace.Dynamic.
	Sources []collector.Source
	// Out receives retention notices for humans; nil discards them.
	Out io.Writer
	// Log receives retention notices in the run log; nil discards them.
	Log io.Writer
}

// finalizationTimeout bounds cleanup work after the workflow context is done.
const finalizationTimeout = 5 * time.Minute

// namespaceDeletionTimeout bounds waiting for an approved namespace deletion.
var namespaceDeletionTimeout = 5 * time.Minute

// waitForNamespaceDeletion bounds the deletion wait so a lingering finalizer or
// stuck API call fails with the namespace name instead of hanging the process.
// When deletion times out, the reported error names the blocking resource if the
// cluster can still be queried.
func waitForNamespaceDeletion(ctx context.Context, service cleanup.NamespaceService, name string, uid types.UID) error {
	waitCtx, cancel := context.WithTimeout(ctx, namespaceDeletionTimeout)
	defer cancel()
	if err := service.WaitDeleted(waitCtx, name, uid); err != nil {
		statusCtx, statusCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer statusCancel()
		if blocker := service.DeletionBlocker(statusCtx, name); blocker != "" {
			return fmt.Errorf("%w (%s)", err, blocker)
		}
		return err
	}
	return nil
}

// Finalize verifies the run's saved artifacts, deletes the ephemeral GitLab
// Secret, and asks whether to delete the run-owned tenant namespace. It returns
// nil when the run has no tenant namespace or the namespace is already gone.
//
// Inspection, secret deletion, and artifact collection use a fresh context
// bounded by finalizationTimeout, so they still complete after ctx is canceled.
// The confirmation prompt and namespace deletion use ctx, so a canceled or
// timed-out run retains its namespace. On failure, artifacts are collected into
// manifest.FailureArtifacts and the manifest is saved when Store.Root is set.
// Secret-deletion and collection errors are joined into the returned error.
//
// A namespace whose ownership cannot be confirmed is an error, except when the
// run already failed and the manifest phase is failed; that case returns without
// deleting the namespace or the Secret. A nil Prompt never deletes the
// namespace. A prompt that cannot read input returns ErrPromptUnavailable: the
// namespace is retained and the reason is written to Out and Log; it is not
// treated as a declined answer. On a successful run with a non-nil Prompt,
// Collector.StateDir must be set because cleanup requires verified artifacts.
func (l Lifecycle) Finalize(ctx context.Context, manifest model.RunManifest, runErr error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()
	namespace := manifest.Fixture.TenantNamespace
	if namespace == "" {
		return nil
	}
	info, err := l.Namespace.Inspect(cleanupCtx, namespace)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Name != namespace || info.Labels[cleanup.ManagedByLabel] != cleanup.ManagedByValue || info.RunID != manifest.RunID {
		if runErr != nil && manifest.Phase == model.PhaseFailed {
			return nil
		}
		return fmt.Errorf("tenant namespace %s is not owned by run %s", namespace, manifest.RunID)
	}
	var secretErr error
	if manifest.Fixture.EphemeralGitLabAuth {
		secretErr = deleteGitLabCredentialSecret(cleanupCtx, l.Namespace.Dynamic, namespace, manifest.RunID)
	}
	if runErr != nil {
		sources := l.Sources
		if sources == nil && l.Namespace.Dynamic != nil {
			sources = collector.KubernetesSources(l.Namespace.Dynamic, namespace)
		}
		report, collectErr := l.Collector.CollectAndVerify(cleanupCtx, manifest.RunID, manifest, sources)
		manifest.FailureArtifacts = &report
		if l.Store.Root != "" {
			if saveErr := l.Store.Save(&manifest); saveErr != nil {
				return errors.Join(secretErr, fmt.Errorf("save failure artifact report: %w", saveErr))
			}
		}
		if collectErr != nil {
			return errors.Join(secretErr, collectErr)
		}
	} else if l.Collector.StateDir != "" {
		sources := l.Sources
		if sources == nil && l.Namespace.Dynamic != nil {
			sources = collector.KubernetesSources(l.Namespace.Dynamic, namespace)
		}
		if _, err := l.Collector.CollectAndVerify(cleanupCtx, manifest.RunID, manifest, sources); err != nil {
			return errors.Join(secretErr, err)
		}
	} else if l.Prompt != nil {
		return errors.Join(secretErr, fmt.Errorf("state directory is required to verify saved artifacts before cleanup"))
	}
	if l.Prompt == nil {
		return secretErr
	}
	approved, err := l.Prompt.Confirm(ctx, fmt.Sprintf("Delete owned tenant namespace %s from run %s", namespace, manifest.RunID))
	if errors.Is(err, ErrPromptUnavailable) {
		l.notice("retained tenant namespace %s: cleanup prompt unavailable (%v)", namespace, err)
		return secretErr
	}
	if err != nil {
		return errors.Join(secretErr, err)
	}
	if !approved {
		l.notice("retained tenant namespace %s", namespace)
		return secretErr
	}
	uid, err := l.Namespace.Delete(ctx, namespace, manifest.RunID)
	if err != nil {
		return errors.Join(secretErr, err)
	}
	return errors.Join(secretErr, waitForNamespaceDeletion(ctx, l.Namespace, namespace, uid))
}

// deleteGitLabCredentialSecret removes the run-scoped GitLab SCM Secret after
// confirming the harness owns it for runID. A missing Secret counts as already
// cleaned up. Deletion is refused when the ownership labels do not match, and
// otherwise carries the Secret's UID as a precondition so a replacement is not
// deleted.
func deleteGitLabCredentialSecret(ctx context.Context, client dynamic.Interface, namespace, runID string) error {
	if client == nil {
		return fmt.Errorf("dynamic client is required to delete the ephemeral GitLab SCM Secret")
	}
	secrets := client.Resource(cluster.SecretGVR).Namespace(namespace)
	secret, err := secrets.Get(ctx, cluster.GitLabCredentialSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get ephemeral GitLab SCM Secret: %w", err)
	}
	labels := secret.GetLabels()
	if labels[cleanup.ManagedByLabel] != cleanup.ManagedByValue || labels[cleanup.RunIDLabel] != runID {
		return fmt.Errorf("refusing to delete GitLab SCM Secret %q in namespace %s: not owned by run %s (%s=%q, %s=%q)", cluster.GitLabCredentialSecretName, namespace, runID, cleanup.ManagedByLabel, labels[cleanup.ManagedByLabel], cleanup.RunIDLabel, labels[cleanup.RunIDLabel])
	}
	options := metav1.DeleteOptions{}
	if uid := secret.GetUID(); uid != "" {
		options.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	if err := secrets.Delete(ctx, cluster.GitLabCredentialSecretName, options); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ephemeral GitLab SCM Secret: %w", err)
	}
	return nil
}

// notice writes one retention decision to the operator and the run log.
func (l Lifecycle) notice(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if l.Out != nil {
		fmt.Fprintln(l.Out, line)
	}
	if l.Log != nil {
		fmt.Fprintln(l.Log, line)
	}
}

// outcomeFromManifest reports the run identity and the stage the run ended on.
// A recorded failure phase takes precedence over the manifest's final phase; a
// deferral reports the phase whose step was waiting.
func outcomeFromManifest(provider string, manifest model.RunManifest) RunOutcome {
	stage := manifest.Phase
	reason := ""
	deferred := manifest.Deferred != nil
	if manifest.Failure != nil {
		stage = manifest.Failure.Phase
		deferred = false
	} else if manifest.Deferred != nil {
		stage = manifest.Deferred.Phase
		reason = manifest.Deferred.Message
	}
	return RunOutcome{Provider: provider, RunID: manifest.RunID, ArtifactDirectory: manifest.ArtifactDirectory, Stage: stage, Deferred: deferred, DeferredReason: reason}
}

// runErrorWithLogFailure joins a workflow error with a provider run-log attachment failure.
func runErrorWithLogFailure(runErr, attachErr error) error {
	if attachErr == nil {
		return runErr
	}
	return errors.Join(runErr, fmt.Errorf("attach run log: %w", attachErr))
}

// revokeGitLabToken revokes a run-acquired GitLab personal access token.
// RunClusters calls it once, after every cluster has finished using the token.
var revokeGitLabToken = func(ctx context.Context, token, apiURL string) error {
	adapter, err := gitlab.NewFromCredentials(token, apiURL)
	if err != nil {
		return err
	}
	return adapter.RevokeToken(ctx)
}

// finalizeDeferredRun deletes the deferred attempt's owned tenant namespace
// without prompting, so its Pending PipelineRuns stop occupying Kueue queue
// positions. It performs no artifact collection and does not touch an unowned or
// replaced namespace. Deletion is bounded by finalizationTimeout.
func (runner LiveRunner) finalizeDeferredRun(ctx context.Context, stage *workflow.LiveStage, manifest model.RunManifest, log io.Writer) error {
	namespace := manifest.Fixture.TenantNamespace
	if namespace == "" {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()
	info, err := stage.Namespace.Inspect(cleanupCtx, namespace)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect deferred tenant namespace %s: %w", namespace, err)
	}
	if info.Name != namespace || !info.Owned() || info.RunID != manifest.RunID {
		return fmt.Errorf("refusing to delete tenant namespace %s: not owned by deferred run %s", namespace, manifest.RunID)
	}
	// DeleteVerified re-reads the namespace and sends UID/resourceVersion
	// preconditions before any finalizer stripping; WaitDeleted then verifies
	// the UID again before each strip. A replacement object is never modified.
	uid, err := stage.Namespace.DeleteVerified(cleanupCtx, info)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := waitForNamespaceDeletion(cleanupCtx, stage.Namespace, namespace, uid); err != nil {
		return err
	}
	if log != nil {
		fmt.Fprintf(log, "deleted tenant namespace %s from deferred run %s\n", namespace, manifest.RunID)
	}
	return nil
}

// finalizeRun collects artifacts and finalizes the tenant namespace for one
// completed or failed run. It derives collector sources from the live cluster,
// including pod logs since manifest.CreatedAt from the tenant and default log
// namespaces, and reuses the stage's prompt so the confirmation policy matches
// the run.
func (runner LiveRunner) finalizeRun(ctx context.Context, request config.Request, clients *cluster.ClientSet, stage *workflow.LiveStage, store evidence.ManifestStore, manifest model.RunManifest, runErr error, log io.Writer) error {
	sources := collector.KubernetesSources(clients.Dynamic, manifest.Fixture.TenantNamespace)
	sources = append(sources, collector.KubernetesPodLogSources(clients.Kubernetes, manifest.Fixture.TenantNamespace, manifest.CreatedAt)...)
	lifecycle := Lifecycle{Store: store, Namespace: stage.Namespace, Collector: collector.Collector{StateDir: request.StateDir}, Prompt: stage.Prompt, Sources: sources, Out: stage.Out, Log: log}
	return lifecycle.Finalize(ctx, manifest, runErr)
}

// providerFor returns the provider client selected by request.Provider and
// request credentials, or an error when the provider is not GitHub or GitLab.
func providerFor(request config.Request) (providers.Provider, error) {
	switch request.Provider {
	case config.ProviderGitHub:
		return github.NewFromCredentials(request.Credentials.GitHubToken)
	case config.ProviderGitLab:
		return gitlab.NewFromCredentials(request.Credentials.GitLabToken, request.Credentials.GitLabAPIURL)
	default:
		return nil, fmt.Errorf("unsupported provider %q", request.Provider)
	}
}
