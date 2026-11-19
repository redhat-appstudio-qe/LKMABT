package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/cleanup"
	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	"github.com/redhat-appstudio/lkmabt/internal/collector"
	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/platform"
	"github.com/redhat-appstudio/lkmabt/internal/providers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var PipelineRunGVR = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}

// githubAppPollInterval bounds how often an interactive preflight rechecks a pending App installation.
var githubAppPollInterval = 10 * time.Second

// githubAppStatusInterval bounds how often the wait reports progress to the user.
var githubAppStatusInterval = 5 * time.Minute

// buildStatusInterval bounds how often a build wait reports progress to the user.
var buildStatusInterval = time.Minute

type LiveStage struct {
	Request              config.Request
	Clients              *cluster.ClientSet
	RealCluster          *cluster.RealCluster
	PreflightCluster     cluster.Cluster
	Provider             providers.Provider
	Source               providers.SourceRepository
	Fixture              providers.FixtureRepository
	Prompt               Confirmation
	Namespace            cleanup.NamespaceService
	FailedBuildCollector *collector.FailedBuildCollector
	Interactive          bool
	Out                  io.Writer
	NoticePrefix         string
	Progress             func(string)
	OpenURL              func(string) error
	// ParentContext is the run context without the preflight deadline. An
	// interactive installation wait uses it to continue after that deadline.
	ParentContext context.Context
	Log           io.Writer
}

// NewLiveStage parses the source repository and wires clients for the live workflow stages.
func NewLiveStage(request config.Request, clients *cluster.ClientSet, provider providers.Provider) (*LiveStage, error) {
	source, err := providers.ParseRepository(request.SourceRepository)
	if err != nil {
		return nil, err
	}
	if clients == nil || provider == nil {
		return nil, fmt.Errorf("cluster clients and provider are required")
	}
	stage := &LiveStage{
		Request:              request,
		Clients:              clients,
		RealCluster:          &cluster.RealCluster{Clients: clients},
		Provider:             provider,
		Source:               source,
		Fixture:              providers.FixtureRepository{Owner: source.Owner, Name: source.Name, URL: source.URL},
		Namespace:            cleanup.NamespaceService{Dynamic: clients.Dynamic, NameLister: cleanup.OCNamespaceNameLister{Kubeconfig: request.Kubeconfig}},
		FailedBuildCollector: &collector.FailedBuildCollector{Dynamic: clients.Dynamic, Kubernetes: clients.Kubernetes},
		OpenURL:              OpenBrowser,
		Log:                  io.Discard,
	}
	return stage, nil
}

// Preflight validates the cluster, required APIs, platform scheduling, and tenant namespace.
func (s *LiveStage) Preflight(ctx context.Context, manifest *model.RunManifest) error {
	spec := cluster.PreflightSpec{ExpectedServer: s.Request.ClusterServer, AccessChecks: s.accessChecks(), Scheduling: cluster.SchedulingSpec{
		Namespace:  cluster.MultiPlatformControllerNamespace,
		Deployment: cluster.MultiPlatformControllerDeployment,
		ConfigMap:  cluster.MultiPlatformControllerConfigMap,
		Platforms:  s.platforms(),
	}}
	preflightCluster := s.PreflightCluster
	if preflightCluster == nil {
		preflightCluster = s.RealCluster
	}
	_, err := cluster.RunPreflight(ctx, preflightCluster, spec)
	if err != nil {
		return err
	}
	if s.Request.Provider == config.ProviderGitHub {
		verification, err := s.verifyGitHubApp(ctx)
		if err != nil {
			return err
		}
		// An approved installation wait outlives the step deadline; continue
		// with the live run context it reports so namespace discovery is not
		// rejected by an already-expired context.
		ctx = verification.Context
		if verification.Verified {
			s.notice("GitHub App %s installation verified", verification.Slug)
		}
	}
	if err := s.ensureTenantNamespaceAvailable(ctx, manifest); err != nil {
		return err
	}
	manifest.TargetClusterServer = s.Request.ClusterServer
	return nil
}

// platforms returns the requested platforms, defaulting to every supported
// platform for requests built without config.Parse.
func (s *LiveStage) platforms() []string {
	if len(s.Request.Platforms) == 0 {
		return platform.All()
	}
	return s.Request.Platforms
}

// accessChecks lists the API versions preflight must verify.
func (s *LiveStage) accessChecks() []cluster.AccessCheck {
	return []cluster.AccessCheck{
		{Name: "pipelineruns", Resource: PipelineRunGVR, Namespace: s.Request.TenantNamespace},
		{Name: "taskruns", Resource: cluster.TaskRunGVR, Namespace: s.Request.TenantNamespace},
		{Name: "applications", Resource: cluster.ApplicationGVR, Namespace: s.Request.TenantNamespace},
		{Name: "components", Resource: cluster.ComponentGVR, Namespace: s.Request.TenantNamespace},
	}
}

// githubAppVerification reports the context remaining valid for preflight and
// whether the installed App was actually verified.
type githubAppVerification struct {
	Context  context.Context
	Slug     string
	Verified bool
}

// verifyGitHubApp rejects clusters whose published GitHub App cannot onboard the fixture repository.
// An interactive run opens the installation page and waits for an organization administrator to approve it.
func (s *LiveStage) verifyGitHubApp(ctx context.Context) (githubAppVerification, error) {
	result := githubAppVerification{Context: ctx}
	if s.Clients == nil {
		return result, fmt.Errorf("cluster clients are not initialized")
	}
	checker, ok := s.Provider.(cluster.GitHubAppChecker)
	if !ok {
		return result, fmt.Errorf("github provider does not support GitHub App verification")
	}
	slug, err := cluster.VerifyGitHubAppOnboarding(ctx, s.Clients.Kubernetes, checker)
	if err == nil {
		result.Slug, result.Verified = slug, true
		return result, nil
	}
	var required *providers.AppInstallationRequiredError
	switch {
	case s.Interactive && errors.As(err, &required):
		wait, waitErr := s.waitForGitHubAppInstallation(ctx, checker, required)
		if waitErr != nil {
			return result, waitErr
		}
		wait.Slug = required.AppSlug
		return wait, nil
	case errors.Is(err, providers.ErrUnverifiable):
		s.notice("warning: %v", err)
		return result, nil
	default:
		return result, err
	}
}

// waitForGitHubAppInstallation opens the App installation page and polls until an administrator installs it.
func (s *LiveStage) waitForGitHubAppInstallation(ctx context.Context, checker cluster.GitHubAppChecker, required *providers.AppInstallationRequiredError) (githubAppVerification, error) {
	result := githubAppVerification{Context: ctx}
	s.notice("GitHub App %s is not installed for organization %s.", required.AppSlug, required.Owner)
	s.notice("LKMABT has already acquired its GitHub API token; the browser page only approves the organization installation.")
	s.notice("LKMABT is opening the installation page in your browser:")
	s.notice("  %s", required.InstallURL)
	s.notice("If it does not open, use that URL manually.")
	s.notice("To complete the installation:")
	s.notice("  1. Approve it in a browser session signed in as an organization owner of %s.", required.Owner)
	s.notice("  2. Select the %s organization.", required.Owner)
	s.notice("  3. Grant access to %s, or to all repositories.", required.Repository)
	s.notice("  4. Return to this terminal; LKMABT checks every %s and continues automatically.", githubAppPollInterval)
	open := s.OpenURL
	if open == nil {
		open = OpenBrowser
	}
	if err := open(required.InstallURL); err != nil {
		s.notice("Could not open a browser: %v", err)
		s.notice("Open the URL above manually.")
	}
	verified, err := s.pollGitHubAppInstallation(ctx, checker, required)
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) || !s.Interactive || s.Prompt == nil {
			return result, err
		}
		// The preflight deadline belongs to the step context; keep waiting on the
		// run context so Ctrl-C still ends the wait.
		waitContext := s.ParentContext
		if waitContext == nil || waitContext.Err() != nil {
			return result, err
		}
		keepWaiting, promptErr := s.Prompt.Confirm(waitContext, fmt.Sprintf("GitHub App %s is still not installed. Keep waiting?", required.AppSlug))
		if promptErr != nil {
			return result, errors.Join(err, fmt.Errorf("confirm keeping the installation wait: %w", promptErr))
		}
		if !keepWaiting {
			return result, err
		}
		s.notice("continuing to wait for GitHub App %s installation; press Ctrl-C to cancel", required.AppSlug)
		verified, err = s.pollGitHubAppInstallation(waitContext, checker, required)
		if err != nil {
			return result, err
		}
		result.Context = waitContext
	}
	result.Verified = verified
	return result, nil
}

// pollGitHubAppInstallation checks the installation every interval and retries
// check failures until the context ends. Checks run in the background so
// progress, including the latest check failure, is reported at most every
// status interval even while a check is pending.
func (s *LiveStage) pollGitHubAppInstallation(ctx context.Context, checker cluster.GitHubAppChecker, required *providers.AppInstallationRequiredError) (bool, error) {
	pollTicker := time.NewTicker(githubAppPollInterval)
	defer pollTicker.Stop()
	statusTicker := time.NewTicker(githubAppStatusInterval)
	defer statusTicker.Stop()
	started := time.Now()
	var lastCheckError string

	checkContext, cancelCheck := context.WithCancel(ctx)
	defer cancelCheck()
	results := make(chan error, 1)
	checking := false
	startCheck := func() {
		checking = true
		go func() {
			results <- checker.VerifyGitHubAppInstallation(checkContext, required.AppSlug)
		}()
	}
	waitError := func(err error) error {
		if lastCheckError != "" {
			return fmt.Errorf("%w: waiting for GitHub App %s installation in %s: %w (last check: %s)", required, required.AppSlug, required.Owner, err, lastCheckError)
		}
		return fmt.Errorf("%w: waiting for GitHub App %s installation in %s: %w", required, required.AppSlug, required.Owner, err)
	}
	startCheck()
	for {
		select {
		case <-ctx.Done():
			return false, waitError(ctx.Err())
		case <-statusTicker.C:
			if lastCheckError != "" {
				s.progress("Still waiting for GitHub App %s installation after %s; last check: %s", required.AppSlug, time.Since(started).Round(time.Second), lastCheckError)
				continue
			}
			s.progress("Still waiting for GitHub App %s installation after %s; install it from %s", required.AppSlug, time.Since(started).Round(time.Second), required.InstallURL)
		case <-pollTicker.C:
			if !checking {
				startCheck()
			}
		case err := <-results:
			checking = false
			if ctx.Err() != nil {
				return false, waitError(ctx.Err())
			}
			if err == nil {
				return true, nil
			}
			if errors.Is(err, providers.ErrUnverifiable) {
				s.notice("warning: %v", err)
				return false, nil
			}
			var selection *providers.AppRepositorySelectionError
			if errors.As(err, &selection) {
				return false, err
			}
			var pending *providers.AppInstallationRequiredError
			if errors.As(err, &pending) {
				lastCheckError = ""
				continue
			}
			lastCheckError = err.Error()
		}
	}
}

// notice writes one durable human-facing line to the console and the run log.
func (s *LiveStage) notice(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if s.Out != nil {
		fmt.Fprintln(s.Out, s.NoticePrefix+line)
	}
	if s.Log != nil {
		fmt.Fprintln(s.Log, line)
	}
}

// progress reports a repeatable status heartbeat. The console updates the
// active step in a terminal and prints a plain line otherwise; the run log
// always receives the full line.
func (s *LiveStage) progress(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if s.Progress != nil {
		s.Progress(line)
	} else if s.Out != nil {
		fmt.Fprintln(s.Out, s.NoticePrefix+line)
	}
	if s.Log != nil {
		fmt.Fprintln(s.Log, line)
	}
}

// OpenBrowser starts the platform's default browser opener and returns without
// waiting for it to exit; xdg-open can live as long as the opened application.
func OpenBrowser(rawURL string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	command := exec.Command(name, rawURL)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

// EnsureFixture validates the provider fixture and records its tenant resources in the manifest.
func (s *LiveStage) EnsureFixture(ctx context.Context, manifest *model.RunManifest) error {
	fixture := s.Fixture
	if recorded := fixtureFromManifest(manifest.Fixture); recorded.Name != "" {
		fixture = recorded
	}
	var err error
	fixture, err = s.Provider.ValidateFixture(ctx, fixture)
	if err != nil {
		return err
	}
	s.Fixture = fixture
	components := []cluster.ComponentSpec{
		componentSpec("mathwizz-web-server", fixture.URL, s.Request.SourceBranch, s.Request.ComponentPaths[0]),
		componentSpec("mathwizz-history-worker", fixture.URL, s.Request.SourceBranch, s.Request.ComponentPaths[1]),
		componentSpec("mathwizz-frontend", fixture.URL, s.Request.SourceBranch, s.Request.ComponentPaths[2]),
	}
	namespace := manifest.Fixture.TenantNamespace
	if namespace == "" {
		namespace = s.Request.TenantNamespace
	}
	application := manifest.Fixture.Application
	if application == "" {
		application = s.Request.ApplicationName
	}
	manifest.Fixture.TenantNamespace = namespace
	manifest.Fixture.EphemeralGitLabAuth = s.Request.Credentials.GitLabTokenEphemeral
	result, err := (cluster.FixtureService{Dynamic: s.Clients.Dynamic, Kubernetes: s.Clients.Kubernetes}).EnsureFixture(ctx, cluster.FixtureSpec{
		Namespace:            namespace,
		RunID:                manifest.RunID,
		Application:          application,
		Provider:             s.Request.Provider,
		GitLabToken:          s.Request.Credentials.GitLabToken,
		GitLabTokenEphemeral: s.Request.Credentials.GitLabTokenEphemeral,
		Repository:           fixture.URL,
		Components:           components,
	})
	if err != nil {
		return err
	}
	manifest.Fixture = model.FixtureIdentity{
		TenantNamespace: result.Namespace,
		Application:     result.Application,
		Components:      result.Components,
		RepositoryOwner: fixture.Owner,
		RepositoryName:  fixture.Name,
		Repository:      fixture.URL,
		Branch:          s.Request.SourceBranch,

		EphemeralGitLabAuth: s.Request.Credentials.GitLabTokenEphemeral,
	}
	return nil
}

// ensureTenantNamespaceAvailable deletes owned stale tenant namespaces and asks before deleting unowned ones.
func (s *LiveStage) ensureTenantNamespaceAvailable(ctx context.Context, manifest *model.RunManifest) error {
	namespace := manifest.Fixture.TenantNamespace
	if namespace == "" {
		namespace = s.Request.TenantNamespace
	}
	scoped, err := s.Namespace.FindScoped(ctx, namespace)
	if err != nil {
		return fmt.Errorf("discover tenant namespace %s: %w", namespace, err)
	}
	for _, info := range scoped {
		if info.Owned() {
			if err := s.deleteOwnedNamespace(ctx, info); err != nil {
				return err
			}
			continue
		}
		if err := s.deleteUnownedNamespace(ctx, info); err != nil {
			return err
		}
	}
	return nil
}

// deleteOwnedNamespace removes a stale LKMABT namespace without prompting and reports the removal.
func (s *LiveStage) deleteOwnedNamespace(ctx context.Context, info cleanup.NamespaceInfo) error {
	s.notice("deleting stale owned tenant namespace %s from run %s", info.Name, info.RunID)
	uid, err := s.Namespace.Delete(ctx, info.Name, info.RunID)
	if err != nil {
		return err
	}
	return s.Namespace.WaitDeleted(ctx, info.Name, uid)
}

// deleteUnownedNamespace asks per namespace before deleting a scoped namespace that lacks exact ownership labels.
func (s *LiveStage) deleteUnownedNamespace(ctx context.Context, info cleanup.NamespaceInfo) error {
	if s.Prompt == nil {
		return fmt.Errorf("tenant namespace %s has absent or different LKMABT ownership labels and requires interactive approval before deletion", info.Name)
	}
	approved, err := s.Prompt.Confirm(ctx, fmt.Sprintf("Namespace %s has absent or different LKMABT ownership labels (run ID %q); delete it", info.Name, info.RunID))
	if err != nil {
		return fmt.Errorf("confirm deletion of tenant namespace %s: %w", info.Name, err)
	}
	if !approved {
		return fmt.Errorf("tenant namespace %s retained; preflight refused to continue", info.Name)
	}
	if err := s.Namespace.DeleteApproved(ctx, info); err != nil {
		return err
	}
	return s.Namespace.WaitDeleted(ctx, info.Name, info.UID)
}

// TriggerComponents waits for PaC onboarding, updates fixture files, and records trigger commits.
func (s *LiveStage) TriggerComponents(ctx context.Context, manifest *model.RunManifest) error {
	if s.Fixture.Name == "" {
		s.Fixture = fixtureFromManifest(manifest.Fixture)
	}
	if s.Fixture.Name == "" {
		return fmt.Errorf("run manifest does not contain a fixture repository")
	}
	paths := s.Request.ComponentPaths
	components := manifest.Fixture.Components
	if len(paths) != len(components) {
		return fmt.Errorf("fixture has %d components but %d Dockerfile paths", len(components), len(paths))
	}
	componentNames := make([]string, 0, len(components))
	for _, component := range components {
		componentNames = append(componentNames, component)
	}
	if err := waitForPaCEnabled(ctx, s.Request.Timeouts.Build, 10*time.Second, componentNames, func(ctx context.Context) ([]unstructured.Unstructured, error) {
		list, err := s.Clients.Dynamic.Resource(cluster.ComponentGVR).Namespace(manifest.Fixture.TenantNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list components while waiting for PaC: %w", err)
		}
		for index := range list.Items {
			if err := paCStatusError(&list.Items[index]); err != nil {
				return nil, err
			}
		}
		return list.Items, nil
	}, s.progress); err != nil {
		return fmt.Errorf("wait for PaC onboarding: %w", err)
	}
	prerequisites, err := s.configureBuildPlatforms(ctx, components)
	if err != nil {
		return err
	}
	for index, component := range components {
		commit, err := s.triggerComponent(ctx, manifest, component, paths[index])
		if err != nil {
			return err
		}
		trigger := model.TriggerCommit{Component: component, SHA: commit.SHA, URL: commit.URL, CreatedAt: commit.CreatedAt}
		if prerequisite := prerequisites[component]; prerequisite != "" {
			trigger.PrerequisiteSHAs = []string{prerequisite}
		}
		manifest.TriggerCommits = append(manifest.TriggerCommits, trigger)
	}
	return nil
}

// triggerComponent commits one Dockerfile comment with the SHA read immediately before it.
func (s *LiveStage) triggerComponent(ctx context.Context, manifest *model.RunManifest, component, path string) (providers.Commit, error) {
	file, err := s.Provider.ReadFile(ctx, s.Fixture, path, s.Request.SourceBranch)
	if err != nil {
		return providers.Commit{}, fmt.Errorf("read %s: %w", path, err)
	}
	comment := fmt.Sprintf(TriggerCommentFormat, manifest.RunID, time.Now().Unix())
	content := strings.TrimRight(file.Content, "\n") + "\n" + comment + "\n"
	commit, err := s.Provider.UpdateFile(ctx, s.Fixture, path, s.Request.SourceBranch, content, file.SHA, providers.CommitMessageTrigger)
	if err != nil {
		return providers.Commit{}, fmt.Errorf("trigger %s: %w", component, err)
	}
	return commit, nil
}

// retriggerCount returns how many automatic retriggers a component has had.
func retriggerCount(manifest *model.RunManifest, component string) int {
	for index := range manifest.TriggerCommits {
		if manifest.TriggerCommits[index].Component == component {
			return manifest.TriggerCommits[index].Retriggers
		}
	}
	return 0
}

// retriggerComponent commits a fresh Dockerfile trigger for one component and
// supersedes its previous trigger in the manifest.
func (s *LiveStage) retriggerComponent(ctx context.Context, manifest *model.RunManifest, component string) error {
	triggerIndex := -1
	for index := range manifest.TriggerCommits {
		if manifest.TriggerCommits[index].Component == component {
			triggerIndex = index
			break
		}
	}
	if triggerIndex < 0 {
		return fmt.Errorf("cannot retrigger %s: run manifest has no trigger commit", component)
	}
	componentIndex := -1
	for index, name := range manifest.Fixture.Components {
		if name == component {
			componentIndex = index
			break
		}
	}
	if componentIndex < 0 || componentIndex >= len(s.Request.ComponentPaths) {
		return fmt.Errorf("cannot retrigger %s: no Dockerfile path for the component", component)
	}
	previousSHA := manifest.TriggerCommits[triggerIndex].SHA
	commit, err := s.triggerComponent(ctx, manifest, component, s.Request.ComponentPaths[componentIndex])
	if err != nil {
		return err
	}
	trigger := &manifest.TriggerCommits[triggerIndex]
	trigger.SHA = commit.SHA
	trigger.URL = commit.URL
	trigger.CreatedAt = commit.CreatedAt
	trigger.Retriggers++
	if previousSHA != "" && previousSHA != commit.SHA && !slices.Contains(trigger.PrerequisiteSHAs, previousSHA) {
		trigger.PrerequisiteSHAs = append(trigger.PrerequisiteSHAs, previousSHA)
	}
	return nil
}

// fixtureFromManifest reconstructs a provider repository from saved fixture identity.
func fixtureFromManifest(identity model.FixtureIdentity) providers.FixtureRepository {
	return providers.FixtureRepository{
		Owner: identity.RepositoryOwner,
		Name:  identity.RepositoryName,
		URL:   identity.Repository,
	}
}

// maxPacRetriggersPerComponent bounds automatic retriggers for a PipelineRun PaC never reconciled.
const maxPacRetriggersPerComponent = 1

// VerifyBuilds waits for successful Buildah TaskRuns and records their PipelineRun identities.
func (s *LiveStage) VerifyBuilds(ctx context.Context, manifest *model.RunManifest) error {
	namespace := manifest.Fixture.TenantNamespace
	if namespace == "" {
		return fmt.Errorf("run manifest does not contain a tenant namespace")
	}
	inspector := cluster.PacAuthInspector{Kubernetes: s.Clients.Kubernetes}
	var identities []model.PipelineRunIdentity
	for {
		var err error
		identities, err = s.waitForBuilds(ctx, manifest, namespace, inspector)
		if err == nil {
			break
		}
		missing, retry := unreconciledPacRun(err)
		if !retry || s.Provider == nil || retriggerCount(manifest, missing.Component) >= maxPacRetriggersPerComponent {
			s.collectFailedBuildLogs(ctx, namespace, manifest, err)
			return err
		}
		s.notice("PaC did not reconcile PipelineRun %s for %s; retriggering it once", missing.PipelineRun, missing.Component)
		if retryErr := s.retriggerComponent(ctx, manifest, missing.Component); retryErr != nil {
			return errors.Join(err, retryErr)
		}
	}
	manifest.PipelineRuns = append(manifest.PipelineRuns, identities...)
	return nil
}

// waitForBuilds waits for one attempt at successful Buildah TaskRuns.
func (s *LiveStage) waitForBuilds(ctx context.Context, manifest *model.RunManifest, namespace string, inspector cluster.PacAuthInspector) ([]model.PipelineRunIdentity, error) {
	inspectionWarned := false
	return waitForBuildTaskRuns(ctx, s.Request.Timeouts.Trigger, s.Request.Timeouts.Build, 10*time.Second, namespace, manifest.TriggerCommits, func(ctx context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, error) {
		components, err := s.Clients.Dynamic.Resource(cluster.ComponentGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, nil, fmt.Errorf("list components while waiting for builds: %w", err)
		}
		for index := range components.Items {
			if err := paCStatusError(&components.Items[index]); err != nil {
				return nil, nil, err
			}
		}
		pipelineRuns, err := s.Clients.Dynamic.Resource(PipelineRunGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, nil, err
		}
		// A matched failed PipelineRun wins over a missing PaC Secret so the
		// failed-build log collection still runs.
		if failed := matchedFailedPipelineRun(pipelineRuns.Items, manifest.TriggerCommits); failed != nil {
			return nil, nil, failed
		}
		// Fail fast when PaC already removed the Secret a pending build needs; a
		// transient read failure only warns, because the build wait still works.
		missing, inspectErr := inspector.Missing(ctx, namespace, candidatePipelineRunsForCommits(pipelineRuns.Items, manifest.TriggerCommits))
		if inspectErr != nil {
			if !inspectionWarned {
				inspectionWarned = true
				s.notice("warning: cannot inspect PaC git authentication Secrets: %v", inspectErr)
			}
		} else if missing != nil {
			return nil, nil, missing
		}
		taskRuns, err := s.Clients.Dynamic.Resource(cluster.TaskRunGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, nil, err
		}
		return pipelineRuns.Items, taskRuns.Items, nil
	}, s.progress)
}

// unreconciledPacRun reports a missing PaC Secret for a PipelineRun PaC never reconciled.
func unreconciledPacRun(err error) (*cluster.MissingPacAuthSecretError, bool) {
	var missing *cluster.MissingPacAuthSecretError
	if errors.As(err, &missing) && !missing.Reconciled {
		return missing, true
	}
	return nil, false
}

// collectFailedBuildLogs appends logs for a failed PipelineRun.
func (s *LiveStage) collectFailedBuildLogs(ctx context.Context, namespace string, manifest *model.RunManifest, runErr error) {
	var pipelineRunError *PipelineRunFailedError
	if !errors.As(runErr, &pipelineRunError) || s.FailedBuildCollector == nil || manifest.ArtifactDirectory == "" {
		return
	}
	logs, err := s.FailedBuildCollector.CollectFailedBuildLogs(ctx, namespace, pipelineRunError.Name, path.Join(s.Request.StateDir, manifest.ArtifactDirectory))
	if err == nil {
		manifest.FailedBuildLogs = append(manifest.FailedBuildLogs, logs...)
	}
}

// paCStatusError returns an error when a component's PaC status reports onboarding failure.
func paCStatusError(object *unstructured.Unstructured) error {
	if object == nil {
		return nil
	}
	annotation := object.GetAnnotations()["build.appstudio.openshift.io/status"]
	if annotation == "" {
		return nil
	}
	var status struct {
		PaC struct {
			State        string `json:"state"`
			ErrorID      int    `json:"error-id"`
			ErrorMessage string `json:"error-message"`
		} `json:"pac"`
	}
	if err := json.Unmarshal([]byte(annotation), &status); err != nil || status.PaC.State != "error" {
		return nil
	}
	if status.PaC.ErrorMessage == "" {
		return fmt.Errorf("component %s PaC onboarding failed with error ID %d", object.GetName(), status.PaC.ErrorID)
	}
	return fmt.Errorf("component %s PaC onboarding failed: %s", object.GetName(), status.PaC.ErrorMessage)
}

// waitForPaCEnabled polls components until each named component reports enabled PaC status.
func waitForPaCEnabled(ctx context.Context, timeout, interval time.Duration, names []string, list func(context.Context) ([]unstructured.Unstructured, error), notice func(string, ...any)) error {
	if timeout <= 0 {
		return fmt.Errorf("PaC timeout must be positive")
	}
	if interval <= 0 {
		return fmt.Errorf("PaC polling interval must be positive")
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	heartbeat := time.NewTicker(buildStatusInterval)
	defer heartbeat.Stop()
	notify(notice, "waiting for PaC onboarding of %d components", len(names))
	for {
		objects, err := list(waitCtx)
		if err != nil {
			return err
		}
		byName := make(map[string]*unstructured.Unstructured, len(objects))
		for index := range objects {
			byName[objects[index].GetName()] = &objects[index]
		}
		pending := false
		for _, name := range names {
			if componentPaCState(byName[name]) != "enabled" {
				pending = true
			}
		}
		if !pending {
			return nil
		}
		if !awaitPoll(waitCtx, interval, heartbeat, func() { notify(notice, "%s", paCWaitStatus(names, objects)) }) {
			return fmt.Errorf("timed out waiting for PaC onboarding: %w", waitCtx.Err())
		}
	}
}

// componentSpec derives the build context and Dockerfile path for a component.
func componentSpec(name, repository, branch, componentPath string) cluster.ComponentSpec {
	return cluster.ComponentSpec{
		Name:       name,
		Repository: repository,
		Branch:     branch,
		Context:    path.Dir(componentPath),
		Dockerfile: path.Base(componentPath),
	}
}

// waitForBuildMatches polls PipelineRuns until each trigger has a successful matching run or times out.
func waitForBuildMatches(ctx context.Context, triggerTimeout, buildTimeout, interval time.Duration, namespace string, commits []model.TriggerCommit, list func(context.Context) ([]unstructured.Unstructured, error)) ([]model.PipelineRunIdentity, error) {
	if triggerTimeout <= 0 {
		return nil, fmt.Errorf("trigger timeout must be positive")
	}
	if buildTimeout <= 0 {
		return nil, fmt.Errorf("build timeout must be positive")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("build polling interval must be positive")
	}
	triggerCtx, cancelTrigger := context.WithTimeout(ctx, triggerTimeout)
	defer cancelTrigger()
	for {
		objects, err := list(triggerCtx)
		if err != nil {
			return nil, err
		}
		identities, pending, err := matchBuildsObjects(objects, commits)
		if err != nil {
			return nil, err
		}
		if !pending {
			return identities, nil
		}
		if allBuildsAppeared(objects, commits) {
			break
		}
		timer := time.NewTimer(interval)
		select {
		case <-triggerCtx.Done():
			timer.Stop()
			return nil, noPipelineRunError(namespace, commits, objects)
		case <-timer.C:
		}
	}

	buildCtx, cancelBuild := context.WithTimeout(ctx, buildTimeout)
	defer cancelBuild()
	for {
		objects, err := list(buildCtx)
		if err != nil {
			return nil, err
		}
		identities, pending, err := matchBuildsObjects(objects, commits)
		if err != nil {
			return nil, err
		}
		if !pending {
			return identities, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-buildCtx.Done():
			timer.Stop()
			return nil, fmt.Errorf("timed out waiting for PipelineRuns after trigger: %w", buildCtx.Err())
		case <-timer.C:
		}
	}
}

// waitForBuildTaskRuns polls for matching PipelineRuns and successful Buildah TaskRuns.
func waitForBuildTaskRuns(ctx context.Context, triggerTimeout, buildTimeout, interval time.Duration, namespace string, commits []model.TriggerCommit, list func(context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, error), notice func(string, ...any)) ([]model.PipelineRunIdentity, error) {
	if triggerTimeout <= 0 {
		return nil, fmt.Errorf("trigger timeout must be positive")
	}
	if buildTimeout <= 0 {
		return nil, fmt.Errorf("build timeout must be positive")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("build polling interval must be positive")
	}
	notify(notice, "waiting for PipelineRuns for %d triggered components", len(commits))
	triggerCtx, cancelTrigger := context.WithTimeout(ctx, triggerTimeout)
	defer cancelTrigger()
	triggerHeartbeat := time.NewTicker(buildStatusInterval)
	defer triggerHeartbeat.Stop()
	for {
		pipelineRuns, taskRuns, err := list(triggerCtx)
		if err != nil {
			return nil, err
		}
		identities, pending, err := matchBuildTaskRuns(pipelineRuns, taskRuns, commits)
		if err != nil {
			return nil, err
		}
		if !pending {
			return identities, nil
		}
		if allBuildsAppeared(pipelineRuns, commits) {
			break
		}
		if !awaitPoll(triggerCtx, interval, triggerHeartbeat, func() {
			notify(notice, "%s", buildWaitStatus(commits, pipelineRuns, taskRuns, time.Now()))
		}) {
			return nil, noPipelineRunError(namespace, commits, pipelineRuns)
		}
	}

	notify(notice, "PipelineRuns appeared for all %d components; waiting for Buildah TaskRuns", len(commits))
	buildCtx, cancelBuild := context.WithTimeout(ctx, buildTimeout)
	defer cancelBuild()
	buildHeartbeat := time.NewTicker(buildStatusInterval)
	defer buildHeartbeat.Stop()
	for {
		pipelineRuns, taskRuns, err := list(buildCtx)
		if err != nil {
			return nil, err
		}
		identities, pending, err := matchBuildTaskRuns(pipelineRuns, taskRuns, commits)
		if err != nil {
			return nil, err
		}
		if !pending {
			return identities, nil
		}
		if !awaitPoll(buildCtx, interval, buildHeartbeat, func() {
			notify(notice, "%s", buildWaitStatus(commits, pipelineRuns, taskRuns, time.Now()))
		}) {
			return nil, fmt.Errorf("timed out waiting for Buildah TaskRuns after trigger: %w", buildCtx.Err())
		}
	}
}

// matchedFailedPipelineRun returns the first matched PipelineRun reporting
// failure, even when another run for the same commit is still pending, so
// callers can fail before other checks hide it.
func matchedFailedPipelineRun(pipelineRuns []unstructured.Unstructured, commits []model.TriggerCommit) *PipelineRunFailedError {
	for _, commit := range commits {
		for _, candidate := range matchingPipelineRuns(pipelineRuns, commit) {
			if pipelineRunConditionStatus(candidate) == "False" {
				return &PipelineRunFailedError{Namespace: candidate.GetNamespace(), Name: candidate.GetName(), Component: commit.Component}
			}
		}
	}
	return nil
}

// matchBuildTaskRuns matches each commit to one PipelineRun and its successful Buildah TaskRuns.
func matchBuildTaskRuns(pipelineRuns, taskRuns []unstructured.Unstructured, commits []model.TriggerCommit) ([]model.PipelineRunIdentity, bool, error) {
	identities := make([]model.PipelineRunIdentity, 0, len(commits))
	// Fail on any matched failed PipelineRun before reporting another component as
	// pending; otherwise a later component's failure can hide behind an earlier
	// component that never produces a PipelineRun.
	if failed := matchedFailedPipelineRun(pipelineRuns, commits); failed != nil {
		return nil, false, failed
	}
	for _, commit := range commits {
		candidates := matchingPipelineRuns(pipelineRuns, commit)
		if len(candidates) == 0 {
			return identities, true, nil
		}
		if len(candidates) > 1 {
			return nil, false, fmt.Errorf("ambiguous PipelineRuns for component %s after trigger %s", commit.Component, commit.SHA)
		}
		candidate := candidates[0]
		if pipelineRunConditionStatus(candidate) == "False" {
			return nil, false, &PipelineRunFailedError{Namespace: candidate.GetNamespace(), Name: candidate.GetName(), Component: commit.Component}
		}
		buildTasks := buildTaskRunsForPipelineRun(taskRuns, candidate)
		for _, taskRun := range buildTasks {
			if pipelineRunConditionStatus(taskRun) == "False" {
				return nil, false, &PipelineRunFailedError{Namespace: candidate.GetNamespace(), Name: candidate.GetName(), Component: commit.Component}
			}
		}
		// Successful Buildah TaskRuns are only evidence after the PipelineRun
		// itself succeeds; later tasks can still be running or fail.
		if pipelineRunConditionStatus(candidate) != "True" || len(buildTasks) == 0 {
			return identities, true, nil
		}
		for _, taskRun := range buildTasks {
			if pipelineRunConditionStatus(taskRun) != "True" {
				return identities, true, nil
			}
		}
		identity := identityFromObject(candidate, commit.Component, pipelineRunSHA(candidate))
		identity.Succeeded = true
		identities = append(identities, identity)
	}
	return identities, false, nil
}

// buildTaskRunsForPipelineRun selects Buildah TaskRuns owned by or labeled for a PipelineRun.
func buildTaskRunsForPipelineRun(taskRuns []unstructured.Unstructured, pipelineRun *unstructured.Unstructured) []*unstructured.Unstructured {
	result := make([]*unstructured.Unstructured, 0)
	for index := range taskRuns {
		taskRun := &taskRuns[index]
		owned := cluster.OwnedByPipelineRun(taskRun, string(pipelineRun.GetUID()), pipelineRun.GetName())
		if !owned && taskRun.GetLabels()["tekton.dev/pipelineRun"] == pipelineRun.GetName() {
			owned = true
		}
		if !cluster.IsBuildTaskRun(taskRun) || !owned {
			continue
		}
		result = append(result, taskRun)
	}
	return result
}

// matchBuildsObjects matches commits against a supplied PipelineRun list.
func matchBuildsObjects(objects []unstructured.Unstructured, commits []model.TriggerCommit) ([]model.PipelineRunIdentity, bool, error) {
	return matchBuildsWithObjects(objects, commits)
}

// matchBuilds lists PipelineRuns and matches them to trigger commits.
func matchBuilds(ctx context.Context, commits []model.TriggerCommit, list func(context.Context) ([]unstructured.Unstructured, error)) ([]model.PipelineRunIdentity, bool, error) {
	objects, err := list(ctx)
	if err != nil {
		return nil, false, err
	}
	return matchBuildsWithObjects(objects, commits)
}

// matchBuildsWithObjects evaluates exact or post-trigger PipelineRun matches for each commit.
func matchBuildsWithObjects(objects []unstructured.Unstructured, commits []model.TriggerCommit) ([]model.PipelineRunIdentity, bool, error) {
	identities := make([]model.PipelineRunIdentity, 0, len(commits))
	for _, commit := range commits {
		candidates := matchingPipelineRuns(objects, commit)
		if len(candidates) == 0 {
			return identities, true, nil
		}
		if len(candidates) > 1 && !hasExactSHA(candidates, commit.SHA) {
			return nil, false, fmt.Errorf("ambiguous PipelineRuns for component %s after trigger %s", commit.Component, commit.SHA)
		}
		pending := false
		succeededCount := 0
		var firstSucceeded *unstructured.Unstructured
		for _, candidate := range candidates {
			switch status := pipelineRunConditionStatus(candidate); status {
			case "True":
				succeededCount++
				if firstSucceeded == nil {
					firstSucceeded = candidate
				}
			case "False":
				return nil, false, &PipelineRunFailedError{Namespace: candidate.GetNamespace(), Name: candidate.GetName(), Component: commit.Component}
			default:
				pending = true
			}
		}
		if succeededCount > 1 {
			return nil, false, fmt.Errorf("PipelineRun overshoot for component %s: %d succeeded for commit %s, expected exactly 1", commit.Component, succeededCount, commit.SHA)
		}
		if succeededCount == 1 && firstSucceeded != nil {
			identities = append(identities, identityFromObject(firstSucceeded, commit.Component, pipelineRunSHA(firstSucceeded)))
			continue
		}
		if pending {
			return identities, true, nil
		}
	}
	return identities, false, nil
}

// matchingPipelineRuns selects exact-SHA runs or eligible post-trigger runs for a component.
func matchingPipelineRuns(objects []unstructured.Unstructured, commit model.TriggerCommit) []*unstructured.Unstructured {
	exact := make([]*unstructured.Unstructured, 0)
	fallback := make([]*unstructured.Unstructured, 0)
	for index := range objects {
		object := &objects[index]
		labels := object.GetLabels()
		if labels["appstudio.openshift.io/component"] != commit.Component {
			continue
		}
		if labels["pipelinesascode.tekton.dev/sha"] == commit.SHA {
			exact = append(exact, object)
		} else if sha := labels["pipelinesascode.tekton.dev/sha"]; sha != "" && afterTrigger(object, commit) && !slices.Contains(commit.PrerequisiteSHAs, sha) {
			fallback = append(fallback, object)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return fallback
}

// hasExactSHA reports whether any PipelineRun carries the requested source SHA.
func hasExactSHA(objects []*unstructured.Unstructured, sha string) bool {
	for _, object := range objects {
		if pipelineRunSHA(object) == sha {
			return true
		}
	}
	return false
}

// pipelineRunSHA returns the PipelineRun source SHA label.
func pipelineRunSHA(object *unstructured.Unstructured) string {
	return object.GetLabels()["pipelinesascode.tekton.dev/sha"]
}

// afterTrigger reports whether a PipelineRun was created after its trigger commit.
func afterTrigger(object *unstructured.Unstructured, commit model.TriggerCommit) bool {
	return commit.CreatedAt.IsZero() || object.GetCreationTimestamp().Time.After(commit.CreatedAt)
}

// allBuildsAppeared reports whether each trigger has at least one matching PipelineRun.
func allBuildsAppeared(objects []unstructured.Unstructured, commits []model.TriggerCommit) bool {
	for _, commit := range commits {
		if len(matchingPipelineRuns(objects, commit)) == 0 {
			return false
		}
	}
	return true
}

// noPipelineRunError describes the first trigger with no matching PipelineRun and observed runs.
func noPipelineRunError(namespace string, commits []model.TriggerCommit, objects []unstructured.Unstructured) error {
	for _, commit := range commits {
		if len(matchingPipelineRuns(objects, commit)) == 0 {
			names := make([]string, 0, len(objects))
			for index := range objects {
				if name := objects[index].GetName(); name != "" {
					names = append(names, name)
				}
			}
			return fmt.Errorf("no PipelineRun for component %s in namespace %s after trigger SHA %s; observed %d PipelineRuns (%s)", commit.Component, namespace, commit.SHA, len(objects), strings.Join(names, ", "))
		}
	}
	return fmt.Errorf("no expected PipelineRuns in namespace %s after trigger; observed %d PipelineRuns", namespace, len(objects))
}

// pipelineRunConditionStatus returns the Succeeded condition status or Unknown when absent.
func pipelineRunConditionStatus(object *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok || condition["type"] != "Succeeded" {
			continue
		}
		status, _ := condition["status"].(string)
		return status
	}
	return "Unknown"
}

// countBaselinePRs counts PipelineRuns and those with a successful Succeeded condition.
func countBaselinePRs(objects []unstructured.Unstructured) (succeeded, total int) {
	total = len(objects)
	for index := range objects {
		if pipelineRunConditionStatus(&objects[index]) == "True" {
			succeeded++
		}
	}
	return succeeded, total
}

// allNewPipelineRunsTerminal reports whether enough new runs are terminal without enough successes.
func allNewPipelineRunsTerminal(objects []unstructured.Unstructured, baselineSucceeded, baselineTotal, baselineTerminal, expected int) bool {
	if expected == 0 || len(objects)-baselineTotal < expected {
		return false
	}
	succeeded, total := countBaselinePRs(objects)
	newTerminal := countTerminalPRs(objects) - baselineTerminal
	return newTerminal >= expected && succeeded-baselineSucceeded < expected && total-baselineTotal >= expected
}

// countTerminalPRs counts PipelineRuns whose Succeeded conditions are True or False.
func countTerminalPRs(objects []unstructured.Unstructured) int {
	terminal := 0
	for index := range objects {
		if status := pipelineRunConditionStatus(&objects[index]); status == "True" || status == "False" {
			terminal++
		}
	}
	return terminal
}

type PipelineRunFailedError struct {
	Namespace string
	Name      string
	Component string
}

// Error formats the failed PipelineRun's namespace and name.
func (e *PipelineRunFailedError) Error() string {
	return fmt.Sprintf("PipelineRun %s/%s failed", e.Namespace, e.Name)
}

// VerifyBuildOutputs checks recorded PipelineRuns and appends their output evidence to the manifest.
func (s *LiveStage) VerifyBuildOutputs(ctx context.Context, manifest *model.RunManifest) error {
	evidence, err := (cluster.BuildOutputInspector{Dynamic: s.Clients.Dynamic, Platforms: s.platforms()}).Verify(ctx, manifest.PipelineRuns)
	if err != nil {
		return err
	}
	manifest.BuildOutputs = append(manifest.BuildOutputs, evidence...)
	return nil
}

// identityFromObject builds a PipelineRun identity from metadata and its Succeeded condition.
func identityFromObject(object *unstructured.Unstructured, component, sourceSHA string) model.PipelineRunIdentity {
	identity := model.PipelineRunIdentity{Namespace: object.GetNamespace(), Name: object.GetName(), UID: object.GetUID(), Component: component, SourceSHA: sourceSHA, StartedAt: object.GetCreationTimestamp().Time}
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok || condition["type"] != "Succeeded" {
			continue
		}
		identity.Succeeded = condition["status"] == "True"
		if value, ok := condition["lastTransitionTime"].(string); ok {
			if parsed, err := time.Parse(time.RFC3339, value); err == nil {
				identity.CompletedAt = parsed
			}
		}
	}
	return identity
}
