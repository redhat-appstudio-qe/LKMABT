package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/clusterfile"
	"github.com/redhat-appstudio/lkmabt/internal/platform"
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
	DefaultEnvFile               = ".lkmabt.env"
)

var DefaultComponentPaths = []string{
	"web-server/Dockerfile",
	"history-worker/Dockerfile",
	"frontend/Dockerfile",
}

// DefaultTektonPaths lists the fixture PipelineRuns whose build-platforms
// parameter is set to the selected architectures. Entries align with
// DefaultComponentPaths.
var DefaultTektonPaths = []string{
	".tekton/konflux-test-mathwizz-web-server-push.yaml",
	".tekton/konflux-test-mathwizz-history-worker-push.yaml",
	".tekton/konflux-test-mathwizz-frontend-push.yaml",
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
	MultiProvider    bool
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
	TektonPaths      []string
	Platforms        []string
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
		TektonPaths:      append([]string(nil), DefaultTektonPaths...),
		Platforms:        platform.All(),
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
		return Request{}, fmt.Errorf("usage: lkmabt <github|gitlab|both> [%s]... [flags] | lkmabt cleanup --cluster <name> --run-id <id> [flags]", strings.Join(platform.Names(), "|"))
	}
	req := Defaults()
	var remaining []string

	switch args[0] {
	case CommandRun:
		return Request{}, errors.New(`the "run" subcommand no longer exists; use "lkmabt github", "lkmabt gitlab", or "lkmabt both"`)
	case CommandCleanup:
		req.Command = CommandCleanup
		remaining = args[1:]
	case ProviderGitHub, ProviderGitLab, ProviderBoth:
		req.Command = CommandRun
		req.Provider = args[0]
		if req.Provider == ProviderGitLab {
			req.SourceRepository = DefaultGitLabRepo
			req.TenantNamespace = DefaultGitLabTenantNamespace
		}
		remaining = args[1:]
		// Architecture arguments follow the fixture command and precede flags,
		// so a flag value can never be read as an architecture. Each argument may
		// also carry a comma-separated list.
		names, rest, err := architectureArguments(remaining)
		if err != nil {
			return Request{}, err
		}
		if len(names) > 0 {
			platforms, err := platform.Combine(names...)
			if err != nil {
				return Request{}, err
			}
			req.Platforms = platforms
		}
		remaining = rest
	default:
		return Request{}, fmt.Errorf("unsupported command %q", args[0])
	}

	flags := flag.NewFlagSet("lkmabt", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&req.ClustersFile, "clusters-file", req.ClustersFile, "YAML file listing the clusters to test")
	flags.StringVar(&req.StateDir, "state-dir", req.StateDir, "run state directory")
	var envFile string
	if req.Command == CommandRun {
		flags.StringVar(&envFile, "env-file", DefaultEnvFile, "dotenv file containing provider credentials (a missing default is ignored)")
		flags.StringVar(&req.TenantNamespace, "tenant-namespace", req.TenantNamespace, "persistent tenant namespace")
		flags.StringVar(&req.ApplicationName, "application", req.ApplicationName, "persistent Application name")
		flags.StringVar(&req.SourceRepository, "source-repository", req.SourceRepository, "source repository")
		flags.StringVar(&req.SourceBranch, "branch", req.SourceBranch, "source branch")
		flags.DurationVar(&req.Timeouts.Preflight, "preflight-timeout", req.Timeouts.Preflight, "preflight timeout")
		flags.DurationVar(&req.Timeouts.Trigger, "trigger-timeout", req.Timeouts.Trigger, "trigger timeout")
		flags.DurationVar(&req.Timeouts.Build, "build-timeout", req.Timeouts.Build, "build timeout")
	} else {
		flags.StringVar(&req.RunID, "run-id", "", "exact run whose owned namespace should be deleted")
		flags.StringVar(&req.ClusterName, "cluster", "", "name of the cluster in the clusters file")
	}
	if err := flags.Parse(remaining); err != nil {
		return Request{}, err
	}
	if flags.NArg() != 0 {
		return Request{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	fileValues := map[string]string{}
	if req.Command == CommandRun {
		envFileExplicit := false
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "env-file" {
				envFileExplicit = true
			}
		})
		if strings.TrimSpace(envFile) != "" {
			values, err := readEnvFile(envFile)
			switch {
			case err == nil:
				fileValues = values
			case !envFileExplicit && errors.Is(err, os.ErrNotExist):
				// An absent default env file is not an error; environment-only runs still work.
			default:
				return Request{}, err
			}
		}
	}
	credentials, err := credentialsFromEnv(fileValues, req.Provider == ProviderGitLab || req.Provider == ProviderBoth)
	if err != nil {
		return Request{}, err
	}
	req.Credentials = credentials
	return req, req.Validate()
}

// architectureArguments consumes leading non-flag arguments and splits each on
// commas, so `s390x arm64` and `s390x, arm64` select the same platforms.
func architectureArguments(args []string) (names, remaining []string, err error) {
	remaining = args
	for len(remaining) > 0 && !strings.HasPrefix(remaining[0], "-") {
		argument := remaining[0]
		remaining = remaining[1:]
		added := false
		for _, part := range strings.Split(argument, ",") {
			name := strings.TrimSpace(part)
			if name == "" {
				continue
			}
			names = append(names, name)
			added = true
		}
		if !added {
			return nil, nil, fmt.Errorf("architecture argument %q names no platforms", argument)
		}
	}
	return names, remaining, nil
}

// credentialsFromEnv resolves provider credentials from the environment and env-file values.
// A GitLab token and its API URL always come from the same source; when the
// token's source has no URL but the other source does, the request fails
// instead of silently sending the token to the default endpoint.
func credentialsFromEnv(fileValues map[string]string, gitlab bool) (Credentials, error) {
	credentials := Credentials{GitHubToken: envValue("GITHUB_TOKEN", fileValues)}
	if !gitlab {
		return credentials, nil
	}
	processToken := strings.TrimSpace(os.Getenv("GITLAB_BOT_TOKEN"))
	processURL := strings.TrimSpace(os.Getenv("GITLAB_API_URL"))
	fileToken := fileValues["GITLAB_BOT_TOKEN"]
	fileURL := fileValues["GITLAB_API_URL"]
	if processToken != "" {
		if processURL == "" && fileURL != "" {
			return Credentials{}, errors.New("GITLAB_BOT_TOKEN is set in the environment but GITLAB_API_URL is only in the env file; set both in the same source")
		}
		credentials.GitLabToken, credentials.GitLabAPIURL = processToken, processURL
		return credentials, nil
	}
	if fileToken == "" {
		credentials.GitLabAPIURL = processURL
		if credentials.GitLabAPIURL == "" {
			credentials.GitLabAPIURL = fileURL
		}
		return credentials, nil
	}
	if fileURL == "" && processURL != "" {
		return Credentials{}, errors.New("GITLAB_BOT_TOKEN is set in the env file but GITLAB_API_URL is only in the environment; set both in the same source")
	}
	credentials.GitLabToken, credentials.GitLabAPIURL = fileToken, fileURL
	return credentials, nil
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
		if len(r.TektonPaths) != len(r.ComponentPaths) {
			return fmt.Errorf("exactly %d Tekton pipeline paths are required", len(r.ComponentPaths))
		}
		if len(r.Platforms) == 0 {
			return errors.New("at least one build platform is required")
		}
		seenPlatforms := make(map[string]struct{}, len(r.Platforms))
		for _, buildPlatform := range r.Platforms {
			if !platform.Supported(buildPlatform) {
				return fmt.Errorf("unsupported build platform %q", buildPlatform)
			}
			if _, duplicate := seenPlatforms[buildPlatform]; duplicate {
				return fmt.Errorf("duplicate build platform %q", buildPlatform)
			}
			seenPlatforms[buildPlatform] = struct{}{}
		}
		if r.Provider == ProviderBoth {
			if r.SourceRepository != DefaultGitHubRepo || r.TenantNamespace != DefaultGitHubTenantNamespace {
				return errors.New("both uses the fixed provider fixtures and namespaces")
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
