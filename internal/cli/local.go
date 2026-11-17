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
)

// LiveRunner executes provider workflows and cleanup against live cluster services.
type LiveRunner struct {
	Prompt workflow.Confirmation
	Out    io.Writer
}

// NewLiveRunner returns a runner wired to live services, the supplied
// confirmation prompt, and the writer for human-facing notices.
func NewLiveRunner(prompt workflow.Confirmation, out io.Writer) Runner {
	return LiveRunner{Prompt: prompt, Out: out}
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
	stage.Prompt = runner.Prompt
	stage.Log = log
	store := evidence.NewManifestStore(request.StateDir)
	var logAttachErr error
	manifest, runErr := (workflow.Runner{Store: store, Stage: stage}).Run(ctx, workflow.Options{
		RunID:            request.RunID,
		Provider:         request.Provider,
		ClusterServer:    request.ClusterServer,
		StateDir:         request.StateDir,
		FixtureNamespace: request.TenantNamespace,
		Application:      request.ApplicationName,
		Timeouts:         workflow.PhaseTimeouts{Preflight: request.Timeouts.Preflight, Build: request.Timeouts.Build},
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
	if failure := (clusterauth.Ladder{Runner: clusterauth.ExecRunner{Log: os.Stdout}}).Authenticate(ctx, []clusterfile.Entry{entry})[entry.Name]; failure != nil {
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
	stateRoot, err := locateRunStateRoot(request.StateDir, entry.Name, request.RunID)
	if err != nil {
		return err
	}
	// Provider artifacts live under the located cluster root, so verification must resolve there.
	request.StateDir = stateRoot
	return cleanupRun(ctx, request, evidence.NewManifestStore(stateRoot), cleanup.NamespaceService{Dynamic: clients.Dynamic}, runner.Prompt, runner.Out)
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
	if err := namespaceService.Delete(ctx, namespace, request.RunID); err != nil {
		return err
	}
	if err := namespaceService.WaitDeleted(ctx, namespace); err != nil {
		return err
	}
	fmt.Printf("deleted tenant namespace %s from run %s\n", namespace, request.RunID)
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

// Finalize collects and verifies run artifacts, then prompts before optionally deleting the owned namespace.
func (l Lifecycle) Finalize(ctx context.Context, manifest model.RunManifest, runErr error) error {
	namespace := manifest.Fixture.TenantNamespace
	if namespace == "" {
		return nil
	}
	info, err := l.Namespace.Inspect(ctx, namespace)
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
	if runErr != nil {
		sources := l.Sources
		if sources == nil && l.Namespace.Dynamic != nil {
			sources = collector.KubernetesSources(l.Namespace.Dynamic, namespace, manifest.RunID)
		}
		report, collectErr := l.Collector.CollectAndVerify(ctx, manifest.RunID, manifest, sources)
		manifest.FailureArtifacts = &report
		if l.Store.Root != "" {
			if saveErr := l.Store.Save(&manifest); saveErr != nil {
				return fmt.Errorf("save failure artifact report: %w", saveErr)
			}
		}
		if collectErr != nil {
			return collectErr
		}
	} else if l.Collector.StateDir != "" {
		sources := l.Sources
		if sources == nil && l.Namespace.Dynamic != nil {
			sources = collector.KubernetesSources(l.Namespace.Dynamic, namespace, manifest.RunID)
		}
		if _, err := l.Collector.CollectAndVerify(ctx, manifest.RunID, manifest, sources); err != nil {
			return err
		}
	} else if l.Prompt != nil {
		return fmt.Errorf("state directory is required to verify saved artifacts before cleanup")
	}
	if l.Prompt == nil {
		return nil
	}
	approved, err := l.Prompt.Confirm(ctx, fmt.Sprintf("Delete owned tenant namespace %s from run %s", namespace, manifest.RunID))
	if err != nil {
		return err
	}
	if !approved {
		fmt.Fprintf(l.logWriter(), "retained tenant namespace %s\n", namespace)
		return nil
	}
	if err := l.Namespace.Delete(ctx, namespace, manifest.RunID); err != nil {
		return err
	}
	return l.Namespace.WaitDeleted(ctx, namespace)
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
