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
	Prompt      workflow.Confirmation
	Out         io.Writer
	Interactive bool
}

// NewLiveRunner returns a runner wired to live services, the supplied
// confirmation prompt, the writer for human-facing notices, and whether the
// run may prompt for input or open a browser.
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

// Run executes a provider workflow against the request's cluster, writing its output to log.
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
	observer := newPhaseObserver(runner.Out, request.Provider)
	var logAttachErr error
	manifest, runErr := (workflow.Runner{Store: store, Stage: stage}).Run(ctx, workflow.Options{
		RunID:            request.RunID,
		Provider:         request.Provider,
		ClusterServer:    request.ClusterServer,
		StateDir:         request.StateDir,
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

// withPrefix returns a runner whose console output carries the provider label.
// The per-provider run log keeps unlabeled lines.
func (runner LiveRunner) withPrefix(prefix string) Runner {
	runner.Out = &providerPrefixWriter{prefix: prefix, out: runner.Out}
	return runner
}

// Cleanup resolves the named cluster, locates its run, and prompts before deleting the tenant namespace.
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
	failure := (clusterauth.Ladder{Runner: clusterauth.ExecRunner{Log: os.Stdout}}).Authenticate(ctx, []clusterfile.Entry{entry})[entry.Name]
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
	stateRoot, err := locateRunStateRoot(request.StateDir, entry.Name, request.RunID)
	locateStep.End(err == nil)
	if err != nil {
		return err
	}
	// Provider artifacts live under the located cluster root, so verification must resolve there.
	request.StateDir = stateRoot
	return cleanupRun(ctx, request, evidence.NewManifestStore(stateRoot), cleanup.NamespaceService{Dynamic: clients.Dynamic}, promptFor(runner), runner.Out)
}

// locateRunStateRoot finds the single <stateDir>/<aggregate>/<cluster> directory holding runID.
func locateRunStateRoot(stateDir, clusterName, runID string) (string, error) {
	aggregates, err := os.ReadDir(stateDir)
	if err != nil {
		return "", fmt.Errorf("read state directory %s: %w", stateDir, err)
	}
	matches := []string{}
	for _, aggregate := range aggregates {
		if !aggregate.IsDir() || aggregate.Name() == "latest" || strings.HasPrefix(aggregate.Name(), ".") {
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

// cleanupRun verifies saved evidence and namespace ownership before prompting
// for deletion. The declined-cleanup retention notice goes to out.
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
	if err != nil {
		return err
	}
	if !approved {
		// The cleanup command writes its retention notice to the console writer supplied by the caller.
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

type Lifecycle struct {
	Store     evidence.ManifestStore
	Namespace cleanup.NamespaceService
	Collector collector.Collector
	Prompt    workflow.Confirmation
	Sources   []collector.Source
	Log       io.Writer
}

// finalizationTimeout bounds cleanup work after the workflow context is done.
const finalizationTimeout = 5 * time.Minute

// namespaceDeletionTimeout bounds waiting for an approved namespace deletion.
var namespaceDeletionTimeout = 5 * time.Minute

// waitForNamespaceDeletion bounds the deletion wait so a lingering finalizer or
// stuck API call fails with the namespace name instead of hanging the process.
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

// Finalize collects and verifies run artifacts, then prompts before optionally deleting the owned namespace.
// Inspection, credential deletion, and artifact collection use a fresh bounded context so a canceled
// run still removes the ephemeral GitLab Secret. The prompt and namespace deletion keep the run context,
// so a canceled run retains the namespace.
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
			sources = collector.KubernetesSources(l.Namespace.Dynamic, namespace, manifest.RunID)
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
			sources = collector.KubernetesSources(l.Namespace.Dynamic, namespace, manifest.RunID)
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
	if err != nil {
		return errors.Join(secretErr, err)
	}
	if !approved {
		fmt.Fprintf(l.logWriter(), "retained tenant namespace %s\n", namespace)
		return secretErr
	}
	uid, err := l.Namespace.Delete(ctx, namespace, manifest.RunID)
	if err != nil {
		return errors.Join(secretErr, err)
	}
	return errors.Join(secretErr, waitForNamespaceDeletion(ctx, l.Namespace, namespace, uid))
}

// deleteGitLabCredentialSecret removes the run-scoped GitLab SCM Secret after
// confirming the harness owns it for runID. A missing Secret is already cleaned up.
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

// logWriter returns the lifecycle log, or a discard writer when none is configured.
func (l Lifecycle) logWriter() io.Writer {
	if l.Log == nil {
		return io.Discard
	}
	return l.Log
}

// outcomeFromManifest reports the run identity and the stage the run ended on.
func outcomeFromManifest(provider string, manifest model.RunManifest) RunOutcome {
	stage := manifest.Phase
	if manifest.Failure != nil {
		stage = manifest.Failure.Phase
	}
	return RunOutcome{Provider: provider, RunID: manifest.RunID, ArtifactDirectory: manifest.ArtifactDirectory, Stage: stage}
}

// runErrorWithLogFailure joins a workflow error with a provider run-log attachment failure.
func runErrorWithLogFailure(runErr, attachErr error) error {
	if attachErr == nil {
		return runErr
	}
	return errors.Join(runErr, fmt.Errorf("attach run log: %w", attachErr))
}

// revokeGitLabToken revokes a run-acquired GitLab personal access token once,
// after every cluster has finished using it.
var revokeGitLabToken = func(ctx context.Context, token, apiURL string) error {
	adapter, err := gitlab.NewFromCredentials(token, apiURL)
	if err != nil {
		return err
	}
	return adapter.RevokeToken(ctx)
}

// finalizeRun collects artifacts and finalizes the tenant namespace for one completed or failed run.
func (runner LiveRunner) finalizeRun(ctx context.Context, request config.Request, clients *cluster.ClientSet, stage *workflow.LiveStage, store evidence.ManifestStore, manifest model.RunManifest, runErr error, log io.Writer) error {
	sources := collector.KubernetesSources(clients.Dynamic, manifest.Fixture.TenantNamespace, manifest.RunID)
	sources = append(sources, collector.KubernetesPodLogSources(clients.Kubernetes, manifest.Fixture.TenantNamespace, manifest.CreatedAt)...)
	lifecycle := Lifecycle{Store: store, Namespace: stage.Namespace, Collector: collector.Collector{StateDir: request.StateDir}, Prompt: stage.Prompt, Sources: sources, Log: log}
	return lifecycle.Finalize(ctx, manifest, runErr)
}

// providerFor constructs the provider client selected by request credentials.
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
