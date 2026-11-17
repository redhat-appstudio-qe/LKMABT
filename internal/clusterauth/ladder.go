package clusterauth

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// Ladder authenticates clusters whose kubeconfig is missing or rejected, via the
// IBM Cloud CLI for ROKS entries and kflux then oc for the rest.
type Ladder struct {
	Runner CommandRunner
	Home   string    // empty means os.UserHomeDir()
	Log    io.Writer // empty means io.Discard
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

// Authenticate ensures every entry has a working kubeconfig and returns one error
// per cluster that remains unauthenticated.
func (l Ladder) Authenticate(ctx context.Context, entries []clusterfile.Entry) map[string]error {
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
	for name, failure := range l.authenticateLegacy(ctx, legacy) {
		failures[name] = failure
	}
	return failures
}

// authenticateLegacy proves every non-ROKS kubeconfig works. Only rejected or
// missing credentials trigger a login through kflux (default paths) and
// oc login; an unreachable API server or missing oc fails the cluster instead,
// because no login can repair it and an interactive oc login would stall the run.
func (l Ladder) authenticateLegacy(ctx context.Context, entries []clusterfile.Entry) map[string]error {
	failures := make(map[string]error, len(entries))
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
		return failures
	}
	if targets := defaultPathEntries(unauthenticated, l.home()); len(targets) > 0 {
		l.runKfluxLogin(ctx, targets)
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
		l.runOCLogin(ctx, entry)
		output, rejected, err := l.verifyLegacyCluster(ctx, entry)
		switch {
		case err == nil:
		case rejected:
			failures[entry.Name] = legacyVerificationFailure(entry, output, err)
		default:
			failures[entry.Name] = verificationFailure(entry, output, err)
		}
	}
	return failures
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

// verifyKubeconfig proves a kubeconfig works and returns oc whoami's output.
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

// runKfluxLogin installs kflux when needed and runs one catalog login for the default-path clusters.
func (l Ladder) runKfluxLogin(ctx context.Context, targets []clusterfile.Entry) {
	binary, err := l.ensureKflux(ctx)
	if err != nil {
		l.logf("kflux unavailable: %v; falling back to oc login", err)
		return
	}
	args := []string{"cluster", "login"}
	// Naming each catalog cluster keeps the login to the configured set; -C would
	// log in to every cluster in kflux's catalog.
	for _, target := range targets {
		args = append(args, "-c", target.KfluxName())
	}
	l.logf("running %s %s", binary, strings.Join(args, " "))
	if err := l.Runner.Run(ctx, CommandSpec{Name: binary, Args: args}); err != nil {
		l.logf("kflux cluster login failed: %v; falling back to oc login", err)
	}
}

// runOCLogin performs an interactive oc login that writes exactly the entry's kubeconfig path.
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
