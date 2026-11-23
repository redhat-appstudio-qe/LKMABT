// Package clusterauth verifies the kubeconfigs of the clusters listed in the
// clusters file and logs in through kflux, oc, or the IBM Cloud CLI when
// credentials are missing or rejected.
package clusterauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/clusterfile"
)

// verifyTimeout bounds the non-interactive oc whoami check, which can stall in
// an exec credential plugin before any API request is made.
const verifyTimeout = time.Minute

// credentialRejections are the oc whoami messages the API server returns when
// the stored identity is expired, revoked, or unknown. They are the only
// verification failures a login can fix; a network or toolchain error cannot.
var credentialRejections = []string{
	"Unauthorized",
	"must be logged in to the server",
	"Authentication failed",
	"Could not find user",
	"the server has asked for the client to provide credentials",
}

// Ladder authenticates clusters whose kubeconfig is missing or rejected, using
// the IBM Cloud CLI for ROKS entries and kflux (default kubeconfig paths only)
// then oc login for the rest. Interactive logins wait on the user; only
// non-interactive probes have fixed timeouts.
type Ladder struct {
	// Runner runs external commands and must be non-nil.
	Runner CommandRunner
	// Home is the home directory for kflux locations and default kubeconfig
	// paths; empty means os.UserHomeDir().
	Home string
	// Log receives progress lines; nil disables progress output.
	Log io.Writer
	// Interactive permits browser-based oc login for identity providers such as
	// FAS that cannot be completed non-interactively.
	Interactive bool
}

// home returns the configured home directory or the process home directory.
func (l Ladder) home() string {
	if l.Home != "" {
		return l.Home
	}
	// No process home (unset HOME) leaves the ladder without a default location.
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// logf writes a ladder progress line to the configured log.
func (l Ladder) logf(format string, args ...any) {
	if l.Log == nil {
		return
	}
	fmt.Fprintf(l.Log, format+"\n", args...)
}

// readableFile reports whether path is an openable regular file.
func readableFile(path string) bool {
	if path == "" {
		return false
	}
	// Directories and special files are not usable kubeconfigs.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	// Stat alone cannot prove the file is readable by this process.
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	file.Close()
	return true
}

// Authenticate ensures every entry has a working kubeconfig by checking readable
// files with oc whoami and logging in where credentials are missing or rejected.
// Non-ROKS entries whose credentials fail are resolved through kflux catalog
// aliases when they use a default kflux kubeconfig path. The returned entries
// follow input order, carrying any catalog and kubeconfig updates; failures holds
// one error per cluster that remains unauthenticated, keyed by cluster name.
func (l Ladder) Authenticate(ctx context.Context, entries []clusterfile.Entry) ([]clusterfile.Entry, map[string]error) {
	failures := map[string]error{}
	// Every kubeconfig is verified on every run: a rejected token leaves the file
	// readable, so readability alone cannot prove the cluster is usable.
	roks := make([]clusterfile.Entry, 0, len(entries))
	legacy := make([]clusterfile.Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsROKS() {
			roks = append(roks, entry)
			continue
		}
		legacy = append(legacy, entry)
	}
	for name, failure := range l.authenticateROKS(ctx, roks) {
		failures[name] = failure
	}
	legacyEntries, legacyFailures := l.authenticateLegacy(ctx, legacy)
	for name, failure := range legacyFailures {
		failures[name] = failure
	}
	resolvedByName := make(map[string]clusterfile.Entry, len(legacyEntries))
	for _, entry := range legacyEntries {
		resolvedByName[entry.Name] = entry
	}
	resolved := make([]clusterfile.Entry, len(entries))
	for index, entry := range entries {
		if updated, found := resolvedByName[entry.Name]; found {
			resolved[index] = updated
			continue
		}
		resolved[index] = entry
	}
	return resolved, failures
}

// authenticateLegacy proves every non-ROKS kubeconfig works. Only rejected or
// missing credentials trigger a login through kflux (default paths) and
// oc login; an unreachable API server or missing oc fails the cluster instead,
// because no login can repair it and an interactive oc login would stall the run.
// After catalog resolution, two clusters that would write one kubeconfig path fail
// rather than overwrite each other.
func (l Ladder) authenticateLegacy(ctx context.Context, entries []clusterfile.Entry) ([]clusterfile.Entry, map[string]error) {
	failures := make(map[string]error, len(entries))
	resolved := append([]clusterfile.Entry(nil), entries...)
	unauthenticated := make([]clusterfile.Entry, 0, len(entries))
	for _, entry := range entries {
		output, rejected, err := l.verifyLegacyCluster(ctx, entry)
		switch {
		case err == nil:
		case rejected:
			unauthenticated = append(unauthenticated, entry)
		default:
			failures[entry.Name] = verificationFailure(entry, output, err)
		}
	}
	if len(unauthenticated) == 0 {
		return resolved, failures
	}
	before := make(map[string]clusterfile.Entry, len(unauthenticated))
	for _, entry := range unauthenticated {
		before[entry.Name] = entry
	}
	kfluxBinary := ""
	for _, entry := range unauthenticated {
		if !entry.UsesDefaultKubeconfig(l.home()) {
			continue
		}
		binary, err := l.ensureKflux(ctx)
		if err != nil {
			l.logf("kflux unavailable: %v; falling back to oc login", err)
		} else {
			kfluxBinary = binary
		}
		break
	}
	unauthenticated = l.resolveCatalogs(ctx, unauthenticated, kfluxBinary)
	for _, entry := range unauthenticated {
		for index := range resolved {
			if resolved[index].Name == entry.Name {
				resolved[index] = entry
				break
			}
		}
	}
	// A resolved catalog may point at an existing kubeconfig that the derived
	// name did not. Re-verify only remapped default-path entries before logging
	// in so a valid kflux kubeconfig is reused without a login.
	effective := make(map[string]string, len(entries))
	for _, entry := range entries {
		effective[entry.Kubeconfig] = entry.Name
	}
	remaining := make([]clusterfile.Entry, 0, len(unauthenticated))
	for _, entry := range unauthenticated {
		if other, found := effective[entry.Kubeconfig]; found && other != entry.Name {
			failures[entry.Name] = fmt.Errorf("clusters %q and %q resolve to the same kubeconfig path %s after kflux catalog resolution", other, entry.Name, entry.Kubeconfig)
			continue
		}
		effective[entry.Kubeconfig] = entry.Name
		previous := before[entry.Name]
		remapped := entry.Kubeconfig != previous.Kubeconfig || entry.CatalogName() != previous.CatalogName()
		if remapped && entry.UsesDefaultKubeconfig(l.home()) {
			output, rejected, err := l.verifyLegacyCluster(ctx, entry)
			switch {
			case err == nil:
				continue
			case !rejected:
				failures[entry.Name] = verificationFailure(entry, output, err)
				continue
			}
		}
		remaining = append(remaining, entry)
	}
	unauthenticated = remaining
	if len(unauthenticated) == 0 {
		return resolved, failures
	}
	if targets := defaultPathEntries(unauthenticated, l.home()); len(targets) > 0 {
		l.runKfluxLogin(ctx, targets, kfluxBinary)
	}
	for _, entry := range unauthenticated {
		// kflux only writes default paths, so another custom path cannot have
		// changed since the first check; it goes straight to the interactive oc login.
		if entry.UsesDefaultKubeconfig(l.home()) {
			output, rejected, err := l.verifyLegacyCluster(ctx, entry)
			if err == nil {
				continue
			}
			if !rejected {
				failures[entry.Name] = verificationFailure(entry, output, err)
				continue
			}
		}
		// oc login without --server falls back to the ambient kubeconfig's
		// current context when the target file is missing, which can log in to a
		// different cluster. Require an explicit login target instead.
		if entry.Server == "" && !readableFile(entry.Kubeconfig) {
			failures[entry.Name] = fmt.Errorf("authentication failed: cluster %s: no readable kubeconfig at %s and no server; set server or kubeconfig, or use a kflux catalog name that resolves to this cluster", entry.Name, entry.Kubeconfig)
			continue
		}
		if server, fas := l.fasWebLoginTarget(ctx, entry); fas {
			l.runOCLoginWeb(ctx, entry, server)
		} else {
			l.runOCLogin(ctx, entry)
		}
		output, rejected, err := l.verifyLegacyCluster(ctx, entry)
		switch {
		case err == nil:
		case rejected:
			failures[entry.Name] = legacyVerificationFailure(entry, output, err)
		default:
			failures[entry.Name] = verificationFailure(entry, output, err)
		}
	}
	return resolved, failures
}

// kfluxCatalogEntry is one cluster from `kflux cluster list --output json`.
type kfluxCatalogEntry struct {
	Name string `json:"name"`
	Host string `json:"host"`
}

// resolveCatalogs maps real cluster host labels (for example stone-stage-p01)
// to their kflux catalog aliases (stg-p01) using kflux's machine-readable list.
// Only default-path entries can be refreshed by kflux; explicit kubeconfig paths
// keep their configured target though they still receive a Catalog override. Any
// lookup failure is logged and leaves entries unchanged. An empty binary skips
// resolution entirely.
func (l Ladder) resolveCatalogs(ctx context.Context, entries []clusterfile.Entry, binary string) []clusterfile.Entry {
	if binary == "" {
		return entries
	}
	home := l.home()
	needsResolution := false
	for _, entry := range entries {
		if entry.UsesDefaultKubeconfig(home) {
			needsResolution = true
			break
		}
	}
	if !needsResolution {
		return entries
	}
	output, err := l.Runner.Stdout(ctx, CommandSpec{Name: binary, Args: []string{"cluster", "list", "--output", "json"}})
	if err != nil {
		l.logf("kflux cluster list failed while resolving catalog names: %v", err)
		return entries
	}
	var catalog []kfluxCatalogEntry
	if err := json.Unmarshal([]byte(output), &catalog); err != nil {
		l.logf("decode kflux cluster list: %v", err)
		return entries
	}
	byName := make(map[string]struct{}, len(catalog))
	byHost := make(map[string][]string, len(catalog))
	for _, item := range catalog {
		name := strings.TrimSpace(item.Name)
		if !clusterfile.ValidName(name) {
			continue
		}
		byName[name] = struct{}{}
		if label := hostLabel(item.Host); label != "" {
			byHost[label] = append(byHost[label], name)
		}
	}
	resolved := make([]clusterfile.Entry, 0, len(entries))
	for _, entry := range entries {
		resolved = append(resolved, l.resolveCatalogEntry(entry, home, byName, byHost))
	}
	return resolved
}

// resolveCatalogEntry returns entry with a Catalog override when its CatalogName
// is not already in the kflux list but its name's host label or its server's host
// label uniquely matches one catalog cluster. An unknown, absent, or ambiguous
// match leaves entry unchanged. A default-path kubeconfig is retargeted to the
// resolved catalog's path.
func (l Ladder) resolveCatalogEntry(entry clusterfile.Entry, home string, byName map[string]struct{}, byHost map[string][]string) clusterfile.Entry {
	if _, known := byName[entry.CatalogName()]; known {
		return entry
	}
	candidates := append([]string(nil), byHost[hostLabel(entry.Name)]...)
	if entry.Server != "" {
		if parsed, err := url.Parse(entry.Server); err == nil {
			candidates = append(candidates, byHost[hostLabel(parsed.Hostname())]...)
		}
	}
	candidates = uniqueStrings(candidates)
	switch len(candidates) {
	case 0:
		return entry
	case 1:
	default:
		l.logf("cluster %s matches multiple kflux catalog clusters: %s", entry.Name, strings.Join(candidates, ", "))
		return entry
	}
	catalog := candidates[0]
	l.logf("cluster %s: resolved kflux catalog cluster %s", entry.Name, catalog)
	if entry.UsesDefaultKubeconfig(home) {
		entry.Kubeconfig = clusterfile.DefaultKubeconfig(home, catalog)
	}
	entry.Catalog = catalog
	return entry
}

// hostLabel returns the cluster host's first DNS label, lowercased and with a
// leading api. or oauth. prefix removed.
func hostLabel(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimPrefix(host, "api.")
	host = strings.TrimPrefix(host, "oauth.")
	if index := strings.IndexByte(host, '.'); index >= 0 {
		host = host[:index]
	}
	return host
}

// uniqueStrings preserves first-seen order while removing duplicates and empty
// values.
func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, found := seen[value]; found {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// defaultPathEntries returns the entries kflux can write, since kflux only writes
// its own per-cluster kubeconfig paths.
func defaultPathEntries(entries []clusterfile.Entry, home string) []clusterfile.Entry {
	targets := make([]clusterfile.Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.UsesDefaultKubeconfig(home) {
			targets = append(targets, entry)
		}
	}
	return targets
}

// verifyLegacyCluster proves a non-ROKS kubeconfig works and returns oc whoami's
// output. rejected reports whether a login can replace the failing credentials;
// it is false for errors a login cannot fix, such as an unreachable API server.
func (l Ladder) verifyLegacyCluster(ctx context.Context, entry clusterfile.Entry) (output string, rejected bool, err error) {
	// oc falls back to the ambient kubeconfig when the configured path is absent,
	// so a missing file must go straight to login instead of reaching oc whoami.
	if !readableFile(entry.Kubeconfig) {
		return "", true, fmt.Errorf("no readable kubeconfig at %s", entry.Kubeconfig)
	}
	output, err = l.verifyKubeconfig(ctx, entry)
	if err == nil {
		return output, false, nil
	}
	return output, isCredentialRejection(strings.TrimSpace(output)), err
}

// verifyKubeconfig proves a kubeconfig works and returns oc whoami's output. The
// command is bounded by verifyTimeout and also passes --request-timeout=30s; a
// timeout is reported as such rather than as the signal that ended the process.
func (l Ladder) verifyKubeconfig(ctx context.Context, entry clusterfile.Entry) (string, error) {
	verifyCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
	output, err := l.Runner.Output(verifyCtx, CommandSpec{Name: "oc", Args: []string{"whoami", "--request-timeout=30s"}, Env: []string{"KUBECONFIG=" + entry.Kubeconfig}})
	timedOut := errors.Is(verifyCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
	cancel()
	if err != nil {
		if detail := strings.TrimSpace(output); detail != "" {
			l.logf("cluster %s: oc whoami said: %s", entry.Name, detail)
		}
		if timedOut {
			return output, fmt.Errorf("oc whoami timed out after %s", verifyTimeout)
		}
		return output, err
	}
	l.logf("cluster %s authenticated as %s", entry.Name, strings.TrimSpace(output))
	return output, nil
}

// legacyVerificationFailure explains why rejected non-ROKS credentials remain unusable.
func legacyVerificationFailure(entry clusterfile.Entry, output string, cause error) error {
	if !readableFile(entry.Kubeconfig) {
		return fmt.Errorf("authentication failed: cluster %s: no readable kubeconfig at %s after kflux and oc login", entry.Name, entry.Kubeconfig)
	}
	return fmt.Errorf("authentication failed: cluster %s: kubeconfig %s was rejected and could not be refreshed: %s", entry.Name, entry.Kubeconfig, failureDetail(output, cause))
}

// verificationFailure reports a kubeconfig that could not be checked at all.
func verificationFailure(entry clusterfile.Entry, output string, cause error) error {
	return fmt.Errorf("authentication failed: cluster %s: could not verify kubeconfig %s: %s", entry.Name, entry.Kubeconfig, failureDetail(output, cause))
}

// failureDetail prefers oc whoami's output over the command error.
func failureDetail(output string, cause error) string {
	if detail := strings.TrimSpace(output); detail != "" {
		return detail
	}
	return cause.Error()
}

// isCredentialRejection reports whether oc whoami output rejects the stored credentials.
func isCredentialRejection(output string) bool {
	for _, rejection := range credentialRejections {
		if strings.Contains(output, rejection) {
			return true
		}
	}
	return false
}

// runKfluxLogin runs one kflux catalog login naming every default-path target, so
// a single invocation refreshes them together. A failed batch login is retried one
// cluster at a time, stopping early when the context is canceled. It is a no-op
// when binary is empty.
func (l Ladder) runKfluxLogin(ctx context.Context, targets []clusterfile.Entry, binary string) {
	if binary == "" {
		return
	}
	args := []string{"cluster", "login"}
	// Naming each catalog cluster keeps the login to the configured set; -C would
	// log in to every cluster in kflux's catalog.
	for _, target := range targets {
		args = append(args, "-c", target.CatalogName())
	}
	l.logf("running %s %s", binary, strings.Join(args, " "))
	if err := l.Runner.Run(ctx, CommandSpec{Name: binary, Args: args}); err == nil {
		return
	} else {
		l.logf("kflux cluster login failed: %v; retrying each cluster individually", err)
		if len(targets) == 1 {
			return
		}
	}
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		single := CommandSpec{Name: binary, Args: []string{"cluster", "login", "-c", target.CatalogName()}}
		if err := l.Runner.Run(ctx, single); err != nil {
			l.logf("kflux cluster login for %s (%s) failed: %v", target.Name, target.CatalogName(), err)
		}
	}
}

// runOCLogin performs an interactive oc login that writes exactly the entry's
// kubeconfig path, creating its parent directory. Without a configured Server the
// command falls back to the ambient kubeconfig's current context, so callers must
// ensure a usable login target exists. Failures are logged, not returned.
func (l Ladder) runOCLogin(ctx context.Context, entry clusterfile.Entry) {
	if err := os.MkdirAll(filepath.Dir(entry.Kubeconfig), 0o700); err != nil {
		l.logf("create kubeconfig directory for cluster %s: %v", entry.Name, err)
		return
	}
	args := []string{"login"}
	if entry.Server != "" {
		args = append(args, "--server="+entry.Server)
	}
	l.logf("running oc %s for cluster %s with KUBECONFIG=%s", strings.Join(args, " "), entry.Name, entry.Kubeconfig)
	if err := l.Runner.Run(ctx, CommandSpec{Name: "oc", Args: args, Env: []string{"KUBECONFIG=" + entry.Kubeconfig}}); err != nil {
		l.logf("oc login for cluster %s failed: %v", entry.Name, err)
	}
}

// runOCLoginWeb performs the browser-based oc login used when an identity
// provider such as FAS cannot be completed by kflux or with a password prompt.
// It always names the server read from the entry or its kubeconfig, so the login
// cannot fall back to the ambient kubeconfig's current context. Failures are
// logged, not returned.
func (l Ladder) runOCLoginWeb(ctx context.Context, entry clusterfile.Entry, server string) {
	if err := os.MkdirAll(filepath.Dir(entry.Kubeconfig), 0o700); err != nil {
		l.logf("create kubeconfig directory for cluster %s: %v", entry.Name, err)
		return
	}
	args := []string{"login", "--web", "--server=" + server}
	l.logf("running oc %s for cluster %s with KUBECONFIG=%s", strings.Join(args, " "), entry.Name, entry.Kubeconfig)
	if err := l.Runner.Run(ctx, CommandSpec{Name: "oc", Args: args, Env: []string{"KUBECONFIG=" + entry.Kubeconfig}}); err != nil {
		l.logf("oc login --web for cluster %s failed: %v", entry.Name, err)
	}
}
