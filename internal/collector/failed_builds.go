package collector

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// TaskRunGVR selects v1 TaskRuns from the Tekton API group.
var TaskRunGVR = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "taskruns"}

// DefaultLogTailLines bounds each failed-step log to its last 80 lines.
const DefaultLogTailLines int64 = 80

// FailedBuildLog is the run manifest entry describing a failed TaskRun step.
type FailedBuildLog = model.FailedBuildLog

// FailedBuildCollector records failed PipelineRun TaskRuns and captures their
// failed-step logs. Dynamic is required; Kubernetes is optional and, when nil,
// leaves LogPath empty. TailLines defaults to DefaultLogTailLines when zero or
// negative.
type FailedBuildCollector struct {
	Dynamic    dynamic.Interface
	Kubernetes kubernetes.Interface
	TailLines  int64
}

// FailedStep identifies a terminated step container and its non-zero exit code.
type FailedStep struct {
	Container string
	ExitCode  int64
}

// FindFailedStepContainers returns terminated TaskRun steps with non-zero exit
// codes. A step without a container name is reported as "step-<name>".
func FindFailedStepContainers(taskRun *unstructured.Unstructured) []FailedStep {
	if taskRun == nil {
		return nil
	}
	steps, _, _ := unstructured.NestedSlice(taskRun.Object, "status", "steps")
	failed := make([]FailedStep, 0)
	for _, raw := range steps {
		step, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		terminated, ok := step["terminated"].(map[string]any)
		if !ok {
			continue
		}
		exitCode, ok := toInt64(terminated["exitCode"])
		if !ok || exitCode == 0 {
			continue
		}
		container, _ := step["container"].(string)
		if container == "" {
			name, _ := step["name"].(string)
			container = "step-" + strings.ReplaceAll(name, " ", "-")
		}
		failed = append(failed, FailedStep{Container: container, ExitCode: exitCode})
	}
	return failed
}

// toInt64 converts supported numeric values to int64 and reports whether conversion succeeded.
func toInt64(value any) (int64, bool) {
	switch value := value.(type) {
	case int64:
		return value, true
	case float64:
		return int64(value), true
	case int:
		return int64(value), true
	default:
		return 0, false
	}
}

// CollectFailedBuildLogs records failed TaskRuns and writes available
// failed-step logs. It returns one entry per failed step, or one entry without a
// step when the TaskRun has no terminated non-zero step. An empty outputDir or a
// missing pod name or Kubernetes client skips log files and leaves LogPath
// empty; a failed log fetch also leaves it empty without failing collection.
// Beyond argument validation and the initial directory creation, only listing
// TaskRuns returns an error. Namespace and pipelineRunName are required.
func (c FailedBuildCollector) CollectFailedBuildLogs(ctx context.Context, namespace, pipelineRunName, outputDir string) ([]FailedBuildLog, error) {
	if c.Dynamic == nil {
		return nil, fmt.Errorf("dynamic client is required")
	}
	if namespace == "" || pipelineRunName == "" {
		return nil, fmt.Errorf("namespace and pipelineRunName are required")
	}
	if outputDir != "" {
		if err := os.MkdirAll(filepath.Join(outputDir, "failed-builds"), 0o750); err != nil {
			return nil, fmt.Errorf("create failed-build log directory: %w", err)
		}
	}

	taskRuns, err := c.Dynamic.Resource(TaskRunGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: "tekton.dev/pipelineRun=" + pipelineRunName})
	if err != nil {
		return nil, fmt.Errorf("list TaskRuns for PipelineRun %s: %w", pipelineRunName, err)
	}

	logs := make([]FailedBuildLog, 0)
	for index := range taskRuns.Items {
		taskRun := &taskRuns.Items[index]
		if taskRunConditionStatus(taskRun) != "False" {
			continue
		}

		taskName := taskRun.GetLabels()["tekton.dev/pipelineTask"]
		conditionMessage := taskRunConditionMessage(taskRun)
		failedSteps := FindFailedStepContainers(taskRun)
		if len(failedSteps) == 0 {
			logs = append(logs, FailedBuildLog{PipelineRunName: pipelineRunName, TaskRunName: taskRun.GetName(), TaskName: taskName, ConditionMessage: conditionMessage})
			continue
		}

		podName, _, _ := unstructured.NestedString(taskRun.Object, "status", "podName")
		for _, step := range failedSteps {
			entry := FailedBuildLog{
				PipelineRunName:  pipelineRunName,
				TaskRunName:      taskRun.GetName(),
				TaskName:         taskName,
				StepContainer:    step.Container,
				ExitCode:         step.ExitCode,
				ConditionMessage: conditionMessage,
			}
			if podName != "" && c.Kubernetes != nil && outputDir != "" {
				logPath, writeErr := c.writeContainerLog(ctx, namespace, podName, step.Container, taskRun.GetName(), outputDir)
				if writeErr == nil {
					entry.LogPath = logPath
				}
			}
			logs = append(logs, entry)
		}
	}
	return logs, nil
}

// writeContainerLog fetches a bounded container log and returns its path under
// failed-builds. Fetching stops after 30 seconds, and TailLines bounds the
// response.
func (c FailedBuildCollector) writeContainerLog(ctx context.Context, namespace, podName, container, taskRunName, outputDir string) (string, error) {
	tailLines := c.TailLines
	if tailLines <= 0 {
		tailLines = DefaultLogTailLines
	}

	logCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stream, err := c.Kubernetes.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{Container: container, TailLines: &tailLines}).Stream(logCtx)
	if err != nil {
		return "", fmt.Errorf("get logs for %s/%s container %s: %w", namespace, podName, container, err)
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		return "", fmt.Errorf("read log stream for %s/%s container %s: %w", namespace, podName, container, err)
	}

	logDir := filepath.Join(outputDir, "failed-builds")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return "", err
	}
	safeTaskRun := safePathPart(taskRunName)
	safeContainer := safePathPart(container)
	fileName := safeTaskRun + "_" + safeContainer + ".log"
	if err := os.WriteFile(filepath.Join(logDir, fileName), data, 0o640); err != nil {
		return "", err
	}
	return filepath.Join("failed-builds", fileName), nil
}

// taskRunConditionStatus returns the TaskRun Succeeded condition or Unknown when absent.
func taskRunConditionStatus(taskRun *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(taskRun.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok || condition["type"] != "Succeeded" {
			continue
		}
		status, _ := condition["status"].(string)
		return status
	}
	return "Unknown"
}

// taskRunConditionMessage returns the TaskRun Succeeded condition message when present.
func taskRunConditionMessage(taskRun *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(taskRun.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok || condition["type"] != "Succeeded" {
			continue
		}
		message, _ := condition["message"].(string)
		return message
	}
	return ""
}
