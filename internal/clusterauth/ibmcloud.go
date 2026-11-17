package clusterauth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/redhat-appstudio/lkmabt/internal/clusterfile"
)

const ibmcloudBinaryName = "ibmcloud"

// ibmcloudRejections are the oc whoami messages a ROKS cluster returns when the
// IBM Cloud user has no working cluster identity.
var ibmcloudRejections = []string{
	"Unauthorized",
	"must be logged in to the server",
	"Authentication failed",
	"Could not find user",
	"the server has asked for the client to provide credentials",
}

// authenticateROKS verifies or writes one kubeconfig per ROKS entry, returning
// one error per cluster that remains unusable.
func (l Ladder) authenticateROKS(ctx context.Context, targets []clusterfile.Entry) map[string]error {
	failures := make(map[string]error, len(targets))
	if len(targets) == 0 {
		return failures
	}
	binary, lookErr := l.Runner.LookPath(ibmcloudBinaryName)
	var sessionErr error
	sessionChecked := false
	for _, target := range targets {
		rejected := false
		// A readable kubeconfig still needs proof: oc whoami can reject it while the
		// file stays on disk, and a rerun would otherwise trust that file.
		if readableFile(target.Kubeconfig) {
			output, err := l.verifyROKSCluster(ctx, target)
			if err == nil {
				continue
			}
			if !isROKSRejection(strings.TrimSpace(output)) {
				failures[target.Name] = roksVerificationFailure(target, output, err)
				continue
			}
			rejected = true
		}
		if lookErr != nil {
			failures[target.Name] = ibmcloudMissingFailure(target, rejected)
			continue
		}
		if !sessionChecked {
			sessionErr = l.ensureIBMCloudSession(ctx, binary)
			sessionChecked = true
		}
		if sessionErr != nil {
			failures[target.Name] = fmt.Errorf("authentication failed: cluster %s: ibmcloud login --sso did not complete: %w", target.Name, sessionErr)
			continue
		}
		if err := l.loginROKSCluster(ctx, binary, target); err != nil {
			failures[target.Name] = err
		}
	}
	return failures
}

// ibmcloudMissingFailure explains a ROKS entry that needs the IBM Cloud CLI but cannot find it.
func ibmcloudMissingFailure(entry clusterfile.Entry, rejected bool) error {
	if rejected {
		return fmt.Errorf("authentication failed: cluster %s is type %s: the existing kubeconfig was rejected and the ibmcloud CLI is not on PATH to refresh it; install it from https://cloud.ibm.com/docs/cli. If you have never opened this cluster's OpenShift web console, complete that first-time login once from the IBM Cloud console", entry.Name, clusterfile.TypeROKS)
	}
	return fmt.Errorf("authentication failed: cluster %s is type %s but the ibmcloud CLI is not on PATH; install it from https://cloud.ibm.com/docs/cli", entry.Name, clusterfile.TypeROKS)
}

// ensureIBMCloudSession reuses an active ibmcloud session and runs the interactive
// SSO login only when the CLI reports no session.
func (l Ladder) ensureIBMCloudSession(ctx context.Context, binary string) error {
	if _, err := l.Runner.Output(ctx, CommandSpec{Name: binary, Args: []string{"target", "-q"}}); err == nil {
		l.logf("ibmcloud session is active")
		return nil
	}
	l.logf("running %s login --sso", binary)
	return l.Runner.Run(ctx, CommandSpec{Name: binary, Args: []string{"login", "--sso"}})
}

// loginROKSCluster writes the entry's kubeconfig through the IBM Cloud CLI, then
// proves it works with oc whoami.
func (l Ladder) loginROKSCluster(ctx context.Context, binary string, entry clusterfile.Entry) error {
	if err := os.MkdirAll(filepath.Dir(entry.Kubeconfig), 0o700); err != nil {
		return fmt.Errorf("authentication failed: create kubeconfig directory for cluster %s: %w", entry.Name, err)
	}
	args := []string{"ks", "cluster", "config", "--cluster", entry.IBMCloud, "--endpoint", entry.IBMCloudEndpoint, "-q"}
	env := []string{"KUBECONFIG=" + entry.Kubeconfig}
	l.logf("running %s %s for cluster %s with KUBECONFIG=%s", binary, strings.Join(args, " "), entry.Name, entry.Kubeconfig)
	if err := l.Runner.Run(ctx, CommandSpec{Name: binary, Args: args, Env: env}); err != nil {
		return fmt.Errorf("authentication failed: cluster %s: ibmcloud ks cluster config: %w", entry.Name, err)
	}
	output, err := l.verifyROKSCluster(ctx, entry)
	if err != nil {
		return roksVerificationFailure(entry, output, err)
	}
	return nil
}

// verifyROKSCluster proves a ROKS kubeconfig works and returns oc whoami's output.
func (l Ladder) verifyROKSCluster(ctx context.Context, entry clusterfile.Entry) (string, error) {
	output, err := l.Runner.Output(ctx, CommandSpec{Name: "oc", Args: []string{"whoami", "--request-timeout=30s"}, Env: []string{"KUBECONFIG=" + entry.Kubeconfig}})
	if err != nil {
		if detail := strings.TrimSpace(output); detail != "" {
			l.logf("cluster %s: oc whoami said: %s", entry.Name, detail)
		}
		return output, err
	}
	l.logf("cluster %s authenticated as %s", entry.Name, strings.TrimSpace(output))
	return output, nil
}

// roksVerificationFailure explains why a written ROKS kubeconfig is unusable.
func roksVerificationFailure(entry clusterfile.Entry, output string, cause error) error {
	detail := strings.TrimSpace(output)
	if isROKSRejection(detail) {
		return fmt.Errorf("authentication failed: cluster %s: the API server rejected the credentials from the kubeconfig written by ibmcloud ks cluster config. If you have never opened this cluster's OpenShift web console, complete that first-time login once from the IBM Cloud console, then re-run. If you have logged in before, run ibmcloud login --sso and retry", entry.Name)
	}
	if detail == "" {
		detail = cause.Error()
	}
	return fmt.Errorf("authentication failed: cluster %s: oc whoami failed: %s", entry.Name, detail)
}

// isROKSRejection reports whether oc whoami output rejects the cluster identity.
func isROKSRejection(output string) bool {
	for _, rejection := range ibmcloudRejections {
		if strings.Contains(output, rejection) {
			return true
		}
	}
	return false
}
