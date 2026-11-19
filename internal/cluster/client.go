package cluster

import (
	"context"
	"fmt"
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

type AccessCheck struct {
	Name      string
	Resource  schema.GroupVersionResource
	Namespace string
	// Verbs requests a SelfSubjectAccessReview for each verb in Namespace.
	Verbs []string
	// Guidance is appended to failure details so preflight errors are actionable.
	Guidance string
}

type CheckResult struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Details string `json:"details,omitempty"`
}

const (
	MultiPlatformControllerNamespace   = "multi-platform-controller"
	MultiPlatformControllerDeployment  = "multi-platform-controller"
	MultiPlatformControllerConfigMap   = "host-config"
	MultiPlatformControllerConfigLabel = "build.appstudio.redhat.com/multi-platform-config"
)

type SchedulingSpec struct {
	Namespace  string
	Deployment string
	ConfigMap  string
	Platforms  []string
}

type PipelineRunSelector struct {
	Labels map[string]string
}

type Cluster interface {
	// Server returns the API server URL for the connected cluster.
	Server(context.Context) (string, error)
	// CheckAccess checks whether requested API group versions are served.
	CheckAccess(context.Context, []AccessCheck) []CheckResult
	// CheckScheduling validates controller readiness and platform configuration.
	CheckScheduling(context.Context, SchedulingSpec) []CheckResult
}

type ClientSet struct {
	Config     *rest.Config
	Kubernetes kubernetes.Interface
	Dynamic    dynamic.Interface
	Discovery  discovery.DiscoveryInterface
}

// NewClientSet loads kubeconfig and creates clients for the selected cluster.
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

// ServerFromKubeconfig returns the API server URL recorded in the kubeconfig at path.
func ServerFromKubeconfig(path string) (string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	// An empty path keeps the ambient KUBECONFIG/default kubeconfig resolution.
	if path != "" {
		loadingRules.ExplicitPath = path
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return "", fmt.Errorf("load kubeconfig: %w", err)
	}
	return strings.TrimRight(config.Host, "/"), nil
}

// NewClientSetFromConfig creates Kubernetes, dynamic, and discovery clients from a REST config.
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

type RealCluster struct {
	Clients *ClientSet
}

// Server returns the configured cluster host without trailing slashes.
func (c *RealCluster) Server(context.Context) (string, error) {
	if c == nil || c.Clients == nil || c.Clients.Config == nil {
		return "", fmt.Errorf("cluster clients are not initialized")
	}
	return strings.TrimRight(c.Clients.Config.Host, "/"), nil
}

// CheckAccess checks whether each requested API group version is served by the cluster
// and, when verbs are listed, whether the current user may perform them in the namespace.
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
		result.Details = authorizeVerbs(ctx, c.Clients.Kubernetes, check)
		result.Passed = result.Details == ""
		results = append(results, result)
	}
	return results
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

// CheckScheduling verifies controller readiness and host-config coverage for each platform.
func (c *RealCluster) CheckScheduling(ctx context.Context, spec SchedulingSpec) []CheckResult {
	if c == nil || c.Clients == nil || c.Clients.Kubernetes == nil {
		return []CheckResult{{Name: "multi-platform-controller", Details: "kubernetes client is not initialized"}}
	}
	if strings.TrimSpace(spec.Namespace) == "" || strings.TrimSpace(spec.Deployment) == "" || strings.TrimSpace(spec.ConfigMap) == "" {
		return []CheckResult{{Name: "multi-platform-controller", Details: "controller namespace, deployment, and config map are required"}}
	}
	deployment, err := c.Clients.Kubernetes.AppsV1().Deployments(spec.Namespace).Get(ctx, spec.Deployment, metav1.GetOptions{})
	if err != nil {
		return []CheckResult{{Name: spec.Deployment, Details: fmt.Sprintf("get controller deployment: %v", err)}}
	}
	if !controllerDeploymentReady(deployment) {
		return []CheckResult{{Name: spec.Deployment, Details: fmt.Sprintf("controller deployment is not ready: generation=%d observed=%d ready=%d available=%d updated=%d", deployment.Generation, deployment.Status.ObservedGeneration, deployment.Status.ReadyReplicas, deployment.Status.AvailableReplicas, deployment.Status.UpdatedReplicas)}}
	}
	configMap, err := c.Clients.Kubernetes.CoreV1().ConfigMaps(spec.Namespace).Get(ctx, spec.ConfigMap, metav1.GetOptions{})
	if err != nil {
		return []CheckResult{{Name: spec.ConfigMap, Details: fmt.Sprintf("get controller host configuration: %v", err)}}
	}
	if strings.TrimSpace(configMap.Labels[MultiPlatformControllerConfigLabel]) == "" {
		return []CheckResult{{Name: spec.ConfigMap, Details: "controller host configuration is not labeled for multi-platform-controller"}}
	}
	results := []CheckResult{{Name: spec.Deployment, Passed: true, Details: "controller deployment is available"}}
	for _, platform := range spec.Platforms {
		result := CheckResult{Name: spec.Namespace + "/" + platform}
		mode := configuredPlatformMode(configMap.Data, platform)
		if mode == "" {
			result.Details = "platform is not configured in host-config"
			results = append(results, result)
			continue
		}
		if mode == "dynamic" || mode == "dynamic-pool" {
			prefix := "dynamic." + strings.ReplaceAll(platform, "/", "-") + "."
			if strings.TrimSpace(configMap.Data[prefix+"type"]) == "" {
				result.Details = fmt.Sprintf("%s platform has no provider type", mode)
				results = append(results, result)
				continue
			}
		}
		result.Passed = true
		result.Details = fmt.Sprintf("platform is configured via %s host scheduling", mode)
		results = append(results, result)
	}
	return results
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

// configuredPlatformMode returns the host-config scheduling mode for a
// platform. Static hosts declare their platform per host instead of in a
// platform list.
func configuredPlatformMode(data map[string]string, target string) string {
	wanted := platform.Normalize(target)
	if wanted == "" {
		return ""
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
				return entry.mode
			}
		}
	}
	for key, value := range data {
		if strings.HasPrefix(key, "host.") && strings.HasSuffix(key, ".platform") && platform.Normalize(strings.TrimSpace(value)) == wanted {
			return "static"
		}
	}
	return ""
}
