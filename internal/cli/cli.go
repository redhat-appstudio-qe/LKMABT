package cli

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
	"github.com/redhat-appstudio/lkmabt/internal/config"
	"github.com/redhat-appstudio/lkmabt/internal/model"
)

type RunOutcome struct {
	Provider          string
	RunID             string
	ArtifactDirectory string
	Stage             model.Phase
}

type Runner interface {
	// Run executes one configured provider workflow and writes its output to log.
	Run(ctx context.Context, request config.Request, log RunLog) (RunOutcome, error)
}

type ProviderResult struct {
	Provider          string
	RunID             string
	ArtifactDirectory string
	Stage             model.Phase
	Err               error
}

type CleanupRunner interface {
	// Cleanup verifies run evidence and namespace ownership, then asks before deleting the tenant namespace.
	Cleanup(context.Context, config.Request) error
}

type UnconfiguredRunner struct{}

// Run reports that no workflow services are configured.
func (UnconfiguredRunner) Run(context.Context, config.Request, RunLog) (RunOutcome, error) {
	return RunOutcome{}, errors.New("workflow services are not configured")
}

// Execute dispatches a run or cleanup request.
func Execute(ctx context.Context, req config.Request, runner Runner, console *Console) error {
	if runner == nil {
		runner = UnconfiguredRunner{}
	}
	switch req.Command {
	case config.CommandRun:
		return RunClusters(ctx, req, runner, console)
	case config.CommandCleanup:
		cleanupRunner, ok := runner.(CleanupRunner)
		if !ok {
			return errors.New("cleanup services are not configured")
		}
		return cleanupRunner.Cleanup(ctx, req)
	default:
		return errors.New("unsupported command")
	}
}

// providerNames lists the providers selected by a run request, in report order.
func providerNames(provider string) []string {
	if provider == config.ProviderBoth {
		return []string{config.ProviderGitHub, config.ProviderGitLab}
	}
	return []string{provider}
}

// runProviders runs every selected provider concurrently and returns one result per provider in fixed order.
func runProviders(ctx context.Context, request config.Request, runner Runner) []ProviderResult {
	names := providerNames(request.Provider)
	results := make([]ProviderResult, len(names))
	var waitGroup sync.WaitGroup
	for index, name := range names {
		waitGroup.Add(1)
		go func(index int, name string) {
			defer waitGroup.Done()
			log := newRunLog()
			defer log.Close()
			outcome, err := runner.Run(ctx, requestForProvider(request, name), log)
			results[index] = providerResult(name, outcome, err)
		}(index, name)
	}
	waitGroup.Wait()
	return results
}

// providerResult converts one workflow outcome into a report-ready result.
func providerResult(provider string, outcome RunOutcome, err error) ProviderResult {
	stage := outcome.Stage
	if stage == "" {
		stage = model.PhasePreflight
	}
	return ProviderResult{Provider: provider, RunID: outcome.RunID, ArtifactDirectory: outcome.ArtifactDirectory, Stage: stage, Err: err}
}

// requestForProvider creates a run request with the fixture and namespace for provider.
func requestForProvider(request config.Request, provider string) config.Request {
	// `both` splits one request into two, so each provider needs its fixed namespace;
	// a single-provider request already carries the namespace the caller asked for.
	both := request.Provider == config.ProviderBoth
	request.Provider = provider
	request.MultiProvider = both
	request.RunID = "run-" + uuid.NewString()
	switch provider {
	case config.ProviderGitHub:
		request.SourceRepository = config.DefaultGitHubRepo
	case config.ProviderGitLab:
		request.SourceRepository = config.DefaultGitLabRepo
	}
	if both {
		request.TenantNamespace = defaultTenantNamespace(provider)
	}
	return request
}

// defaultTenantNamespace returns the fixed tenant namespace configured for provider.
func defaultTenantNamespace(provider string) string {
	if provider == config.ProviderGitLab {
		return config.DefaultGitLabTenantNamespace
	}
	return config.DefaultGitHubTenantNamespace
}
