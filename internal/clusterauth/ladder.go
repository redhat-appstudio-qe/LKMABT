package clusterauth

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/redhat-appstudio/lkmabt/internal/clusterfile"
)

// Ladder authenticates clusters whose kubeconfig is missing, via kflux then oc.
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

// Authenticate ensures every entry has a readable kubeconfig and returns one error per
// cluster that remains unauthenticated.
func (l Ladder) Authenticate(ctx context.Context, entries []clusterfile.Entry) map[string]error {
	failures := map[string]error{}
	missing := make([]clusterfile.Entry, 0, len(entries))
	for _, entry := range entries {
		if !readableFile(entry.Kubeconfig) {
			missing = append(missing, entry)
		}
	}
	if len(missing) == 0 {
		return failures
	}
	// kflux can only write kubeconfigs at its default per-cluster paths.
	kfluxTargets := make([]clusterfile.Entry, 0, len(missing))
	for _, entry := range missing {
		if entry.UsesDefaultKubeconfig(l.home()) {
			kfluxTargets = append(kfluxTargets, entry)
		}
	}
	if len(kfluxTargets) > 0 {
		l.runKfluxLogin(ctx, kfluxTargets)
	}
	remaining := make([]clusterfile.Entry, 0, len(missing))
	for _, entry := range missing {
		if !readableFile(entry.Kubeconfig) {
			remaining = append(remaining, entry)
		}
	}
	for _, entry := range remaining {
		l.runOCLogin(ctx, entry)
	}
	for _, entry := range remaining {
		if !readableFile(entry.Kubeconfig) {
			failures[entry.Name] = fmt.Errorf("authentication failed: no readable kubeconfig at %s after kflux and oc login", entry.Kubeconfig)
		}
	}
	return failures
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
