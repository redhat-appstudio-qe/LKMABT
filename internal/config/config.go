package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/clusterfile"
)

const (
	CommandRun                   = "run"
	CommandCleanup               = "cleanup"
	ProviderGitHub               = "github"
	ProviderGitLab               = "gitlab"
	ProviderBoth                 = "both"
	DefaultStateDir              = ".lkmabt-runs"
	DefaultGitHubRepo            = "https://github.com/redhat-appstudio-qe/dr_test_mathwizz"
	DefaultGitLabRepo            = "https://gitlab.com/konflux-qe/dr_test_mathwizz_gl"
	DefaultGitHubTenantNamespace = "mathwizz-test-github"
	DefaultGitLabTenantNamespace = "mathwizz-test-gitlab"
	DefaultBranch                = "main"
)

var DefaultComponentPaths = []string{
	"web-server/Dockerfile",
	"history-worker/Dockerfile",
	"frontend/Dockerfile",
}

type Credentials struct {
	GitHubToken  string
	GitLabToken  string
	GitLabAPIURL string
	// GitLabTokenEphemeral marks a token acquired for this run only. Its SCM
	// Secret is removed after the run.
	GitLabTokenEphemeral bool
}

type Timeouts struct {
	Preflight time.Duration
	Trigger   time.Duration
	Build     time.Duration
}

type Request struct {
	Command          string
	Provider         string
	RunID            string
	ClusterServer    string
	ClustersFile     string
	ClusterName      string
	Kubeconfig       string
	StateDir         string
	TenantNamespace  string
	ApplicationName  string
	SourceRepository string
	SourceBranch     string
	ComponentPaths   []string
	Timeouts         Timeouts
	Credentials      Credentials
}

// Defaults returns a request populated with the tool's default run settings.
func Defaults() Request {
	return Request{
		ClustersFile:     clusterfile.DefaultPath,
		StateDir:         DefaultStateDir,
		SourceRepository: DefaultGitHubRepo,
		SourceBranch:     DefaultBranch,
		TenantNamespace:  DefaultGitHubTenantNamespace,
		ApplicationName:  "mathwizz",
		ComponentPaths:   append([]string(nil), DefaultComponentPaths...),
		Timeouts: Timeouts{
			Preflight: 10 * time.Minute,
			Trigger:   5 * time.Minute,
			Build:     60 * time.Minute,
		},
	}
}

// Parse builds and validates a request from command-line arguments and environment values.
func Parse(args []string) (Request, error) {
	if len(args) == 0 {
		return Request{}, errors.New("usage: lkmabt run <github|gitlab|both> ... | cleanup --run-id <id> ...")
	}
	req := Defaults()
	req.Command = args[0]
	remaining := args[1:]

	switch req.Command {
	case CommandRun:
		if len(remaining) == 0 {
			return Request{}, errors.New("run requires provider github, gitlab, or both")
		}
		req.Provider = remaining[0]
		if req.Provider == ProviderGitLab {
			req.SourceRepository = DefaultGitLabRepo
			req.TenantNamespace = DefaultGitLabTenantNamespace
		}
		remaining = remaining[1:]
		if req.Provider != ProviderGitHub && req.Provider != ProviderGitLab && req.Provider != ProviderBoth {
			return Request{}, fmt.Errorf("unsupported provider %q", req.Provider)
		}
	case CommandCleanup:
	default:
		return Request{}, fmt.Errorf("unsupported command %q", req.Command)
	}

	fs := flag.NewFlagSet("lkmabt", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&req.ClustersFile, "clusters-file", req.ClustersFile, "YAML file listing the clusters to test")
	fs.StringVar(&req.StateDir, "state-dir", req.StateDir, "run state directory")
	var envFile string
	fs.StringVar(&envFile, "env-file", "", "dotenv file containing provider credentials")
	if req.Command == CommandRun {
		fs.StringVar(&req.TenantNamespace, "tenant-namespace", req.TenantNamespace, "persistent tenant namespace")
		fs.StringVar(&req.ApplicationName, "application", req.ApplicationName, "persistent Application name")
		fs.StringVar(&req.SourceRepository, "source-repository", req.SourceRepository, "source repository")
		fs.StringVar(&req.SourceBranch, "branch", req.SourceBranch, "source branch")
		fs.DurationVar(&req.Timeouts.Preflight, "preflight-timeout", req.Timeouts.Preflight, "preflight timeout")
		fs.DurationVar(&req.Timeouts.Trigger, "trigger-timeout", req.Timeouts.Trigger, "trigger timeout")
		fs.DurationVar(&req.Timeouts.Build, "build-timeout", req.Timeouts.Build, "build timeout")
	} else {
		fs.StringVar(&req.RunID, "run-id", "", "exact run whose owned namespace should be deleted")
		fs.StringVar(&req.ClusterName, "cluster", "", "name of the cluster in the clusters file")
	}
	if err := fs.Parse(remaining); err != nil {
		return Request{}, err
	}
	if fs.NArg() != 0 {
		return Request{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	fileValues := map[string]string{}
	if strings.TrimSpace(envFile) != "" {
		var err error
		fileValues, err = readEnvFile(envFile)
		if err != nil {
			return Request{}, err
		}
	}
	req.Credentials = credentialsFromEnv(fileValues)
	return req, req.Validate()
}

// credentialsFromEnv resolves provider credentials from the environment and env-file values.
func credentialsFromEnv(fileValues map[string]string) Credentials {
	return Credentials{
		GitHubToken:  envValue("GITHUB_TOKEN", fileValues),
		GitLabToken:  envValue("GITLAB_BOT_TOKEN", fileValues),
		GitLabAPIURL: envValue("GITLAB_API_URL", fileValues),
	}
}

// envValue returns a nonempty process value for key, falling back to fileValues.
func envValue(key string, fileValues map[string]string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fileValues[key]
}

// readEnvFile parses KEY=VALUE entries from path, ignoring blank lines and comments.
func readEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read env file %q: %w", path, err)
	}
	values := make(map[string]string)
	for lineNumber, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("env file %q line %d must be KEY=VALUE", path, lineNumber+1)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	return values, nil
}

// Validate checks that the request has supported commands, settings, and credentials.
func (r Request) Validate() error {
	if r.Command == "" {
		return errors.New("command is required")
	}
	if strings.TrimSpace(r.ClustersFile) == "" {
		return errors.New("--clusters-file is required")
	}
	if (r.Command == CommandRun || r.Command == CommandCleanup) && strings.TrimSpace(r.StateDir) == "" {
		return errors.New("state directory is required")
	}
	switch r.Command {
	case CommandCleanup:
		if !validRunID(r.RunID) {
			return fmt.Errorf("cleanup requires a valid --run-id")
		}
		if !clusterfile.ValidName(strings.TrimSpace(r.ClusterName)) {
			return errors.New("cleanup requires --cluster naming a cluster in the clusters file")
		}
		return nil
	case CommandRun:
		if r.Provider != ProviderGitHub && r.Provider != ProviderGitLab && r.Provider != ProviderBoth {
			return fmt.Errorf("unsupported provider %q", r.Provider)
		}
		if len(r.ComponentPaths) != len(DefaultComponentPaths) {
			return fmt.Errorf("exactly %d component paths are required", len(DefaultComponentPaths))
		}
		if r.Provider == ProviderBoth {
			if r.SourceRepository != DefaultGitHubRepo || r.TenantNamespace != DefaultGitHubTenantNamespace {
				return errors.New("run both uses the fixed provider fixtures and namespaces")
			}
			if strings.TrimSpace(r.ApplicationName) == "" {
				return errors.New("application is required")
			}
		} else {
			wantRepository := DefaultGitHubRepo
			if r.Provider == ProviderGitLab {
				wantRepository = DefaultGitLabRepo
			}
			if r.SourceRepository != wantRepository {
				return fmt.Errorf("%s fixture must be %s", r.Provider, wantRepository)
			}
			if r.TenantNamespace == "" || r.ApplicationName == "" {
				return errors.New("tenant namespace and application are required")
			}
		}
		for _, timeout := range []struct {
			name  string
			value time.Duration
		}{
			{"preflight", r.Timeouts.Preflight}, {"trigger", r.Timeouts.Trigger}, {"build", r.Timeouts.Build},
		} {
			if timeout.value <= 0 {
				return fmt.Errorf("%s timeout must be positive", timeout.name)
			}
		}
	default:
		return fmt.Errorf("unsupported command %q", r.Command)
	}
	return nil
}

// validRunID reports whether value uses only allowed run ID characters and length.
func validRunID(value string) bool {
	if value == "" || len(value) > 100 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}
