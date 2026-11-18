package cleanup

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

const (
	ManagedByLabel = "app.konflux-ci.org/managed-by"
	RunIDLabel     = "app.konflux.org/run-id"
	ManagedByValue = "lkmabt"
)

var NamespaceGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// NamespaceInfo is a namespace's name, ownership labels, run ID, and object
// identity; UID and ResourceVersion are the approval and delete preconditions.
type NamespaceInfo struct {
	Name            string
	RunID           string
	UID             types.UID
	ResourceVersion string
	Labels          map[string]string
}

// Owned reports whether the namespace carries both exact LKMABT ownership labels.
func (i NamespaceInfo) Owned() bool {
	return i.Labels[ManagedByLabel] == ManagedByValue && strings.TrimSpace(i.RunID) != ""
}

type NamespaceNameLister interface {
	// List returns the names of namespaces visible to the lister.
	List(context.Context) ([]string, error)
}

type NamespaceService struct {
	Dynamic      dynamic.Interface
	NameLister   NamespaceNameLister
	PollInterval time.Duration
}

type OCNamespaceNameLister struct {
	Command    string
	Kubeconfig string
}

// List runs the configured namespace CLI to retrieve and parse namespace names.
// An empty Kubeconfig inherits the ambient environment, so the CLI targets
// whatever cluster the process's current context points at.
func (l OCNamespaceNameLister) List(ctx context.Context) ([]string, error) {
	name := strings.TrimSpace(l.Command)
	if name == "" {
		name = strings.TrimSpace(os.Getenv("LKMABT_OC"))
	}
	if name == "" {
		name = "oc"
	}
	var stderr bytes.Buffer
	command := exec.CommandContext(ctx, name, "get", "namespaces", "-o", "name", "--no-headers")
	// List the selected cluster: bind the CLI to the run's kubeconfig, never the ambient one.
	if kubeconfig := strings.TrimSpace(l.Kubeconfig); kubeconfig != "" {
		command.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	}
	output, err := command.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr.Write(exitErr.Stderr)
		}
		return nil, fmt.Errorf("list namespaces with %s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return parseNamespaceNames(output)
}

// parseNamespaceNames validates, deduplicates, and sorts namespace names from CLI output.
func parseNamespaceNames(output []byte) ([]string, error) {
	names := make([]string, 0)
	seen := make(map[string]struct{})
	for _, line := range strings.Split(string(output), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		name = strings.TrimPrefix(name, "namespace/")
		name = strings.TrimPrefix(name, "namespaces/")
		if strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("unexpected namespace listing line %q", line)
		}
		if _, found := seen[name]; found {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Inspect fetches one namespace and returns its name, labels, and run ID.
func (s NamespaceService) Inspect(ctx context.Context, name string) (NamespaceInfo, error) {
	if s.Dynamic == nil {
		return NamespaceInfo{}, fmt.Errorf("dynamic client is required")
	}
	if name == "" {
		return NamespaceInfo{}, fmt.Errorf("namespace name is required")
	}
	object, err := s.Dynamic.Resource(NamespaceGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return NamespaceInfo{}, err
	}
	return namespaceInfo(object), nil
}

// FindScoped returns every namespace named target or prefixed with "target-", owned or not.
func (s NamespaceService) FindScoped(ctx context.Context, target string) ([]NamespaceInfo, error) {
	if s.Dynamic == nil {
		return nil, fmt.Errorf("dynamic client is required")
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("tenant namespace is required")
	}
	lister := s.NameLister
	if lister == nil {
		lister = OCNamespaceNameLister{}
	}
	names, err := lister.List(ctx)
	if err != nil {
		return nil, err
	}
	scoped := make([]NamespaceInfo, 0, len(names))
	// names are already deduplicated and sorted by parseNamespaceNames.
	for _, name := range names {
		if name != target && !strings.HasPrefix(name, target+"-") {
			continue
		}
		info, err := s.Inspect(ctx, name)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect tenant namespace %s: %w", name, err)
		}
		scoped = append(scoped, info)
	}
	sort.Slice(scoped, func(i, j int) bool { return scoped[i].Name < scoped[j].Name })
	return scoped, nil
}

// Delete removes name only after confirming exact manager and run ID ownership labels.
func (s NamespaceService) Delete(ctx context.Context, name, runID string) error {
	if runID == "" {
		return fmt.Errorf("run ID is required for namespace deletion")
	}
	info, err := s.Inspect(ctx, name)
	if err != nil {
		return fmt.Errorf("re-read namespace %s: %w", name, err)
	}
	if info.Name != name {
		return fmt.Errorf("namespace identity mismatch: expected %q, got %q", name, info.Name)
	}
	if info.Labels[ManagedByLabel] != ManagedByValue || info.RunID != runID {
		return fmt.Errorf("refusing to delete namespace %q without exact LKMABT ownership labels", name)
	}
	_, _ = StripAllFinalizers(ctx, s.Dynamic, name)
	if err := s.Dynamic.Resource(NamespaceGVR).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete namespace %s: %w", name, err)
	}
	return nil
}

// DeleteApproved removes an explicitly approved namespace after re-reading it and
// confirming its name, identity, labels, and run ID are unchanged since the
// approval. The delete request carries the approved UID and resourceVersion as
// preconditions, so a replaced object fails the deletion. Finalizer stripping
// runs after that re-inspection; its failure aborts the deletion.
func (s NamespaceService) DeleteApproved(ctx context.Context, approved NamespaceInfo) error {
	if strings.TrimSpace(approved.Name) == "" {
		return fmt.Errorf("namespace name is required")
	}
	current, err := s.Inspect(ctx, approved.Name)
	if err != nil {
		return fmt.Errorf("re-read namespace %s: %w", approved.Name, err)
	}
	// Any drift since the user's approval, including object identity, invalidates the approval.
	if current.Name != approved.Name || current.RunID != approved.RunID || current.UID != approved.UID || current.ResourceVersion != approved.ResourceVersion || !maps.Equal(current.Labels, approved.Labels) {
		return fmt.Errorf("namespace %q changed after approval; refusing to delete", approved.Name)
	}
	if _, err := StripAllFinalizers(ctx, s.Dynamic, approved.Name); err != nil {
		return fmt.Errorf("strip finalizers in namespace %s: %w", approved.Name, err)
	}
	options := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &approved.UID, ResourceVersion: &approved.ResourceVersion}}
	if err := s.Dynamic.Resource(NamespaceGVR).Delete(ctx, approved.Name, options); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete namespace %s: %w", approved.Name, err)
	}
	return nil
}

// finalizerStripInterval bounds how often WaitDeleted re-strips finalizers that
// controllers can re-add after Delete's initial strip.
var finalizerStripInterval = 5 * time.Second

// WaitDeleted polls until name no longer exists or ctx is canceled. It re-strips
// finalizers because controllers can re-add them after Delete, which would
// otherwise leave the namespace stuck terminating.
func (s NamespaceService) WaitDeleted(ctx context.Context, name string) error {
	if s.Dynamic == nil {
		return fmt.Errorf("dynamic client is required")
	}
	interval := s.PollInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	var stripErr error
	lastStrip := time.Time{}
	for {
		_, err := s.Dynamic.Resource(NamespaceGVR).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("wait for namespace %s deletion: %w", name, err)
		}
		if time.Since(lastStrip) >= finalizerStripInterval {
			lastStrip = time.Now()
			if _, err := StripAllFinalizers(ctx, s.Dynamic, name); err != nil {
				stripErr = err
			}
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if stripErr != nil {
				return fmt.Errorf("wait for namespace %s deletion: %w (last finalizer strip: %v)", name, ctx.Err(), stripErr)
			}
			return fmt.Errorf("wait for namespace %s deletion: %w", name, ctx.Err())
		case <-timer.C:
		}
	}
}

// DeletionBlocker summarizes why a terminating namespace has not disappeared.
// It returns an empty string when the namespace is gone or reports no blockers.
func (s NamespaceService) DeletionBlocker(ctx context.Context, name string) string {
	if s.Dynamic == nil {
		return ""
	}
	object, err := s.Dynamic.Resource(NamespaceGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	blockers := make([]string, 0, 2)
	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		conditionType, _ := condition["type"].(string)
		if conditionType != "NamespaceContentRemaining" && conditionType != "NamespaceFinalizersRemaining" {
			continue
		}
		if status, _ := condition["status"].(string); status != "True" {
			continue
		}
		if message, _ := condition["message"].(string); strings.TrimSpace(message) != "" {
			blockers = append(blockers, strings.TrimSpace(message))
		}
	}
	return strings.Join(blockers, "; ")
}

// namespaceInfo copies namespace identity and labels into the service's value representation.
func namespaceInfo(object *unstructured.Unstructured) NamespaceInfo {
	labels := object.GetLabels()
	copyLabels := make(map[string]string, len(labels))
	for key, value := range labels {
		copyLabels[key] = value
	}
	return NamespaceInfo{Name: object.GetName(), RunID: labels[RunIDLabel], UID: object.GetUID(), ResourceVersion: object.GetResourceVersion(), Labels: copyLabels}
}
