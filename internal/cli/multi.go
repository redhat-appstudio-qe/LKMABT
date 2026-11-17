package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	"github.com/redhat-appstudio/lkmabt/internal/clusterauth"
	"github.com/redhat-appstudio/lkmabt/internal/clusterfile"
	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/githubauth"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/report"
)

// authenticateGitHub acquires interactive GitHub credentials; tests replace it.
var authenticateGitHub = githubauth.Authenticate

type clusterRun struct {
	Entry   clusterfile.Entry
	AuthErr error
	Results []ProviderResult
}

// RunClusters authenticates every listed cluster, acquires GitHub credentials
// when needed, rejects entries that resolve to one API server, runs the clusters
// one at a time in clusters-file order, and writes the aggregate reports.
func RunClusters(ctx context.Context, request config.Request, runner Runner, console *Console) (runErr error) {
	entries, err := clusterfile.Load(request.ClustersFile)
	if err != nil {
		return err
	}
	aggregate, err := newAggregateRun(request.StateDir)
	if err != nil {
		return err
	}
	defer aggregate.Log.Close()
	if len(entries) > 0 && providerNeedsGitHub(request.Provider) && strings.TrimSpace(request.Credentials.GitHubToken) == "" {
		if console.Interactive() {
			console.Printf("github: authentication required; starting the gh browser login")
		}
		token, cleanup, err := authenticateGitHub(ctx, clusterauth.ExecRunner{Log: aggregate.Log}, console.Interactive())
		if err != nil {
			return err
		}
		defer func() {
			if cleanupErr := cleanup(); cleanupErr != nil {
				runErr = errors.Join(runErr, cleanupErr)
			}
		}()
		request.Credentials.GitHubToken = token
	}
	ladder := clusterauth.Ladder{Runner: clusterauth.ExecRunner{Log: aggregate.Log}, Log: aggregate.Log}
	authFailures := ladder.Authenticate(ctx, entries)
	if err := rejectDuplicateServers(entries, authFailures); err != nil {
		return err
	}
	runs := scheduleClusters(ctx, request, entries, authFailures, aggregate, runner, console)
	return finishAggregateRun(aggregate, request, runs, console)
}

// providerNeedsGitHub reports whether the selected provider includes GitHub.
func providerNeedsGitHub(provider string) bool {
	return provider == config.ProviderGitHub || provider == config.ProviderBoth
}

// serverIdentity returns a comparison key for an API server URL, so equivalent
// spellings cannot pass as distinct clusters. Scheme, host, port, and path all
// participate; host case and an explicit default port are normalized away.
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

// scheduleClusters runs authenticated clusters one at a time in clusters-file order.
func scheduleClusters(ctx context.Context, request config.Request, entries []clusterfile.Entry, authFailures map[string]error, aggregate *aggregateRun, runner Runner, console *Console) []clusterRun {
	runs := make([]clusterRun, len(entries))
	for index, entry := range entries {
		runs[index] = clusterRun{Entry: entry, AuthErr: authFailures[entry.Name]}
		if runs[index].AuthErr != nil {
			console.Printf("cluster %s: authentication failed: %v", entry.Name, runs[index].AuthErr)
			continue
		}
		console.Printf("cluster %s: authenticated", entry.Name)
		console.Printf("cluster %s: started", entry.Name)
		runs[index].Results = runCluster(ctx, request, entry, aggregate.Directory, runner)
		console.Printf("cluster %s: finished (%s)", entry.Name, resultSummary(runs[index].Results))
	}
	return runs
}

// resultSummary renders "<passed> passed, <failed> failed" for one cluster's provider results.
func resultSummary(results []ProviderResult) string {
	passed := 0
	for _, result := range results {
		if result.Err == nil {
			passed++
		}
	}
	return fmt.Sprintf("%d passed, %d failed", passed, len(results)-passed)
}

// finishAggregateRun renders both reports, prints the summary, and reports the aggregate failure.
func finishAggregateRun(aggregate *aggregateRun, request config.Request, runs []clusterRun, console *Console) error {
	rows := reportRows(aggregate.Directory, runs, providerNames(request.Provider))
	document := report.Document{RunID: aggregate.RunID, ClustersFile: request.ClustersFile, StartedAt: aggregate.StartedAt, EndedAt: time.Now().UTC(), Passed: allPassed(rows), Rows: rows}
	writeErr := report.Write(aggregate.Directory, document)
	for _, row := range rows {
		console.Printf("%s %s: %s (%s) %s", row.Cluster, row.Provider, row.Result(), row.Stage, row.Error)
	}
	markdownPath, htmlPath := aggregate.reportPaths()
	console.Printf("report: %s", markdownPath)
	console.Printf("report: %s", htmlPath)
	return errors.Join(aggregateError(runs), writeErr)
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

// runCluster prepares one cluster's state root and run log, then runs its providers.
func runCluster(ctx context.Context, request config.Request, entry clusterfile.Entry, aggregateDirectory string, runner Runner) []ProviderResult {
	directory := filepath.Join(aggregateDirectory, entry.Name)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return failedResults(request.Provider, fmt.Errorf("create cluster directory %s: %w", directory, err))
	}
	log, err := newAttachedRunLog(filepath.Join(directory, "run.log"))
	if err != nil {
		return failedResults(request.Provider, err)
	}
	defer log.Close()
	server := entry.Server
	if server == "" {
		server, err = cluster.ServerFromKubeconfig(entry.Kubeconfig)
		if err != nil {
			fmt.Fprintf(log, "resolve API server: %v\n", err)
			return failedResults(request.Provider, fmt.Errorf("resolve API server for cluster %s: %w", entry.Name, err))
		}
	}
	clusterRequest := request
	clusterRequest.ClusterName, clusterRequest.Kubeconfig, clusterRequest.ClusterServer, clusterRequest.StateDir = entry.Name, entry.Kubeconfig, server, directory
	fmt.Fprintf(log, "cluster %s: running %s against %s with kubeconfig %s\n", entry.Name, request.Provider, server, entry.Kubeconfig)
	return runProviders(ctx, clusterRequest, runner)
}

// reportRows builds one row per cluster and provider in clusters-file order.
func reportRows(aggregateDirectory string, runs []clusterRun, providers []string) []report.Row {
	rows := make([]report.Row, 0, len(runs)*len(providers))
	for _, run := range runs {
		if run.AuthErr != nil {
			for _, provider := range providers {
				rows = append(rows, report.Row{Cluster: run.Entry.Name, Provider: provider, Passed: false, Stage: report.StageAuthentication, Error: run.AuthErr.Error(), Links: report.Links(aggregateDirectory, "", "")})
			}
			continue
		}
		for _, result := range run.Results {
			rows = append(rows, report.Row{Cluster: run.Entry.Name, Provider: result.Provider, Passed: result.Err == nil, Stage: string(result.Stage), Error: errorText(result.Err), Links: report.Links(aggregateDirectory, run.Entry.Name, result.ArtifactDirectory)})
		}
	}
	return rows
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

// failedResults reports the same cluster-level failure for every selected provider.
func failedResults(provider string, err error) []ProviderResult {
	names := providerNames(provider)
	results := make([]ProviderResult, 0, len(names))
	for _, name := range names {
		results = append(results, ProviderResult{Provider: name, Stage: model.PhasePreflight, Err: err})
	}
	return results
}
