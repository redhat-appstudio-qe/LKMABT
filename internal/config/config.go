// Package config parses command-line arguments and environment values into a
// validated LKMABT request.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/clusterfile"
	"github.com/redhat-appstudio/lkmabt/internal/platform"
)

const (
	// CommandRun is the request command recorded for fixture runs; it is not a
	// CLI subcommand.
	CommandRun = "run"
	// CommandCleanup is the request command for removing one run's namespace.
	CommandCleanup = "cleanup"
	// ProviderGitHub runs the GitHub fixture only.
	ProviderGitHub = "github"
	// ProviderGitLab runs the GitLab fixture only.
	ProviderGitLab = "gitlab"
	// ProviderBoth runs the GitHub and GitLab fixtures for every cluster.
	ProviderBoth = "both"
	// DefaultStateDir is the default run state directory, relative to the
	// working directory.
	DefaultStateDir = ".lkmabt-runs"
	// DefaultGitHubRepo is the fixed GitHub fixture repository.
	DefaultGitHubRepo = "https://github.com/redhat-appstudio-qe/dr_test_mathwizz"
	// DefaultGitLabRepo is the fixed GitLab fixture repository.
	DefaultGitLabRepo = "https://gitlab.com/konflux-qe/dr_test_mathwizz_gl"
	// DefaultGitHubTenantNamespace is the persistent tenant namespace of the
	// GitHub fixture.
	DefaultGitHubTenantNamespace = "mathwizz-test-github"
	// DefaultGitLabTenantNamespace is the persistent tenant namespace of the
	// GitLab fixture.
	DefaultGitLabTenantNamespace = "mathwizz-test-gitlab"
	// DefaultBranch is the default branch of the fixture repositories.
	DefaultBranch = "main"
	// DefaultEnvFile is the dotenv file Parse reads when --env-file is not given.
	DefaultEnvFile = ".lkmabt.env"
	// SmokeComponentName is the only component a --smoke run builds.
	SmokeComponentName = "mathwizz-history-worker"
)

// componentProfile is one fixture component: the Konflux Component name, the
// Dockerfile a trigger commit appends to, and the PaC PipelineRun whose
// build-platforms parameter the run rewrites.
type componentProfile struct {
	Name       string
	Dockerfile string
	TektonPath string
}

// fullProfile is the complete MathWizz fixture selection in fixture order.
// It is the single definition of the component set; every other list is
// derived from it.
var fullProfile = []componentProfile{
	{Name: "mathwizz-web-server", Dockerfile: "web-server/Dockerfile", TektonPath: ".tekton/konflux-test-mathwizz-web-server-push.yaml"},
	{Name: "mathwizz-history-worker", Dockerfile: "history-worker/Dockerfile", TektonPath: ".tekton/konflux-test-mathwizz-history-worker-push.yaml"},
	{Name: "mathwizz-frontend", Dockerfile: "frontend/Dockerfile", TektonPath: ".tekton/konflux-test-mathwizz-frontend-push.yaml"},
}

// selection splits profiles into aligned name, Dockerfile, and PipelineRun
// slices, freshly allocated so no caller shares package storage.
func selection(profiles []componentProfile) (names, dockerfiles, tektonPaths []string) {
	names = make([]string, 0, len(profiles))
	dockerfiles = make([]string, 0, len(profiles))
	tektonPaths = make([]string, 0, len(profiles))
	// Collect each profile's fields in fixture order so the three slices stay
	// aligned by index.
	for _, profile := range profiles {
		names = append(names, profile.Name)
		dockerfiles = append(dockerfiles, profile.Dockerfile)
		tektonPaths = append(tektonPaths, profile.TektonPath)
	}
	return names, dockerfiles, tektonPaths
}

// FullComponents returns the complete fixture selection: component names,
// their Dockerfiles, and their PipelineRun manifests, aligned by index and
// freshly allocated for the caller.
func FullComponents() (names, dockerfiles, tektonPaths []string) {
	return selection(fullProfile)
}

// SmokeComponents returns the single-component smoke selection in the same
// aligned form, reusing the full profile entry so the two cannot drift.
func SmokeComponents() (names, dockerfiles, tektonPaths []string) {
	for _, profile := range fullProfile {
		if profile.Name == SmokeComponentName {
			return selection([]componentProfile{profile})
		}
	}
	return nil, nil, nil
}

// Credentials holds the provider tokens resolved for one request. The zero
// value leaves authentication to the caller's auth ladder.
type Credentials struct {
	// GitHubToken is the GitHub API token; empty means no token was found.
	GitHubToken string
	// GitLabToken is the GitLab API token; empty means no token was found.
	GitLabToken string
	// GitLabAPIURL is the GitLab API base URL; empty means the provider default.
	GitLabAPIURL string
	// GitLabTokenEphemeral marks a token acquired for this run only. Its SCM
	// Secret is removed after the run.
	GitLabTokenEphemeral bool
}

// Timeouts bounds each workflow phase; each value must be positive for a run.
type Timeouts struct {
	Preflight time.Duration
	Trigger   time.Duration
	Build     time.Duration
}

// Request is one parsed and validated invocation. Fields left empty by Parse
// are filled in by later stages, such as the cluster connection.
type Request struct {
	// Command is CommandRun or CommandCleanup.
	Command string
	// Provider is the selected fixture for a run; empty for cleanup.
	Provider string
	// MultiProvider reports whether both fixtures run in this invocation.
	MultiProvider bool
	// RunID names the cleanup target; run requests leave it empty and generate
	// identifiers later.
	RunID string
	// ClusterServer is the cluster API URL, set during preflight or by cleanup.
	ClusterServer string
	// ClustersFile is the YAML cluster list.
	ClustersFile string
	// ClusterName and Kubeconfig identify the cleanup target or the current cluster.
	ClusterName string
	Kubeconfig  string
	// StateDir is the run state root; a relative path resolves against the
	// working directory.
	StateDir         string
	TenantNamespace  string
	ApplicationName  string
	SourceRepository string
	SourceBranch     string
	// Smoke limits the run to SmokeComponentName.
	Smoke bool
	// ComponentNames holds the Konflux Component names this run creates and
	// verifies, aligned with ComponentPaths and TektonPaths.
	ComponentNames []string
	ComponentPaths []string
	TektonPaths    []string
	// Platforms holds canonical platform names; Parse expands architecture
	// aliases before storing them here.
	Platforms   []string
	Timeouts    Timeouts
	Credentials Credentials
}

// Defaults returns a request populated with the tool's default run settings.
// Slice fields are fresh copies safe for the caller to modify; Parse applies
// flags and environment values on top of these defaults.
func Defaults() Request {
	names, dockerfiles, tektonPaths := FullComponents()
	return Request{
		ClustersFile:     clusterfile.DefaultPath,
		StateDir:         DefaultStateDir,
		SourceRepository: DefaultGitHubRepo,
		SourceBranch:     DefaultBranch,
		TenantNamespace:  DefaultGitHubTenantNamespace,
		ApplicationName:  "mathwizz",
		ComponentNames:   names,
		ComponentPaths:   dockerfiles,
		TektonPaths:      tektonPaths,
		Platforms:        platform.All(),
		Timeouts: Timeouts{
			Preflight: 10 * time.Minute,
			Trigger:   5 * time.Minute,
			Build:     60 * time.Minute,
		},
	}
}

// ExpectedComponents returns how many fixture components this run must
// create and verify: one in smoke mode, the whole fixture otherwise. The
// count is the contract, not a measurement of the request's slices.
func (r Request) ExpectedComponents() int {
	if r.Smoke {
		return 1
	}
	return len(fullProfile)
}

// Parse builds and validates a request from command-line arguments and environment values.
// The first argument selects the fixture (github, gitlab, or both) or cleanup.
// Cleanup defines only --run-id, --cluster, --clusters-file, and --state-dir.
// For a fixture command, optional architecture arguments follow the command and
// must precede flags; without them every supported platform is selected.
// --smoke is a run-only flag that narrows the fixture to SmokeComponentName; cleanup rejects it as an unknown flag.
// Process environment values take precedence over env-file values, except that a
// GitLab token and API URL must come from the same source. The default env file
// is optional, but an explicitly named one must be readable and well formed;
// an empty --env-file disables env-file reading. The env file is read only for
// a fixture command. Flag parse errors are printed to stderr and returned.
// Parse returns the request along with the validation error, if any.
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
		flags.BoolVar(&req.Smoke, "smoke", false, "build only the mathwizz-history-worker component")
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
	// --smoke is a run-only flag that narrows the fixture to SmokeComponentName; cleanup rejects it as an unknown flag.
	if req.Smoke {
		req.ComponentNames, req.ComponentPaths, req.TektonPaths = SmokeComponents()
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
// commas, so `s390x arm64` and `s390x, arm64` select the same platforms. It
// stops at the first argument beginning with "-" and returns the rest for flag
// parsing; an empty entry in any argument is an error.
func architectureArguments(args []string) (names, remaining []string, err error) {
	remaining = args
	for len(remaining) > 0 && !strings.HasPrefix(remaining[0], "-") {
		argument := remaining[0]
		remaining = remaining[1:]
		parts := strings.Split(argument, ",")
		// A single trailing comma is the shell form `s390x, arm64`; any other
		// empty entry is malformed.
		if len(parts) > 1 && strings.TrimSpace(parts[len(parts)-1]) == "" {
			parts = parts[:len(parts)-1]
		}
		for _, part := range parts {
			name := strings.TrimSpace(part)
			if name == "" {
				return nil, nil, fmt.Errorf("architecture argument %q contains an empty entry", argument)
			}
			names = append(names, name)
		}
	}
	return names, remaining, nil
}

// credentialsFromEnv resolves provider credentials from the environment and env-file values.
// GITHUB_TOKEN prefers the process environment and falls back to the env file.
// For GitLab, a process GITLAB_BOT_TOKEN wins over an env-file one. A GitLab token
// and its API URL always come from the same source; when the token's source has
// no URL but the other source does, the call fails instead of silently sending
// the token to the default endpoint.
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

// envValue returns a trimmed, nonempty process value for key, falling back to
// fileValues.
func envValue(key string, fileValues map[string]string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fileValues[key]
}

// readEnvFile parses KEY=VALUE entries from path, ignoring blank lines and
// comments. An optional "export " prefix and one pair of surrounding single or
// double quotes are stripped. A line without "=" or with an empty key is an
// error that names the path and 1-based line number.
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

// Validate checks that the request has a supported command and a complete
// configuration. Both commands require a clusters file and state directory; a
// cleanup also requires a valid RunID and ClusterName. A run additionally
// requires a supported provider, exactly ExpectedComponents aligned component
// names, Dockerfile paths, and Tekton pipeline paths, without duplicate names,
// exactly the canonical SmokeComponents selection in smoke mode, positive
// timeouts, a nonempty application and tenant namespace, the selected
// provider's fixed fixture repository, and for both the GitHub fixture
// repository and tenant namespace. Validate does not read the clusters file or
// contact providers.
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
		expected := r.ExpectedComponents()
		if len(r.ComponentNames) != expected || len(r.ComponentPaths) != expected || len(r.TektonPaths) != expected {
			return fmt.Errorf("exactly %d component names, Dockerfile paths, and Tekton pipeline paths are required; got %d, %d, %d", expected, len(r.ComponentNames), len(r.ComponentPaths), len(r.TektonPaths))
		}
		if r.Smoke {
			names, dockerfiles, tektonPaths := SmokeComponents()
			if len(r.ComponentNames) != 1 ||
				!slices.Equal(r.ComponentNames, names) ||
				!slices.Equal(r.ComponentPaths, dockerfiles) ||
				!slices.Equal(r.TektonPaths, tektonPaths) {
				name, dockerfile, tektonPath := "", "", ""
				if len(r.ComponentNames) > 0 {
					name = r.ComponentNames[0]
				}
				if len(r.ComponentPaths) > 0 {
					dockerfile = r.ComponentPaths[0]
				}
				if len(r.TektonPaths) > 0 {
					tektonPath = r.TektonPaths[0]
				}
				return fmt.Errorf("smoke runs require the %s selection; got component %q, Dockerfile %q, and Tekton pipeline %q", SmokeComponentName, name, dockerfile, tektonPath)
			}
		}
		seenComponents := make(map[string]struct{}, len(r.ComponentNames))
		for _, name := range r.ComponentNames {
			if _, duplicate := seenComponents[name]; duplicate {
				return fmt.Errorf("duplicate component name %q", name)
			}
			seenComponents[name] = struct{}{}
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

// validRunID reports whether value uses only allowed run ID characters and length:
// 1 to 100 ASCII letters, digits, '-', '_', or '.'.
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
