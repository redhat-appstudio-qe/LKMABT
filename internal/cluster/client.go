package cluster

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/platform"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// AccessCheck describes one API resource preflight check.
type AccessCheck struct {
	Name string
	// Resource identifies the API group, version, and resource.
	Resource schema.GroupVersionResource
	// Namespace scopes the verb authorization and is required when Verbs is set.
	Namespace string
	// Verbs requests a SelfSubjectAccessReview for each verb in Namespace.
	Verbs []string
	// Guidance is appended to failure details so preflight errors are actionable.
	Guidance string
}

// CheckResult is the outcome of one preflight check.
type CheckResult struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Details string `json:"details,omitempty"`
}

// MultiPlatformController* name the multi-platform-controller objects that
// preflight checks.
const (
	MultiPlatformControllerNamespace   = "multi-platform-controller"
	MultiPlatformControllerDeployment  = "multi-platform-controller"
	MultiPlatformControllerConfigMap   = "host-config"
	MultiPlatformControllerConfigLabel = "build.appstudio.redhat.com/multi-platform-config"
)

// SchedulingSpec names the controller objects and platforms to verify.
// Namespace, Deployment, and ConfigMap are required; an empty Platforms list
// checks only controller readiness and the config map label.
type SchedulingSpec struct {
	Namespace  string
	Deployment string
	ConfigMap  string
	Platforms  []string
}

// SchedulingResult reports controller readiness and per-platform host-config
// support. Checks are hard prerequisites; Platforms classifies each requested
// platform so callers can narrow a selection without treating an undeclared
// architecture as a preflight failure.
type SchedulingResult struct {
	Checks    []CheckResult     `json:"checks"`
	Platforms []PlatformSupport `json:"platforms,omitempty"`
}

// PlatformSupport classifies one requested platform against the cluster's
// live multi-platform-controller host-config.
type PlatformSupport struct {
	Platform string `json:"platform"`
	// Supported is true when host-config can schedule the platform.
	Supported bool `json:"supported"`
	// Configured is true when host-config declares the platform at all,
	// whether or not the declaration is complete.
	Configured bool   `json:"configured"`
	Details    string `json:"details,omitempty"`
}

// PipelineRunSelector restricts PipelineRun lookups by label.
type PipelineRunSelector struct {
	Labels map[string]string
}

// Cluster is the cluster operations the workflow depends on.
type Cluster interface {
	// Server returns the API server URL for the connected cluster.
	Server(context.Context) (string, error)
	// CheckAccess checks served API group versions and, when requested, verb
	// authorization in the check namespace.
	CheckAccess(context.Context, []AccessCheck) []CheckResult
	// CheckScheduling validates controller readiness and classifies platform
	// configuration.
	CheckScheduling(context.Context, SchedulingSpec) SchedulingResult
}

// ClientSet holds one REST config and the typed, dynamic, and discovery clients
// built from it.
type ClientSet struct {
	Config     *rest.Config
	Kubernetes kubernetes.Interface
	Dynamic    dynamic.Interface
	Discovery  discovery.DiscoveryInterface
}

// NewClientSet creates clients from the kubeconfig at path. An empty path uses
// the ambient KUBECONFIG resolution chain. Client configuration, including the
// default request timeout, comes from NewClientSetFromConfig.
func NewClientSet(kubeconfig string) (*ClientSet, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return NewClientSetFromConfig(config)
}

// ServerFromKubeconfig returns the API server URL recorded in the kubeconfig at
// path, without a trailing slash. An empty path uses the ambient kubeconfig
// resolution.
func ServerFromKubeconfig(path string) (string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		loadingRules.ExplicitPath = path
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return "", fmt.Errorf("load kubeconfig: %w", err)
	}
	return strings.TrimRight(config.Host, "/"), nil
}

// NewClientSetFromConfig creates Kubernetes, dynamic, and discovery clients from
// config, which must not be nil. When config.Timeout is zero, the returned
// ClientSet uses a copy with a 20-second request timeout; an explicit timeout
// and all other fields are preserved.
func NewClientSetFromConfig(config *rest.Config) (*ClientSet, error) {
	if config == nil {
		return nil, fmt.Errorf("rest config is nil")
	}
	if config.Timeout == 0 {
		config = rest.CopyConfig(config)
		config.Timeout = 20 * time.Second
	}
	kubernetesClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}
	return &ClientSet{Config: config, Kubernetes: kubernetesClient, Dynamic: dynamicClient, Discovery: discoveryClient}, nil
}

// RealCluster implements Cluster against a live API server.
type RealCluster struct {
	Clients *ClientSet
}

// Server returns the configured cluster host without trailing slashes. It
// performs no API call and ignores the context.
func (c *RealCluster) Server(context.Context) (string, error) {
	if c == nil || c.Clients == nil || c.Clients.Config == nil {
		return "", fmt.Errorf("cluster clients are not initialized")
	}
	return strings.TrimRight(c.Clients.Config.Host, "/"), nil
}

// CheckAccess reports whether each check's API group version is served by the
// cluster and, when Verbs is non-empty, whether the current user may perform
// every verb in the check namespace. The context bounds the authorization
// reviews; discovery requests use the client's configured timeout. Failures are
// returned as failed results, never as errors.
func (c *RealCluster) CheckAccess(ctx context.Context, checks []AccessCheck) []CheckResult {
	results := make([]CheckResult, 0, len(checks))
	for _, check := range checks {
		result := CheckResult{Name: check.Name}
		if c == nil || c.Clients == nil || c.Clients.Discovery == nil {
			result.Details = "discovery client is not initialized"
			results = append(results, result)
			continue
		}
		_, err := c.Clients.Discovery.ServerResourcesForGroupVersion(check.Resource.GroupVersion().String())
		result.Passed = err == nil
		if err != nil {
			result.Details = err.Error()
			results = append(results, result)
			continue
		}
		if len(check.Verbs) == 0 {
			results = append(results, result)
			continue
		}
		result.Details = AuthorizeVerbs(ctx, c.Clients.Kubernetes, check)
		result.Passed = result.Details == ""
		results = append(results, result)
	}
	return results
}

// AuthorizeVerbs returns failure details when the current user may not perform
// every verb in check; it returns an empty string when all verbs are allowed.
// It verifies authorization only, not that the resource is served.
func AuthorizeVerbs(ctx context.Context, kubernetesClient kubernetes.Interface, check AccessCheck) string {
	return authorizeVerbs(ctx, kubernetesClient, check)
}

// authorizeVerbs returns failure details from one SelfSubjectAccessReview per verb,
// or an empty string when every request is allowed.
func authorizeVerbs(ctx context.Context, kubernetesClient kubernetes.Interface, check AccessCheck) string {
	if kubernetesClient == nil {
		return withGuidance("kubernetes client is not initialized", check.Guidance)
	}
	if strings.TrimSpace(check.Namespace) == "" {
		return withGuidance(fmt.Sprintf("namespace is required to authorize %s", check.Resource.Resource), check.Guidance)
	}
	failures := make([]string, 0, len(check.Verbs))
	for _, verb := range check.Verbs {
		review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: check.Namespace,
				Verb:      verb,
				Group:     check.Resource.Group,
				Resource:  check.Resource.Resource,
			},
		}}
		response, err := kubernetesClient.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s %s in namespace %s: %v", verb, check.Resource.Resource, check.Namespace, err))
			continue
		}
		if response.Status.Allowed {
			continue
		}
		reason := strings.TrimSpace(response.Status.Reason)
		if reason == "" {
			reason = "no reason reported"
		}
		failure := fmt.Sprintf("%s %s in namespace %s denied: %s", verb, check.Resource.Resource, check.Namespace, reason)
		if evaluationError := strings.TrimSpace(response.Status.EvaluationError); evaluationError != "" {
			failure += fmt.Sprintf(" (evaluation error: %s)", evaluationError)
		}
		failures = append(failures, failure)
	}
	if len(failures) == 0 {
		return ""
	}
	return withGuidance(strings.Join(failures, "; "), check.Guidance)
}

// withGuidance appends actionable guidance to a check failure.
func withGuidance(details, guidance string) string {
	if strings.TrimSpace(guidance) == "" {
		return details
	}
	return details + "; " + guidance
}

// CheckScheduling verifies that the controller deployment is available and
// classifies every requested platform against the live host-config ConfigMap.
// Checks are hard prerequisites; Platforms separates undeclared platforms,
// which callers may narrow around, from declarations that are present but
// unusable, which remain failures.
//
// The live ConfigMap is authoritative for this cluster. The host-values.yaml
// files in infra-deployments are its source, but reading the deployed state
// avoids guessing the cluster's ring and adds no network dependency.
func (c *RealCluster) CheckScheduling(ctx context.Context, spec SchedulingSpec) SchedulingResult {
	if c == nil || c.Clients == nil || c.Clients.Kubernetes == nil {
		return SchedulingResult{Checks: []CheckResult{{Name: "multi-platform-controller", Details: "kubernetes client is not initialized"}}}
	}
	if strings.TrimSpace(spec.Namespace) == "" || strings.TrimSpace(spec.Deployment) == "" || strings.TrimSpace(spec.ConfigMap) == "" {
		return SchedulingResult{Checks: []CheckResult{{Name: "multi-platform-controller", Details: "controller namespace, deployment, and config map are required"}}}
	}
	deployment, err := c.Clients.Kubernetes.AppsV1().Deployments(spec.Namespace).Get(ctx, spec.Deployment, metav1.GetOptions{})
	if err != nil {
		return SchedulingResult{Checks: []CheckResult{{Name: spec.Deployment, Details: fmt.Sprintf("get controller deployment: %v", err)}}}
	}
	if !controllerDeploymentReady(deployment) {
		return SchedulingResult{Checks: []CheckResult{{Name: spec.Deployment, Details: fmt.Sprintf("controller deployment is not ready: generation=%d observed=%d ready=%d available=%d updated=%d", deployment.Generation, deployment.Status.ObservedGeneration, deployment.Status.ReadyReplicas, deployment.Status.AvailableReplicas, deployment.Status.UpdatedReplicas)}}}
	}
	configMap, err := c.Clients.Kubernetes.CoreV1().ConfigMaps(spec.Namespace).Get(ctx, spec.ConfigMap, metav1.GetOptions{})
	if err != nil {
		return SchedulingResult{Checks: []CheckResult{{Name: spec.ConfigMap, Details: fmt.Sprintf("get controller host configuration: %v", err)}}}
	}
	if strings.TrimSpace(configMap.Labels[MultiPlatformControllerConfigLabel]) == "" {
		return SchedulingResult{Checks: []CheckResult{{Name: spec.ConfigMap, Details: "controller host configuration is not labeled for multi-platform-controller"}}}
	}
	result := SchedulingResult{Checks: []CheckResult{{Name: spec.Deployment, Passed: true, Details: "controller deployment is available"}}}
	for _, requested := range spec.Platforms {
		result.Platforms = append(result.Platforms, platformSupport(configMap.Data, requested))
	}
	return result
}

// platformSupport classifies one platform against host-config. An undeclared
// platform is not a failure; a declared but incomplete or untyped platform is.
func platformSupport(data map[string]string, target string) PlatformSupport {
	support := PlatformSupport{Platform: target}
	scheduling := configuredPlatform(data, target)
	if scheduling.mode == "" {
		support.Details = "platform is not configured in host-config"
		return support
	}
	support.Configured = true
	if scheduling.mode == "static" {
		host, details := staticHostForPlatform(data, target, scheduling.hosts)
		if host == "" {
			support.Details = details
			return support
		}
		support.Supported = true
		support.Details = fmt.Sprintf("platform is configured via static host %s", host)
		return support
	}
	if scheduling.mode == "dynamic" || scheduling.mode == "dynamic-pool" {
		prefix := "dynamic." + strings.ReplaceAll(target, "/", "-") + "."
		if strings.TrimSpace(data[prefix+"type"]) == "" {
			support.Details = fmt.Sprintf("%s platform has no provider type", scheduling.mode)
			return support
		}
	}
	support.Supported = true
	support.Details = fmt.Sprintf("platform is configured via %s host scheduling", scheduling.mode)
	return support
}

// controllerDeploymentReady reports whether the deployment has observed and available desired replicas.
func controllerDeploymentReady(deployment *appsv1.Deployment) bool {
	if deployment == nil {
		return false
	}
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	return desired > 0 && deployment.Status.ObservedGeneration >= deployment.Generation && deployment.Status.ReadyReplicas >= desired && deployment.Status.AvailableReplicas >= desired && deployment.Status.UpdatedReplicas >= desired
}

// platformScheduling describes how host-config provides a platform.
type platformScheduling struct {
	mode  string
	hosts []string
}

// configuredPlatform returns the host-config scheduling mode for a platform.
// Static hosts declare their platform per host instead of in a platform list;
// every matching host is returned so one incomplete entry cannot mask a usable
// host.
func configuredPlatform(data map[string]string, target string) platformScheduling {
	wanted := platform.Normalize(target)
	if wanted == "" {
		return platformScheduling{}
	}
	for _, entry := range []struct {
		key  string
		mode string
	}{
		{key: "local-platforms", mode: "local"},
		{key: "dynamic-platforms", mode: "dynamic"},
		{key: "dynamic-pool-platforms", mode: "dynamic-pool"},
	} {
		for _, configured := range strings.Split(data[entry.key], ",") {
			if platform.Normalize(strings.TrimSpace(configured)) == wanted {
				return platformScheduling{mode: entry.mode}
			}
		}
	}
	hosts := make([]string, 0)
	for key, value := range data {
		if !strings.HasPrefix(key, "host.") || !strings.HasSuffix(key, ".platform") {
			continue
		}
		if platform.Normalize(strings.TrimSpace(value)) != wanted {
			continue
		}
		host := strings.TrimSuffix(strings.TrimPrefix(key, "host."), ".platform")
		if host != "" {
			hosts = append(hosts, host)
		}
	}
	if len(hosts) == 0 {
		return platformScheduling{}
	}
	slices.Sort(hosts)
	return platformScheduling{mode: "static", hosts: hosts}
}

// missingStaticHostFields lists required host-config fields absent from a
// static host entry. Concurrency is optional and validated by the controller.
func missingStaticHostFields(data map[string]string, host string) []string {
	missing := make([]string, 0, 3)
	for _, field := range []string{"address", "user", "secret"} {
		if strings.TrimSpace(data["host."+host+"."+field]) == "" {
			missing = append(missing, field)
		}
	}
	return missing
}

// staticHostForPlatform returns a complete static host for the platform, or a
// detail string naming every incomplete matching host.
func staticHostForPlatform(data map[string]string, target string, hosts []string) (string, string) {
	unavailable := make([]string, 0, len(hosts))
	for _, host := range hosts {
		if missing := missingStaticHostFields(data, host); len(missing) > 0 {
			unavailable = append(unavailable, fmt.Sprintf("%s (missing %s)", host, strings.Join(missing, ", ")))
			continue
		}
		return host, ""
	}
	return "", fmt.Sprintf("no usable static host for %s: %s", target, strings.Join(unavailable, "; "))
}
