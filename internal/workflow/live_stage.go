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
	"k8s.io/apimachinery/pkg/types"
)

// PipelineRunGVR identifies Tekton PipelineRuns in the tenant namespace.
var PipelineRunGVR = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}

// githubAppPollInterval bounds how often an interactive preflight rechecks a pending App installation.
var githubAppPollInterval = 10 * time.Second

// githubAppStatusInterval bounds how often the wait reports progress to the user.
var githubAppStatusInterval = 5 * time.Minute

// buildStatusInterval bounds how often a build wait reports progress to the user.
var buildStatusInterval = time.Minute

// buildPollInterval bounds how often a build wait lists PipelineRuns and TaskRuns.
var buildPollInterval = 10 * time.Second

// buildEvidenceCache records build outputs already captured during a wait.
// Nil callbacks make every operation a no-op or miss.
type buildEvidenceCache struct {
	Captured func(component string) (model.PipelineRunIdentity, bool)
	Capture  func(model.PipelineRunIdentity, *unstructured.Unstructured, []unstructured.Unstructured) error
	// ExpectedPlatforms is the set of platforms one component's matrix must
	// cover before the component can be accepted. Empty disables the coverage
	// check for callers that do not know the platform selection.
	ExpectedPlatforms []string
	// AdmissionReason resolves the scheduler reason for an admission-pending
	// PipelineRun. It is optional and diagnostic; the PipelineRun condition is
	// the fallback.
	AdmissionReason func(*unstructured.Unstructured) string
}

// captured returns the evidence identity for a component when already captured.
func (c buildEvidenceCache) captured(component string) (model.PipelineRunIdentity, bool) {
	if c.Captured == nil {
		return model.PipelineRunIdentity{}, false
	}
	return c.Captured(component)
}

// isCaptured reports whether a component's build outputs are already captured.
func (c buildEvidenceCache) isCaptured(component string) bool {
	_, ok := c.captured(component)
	return ok
}

// capture records build evidence for a component from the poll that observed it.
func (c buildEvidenceCache) capture(identity model.PipelineRunIdentity, pipelineRun *unstructured.Unstructured, taskRuns []unstructured.Unstructured) error {
	if c.Capture == nil {
		return nil
	}
	return c.Capture(identity, pipelineRun, taskRuns)
}

// admissionReason resolves an optional scheduler reason for a pending run.
func (c buildEvidenceCache) admissionReason(pipelineRun *unstructured.Unstructured) string {
	if c.AdmissionReason == nil {
		return ""
	}
	return c.AdmissionReason(pipelineRun)
}

// PipelineRunDisappearedError reports a PipelineRun observed earlier in a wait
// that a later poll no longer lists, usually because PaC pruned it.
//
// PaC's max-keep-runs is a shredder on a schedule: a successful PipelineRun can
// vanish while a slower component is still building, which is why evidence is
// captured on the poll that observes the success.
type PipelineRunDisappearedError struct {
	Component string
	Namespace string
	Name      string
	UID       types.UID
}

// Error formats the disappeared PipelineRun and the likely pruning cause.
func (e *PipelineRunDisappearedError) Error() string {
	return fmt.Sprintf("PipelineRun for component %s was deleted while waiting (last observed %s/%s); check PaC max-keep-runs or other pruning", e.Component, e.Namespace, e.Name)
}

// LiveStage implements Stage against a live cluster and provider. Each stage
// mutates the manifest it is given, reporting durable lines through notice and
// repeatable heartbeats through progress. Out, Log, and Progress are optional;
// Prompts and the interactive GitHub App installation wait require Prompt and
// Interactive.
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
	// Interactive enables prompts and the GitHub App installation wait.
	Interactive  bool
	Out          io.Writer
	NoticePrefix string
	Progress     func(string)
	// OpenURL opens the GitHub App installation page; OpenBrowser is used when nil.
	OpenURL func(string) error
	// ParentContext is the run context without the preflight deadline. An
	// interactive installation wait uses it to continue after that deadline.
	ParentContext context.Context
	Log           io.Writer
}

// NewLiveStage parses the source repository and wires clients for the live workflow stages.
// It fails when the source repository cannot be parsed or clients or provider are nil.
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
// For GitHub it also verifies that the published App can onboard the fixture repository,
// waiting when an interactive run approves a pending installation. It records the target
// cluster in manifest and deletes stale tenant namespaces, so a partially executed
// preflight can already have removed cluster resources.
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
	result, err := cluster.RunPreflight(ctx, preflightCluster, spec)
	if err != nil {
		return err
	}
	s.limitToSupportedPlatforms(result)
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
// platform when the request lists none.
func (s *LiveStage) platforms() []string {
	if len(s.Request.Platforms) == 0 {
		return platform.All()
	}
	return s.Request.Platforms
}

// Platforms returns the effective selection after preflight narrowing.
func (s *LiveStage) Platforms() []string {
	return s.platforms()
}

// limitToSupportedPlatforms narrows this cluster's selection to the
// architectures its live host-config can schedule. An undeclared architecture is
// information, not a warning or failure: the run continues with the rest.
func (s *LiveStage) limitToSupportedPlatforms(result cluster.PreflightResult) {
	unsupported := result.Unsupported()
	if len(unsupported) == 0 {
		return
	}
	supported := result.Supported()
	s.Request.Platforms = supported
	label := "cluster"
	if name := strings.TrimSpace(s.Request.ClusterName); name != "" {
		label += " " + name
	}
	s.notice("%s does not declare %s in host-config; continuing with %s", label, strings.Join(unsupported, ", "), strings.Join(supported, ", "))
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
// ErrUnverifiable downgrades to a warning with Verified false; the returned Context is the caller's
// context unless an approved installation wait outlived it and continued on ParentContext.
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
// When the step deadline expires, an interactive run asks on ParentContext whether to keep waiting and,
// if approved, continues on ParentContext and returns it as the result Context.
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
// status interval even while a check is pending. It returns true when installed,
// false with no error when the App state is unverifiable, and fails immediately
// on a repository selection error; any in-flight check is canceled on return.
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
// Missing writers are skipped, so a stage can run headless.
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
// The manifest's recorded fixture wins over the stage's, so a resumed run keeps its repository.
// It requires at least three component paths, panicking otherwise, and replaces manifest.Fixture
// and s.Fixture.
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
// PaC onboarding is bounded by the build timeout. Each component gets one commit; prerequisite
// commits for platform configuration are recorded in the trigger's PrerequisiteSHAs so build
// matching ignores them. Triggers are appended to manifest as they succeed, leaving earlier
// commits recorded when a later component fails.
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

// triggerComponent appends a trigger comment to one Dockerfile and commits it
// with the file SHA read immediately before the update, which the provider
// rejects when the branch moved in between.
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
// supersedes its previous trigger in the manifest. The superseded SHA is added
// to PrerequisiteSHAs so the old PaC run can no longer match the new trigger.
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
// Build evidence is captured from the same polls that prove each build succeeded. LKMABT stops
// at the build stage: a PipelineRun's image-index, scans, and other post-build tasks are not a
// completion gate. A failed Buildah TaskRun, or a PipelineRun failure before every Buildah
// TaskRun completes, still fails the wait and collects failed build logs. A PipelineRun PaC never
// reconciled is retriggered once per component, restarting the wait with fresh trigger and build
// timeouts. Successful identities are merged into manifest.PipelineRuns.
func (s *LiveStage) VerifyBuilds(ctx context.Context, manifest *model.RunManifest) error {
	namespace := manifest.Fixture.TenantNamespace
	if namespace == "" {
		return fmt.Errorf("run manifest does not contain a tenant namespace")
	}
	pacInspector := cluster.PacAuthInspector{Kubernetes: s.Clients.Kubernetes}
	inspector := cluster.BuildOutputInspector{Dynamic: s.Clients.Dynamic, Platforms: s.platforms()}
	captured := make(map[string]model.PipelineRunIdentity)
	evidence := buildEvidenceCache{
		Captured: func(component string) (model.PipelineRunIdentity, bool) {
			identity, ok := captured[component]
			return identity, ok
		},
		ExpectedPlatforms: s.platforms(),
		AdmissionReason:   s.admissionDiagnostic(ctx, namespace),
		Capture: func(identity model.PipelineRunIdentity, pipelineRun *unstructured.Unstructured, taskRuns []unstructured.Unstructured) error {
			output, err := inspector.VerifyListed(identity, pipelineRun, taskRuns)
			if err != nil {
				return fmt.Errorf("capture build outputs for %s: %w", identity.Component, err)
			}
			manifest.BuildOutputs = setBuildOutputEvidence(manifest.BuildOutputs, output)
			captured[identity.Component] = output.PipelineRun
			return nil
		},
	}
	var identities []model.PipelineRunIdentity
	for {
		var err error
		identities, err = s.waitForBuilds(ctx, manifest, namespace, pacInspector, evidence)
		if err == nil {
			break
		}
		if IsDeferred(err) {
			// A deferral returns before the normal identity merge. Keep the
			// already captured successful builds in the durable manifest.
			manifest.PipelineRuns = mergePipelineRunIdentities(manifest.PipelineRuns, buildOutputIdentities(manifest.BuildOutputs))
		}
		missing, retry := unreconciledPacRun(err)
		if !retry || s.Provider == nil || retriggerCount(manifest, missing.Component) >= maxPacRetriggersPerComponent {
			s.collectFailedBuildLogs(ctx, namespace, manifest, err)
			return err
		}
		s.notice("PaC did not reconcile PipelineRun %s for %s; retriggering it once", missing.PipelineRun, missing.Component)
		// Stale evidence from the superseded attempt must not satisfy the retrigger.
		delete(captured, missing.Component)
		manifest.BuildOutputs = removeBuildOutputEvidence(manifest.BuildOutputs, missing.Component)
		if retryErr := s.retriggerComponent(ctx, manifest, missing.Component); retryErr != nil {
			return errors.Join(err, retryErr)
		}
	}
	manifest.PipelineRuns = mergePipelineRunIdentities(manifest.PipelineRuns, identities)
	return nil
}

// waitForBuilds waits for one attempt at successful Buildah TaskRuns.
// The check runs after each poll has matched and captured evidence, so a
// component onboarding failure or a missing PaC git auth Secret is reported
// only once another component's completed build is safely recorded. A failed
// Buildah TaskRun or a PipelineRun failure before its builds complete is
// returned by the match, ahead of any Secret diagnosis, so failed-build log
// collection still runs.
func (s *LiveStage) waitForBuilds(ctx context.Context, manifest *model.RunManifest, namespace string, inspector cluster.PacAuthInspector, evidence buildEvidenceCache) ([]model.PipelineRunIdentity, error) {
	inspectionWarned := false
	var componentErr error
	check := func(checkCtx context.Context, pipelineRuns, taskRuns []unstructured.Unstructured) error {
		// Report a component onboarding failure only after completed builds are
		// captured, so another component's evidence is never lost to it.
		if componentErr != nil {
			return componentErr
		}
		// Inspect the PaC Secret only after completed builds are captured, so a
		// pending run's missing Secret cannot preempt another component's evidence.
		// Admission-pending runs are left to the deferral threshold: while Kueue
		// holds the run, a missing Secret is not a reliable PaC diagnosis.
		candidates := candidatePipelineRunsForCommits(pipelineRuns, manifest.TriggerCommits, evidence.isCaptured)
		inspectable := make([]unstructured.Unstructured, 0, len(candidates))
		for index := range candidates {
			if admissionPending(&candidates[index], taskRuns) {
				continue
			}
			inspectable = append(inspectable, candidates[index])
		}
		missing, inspectErr := inspector.Missing(checkCtx, namespace, inspectable)
		if inspectErr != nil {
			if !inspectionWarned {
				inspectionWarned = true
				s.notice("warning: cannot inspect PaC git authentication Secrets: %v", inspectErr)
			}
			return nil
		}
		if missing != nil {
			return missing
		}
		return nil
	}
	return waitForBuildTaskRuns(ctx, s.Request.Timeouts.Trigger, s.Request.Timeouts.Build, buildPollInterval, namespace, manifest.TriggerCommits, func(ctx context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, error) {
		componentErr = nil
		components, err := s.Clients.Dynamic.Resource(cluster.ComponentGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, nil, fmt.Errorf("list components while waiting for builds: %w", err)
		}
		for index := range components.Items {
			if err := paCStatusError(&components.Items[index]); err != nil {
				componentErr = err
				break
			}
		}
		pipelineRuns, err := s.Clients.Dynamic.Resource(PipelineRunGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, nil, err
		}
		taskRuns, err := s.Clients.Dynamic.Resource(cluster.TaskRunGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, nil, err
		}
		return pipelineRuns.Items, taskRuns.Items, nil
	}, s.progress, check, evidence)
}

// unreconciledPacRun reports a missing PaC Secret for a PipelineRun PaC never reconciled.
// Only that case is returned for retriggering: when Reconciled is true PaC already saw the
// run, so the caller treats the error as final instead of retriggering.
func unreconciledPacRun(err error) (*cluster.MissingPacAuthSecretError, bool) {
	var missing *cluster.MissingPacAuthSecretError
	if errors.As(err, &missing) && !missing.Reconciled {
		return missing, true
	}
	return nil, false
}

// collectFailedBuildLogs appends logs for a failed PipelineRun.
// It is a no-op unless runErr carries a PipelineRunFailedError, the collector is
// configured, and the manifest already has an artifact directory. Collection
// errors are discarded so they never mask the build failure that caused them.
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
// A missing, unparsable, or non-error status annotation is not a failure.
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
// A non-positive timeout or interval is rejected, list errors abort the wait, and the
// heartbeat reports pending components through notice at buildStatusInterval.
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
// componentPath is a repository-relative file path; its directory becomes the
// build context and its base name the Dockerfile.
func componentSpec(name, repository, branch, componentPath string) cluster.ComponentSpec {
	return cluster.ComponentSpec{
		Name:       name,
		Repository: repository,
		Branch:     branch,
		Context:    path.Dir(componentPath),
		Dockerfile: path.Base(componentPath),
	}
}

// waitForBuildTaskRuns polls for matching PipelineRuns and successful Buildah TaskRuns.
// It first waits up to triggerTimeout for every uncaptured commit to get a PipelineRun,
// then up to buildTimeout for each component's Buildah TaskRuns to succeed. Each poll
// matches, records observed runs, captures newly completed evidence, and only then runs
// check, so check errors never discard evidence from the same poll; a matched build
// failure returns before that capture step. A PipelineRun's post-build tasks are not a
// completion gate. It returns only when every commit has evidence, either captured
// earlier or proven here.
func waitForBuildTaskRuns(ctx context.Context, triggerTimeout, buildTimeout, interval time.Duration, namespace string, commits []model.TriggerCommit, list func(context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, error), notice func(string, ...any), check func(context.Context, []unstructured.Unstructured, []unstructured.Unstructured) error, evidence buildEvidenceCache) ([]model.PipelineRunIdentity, error) {
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
	observed := make(map[string]model.PipelineRunIdentity, len(commits))
	admission := newAdmissionTracker(admissionPendingThreshold, evidence.admissionReason)

	triggerCtx, cancelTrigger := context.WithTimeout(ctx, triggerTimeout)
	defer cancelTrigger()
	triggerHeartbeat := time.NewTicker(buildStatusInterval)
	defer triggerHeartbeat.Stop()
	for {
		pipelineRuns, taskRuns, err := list(triggerCtx)
		if err != nil {
			return nil, err
		}
		identities, pending, err := matchBuildTaskRunsWithExpected(pipelineRuns, taskRuns, commits, evidence.ExpectedPlatforms, evidence.isCaptured)
		if err != nil {
			return nil, err
		}
		if err := observeBuildPipelineRuns(commits, pipelineRuns, evidence, observed); err != nil {
			return nil, err
		}
		if err := captureCompletedBuilds(commits, pipelineRuns, taskRuns, identities, evidence); err != nil {
			return nil, err
		}
		if check != nil {
			if err := check(triggerCtx, pipelineRuns, taskRuns); err != nil {
				return nil, err
			}
		}
		if deferred := admission.deferred(namespace, time.Now(), commits, pipelineRuns, taskRuns, evidence.isCaptured); deferred != nil {
			return nil, deferred
		}
		if !pending {
			return assembleBuildIdentities(commits, identities, evidence)
		}
		if allBuildsAppeared(pipelineRuns, commits, evidence.isCaptured) {
			break
		}
		if !awaitPoll(triggerCtx, interval, triggerHeartbeat, func() {
			notify(notice, "%s", buildWaitStatus(commits, pipelineRuns, taskRuns, time.Now(), evidence.isCaptured, evidence.admissionReason, evidence.ExpectedPlatforms))
		}) {
			return nil, noPipelineRunError(namespace, commits, pipelineRuns, evidence.isCaptured)
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
		identities, pending, err := matchBuildTaskRunsWithExpected(pipelineRuns, taskRuns, commits, evidence.ExpectedPlatforms, evidence.isCaptured)
		if err != nil {
			return nil, err
		}
		if err := observeBuildPipelineRuns(commits, pipelineRuns, evidence, observed); err != nil {
			return nil, err
		}
		if err := captureCompletedBuilds(commits, pipelineRuns, taskRuns, identities, evidence); err != nil {
			return nil, err
		}
		if check != nil {
			if err := check(buildCtx, pipelineRuns, taskRuns); err != nil {
				return nil, err
			}
		}
		if deferred := admission.deferred(namespace, time.Now(), commits, pipelineRuns, taskRuns, evidence.isCaptured); deferred != nil {
			return nil, deferred
		}
		if !pending {
			return assembleBuildIdentities(commits, identities, evidence)
		}
		if !awaitPoll(buildCtx, interval, buildHeartbeat, func() {
			notify(notice, "%s", buildWaitStatus(commits, pipelineRuns, taskRuns, time.Now(), evidence.isCaptured, evidence.admissionReason, evidence.ExpectedPlatforms))
		}) {
			return nil, fmt.Errorf("timed out waiting for Buildah TaskRuns after trigger: %w", buildCtx.Err())
		}
	}
}

// observeBuildPipelineRuns records each uncaptured component's current
// PipelineRun and fails when an observed run disappears.
func observeBuildPipelineRuns(commits []model.TriggerCommit, pipelineRuns []unstructured.Unstructured, evidence buildEvidenceCache, observed map[string]model.PipelineRunIdentity) error {
	for _, commit := range commits {
		if evidence.isCaptured(commit.Component) {
			continue
		}
		candidates := matchingPipelineRuns(pipelineRuns, commit)
		switch len(candidates) {
		case 0:
			if previous, ok := observed[commit.Component]; ok {
				return &PipelineRunDisappearedError{Component: commit.Component, Namespace: previous.Namespace, Name: previous.Name, UID: previous.UID}
			}
		case 1:
			observed[commit.Component] = identityFromObject(candidates[0], commit.Component, pipelineRunSHA(candidates[0]))
		}
	}
	return nil
}

// captureCompletedBuilds captures evidence for newly completed components from
// the same poll objects that proved them successful. It requires exactly one
// matching PipelineRun per completed component and fails otherwise, so evidence
// is never attached to the wrong attempt.
func captureCompletedBuilds(commits []model.TriggerCommit, pipelineRuns, taskRuns []unstructured.Unstructured, completed []model.PipelineRunIdentity, evidence buildEvidenceCache) error {
	if evidence.Capture == nil {
		return nil
	}
	for _, identity := range completed {
		if evidence.isCaptured(identity.Component) {
			continue
		}
		commit, ok := triggerCommitForComponent(commits, identity.Component)
		if !ok {
			return fmt.Errorf("cannot capture build outputs for %s: no trigger commit", identity.Component)
		}
		candidates := matchingPipelineRuns(pipelineRuns, commit)
		if len(candidates) != 1 {
			return fmt.Errorf("cannot capture build outputs for %s: found %d matching PipelineRuns", identity.Component, len(candidates))
		}
		if err := evidence.capture(identity, candidates[0], taskRuns); err != nil {
			return err
		}
	}
	return nil
}

// triggerCommitForComponent returns a component's trigger commit.
func triggerCommitForComponent(commits []model.TriggerCommit, component string) (model.TriggerCommit, bool) {
	for _, commit := range commits {
		if commit.Component == component {
			return commit, true
		}
	}
	return model.TriggerCommit{}, false
}

// assembleBuildIdentities returns one identity per commit: captured evidence
// first, then the identity completed in this attempt.
func assembleBuildIdentities(commits []model.TriggerCommit, completed []model.PipelineRunIdentity, evidence buildEvidenceCache) ([]model.PipelineRunIdentity, error) {
	byComponent := make(map[string]model.PipelineRunIdentity, len(completed))
	for _, identity := range completed {
		byComponent[identity.Component] = identity
	}
	identities := make([]model.PipelineRunIdentity, 0, len(commits))
	for _, commit := range commits {
		if cached, ok := evidence.captured(commit.Component); ok {
			identities = append(identities, cached)
			continue
		}
		identity, ok := byComponent[commit.Component]
		if !ok {
			return nil, fmt.Errorf("component %s completed without build evidence", commit.Component)
		}
		identities = append(identities, identity)
	}
	return identities, nil
}

// matchBuildTaskRuns matches every commit to one PipelineRun and its
// successful Buildah TaskRuns. Captured components count as satisfied; any
// other commit without a successful build is pending. A component is proven
// when every Buildah TaskRun for its PipelineRun succeeds; the PipelineRun's
// post-build tasks are outside LKMABT's scope and are not a completion gate.
// It returns the components proven in this poll and whether another poll is
// still needed.
func matchBuildTaskRuns(pipelineRuns, taskRuns []unstructured.Unstructured, commits []model.TriggerCommit, captured func(string) bool) ([]model.PipelineRunIdentity, bool, error) {
	return matchBuildTaskRunsWithExpected(pipelineRuns, taskRuns, commits, nil, captured)
}

// matchBuildTaskRunsWithExpected is matchBuildTaskRuns with a required matrix
// platform set. A multi-platform matrix can create one TaskRun before the
// others, or a duplicate TaskRun for one platform; accepting that poll would
// capture evidence before every requested platform has an output and turn a
// normal startup race into a verification failure. Nil disables the coverage
// check.
func matchBuildTaskRunsWithExpected(pipelineRuns, taskRuns []unstructured.Unstructured, commits []model.TriggerCommit, expectedPlatforms []string, captured func(string) bool) ([]model.PipelineRunIdentity, bool, error) {
	identities := make([]model.PipelineRunIdentity, 0, len(commits))
	pending := false
	for _, commit := range commits {
		candidates := matchingPipelineRuns(pipelineRuns, commit)
		switch len(candidates) {
		case 0:
			if captured != nil && captured(commit.Component) {
				continue
			}
			pending = true
			continue
		case 1:
		default:
			if hasExactSHA(candidates, commit.SHA) {
				succeeded := 0
				for _, candidate := range candidates {
					if pipelineRunConditionStatus(candidate) == "True" {
						succeeded++
					}
				}
				if succeeded > 1 {
					return nil, false, fmt.Errorf("PipelineRun overshoot for component %s: %d succeeded for commit %s, expected exactly 1", commit.Component, succeeded, commit.SHA)
				}
			}
			return nil, false, fmt.Errorf("ambiguous PipelineRuns for component %s after trigger %s", commit.Component, commit.SHA)
		}
		// A captured component is satisfied even if its PipelineRun was pruned.
		// Duplicate exact runs are rejected above; several successes are the
		// overshoot alarm.
		if captured != nil && captured(commit.Component) {
			continue
		}
		candidate := candidates[0]
		pipelineStatus := pipelineRunConditionStatus(candidate)
		buildTasks := buildTaskRunsForPipelineRun(taskRuns, candidate)
		if len(buildTasks) == 0 {
			if pipelineStatus == "False" {
				return nil, false, &PipelineRunFailedError{Namespace: candidate.GetNamespace(), Name: candidate.GetName(), Component: commit.Component}
			}
			if pipelineStatus == "True" {
				return nil, false, fmt.Errorf("PipelineRun %s/%s succeeded without Buildah TaskRuns for component %s", candidate.GetNamespace(), candidate.GetName(), commit.Component)
			}
			pending = true
			continue
		}
		failed := false
		allSucceeded := true
		for _, taskRun := range buildTasks {
			switch pipelineRunConditionStatus(taskRun) {
			case "False":
				failed = true
			case "True":
			default:
				allSucceeded = false
			}
		}
		if failed {
			return nil, false, &PipelineRunFailedError{Namespace: candidate.GetNamespace(), Name: candidate.GetName(), Component: commit.Component}
		}
		if len(expectedPlatforms) > 0 {
			observed := make(map[string]bool, len(buildTasks))
			platformPending := false
			for _, taskRun := range buildTasks {
				taskPlatform := cluster.BuildTaskRunPlatform(taskRun)
				if taskPlatform == "" {
					if pipelineRunConditionStatus(taskRun) == "True" {
						return nil, false, fmt.Errorf("build TaskRun %s/%s has no platform label — pipeline misconfiguration", taskRun.GetNamespace(), taskRun.GetName())
					}
					// A running TaskRun may still publish its platform in its
					// PLATFORM result; do not reject it until it succeeds.
					platformPending = true
					continue
				}
				observed[taskPlatform] = true
			}
			missing := make([]string, 0, len(expectedPlatforms))
			for _, expected := range expectedPlatforms {
				if !observed[expected] {
					missing = append(missing, expected)
				}
			}
			if len(missing) > 0 || platformPending {
				if pipelineStatus == "False" {
					return nil, false, &PipelineRunFailedError{Namespace: candidate.GetNamespace(), Name: candidate.GetName(), Component: commit.Component}
				}
				if pipelineStatus == "True" {
					if len(missing) > 0 {
						return nil, false, fmt.Errorf("PipelineRun %s/%s is missing Buildah TaskRuns for %s", candidate.GetNamespace(), candidate.GetName(), strings.Join(missing, ", "))
					}
					return nil, false, fmt.Errorf("PipelineRun %s/%s has a Buildah TaskRun without platform metadata", candidate.GetNamespace(), candidate.GetName())
				}
				pending = true
				continue
			}
		}
		if !allSucceeded {
			// A failure before every Buildah TaskRun completes is an early
			// pipeline failure; a post-build failure leaves all builds done.
			if pipelineStatus == "False" {
				return nil, false, &PipelineRunFailedError{Namespace: candidate.GetNamespace(), Name: candidate.GetName(), Component: commit.Component}
			}
			pending = true
			continue
		}
		// Every Buildah TaskRun succeeded. Do not wait for image-index, scans, or
		// other post-build tasks; LKMABT verifies the build outputs, not the full
		// PipelineRun lifecycle.
		identity := identityFromObject(candidate, commit.Component, pipelineRunSHA(candidate))
		identity.Succeeded = true
		identities = append(identities, identity)
	}
	return identities, pending, nil
}

// taskRunsForPipelineRun selects TaskRuns owned by or labeled for a PipelineRun.
func taskRunsForPipelineRun(taskRuns []unstructured.Unstructured, pipelineRun *unstructured.Unstructured) []*unstructured.Unstructured {
	result := make([]*unstructured.Unstructured, 0)
	for index := range taskRuns {
		taskRun := &taskRuns[index]
		owned := cluster.OwnedByPipelineRun(taskRun, string(pipelineRun.GetUID()), pipelineRun.GetName())
		if !owned && taskRun.GetLabels()["tekton.dev/pipelineRun"] == pipelineRun.GetName() {
			owned = true
		}
		if owned {
			result = append(result, taskRun)
		}
	}
	return result
}

// buildTaskRunsForPipelineRun selects Buildah TaskRuns owned by or labeled for a PipelineRun.
func buildTaskRunsForPipelineRun(taskRuns []unstructured.Unstructured, pipelineRun *unstructured.Unstructured) []*unstructured.Unstructured {
	all := taskRunsForPipelineRun(taskRuns, pipelineRun)
	result := make([]*unstructured.Unstructured, 0, len(all))
	for _, taskRun := range all {
		if cluster.IsBuildTaskRun(taskRun) {
			result = append(result, taskRun)
		}
	}
	return result
}

// matchingPipelineRuns selects exact-SHA runs for a component, falling back to
// later post-trigger runs whose SHA is not a recorded prerequisite. The
// prerequisite exclusion keeps runs from superseded triggers, including
// previous retrigger commits, from matching the current one.
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
// A zero trigger time disables the comparison, so any run for the component matches.
func afterTrigger(object *unstructured.Unstructured, commit model.TriggerCommit) bool {
	return commit.CreatedAt.IsZero() || object.GetCreationTimestamp().Time.After(commit.CreatedAt)
}

// allBuildsAppeared reports whether each uncaptured trigger has at least one matching PipelineRun.
func allBuildsAppeared(objects []unstructured.Unstructured, commits []model.TriggerCommit, captured func(string) bool) bool {
	for _, commit := range commits {
		if captured != nil && captured(commit.Component) {
			continue
		}
		if len(matchingPipelineRuns(objects, commit)) == 0 {
			return false
		}
	}
	return true
}

// noPipelineRunError describes the first uncaptured trigger with no matching PipelineRun and observed runs.
// The message includes the observed PipelineRun names so a maintainer can tell an unreconciled
// trigger from a run labeled with an unexpected SHA.
func noPipelineRunError(namespace string, commits []model.TriggerCommit, objects []unstructured.Unstructured, captured func(string) bool) error {
	for _, commit := range commits {
		if captured != nil && captured(commit.Component) {
			continue
		}
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
// It also returns an empty string when the condition exists without a string status; callers
// treat anything other than True or False as non-terminal.
func pipelineRunConditionStatus(object *unstructured.Unstructured) string {
	status, _, _ := pipelineRunSucceededCondition(object)
	if status == "" {
		return "Unknown"
	}
	return status
}

// PipelineRunFailedError reports a trigger's PipelineRun or one of its Buildah
// TaskRuns reaching a failed Succeeded condition; Name and Namespace identify
// the PipelineRun whose logs diagnose the failure.
type PipelineRunFailedError struct {
	Namespace string
	Name      string
	Component string
}

// Error formats the failed PipelineRun's namespace and name.
func (e *PipelineRunFailedError) Error() string {
	return fmt.Sprintf("PipelineRun %s/%s failed", e.Namespace, e.Name)
}

// VerifyBuildOutputs checks recorded PipelineRuns and records their output
// evidence, reusing cached evidence when it matches the current identity and
// covers every expected platform; the rest is fetched from the cluster.
// It requires manifest.PipelineRuns and replaces manifest.BuildOutputs with one
// entry per run in manifest order.
func (s *LiveStage) VerifyBuildOutputs(ctx context.Context, manifest *model.RunManifest) error {
	manifest.PipelineRuns = mergePipelineRunIdentities(nil, manifest.PipelineRuns)
	if len(manifest.PipelineRuns) == 0 {
		return fmt.Errorf("no PipelineRuns to verify build outputs")
	}
	expected := s.platforms()
	cached := make(map[string]model.BuildOutputEvidence, len(manifest.BuildOutputs))
	for _, output := range manifest.BuildOutputs {
		cached[buildEvidenceKey(output.PipelineRun)] = output
	}
	fetched := make(map[string]model.BuildOutputEvidence, len(manifest.PipelineRuns))
	missing := make([]model.PipelineRunIdentity, 0, len(manifest.PipelineRuns))
	for _, identity := range manifest.PipelineRuns {
		key := buildEvidenceKey(identity)
		if output, ok := cached[key]; ok && evidenceCoversPlatforms(output, expected) {
			fetched[key] = output
			continue
		}
		missing = append(missing, identity)
	}
	if len(missing) > 0 {
		inspector := cluster.BuildOutputInspector{Dynamic: s.Clients.Dynamic, Platforms: expected}
		evidence, err := inspector.Verify(ctx, missing)
		if err != nil {
			return err
		}
		if len(evidence) != len(missing) {
			return fmt.Errorf("verified %d build outputs for %d missing PipelineRuns", len(evidence), len(missing))
		}
		for index, output := range evidence {
			// Key by the requested identity as well as the verified one so an
			// identity without a UID still resolves to its evidence.
			fetched[buildEvidenceKey(missing[index])] = output
			fetched[buildEvidenceKey(output.PipelineRun)] = output
		}
	}
	outputs := make([]model.BuildOutputEvidence, 0, len(manifest.PipelineRuns))
	for _, identity := range manifest.PipelineRuns {
		output, ok := fetched[buildEvidenceKey(identity)]
		if !ok {
			return fmt.Errorf("no build output evidence for component %s PipelineRun %s/%s", identity.Component, identity.Namespace, identity.Name)
		}
		outputs = append(outputs, output)
	}
	manifest.BuildOutputs = outputs
	return nil
}

// setBuildOutputEvidence replaces a component's evidence or appends it.
func setBuildOutputEvidence(evidence []model.BuildOutputEvidence, output model.BuildOutputEvidence) []model.BuildOutputEvidence {
	for index := range evidence {
		if evidence[index].Component == output.Component {
			evidence[index] = output
			return evidence
		}
	}
	return append(evidence, output)
}

// removeBuildOutputEvidence drops a component's evidence.
func removeBuildOutputEvidence(evidence []model.BuildOutputEvidence, component string) []model.BuildOutputEvidence {
	kept := make([]model.BuildOutputEvidence, 0, len(evidence))
	for _, output := range evidence {
		if output.Component == component {
			continue
		}
		kept = append(kept, output)
	}
	return kept
}

// buildOutputIdentities returns the PipelineRun identity recorded in each
// captured build evidence entry.
func buildOutputIdentities(outputs []model.BuildOutputEvidence) []model.PipelineRunIdentity {
	identities := make([]model.PipelineRunIdentity, 0, len(outputs))
	for _, output := range outputs {
		identities = append(identities, output.PipelineRun)
	}
	return identities
}

// mergePipelineRunIdentities merges identities, deduplicating by the build evidence key
// so the same attempt from both lists is kept once.
func mergePipelineRunIdentities(existing, additional []model.PipelineRunIdentity) []model.PipelineRunIdentity {
	merged := make([]model.PipelineRunIdentity, 0, len(existing)+len(additional))
	seen := make(map[string]bool, len(existing)+len(additional))
	for _, group := range [][]model.PipelineRunIdentity{existing, additional} {
		for _, identity := range group {
			key := buildEvidenceKey(identity)
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, identity)
		}
	}
	return merged
}

// buildEvidenceKey identifies one PipelineRun attempt.
func buildEvidenceKey(identity model.PipelineRunIdentity) string {
	return identity.Component + "\x00" + string(identity.UID) + "\x00" + identity.Namespace + "/" + identity.Name
}

// evidenceCoversPlatforms reports whether cached evidence proves every expected platform,
// with non-empty output and digest for each; empty expected falls back to all supported platforms.
func evidenceCoversPlatforms(evidence model.BuildOutputEvidence, expected []string) bool {
	if len(expected) == 0 {
		expected = platform.All()
	}
	covered := make(map[string]bool, len(evidence.Platforms))
	for _, buildPlatform := range evidence.Platforms {
		covered[buildPlatform] = true
	}
	for _, buildPlatform := range expected {
		if !covered[buildPlatform] {
			return false
		}
		if evidence.CreatedOutputs[buildPlatform] == "" || evidence.OutputDigests[buildPlatform] == "" {
			return false
		}
	}
	return true
}

// identityFromObject builds a PipelineRun identity from metadata and its Succeeded condition.
// Succeeded stays false when the condition is absent, and CompletedAt is set only when the
// condition carries a parseable lastTransitionTime.
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
