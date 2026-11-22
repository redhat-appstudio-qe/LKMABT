package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/model"
	"github.com/redhat-appstudio/lkmabt/internal/platform"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var (
	// PipelineRunGVR locates Tekton PipelineRuns.
	PipelineRunGVR = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}
	// TaskRunGVR locates Tekton TaskRuns.
	TaskRunGVR = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "taskruns"}
)

// BuildOutputInspector verifies that a PipelineRun's successful Buildah
// TaskRuns produced one image per expected platform. It does not require the
// PipelineRun to finish, so post-build tasks cannot mask verified build outputs.
type BuildOutputInspector struct {
	// Dynamic reads PipelineRuns and TaskRuns for Verify; VerifyListed ignores it.
	Dynamic dynamic.Interface
	// Platforms lists the canonical platforms each PipelineRun must produce.
	// Empty means every supported platform.
	Platforms []string
	// Now supplies the evidence timestamp; nil means time.Now.
	Now func() time.Time
}

// Verify collects build evidence for each identity and requires outputs for
// every expected platform. It requires Dynamic and at least one identity, and
// rejects configured platforms that are not canonical supported names. The
// first failing component aborts the call without returning partial evidence;
// all components in one call share a single evidence timestamp. The context
// bounds every API request.
func (i BuildOutputInspector) Verify(ctx context.Context, identities []model.PipelineRunIdentity) ([]model.BuildOutputEvidence, error) {
	if i.Dynamic == nil {
		return nil, fmt.Errorf("dynamic client is required")
	}
	if len(identities) == 0 {
		return nil, fmt.Errorf("at least one PipelineRun is required")
	}
	expected, err := i.expectedPlatforms()
	if err != nil {
		return nil, err
	}
	verifiedAt := i.verifiedAt()
	evidence := make([]model.BuildOutputEvidence, 0, len(identities))
	for _, identity := range identities {
		output, err := i.verifyPipelineRun(ctx, identity, expected, verifiedAt)
		if err != nil {
			return nil, err
		}
		evidence = append(evidence, output)
	}
	return evidence, nil
}

// VerifyListed verifies a PipelineRun and TaskRuns already listed in one poll,
// so a delete between a list and its evidence capture cannot lose evidence.
// It never uses the dynamic client and works with Dynamic unset; the evidence
// timestamp comes from Now.
func (i BuildOutputInspector) VerifyListed(identity model.PipelineRunIdentity, pipelineRun *unstructured.Unstructured, taskRuns []unstructured.Unstructured) (model.BuildOutputEvidence, error) {
	expected, err := i.expectedPlatforms()
	if err != nil {
		return model.BuildOutputEvidence{}, err
	}
	return i.verifyObjects(identity, pipelineRun, taskRuns, expected, i.verifiedAt())
}

// expectedPlatforms returns the requested canonical platforms, defaulting to
// platform.All(), and rejects values that are not canonical supported platforms.
func (i BuildOutputInspector) expectedPlatforms() ([]string, error) {
	expected := i.Platforms
	if len(expected) == 0 {
		expected = platform.All()
	}
	for _, buildPlatform := range expected {
		if !platform.Supported(buildPlatform) {
			return nil, fmt.Errorf("unsupported build platform %q", buildPlatform)
		}
	}
	return expected, nil
}

// verifiedAt returns the configured clock's UTC time, defaulting to time.Now.
func (i BuildOutputInspector) verifiedAt() time.Time {
	now := time.Now
	if i.Now != nil {
		now = i.Now
	}
	return now().UTC()
}

// verifyPipelineRun fetches one PipelineRun and every TaskRun in its namespace,
// then delegates to verifyObjects. The context bounds both requests.
func (i BuildOutputInspector) verifyPipelineRun(ctx context.Context, identity model.PipelineRunIdentity, expected []string, verifiedAt time.Time) (model.BuildOutputEvidence, error) {
	if strings.TrimSpace(identity.Namespace) == "" || strings.TrimSpace(identity.Name) == "" {
		return model.BuildOutputEvidence{}, fmt.Errorf("PipelineRun identity is incomplete")
	}
	pipelineRuns := i.Dynamic.Resource(PipelineRunGVR).Namespace(identity.Namespace)
	pipelineRun, err := pipelineRuns.Get(ctx, identity.Name, metav1.GetOptions{})
	if err != nil {
		return model.BuildOutputEvidence{}, fmt.Errorf("get PipelineRun %s/%s: %w", identity.Namespace, identity.Name, err)
	}
	taskRuns, err := i.Dynamic.Resource(TaskRunGVR).Namespace(identity.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return model.BuildOutputEvidence{}, fmt.Errorf("list TaskRuns for PipelineRun %s/%s: %w", identity.Namespace, identity.Name, err)
	}
	return i.verifyObjects(identity, pipelineRun, taskRuns.Items, expected, verifiedAt)
}

// verifyObjects validates a supplied PipelineRun and its TaskRuns and extracts
// the successful Buildah outputs. The PipelineRun must match the identity UID
// when set and carry a component identity; its own Succeeded condition is not a
// gate, because LKMABT stops at the build stage and post-build tasks are out of
// scope. A failed Buildah TaskRun is an error; TaskRuns that have not succeeded
// are ignored. Every expected platform must have an output URL and digest.
func (i BuildOutputInspector) verifyObjects(identity model.PipelineRunIdentity, pipelineRun *unstructured.Unstructured, taskRuns []unstructured.Unstructured, expected []string, verifiedAt time.Time) (model.BuildOutputEvidence, error) {
	if strings.TrimSpace(identity.Namespace) == "" || strings.TrimSpace(identity.Name) == "" {
		return model.BuildOutputEvidence{}, fmt.Errorf("PipelineRun identity is incomplete")
	}
	if pipelineRun == nil {
		return model.BuildOutputEvidence{}, fmt.Errorf("PipelineRun %s/%s is required", identity.Namespace, identity.Name)
	}
	if identity.UID != "" && pipelineRun.GetUID() != identity.UID {
		return model.BuildOutputEvidence{}, fmt.Errorf("PipelineRun %s/%s identity changed", identity.Namespace, identity.Name)
	}
	if identity.Component == "" {
		return model.BuildOutputEvidence{}, fmt.Errorf("PipelineRun %s/%s has no component identity", identity.Namespace, identity.Name)
	}
	createdOutputs := make(map[string]string)
	outputDigests := make(map[string]string)
	for index := range taskRuns {
		taskRun := &taskRuns[index]
		if !ownedByPipelineRun(taskRun, string(pipelineRun.GetUID()), pipelineRun.GetName()) {
			continue
		}
		if !IsBuildTaskRun(taskRun) {
			continue
		}
		if conditionStatus(taskRun) == "False" {
			return model.BuildOutputEvidence{}, fmt.Errorf("build TaskRun %s/%s failed", taskRun.GetNamespace(), taskRun.GetName())
		}
		if conditionStatus(taskRun) != "True" {
			continue
		}
		platform, output, digest := taskRunOutput(taskRun)
		if output == "" || digest == "" {
			return model.BuildOutputEvidence{}, fmt.Errorf("build TaskRun %s/%s is missing IMAGE_URL or IMAGE_DIGEST", taskRun.GetNamespace(), taskRun.GetName())
		}
		if platform == "" {
			return model.BuildOutputEvidence{}, fmt.Errorf("build TaskRun %s/%s has no platform label — pipeline misconfiguration", taskRun.GetNamespace(), taskRun.GetName())
		}
		// Duplicate platforms keep the last TaskRun's output; the pipeline should
		// produce one, but this verifier does not reject extras.
		createdOutputs[platform] = output
		outputDigests[platform] = digest
	}
	platforms := make([]string, 0, len(expected))
	for _, buildPlatform := range expected {
		if createdOutputs[buildPlatform] == "" {
			return model.BuildOutputEvidence{}, fmt.Errorf("PipelineRun %s/%s build outputs do not prove %s", identity.Namespace, identity.Name, strings.Join(expected, ", "))
		}
		platforms = append(platforms, buildPlatform)
	}
	verifiedIdentity := identity
	verifiedIdentity.UID = pipelineRun.GetUID()
	verifiedIdentity.Succeeded = true
	return model.BuildOutputEvidence{Component: identity.Component, PipelineRun: verifiedIdentity, Platforms: platforms, CreatedOutputs: createdOutputs, OutputDigests: outputDigests, VerifiedAt: verifiedAt}, nil
}

// ownedByPipelineRun reports whether a TaskRun has a PipelineRun owner
// reference. It matches the owner UID when uid is non-empty and falls back to
// the owner name when uid is empty.
func ownedByPipelineRun(taskRun *unstructured.Unstructured, uid, name string) bool {
	owners, found, err := unstructured.NestedSlice(taskRun.Object, "metadata", "ownerReferences")
	if err != nil || !found {
		return false
	}
	for _, raw := range owners {
		owner, ok := raw.(map[string]any)
		if !ok || owner["kind"] != "PipelineRun" {
			continue
		}
		ownerUID, _ := owner["uid"].(string)
		ownerName, _ := owner["name"].(string)
		if (uid != "" && ownerUID == uid) || (uid == "" && ownerName == name) {
			return true
		}
	}
	return false
}

// OwnedByPipelineRun reports whether a TaskRun belongs to the PipelineRun
// identified by uid or, when uid is empty, by name.
func OwnedByPipelineRun(taskRun *unstructured.Unstructured, uid, name string) bool {
	return ownedByPipelineRun(taskRun, uid, name)
}

// conditionStatus returns the Succeeded condition status, or Unknown when the
// condition is absent or malformed.
func conditionStatus(object *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
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

// IsBuildTaskRun reports whether a TaskRun is the build-container or
// build-images pipeline task and identifies its task as Buildah through the
// tekton.dev/task label or the taskRef name parameter.
func IsBuildTaskRun(taskRun *unstructured.Unstructured) bool {
	labels := taskRun.GetLabels()
	pipelineTask := labels["tekton.dev/pipelineTask"]
	if pipelineTask != "build-container" && pipelineTask != "build-images" {
		return false
	}
	if strings.Contains(strings.ToLower(labels["tekton.dev/task"]), "buildah") {
		return true
	}
	params, _, _ := unstructured.NestedSlice(taskRun.Object, "spec", "taskRef", "params")
	for _, raw := range params {
		param, ok := raw.(map[string]any)
		if !ok || param["name"] != "name" {
			continue
		}
		value, _ := param["value"].(string)
		return strings.Contains(strings.ToLower(value), "buildah")
	}
	return false
}

// taskRunOutput extracts the normalized platform, image URL, and digest from a
// TaskRun. A result whose name contains PLATFORM overrides the platform
// declared by metadata or params, and a later matching result wins; an
// unrecognized platform is returned as an empty string.
func taskRunOutput(taskRun *unstructured.Unstructured) (string, string, string) {
	platform := taskRunPlatform(taskRun)
	imageURL := ""
	digest := ""
	results, _, _ := unstructured.NestedSlice(taskRun.Object, "status", "results")
	for _, raw := range results {
		result, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := result["name"].(string)
		value, _ := result["value"].(string)
		if strings.Contains(strings.ToUpper(name), "PLATFORM") {
			if normalized := normalizeBuildPlatform(value); normalized != "" {
				platform = normalized
			}
		}
		if strings.TrimSpace(value) == "" {
			continue
		}
		switch strings.ToUpper(name) {
		case "IMAGE_URL":
			imageURL = value
		case "IMAGE_DIGEST":
			digest = value
		}
	}
	if !isBuildPlatform(platform) {
		platform = ""
	}
	return platform, imageURL, digest
}

// BuildTaskRunPlatform returns the canonical platform a Buildah TaskRun
// declares in its labels, annotations, spec, or results.
func BuildTaskRunPlatform(taskRun *unstructured.Unstructured) string {
	taskPlatform, _, _ := taskRunOutput(taskRun)
	return taskPlatform
}

// taskRunPlatform returns the first recognized platform declared by the
// build.appstudio.redhat.com/platform, build.appstudio.redhat.com/target-platform,
// or build.appstudio.openshift.io/platform label, then annotation, then spec
// PLATFORM param. It returns "" when none normalize and does not read results.
func taskRunPlatform(taskRun *unstructured.Unstructured) string {
	for _, values := range []map[string]string{taskRun.GetLabels(), taskRun.GetAnnotations()} {
		for _, key := range []string{
			"build.appstudio.redhat.com/platform",
			"build.appstudio.redhat.com/target-platform",
			"build.appstudio.openshift.io/platform",
		} {
			if platform := normalizeBuildPlatform(values[key]); platform != "" {
				return platform
			}
		}
	}
	params, _, _ := unstructured.NestedSlice(taskRun.Object, "spec", "params")
	for _, raw := range params {
		param, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := param["name"].(string)
		if strings.EqualFold(name, "PLATFORM") {
			if value, ok := param["value"].(string); ok {
				if platform := normalizeBuildPlatform(value); platform != "" {
					return platform
				}
			}
		}
	}
	return ""
}

func isBuildPlatform(value string) bool {
	return normalizeBuildPlatform(value) != ""
}

func normalizeBuildPlatform(value string) string {
	return platform.Normalize(value)
}
