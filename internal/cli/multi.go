package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/auth"
	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	"github.com/redhat-appstudio/lkmabt/internal/clusterauth"
	"github.com/redhat-appstudio/lkmabt/internal/clusterfile"
	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/platform"
	"github.com/redhat-appstudio/lkmabt/internal/providers"
	"github.com/redhat-appstudio/lkmabt/internal/providers/github"
	"github.com/redhat-appstudio/lkmabt/internal/report"
	"github.com/redhat-appstudio/lkmabt/internal/workflow"
	"golang.org/x/term"
)

// Credential acquisition and destruction are replaceable in tests.
var (
	authenticateGitHub      = auth.GitHubToken
	readExistingGitHubToken = auth.ExistingGitHubToken
	destroyCredentials      = (*auth.Store).Destroy
	checkGitHubAccess       = func(ctx context.Context, token string) error {
		adapter, err := github.NewFromCredentials(token)
		if err != nil {
			return err
		}
		if err := adapter.CheckOrganizationAccess(ctx); err != nil {
			return err
		}
		if err := adapter.CheckFixtureWriteAccess(ctx); err != nil {
			return err
		}
		return adapter.CheckInstallationRepositoryAccess(ctx)
	}
)

// githubAccessCheckTimeout bounds each pre-workflow GitHub credential check.
var githubAccessCheckTimeout = 30 * time.Second

// checkGitHubToken verifies, under a bounded context, that a token can list App
// installations, confirm push access to the fixture repository, and list the
// repositories of selected App installations. It checks the configured token
// first, then the existing gh login, and reports needsLogin when both are
// rejected so the caller can start a browser login. Only an access rejection
// triggers that fallback; transient, rate-limit, timeout, and cancellation
// errors abort the run without replacing a usable token.
func checkGitHubToken(ctx context.Context, request *config.Request, console *Console) (needsLogin bool, err error) {
	check := func(token string) error {
		checkCtx, cancel := context.WithTimeout(ctx, githubAccessCheckTimeout)
		defer cancel()
		return checkGitHubAccess(checkCtx, token)
	}
	err = check(request.Credentials.GitHubToken)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, providers.ErrInsufficientGitHubAccess) {
		return false, fmt.Errorf("check the configured GitHub token: %w", err)
	}
	console.Printf("github: configured token is not usable: %v", err)
	readCtx, cancelRead := context.WithTimeout(ctx, githubAccessCheckTimeout)
	token, readErr := readExistingGitHubToken(readCtx, clusterauth.ExecRunner{})
	readEnded := readCtx.Err() != nil
	cancelRead()
	if readErr == nil {
		checkErr := check(token)
		switch {
		case checkErr == nil:
			console.Printf("github: using the existing gh login")
			request.Credentials.GitHubToken = token
			return false, nil
		case !errors.Is(checkErr, providers.ErrInsufficientGitHubAccess):
			return false, fmt.Errorf("check the existing gh login: %w", checkErr)
		}
		console.Printf("github: existing gh login is not usable: %v; starting the gh browser login", checkErr)
		return true, nil
	}
	if readEnded || errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
		return false, fmt.Errorf("read the existing gh login: %w", readErr)
	}
	console.Printf("github: existing gh login is not usable: %v; starting the gh browser login", readErr)
	return true, nil
}

// GitLabTokenURL is the prefilled personal access token page for a GitLab run.
const GitLabTokenURL = "https://gitlab.com/-/user_settings/personal_access_tokens/legacy/new?scopes=api%2Cwrite_repository&name=lkmabt-live-run"

// openGitLabTokenPage opens the token page; tests replace it.
var openGitLabTokenPage = workflow.OpenBrowser

// readGitLabToken reads a personal access token without echoing it; tests replace it.
var readGitLabToken = func() (string, error) {
	fmt.Fprint(os.Stderr, "Paste the personal access token: ")
	data, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// clusterRun holds one clusters-file entry's authentication failure or the
// results of its providers. FirstAttempt is set when the first pass deferred and
// Results comes from the fresh retry, so reports can retain both attempts.
type clusterRun struct {
	Entry              clusterfile.Entry
	AuthErr            error
	Results            []ProviderResult
	FirstAttempt       []ProviderResult
	RequestedPlatforms []string
}

// RunClusters loads the clusters file, acquires provider credentials,
// authenticates the entries, runs each cluster in clusters-file order, and
// writes the aggregate reports.
//
// Credentials are resolved before cluster authentication. In an interactive
// run, a configured GitHub token is checked and replaced by the existing gh
// login or a browser login only when GitHub rejects it; a non-interactive run
// uses the configured token without checking it. A pasted GitLab token is
// marked ephemeral and revoked after the last cluster finishes. Entries that
// resolve to one API server are rejected after authentication, so one run
// cannot delete another's namespace. Clusters run one at a time, while each
// cluster's selected providers run concurrently. The run rotates the state
// directory's latest aggregate directory before cluster authentication, falling
// back to a permanent directory with a logged warning when preparation fails for
// another reason; a concurrent run already holding the lock is refused.
//
// GitLab token revocation failures and gh credential-store destruction failures
// are reported as a credentials row in the aggregate report and joined into the
// returned error. Both operations are idempotent, so the deferred calls still
// clean up when RunClusters returns before writing the report.
func RunClusters(ctx context.Context, request config.Request, runner Runner, console *Console) (runErr error) {
	entries, err := clusterfile.Load(request.ClustersFile)
	if err != nil {
		return err
	}
	console.Printf("LKMABT: %d cluster(s), provider %s", len(entries), request.Provider)
	githubTokenNeeded := providerNeedsGitHub(request.Provider) && strings.TrimSpace(request.Credentials.GitHubToken) == ""
	gitlabTokenNeeded := providerNeedsGitLab(request.Provider) && strings.TrimSpace(request.Credentials.GitLabToken) == ""
	if len(entries) > 0 && providerNeedsGitHub(request.Provider) && !githubTokenNeeded && console.Interactive() {
		needsLogin, err := checkGitHubToken(ctx, &request, console)
		if err != nil {
			return err
		}
		if needsLogin {
			request.Credentials.GitHubToken = ""
			githubTokenNeeded = true
		}
	}
	var (
		store       *auth.Store
		credentials *Step
	)
	if len(entries) > 0 && (githubTokenNeeded || gitlabTokenNeeded) {
		credentials = console.StartStep("Authenticate provider credentials 🔑")
	}
	finishCredentials := func(credentialErr error) {
		if credentials != nil {
			credentials.End(credentialErr == nil)
			credentials = nil
		}
	}
	if len(entries) > 0 && githubTokenNeeded {
		store, err = auth.NewStore()
		if err != nil {
			finishCredentials(err)
			return err
		}
		defer func() {
			if store != nil {
				runErr = errors.Join(runErr, destroyCredentials(store))
				store = nil
			}
		}()
		if console.Interactive() {
			console.Printf("github: authentication required; starting the gh browser login")
		}
		resume := console.Suspend()
		token, authErr := authenticateGitHub(ctx, clusterauth.ExecRunner{}, store, console.Interactive())
		resume()
		if authErr != nil {
			finishCredentials(authErr)
			return authErr
		}
		request.Credentials.GitHubToken = token
	}
	var revokeEphemeralGitLab func() error
	if len(entries) > 0 && gitlabTokenNeeded {
		if !console.Interactive() {
			err := fmt.Errorf("GitLab authentication is required: set GITLAB_BOT_TOKEN or run in a terminal to paste a personal access token")
			finishCredentials(err)
			return err
		}
		if err := validateInteractiveGitLabURL(request.Credentials.GitLabAPIURL); err != nil {
			finishCredentials(err)
			return err
		}
		console.Printf("gitlab: authentication required\nA browser window is opening at GitLab's personal access token page.\nSet the expiration to 1 day, keep the api and write_repository scopes, and create the token.")
		resume := console.Suspend()
		if err := openGitLabTokenPage(GitLabTokenURL); err != nil {
			console.Printf("could not open the token page: %v\nOpen it manually: %s", err, GitLabTokenURL)
		}
		token, tokenErr := readGitLabToken()
		resume()
		if tokenErr != nil {
			err := fmt.Errorf("read GitLab personal access token: %w", tokenErr)
			finishCredentials(err)
			return err
		}
		if !strings.HasPrefix(token, "glpat-") {
			err := fmt.Errorf("GitLab requires a personal access token with api and write_repository; create one at %s or set GITLAB_BOT_TOKEN", GitLabTokenURL)
			finishCredentials(err)
			return err
		}
		request.Credentials.GitLabToken = token
		request.Credentials.GitLabTokenEphemeral = true
		revoked := false
		revokeEphemeralGitLab = func() error {
			if revoked {
				return nil
			}
			revoked = true
			revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
			defer cancel()
			if err := revokeGitLabToken(revokeCtx, request.Credentials.GitLabToken, request.Credentials.GitLabAPIURL); err != nil {
				return fmt.Errorf("revoke GitLab token: %w", err)
			}
			return nil
		}
		// Early returns before the report exists still revoke; the finalization
		// path calls the closure first so its failure lands in the report.
		defer func() {
			runErr = errors.Join(runErr, revokeEphemeralGitLab())
		}()
	}
	finishCredentials(nil)
	aggregate, err := newAggregateRun(request.StateDir)
	if err != nil {
		return err
	}
	defer aggregate.Close()
	ladder := clusterauth.Ladder{Runner: clusterauth.ExecRunner{Log: aggregate.Log}, Log: aggregate.Log}
	entries, authFailures := ladder.Authenticate(ctx, entries)
	if err := rejectDuplicateServers(entries, authFailures); err != nil {
		return err
	}
	runs := scheduleClusters(ctx, request, entries, authFailures, aggregate, runner, console)
	// Clean up credentials before rendering so their failures appear in the
	// report; the defers above remain a no-op after these calls.
	var cleanupErr error
	if store != nil {
		cleanupErr = destroyCredentials(store)
		store = nil
	}
	if revokeEphemeralGitLab != nil {
		cleanupErr = errors.Join(cleanupErr, revokeEphemeralGitLab())
	}
	return finishAggregateRun(aggregate, request, runs, console, cleanupErr)
}

// validateInteractiveGitLabURL rejects the token-page prompt for API endpoints
// other than gitlab.com, because the page issues gitlab.com tokens. An empty URL
// means the default gitlab.com endpoint.
func validateInteractiveGitLabURL(apiURL string) error {
	apiURL = strings.TrimSpace(apiURL)
	if apiURL == "" {
		return nil
	}
	parsed, err := url.Parse(apiURL)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("invalid GitLab API URL %q", apiURL)
	}
	if parsed.Scheme != "https" || parsed.Port() != "" || parsed.Hostname() != "gitlab.com" {
		return fmt.Errorf("interactive GitLab authentication is only supported for gitlab.com; set GITLAB_BOT_TOKEN for %s", apiURL)
	}
	return nil
}

// providerNeedsGitHub reports whether the selected provider includes GitHub.
func providerNeedsGitHub(provider string) bool {
	return provider == config.ProviderGitHub || provider == config.ProviderBoth
}

// providerNeedsGitLab reports whether the selected provider includes GitLab.
func providerNeedsGitLab(provider string) bool {
	return provider == config.ProviderGitLab || provider == config.ProviderBoth
}

// serverIdentity returns a comparison key for an API server URL, so equivalent
// spellings cannot pass as distinct clusters. Scheme, host, port, and path all
// participate; scheme and host case and an explicit default port are normalized
// away.
func serverIdentity(server string) string {
	normalized := normalizeServer(server)
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Host == "" {
		// Unparsable or hostless values cannot be canonicalized; compare the
		// lowercased spelling so at least case-only differences still collide.
		return strings.ToLower(normalized)
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	// An explicit default port names the same endpoint as the implicit one.
	if port == defaultPort(scheme) {
		port = ""
	}
	// A path-routed gateway serves several API servers from one authority, so
	// the path is part of the endpoint identity. A root path is no path.
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	if port == "" {
		return scheme + "://" + host + path
	}
	return scheme + "://" + net.JoinHostPort(host, port) + path
}

// defaultPort returns the port scheme implies, or "" when it implies none.
func defaultPort(scheme string) string {
	switch scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// rejectDuplicateServers fails when two authenticated entries resolve to the same
// API server, so one run's stale-namespace policy cannot delete another's namespace.
func rejectDuplicateServers(entries []clusterfile.Entry, authFailures map[string]error) error {
	seen := make(map[string]string, len(entries))
	for _, entry := range entries {
		if authFailures[entry.Name] != nil {
			continue
		}
		server := entry.Server
		if server == "" {
			resolved, err := cluster.ServerFromKubeconfig(entry.Kubeconfig)
			if err != nil {
				// runCluster reports this per cluster; it cannot collide with a resolved server here.
				continue
			}
			server = resolved
		}
		server = normalizeServer(server)
		// Two spellings of one endpoint must collide: host case and an explicit
		// default port do not make different API servers.
		identity := serverIdentity(server)
		if other, found := seen[identity]; found {
			return fmt.Errorf("clusters %q and %q resolve to the same API server %s", other, entry.Name, server)
		}
		seen[identity] = entry.Name
	}
	return nil
}

// scheduleClusters runs authenticated clusters one at a time in clusters-file
// order. A cluster whose authentication failed keeps its AuthErr and produces no
// provider results; the remaining clusters still run. A cluster whose providers
// settle with at least one admission deferral is retried once after the first
// pass, with a fresh run ID and trigger commits. Waiting for every provider
// before deferring keeps the shared fixture from being mutated by an interleaved
// cluster; the retry cap and each attempt's normal phase timeouts bound the run.
func scheduleClusters(ctx context.Context, request config.Request, entries []clusterfile.Entry, authFailures map[string]error, aggregate *aggregateRun, runner Runner, console *Console) []clusterRun {
	runs := make([]clusterRun, len(entries))
	deferred := make([]int, 0, len(entries))
	for index, entry := range entries {
		runs[index] = clusterRun{Entry: entry, AuthErr: authFailures[entry.Name], RequestedPlatforms: request.Platforms}
		if runs[index].AuthErr != nil {
			console.Result(false, fmt.Sprintf("Authenticate cluster %s 🔐", entry.Name))
			console.Printf("  %v", runs[index].AuthErr)
			continue
		}
		console.Printf("")
		console.Printf("cluster %s", entry.Name)
		runs[index].Results = runCluster(ctx, request, entry, aggregate.Directory, runner)
		if attemptDeferred(runs[index].Results) {
			runs[index].FirstAttempt = runs[index].Results
			deferred = append(deferred, index)
			console.Printf("cluster %s: deferred (%s); continuing with the next cluster", entry.Name, deferredSummary(runs[index].Results))
			console.Printf("cluster %s: will retry after the first pass", entry.Name)
			continue
		}
		console.Printf("cluster %s: finished (%s)", entry.Name, resultSummary(runs[index].Results))
	}
	for _, index := range deferred {
		// A failed deferred cleanup stays in FirstAttempt.CleanupErr. The retry's
		// preflight must clear any stale scoped namespace before its builds start;
		// if cleanup failed, the matching retry result is failed below even when
		// the retry builds succeed, and the row note keeps the reason visible.
		entry := runs[index].Entry
		console.Printf("")
		console.Printf("cluster %s: retrying deferred attempt (fresh run)", entry.Name)
		if ctx.Err() != nil {
			runs[index].Results = exhaustedDeferred(runs[index].FirstAttempt, ctx.Err())
			console.Printf("cluster %s: retry skipped (%v)", entry.Name, ctx.Err())
			continue
		}
		retryNames := deferredProviderNames(runs[index].FirstAttempt)
		results := runClusterProviders(ctx, request, entry, aggregate.Directory, runner, retryNames)
		if attemptDeferred(results) {
			results = exhaustedDeferred(results, nil)
		}
		final := mergeProviderResults(runs[index].FirstAttempt, results)
		for resultIndex := range final {
			first, ok := firstAttemptResult(runs[index].FirstAttempt, final[resultIndex].Provider)
			if !ok || first.CleanupErr == nil {
				continue
			}
			// A deferred cleanup failure is not erased by a successful build
			// retry; surface it as a cluster failure even though the retry's
			// preflight should have reconciled the stale namespace.
			final[resultIndex].Err = errors.Join(final[resultIndex].Err, fmt.Errorf("first attempt deferred cleanup failed: %w", first.CleanupErr))
		}
		runs[index].Results = final
		console.Printf("cluster %s: retry finished (%s)", entry.Name, resultSummary(final))
	}
	return runs
}

// attemptDeferred reports whether any provider in the attempt stopped in
// scheduler admission.
func attemptDeferred(results []ProviderResult) bool {
	for _, result := range results {
		if result.Deferred {
			return true
		}
	}
	return false
}

// deferredProviderNames returns the providers that need a fresh retry.
func deferredProviderNames(results []ProviderResult) []string {
	names := make([]string, 0, len(results))
	for _, result := range results {
		if result.Deferred {
			names = append(names, result.Provider)
		}
	}
	return names
}

// mergeProviderResults replaces retried providers in their original positions
// and preserves every non-retried sibling result.
func mergeProviderResults(first, retried []ProviderResult) []ProviderResult {
	merged := append([]ProviderResult(nil), first...)
	positions := make(map[string]int, len(merged))
	for index, result := range merged {
		positions[result.Provider] = index
	}
	for _, result := range retried {
		if index, ok := positions[result.Provider]; ok {
			merged[index] = result
			continue
		}
		merged = append(merged, result)
	}
	return merged
}

// deferredSummary names the scheduler reason that deferred the attempt.
func deferredSummary(results []ProviderResult) string {
	for _, result := range results {
		if result.Deferred && result.DeferredReason != "" {
			return result.DeferredReason
		}
	}
	return "admission pending"
}

// exhaustedDeferred converts a retry that deferred again into terminal failures,
// so the aggregate exits non-zero and reports the scheduler reason once.
func exhaustedDeferred(results []ProviderResult, cause error) []ProviderResult {
	exhausted := append([]ProviderResult(nil), results...)
	for index := range exhausted {
		if !exhausted[index].Deferred {
			continue
		}
		reason := exhausted[index].DeferredReason
		if reason == "" {
			reason = "admission pending"
		}
		if cause != nil {
			exhausted[index].Err = fmt.Errorf("cluster admission-pending retry skipped: %v: %s", cause, reason)
		} else {
			exhausted[index].Err = fmt.Errorf("cluster remained admission-pending after one retry: %s", reason)
		}
		exhausted[index].Deferred = false
	}
	return exhausted
}

// resultSummary renders "<passed> passed, <failed> failed" for one cluster's
// provider results, counting a first-attempt deferral separately so it never
// reads as a build failure.
func resultSummary(results []ProviderResult) string {
	passed, deferred, failed := 0, 0, 0
	for _, result := range results {
		switch {
		case result.Deferred:
			deferred++
		case result.Err == nil:
			passed++
		default:
			failed++
		}
	}
	if deferred > 0 {
		if failed > 0 {
			return fmt.Sprintf("%d passed, %d failed, %d deferred", passed, failed, deferred)
		}
		return fmt.Sprintf("%d passed, %d deferred", passed, deferred)
	}
	return fmt.Sprintf("%d passed, %d failed", passed, failed)
}

// finishAggregateRun writes the run-named Markdown and HTML reports, prints one
// summary line per row, and joins cluster, credential-cleanup, and report-writing
// failures. A non-nil cleanupErr adds a credentials row to the report.
func finishAggregateRun(aggregate *aggregateRun, request config.Request, runs []clusterRun, console *Console, cleanupErr error) error {
	rows := reportRows(aggregate.Directory, aggregate.runLogName(), runs, providerNames(request.Provider))
	if cleanupErr != nil {
		rows = append(rows, report.Row{Cluster: "credentials", Provider: "auth", Stage: report.StageCleanup, Error: cleanupErr.Error(), Links: report.Links(aggregate.Directory, aggregate.runLogName(), "", "")})
	}
	document := report.Document{RunID: aggregate.RunID, ClustersFile: request.ClustersFile, Architectures: request.Platforms, StartedAt: aggregate.StartedAt, EndedAt: time.Now().UTC(), Passed: allPassed(rows), Rows: rows}
	step := console.StartStep("Write reports 📝")
	writeErr := report.Write(aggregate.Directory, document, aggregate.Name)
	step.End(writeErr == nil)
	console.Printf("")
	for _, row := range rows {
		note := ""
		if row.Note != "" {
			note = "; " + row.Note
		}
		console.Printf("%s %s: %s (%s); requested architectures: %s%s", row.Cluster, row.Provider, row.Result(), row.Stage, document.ArchitecturesText(), note)
	}
	markdownPath, htmlPath := aggregate.reportPaths()
	console.Printf("report: %s", markdownPath)
	console.Printf("report: %s", htmlPath)
	return errors.Join(aggregateError(runs), cleanupErr, writeErr)
}

// aggregateError joins every cluster and provider failure, preserving clusters-file order.
func aggregateError(runs []clusterRun) error {
	failures := make([]error, 0, len(runs))
	for _, run := range runs {
		if run.AuthErr != nil {
			failures = append(failures, fmt.Errorf("cluster %s: %w", run.Entry.Name, run.AuthErr))
			continue
		}
		for _, result := range run.Results {
			if result.Err != nil {
				failures = append(failures, fmt.Errorf("cluster %s %s run: %w", run.Entry.Name, result.Provider, result.Err))
			}
		}
	}
	return errors.Join(failures...)
}

// runCluster prepares one cluster's state root and run log, resolves the API
// server when the entry carries none, and runs the cluster's selected providers
// concurrently. Setup failures are reported as the same failure for every
// selected provider, so the aggregate report always has a row per provider.
func runCluster(ctx context.Context, request config.Request, entry clusterfile.Entry, aggregateDirectory string, runner Runner) []ProviderResult {
	return runClusterProviders(ctx, request, entry, aggregateDirectory, runner, providerNames(request.Provider))
}

// runClusterProviders runs the named providers for one cluster and returns one
// result per name. A subset retry uses it to leave a sibling provider's
// first-attempt result untouched.
func runClusterProviders(ctx context.Context, request config.Request, entry clusterfile.Entry, aggregateDirectory string, runner Runner, names []string) []ProviderResult {
	directory := filepath.Join(aggregateDirectory, entry.Name)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return failedResultsFor(names, fmt.Errorf("create cluster directory %s: %w", directory, err))
	}
	log, err := newAttachedRunLog(filepath.Join(directory, "run.log"))
	if err != nil {
		return failedResultsFor(names, err)
	}
	defer log.Close()
	server := entry.Server
	if server == "" {
		server, err = cluster.ServerFromKubeconfig(entry.Kubeconfig)
		if err != nil {
			fmt.Fprintf(log, "resolve API server: %v\n", err)
			return failedResultsFor(names, fmt.Errorf("resolve API server for cluster %s: %w", entry.Name, err))
		}
	}
	clusterRequest := request
	clusterRequest.ClusterName, clusterRequest.Kubeconfig, clusterRequest.ClusterServer, clusterRequest.StateDir = entry.Name, entry.Kubeconfig, server, directory
	label := request.Provider
	if len(names) < len(providerNames(request.Provider)) {
		label = strings.Join(names, ", ")
	}
	fmt.Fprintf(log, "cluster %s: running %s against %s with kubeconfig %s\n", entry.Name, label, server, entry.Kubeconfig)
	return runProvidersFor(ctx, clusterRequest, runner, names)
}

// reportRows builds one row per cluster and provider in clusters-file order. An
// authentication failure yields one failed row per selected provider; otherwise
// rows follow the provider results in their fixed order.
func reportRows(aggregateDirectory, aggregateLog string, runs []clusterRun, providers []string) []report.Row {
	rows := make([]report.Row, 0, len(runs)*len(providers))
	for _, run := range runs {
		if run.AuthErr != nil {
			for _, provider := range providers {
				rows = append(rows, report.Row{Cluster: run.Entry.Name, Provider: provider, Passed: false, Stage: report.StageAuthentication, Error: run.AuthErr.Error(), Links: report.Links(aggregateDirectory, aggregateLog, "", "")})
			}
			continue
		}
		for _, result := range run.Results {
			note := retryNote(run, result.Provider)
			if tested := testedPlatformsNote(run.RequestedPlatforms, result.Platforms); tested != "" {
				if note != "" {
					note += "; "
				}
				note += tested
			}
			rows = append(rows, report.Row{
				Cluster:  run.Entry.Name,
				Provider: result.Provider,
				Passed:   result.Err == nil,
				Stage:    string(result.Stage),
				Note:     note,
				Error:    errorText(result.Err),
				Links:    resultLinks(aggregateDirectory, aggregateLog, run, result),
			})
		}
	}
	return rows
}

// retryNote describes a first attempt that deferred or failed before the final
// attempt, so a retry never silently erases the earlier outcome.
func retryNote(run clusterRun, provider string) string {
	first, ok := firstAttemptResult(run.FirstAttempt, provider)
	if !ok {
		return ""
	}
	final, ok := resultForProvider(run.Results, provider)
	if !ok || !wasRetried(first, final) {
		return ""
	}
	if first.Deferred {
		reason := first.DeferredReason
		if reason == "" {
			reason = "admission pending"
		}
		return "first attempt deferred: " + reason
	}
	if first.Err != nil {
		return "first attempt failed: " + first.Err.Error()
	}
	return ""
}

// testedPlatformsNote reports a per-cluster architecture narrowing so the row
// and console summary do not claim a skipped architecture was exercised.
func testedPlatformsNote(requested, tested []string) string {
	if len(tested) == 0 {
		return ""
	}
	if len(requested) == 0 {
		// An empty request means the default all-platform selection.
		requested = platform.All()
	}
	if slices.Equal(requested, tested) {
		return ""
	}
	return "tested: " + strings.Join(tested, ", ")
}

// firstAttemptResult returns a provider's result from the deferred first pass.
func firstAttemptResult(results []ProviderResult, provider string) (ProviderResult, bool) {
	return resultForProvider(results, provider)
}

// resultForProvider returns a provider's result from a result list.
func resultForProvider(results []ProviderResult, provider string) (ProviderResult, bool) {
	for _, result := range results {
		if result.Provider == provider {
			return result, true
		}
	}
	return ProviderResult{}, false
}

// wasRetried reports whether first was replaced by final. A preserved sibling
// keeps the same run identity and artifact directory; a retry gets a fresh run.
func wasRetried(first, final ProviderResult) bool {
	if first.RunID != "" || final.RunID != "" {
		return first.RunID != final.RunID
	}
	if first.ArtifactDirectory != "" || final.ArtifactDirectory != "" {
		return first.ArtifactDirectory != final.ArtifactDirectory
	}
	return first.Deferred != final.Deferred || first.CleanupErr != nil
}

// resultLinks links the final attempt and keeps the deferred first attempt's
// provider directory and log reachable.
func resultLinks(aggregateDirectory, aggregateLog string, run clusterRun, result ProviderResult) []report.Link {
	links := report.Links(aggregateDirectory, aggregateLog, run.Entry.Name, result.ArtifactDirectory)
	first, ok := firstAttemptResult(run.FirstAttempt, result.Provider)
	if !ok || first.ArtifactDirectory == "" || !wasRetried(first, result) {
		return links
	}
	for _, link := range report.Links(aggregateDirectory, aggregateLog, run.Entry.Name, first.ArtifactDirectory) {
		switch link.Label {
		case "run directory", "run.log":
			links = append(links, report.Link{Label: "first attempt " + link.Label, Target: link.Target})
		}
	}
	return links
}

// allPassed reports whether every row passed.
func allPassed(rows []report.Row) bool {
	for _, row := range rows {
		if !row.Passed {
			return false
		}
	}
	return true
}

// errorText renders an error for a report cell.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// failedResults reports the same cluster-level failure for every selected
// provider, at the preflight stage because the cluster failed before a workflow
// started.
func failedResults(provider string, err error) []ProviderResult {
	return failedResultsFor(providerNames(provider), err)
}

// failedResultsFor reports a cluster setup failure for the named providers.
func failedResultsFor(names []string, err error) []ProviderResult {
	results := make([]ProviderResult, 0, len(names))
	for _, name := range names {
		results = append(results, ProviderResult{Provider: name, Stage: model.PhasePreflight, Err: err})
	}
	return results
}
