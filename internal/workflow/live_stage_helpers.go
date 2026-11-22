package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	"github.com/redhat-appstudio/lkmabt/internal/model"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// notify calls a progress reporter, tolerating a stage without one.
func notify(notice func(string, ...any), format string, args ...any) {
	if notice == nil {
		return
	}
	notice(format, args...)
}

// awaitPoll waits one polling interval, calling status on each heartbeat tick.
// heartbeat must be a running ticker. It reports false when the context ends
// before the interval elapses and true when the interval completes. status runs
// synchronously in the caller's goroutine.
func awaitPoll(ctx context.Context, interval time.Duration, heartbeat *time.Ticker, status func()) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-heartbeat.C:
			status()
		case <-timer.C:
			return true
		}
	}
}

// componentPaCState returns the PaC onboarding state recorded in a component's
// build.appstudio.openshift.io/status annotation. It returns an empty string
// when the object, annotation, or JSON is missing or malformed.
func componentPaCState(object *unstructured.Unstructured) string {
	if object == nil {
		return ""
	}
	var annotation struct {
		PaC struct {
			State string `json:"state"`
		} `json:"pac"`
	}
	if err := json.Unmarshal([]byte(object.GetAnnotations()["build.appstudio.openshift.io/status"]), &annotation); err != nil {
		return ""
	}
	return annotation.PaC.State
}

// paCWaitStatus renders enabled/total and the pending component names in the
// requested order. Components with no matching object count as pending.
func paCWaitStatus(names []string, objects []unstructured.Unstructured) string {
	byName := make(map[string]*unstructured.Unstructured, len(objects))
	for index := range objects {
		byName[objects[index].GetName()] = &objects[index]
	}
	enabled := 0
	pending := make([]string, 0, len(names))
	for _, name := range names {
		if componentPaCState(byName[name]) == "enabled" {
			enabled++
			continue
		}
		pending = append(pending, name)
	}
	if len(pending) == 0 {
		return fmt.Sprintf("PaC onboarding: %d/%d enabled", enabled, len(names))
	}
	return fmt.Sprintf("PaC onboarding: %d/%d enabled; pending %s", enabled, len(names), strings.Join(pending, ", "))
}

// buildWaitStatus names each component's current build phase in trigger order.
// captured marks components whose outputs were already evidenced; a nil
// captured treats every component as uncaptured. admissionReason supplies an
// optional Kueue diagnostic for admission-pending runs. expectedPlatforms
// prevents a partially created matrix from being reported as success.
func buildWaitStatus(commits []model.TriggerCommit, pipelineRuns, taskRuns []unstructured.Unstructured, now time.Time, captured func(string) bool, admissionReason func(*unstructured.Unstructured) string, expectedPlatforms []string) string {
	parts := make([]string, 0, len(commits))
	for _, commit := range commits {
		parts = append(parts, commit.Component+": "+buildComponentStatus(commit, pipelineRuns, taskRuns, now, captured, admissionReason, expectedPlatforms))
	}
	return "builds: " + strings.Join(parts, "; ")
}

// buildComponentStatus describes one component as missing, failed, running, or
// succeeded. A captured component with no matching PipelineRun reports success
// from the recorded evidence. now supplies the age shown for pending runs.
func buildComponentStatus(commit model.TriggerCommit, pipelineRuns, taskRuns []unstructured.Unstructured, now time.Time, captured func(string) bool, admissionReason func(*unstructured.Unstructured) string, expectedPlatforms []string) string {
	isCaptured := captured != nil && captured(commit.Component)
	candidates := matchingPipelineRuns(pipelineRuns, commit)
	switch len(candidates) {
	case 0:
		if isCaptured {
			return "succeeded (outputs captured)"
		}
		return "no PipelineRun"
	case 1:
	default:
		return fmt.Sprintf("%d PipelineRuns after trigger", len(candidates))
	}
	candidate := candidates[0]
	buildTasks := buildTaskRunsForPipelineRun(taskRuns, candidate)
	succeeded := 0
	for _, taskRun := range buildTasks {
		if pipelineRunConditionStatus(taskRun) == "True" {
			succeeded++
		}
	}
	covered := buildPlatformsCovered(buildTasks, expectedPlatforms)
	if len(buildTasks) > 0 && succeeded == len(buildTasks) {
		if covered {
			return fmt.Sprintf("succeeded (PipelineRun %s, buildah %d/%d)", candidate.GetName(), succeeded, len(buildTasks))
		}
		return fmt.Sprintf("buildah %d/%d succeeded; waiting for platform results (PipelineRun %s)", succeeded, len(buildTasks), candidate.GetName())
	}
	switch status := pipelineRunConditionStatus(candidate); status {
	case "False":
		return fmt.Sprintf("failed (PipelineRun %s)", candidate.GetName())
	case "True":
		if len(buildTasks) == 0 {
			return fmt.Sprintf("PipelineRun %s succeeded; no Buildah TaskRuns", candidate.GetName())
		}
		if !covered {
			return fmt.Sprintf("PipelineRun %s succeeded; waiting for platform results (buildah %d/%d)", candidate.GetName(), succeeded, len(buildTasks))
		}
		return fmt.Sprintf("PipelineRun %s succeeded; buildah %d/%d succeeded", candidate.GetName(), succeeded, len(buildTasks))
	default:
		if admission := admissionStatusDetail(candidate, taskRuns, now, admissionReason); admission != "" {
			return admission
		}
		age := buildAge(candidate, now)
		if len(buildTasks) == 0 {
			return fmt.Sprintf("pending for %s (PipelineRun %s, no Buildah TaskRun)", age, candidate.GetName())
		}
		return fmt.Sprintf("running for %s (PipelineRun %s, buildah %d/%d succeeded)", age, candidate.GetName(), succeeded, len(buildTasks))
	}
}

// buildPlatformsCovered reports whether the listed Buildah TaskRuns expose
// every expected platform. An empty expected set disables the check.
func buildPlatformsCovered(buildTasks []*unstructured.Unstructured, expected []string) bool {
	if len(expected) == 0 {
		return true
	}
	observed := make(map[string]bool, len(buildTasks))
	for _, taskRun := range buildTasks {
		if taskPlatform := cluster.BuildTaskRunPlatform(taskRun); taskPlatform != "" {
			observed[taskPlatform] = true
		}
	}
	for _, taskPlatform := range expected {
		if !observed[taskPlatform] {
			return false
		}
	}
	return true
}

// buildAge renders how long a PipelineRun has been pending, rounded to the
// second, or "an unknown duration" when it has no creation timestamp.
func buildAge(object *unstructured.Unstructured, now time.Time) string {
	created := object.GetCreationTimestamp().Time
	if created.IsZero() {
		return "an unknown duration"
	}
	return now.Sub(created).Round(time.Second).String()
}

// candidatePipelineRunsForCommits lists the PipelineRuns a build wait would
// match each uncaptured trigger to, deduplicated by namespace and name and kept
// in trigger order. A nil captured treats every trigger as uncaptured.
func candidatePipelineRunsForCommits(objects []unstructured.Unstructured, commits []model.TriggerCommit, captured func(string) bool) []unstructured.Unstructured {
	result := make([]unstructured.Unstructured, 0, len(commits))
	seen := make(map[string]bool, len(commits))
	for _, commit := range commits {
		if captured != nil && captured(commit.Component) {
			continue
		}
		for _, candidate := range matchingPipelineRuns(objects, commit) {
			key := candidate.GetNamespace() + "/" + candidate.GetName()
			if seen[key] {
				continue
			}
			seen[key] = true
			result = append(result, *candidate)
		}
	}
	return result
}
