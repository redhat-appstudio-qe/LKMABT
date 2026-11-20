package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	konfluxInfoNamespace       = "konflux-info"
	konfluxPublicInfoConfigMap = "konflux-public-info"
)

var githubAppSlugPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ReadGitHubAppSlug returns the GitHub App slug the cluster publishes for
// Konflux onboarding, taken from application_url in the
// konflux-info/konflux-public-info ConfigMap. It requires client and fails on a
// missing ConfigMap, unparsable info.json, or an application_url that is not a
// github.com/apps URL.
func ReadGitHubAppSlug(ctx context.Context, client kubernetes.Interface) (string, error) {
	if client == nil {
		return "", fmt.Errorf("kubernetes client is required")
	}
	configMap, err := client.CoreV1().ConfigMaps(konfluxInfoNamespace).Get(ctx, konfluxPublicInfoConfigMap, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read %s/%s: %w", konfluxInfoNamespace, konfluxPublicInfoConfigMap, err)
	}
	var info struct {
		Integrations struct {
			GitHub struct {
				ApplicationURL string `json:"application_url"`
			} `json:"github"`
		} `json:"integrations"`
	}
	if err := json.Unmarshal([]byte(configMap.Data["info.json"]), &info); err != nil {
		return "", fmt.Errorf("parse %s/%s info.json: %w", konfluxInfoNamespace, konfluxPublicInfoConfigMap, err)
	}
	return githubAppSlugFromURL(info.Integrations.GitHub.ApplicationURL)
}

// githubAppSlugFromURL extracts the slug from a github.com/apps/<slug> URL,
// rejecting other hosts, paths outside apps/, and invalid slug characters. The
// scheme is not checked.
func githubAppSlugFromURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host != "github.com" || !strings.HasPrefix(strings.Trim(parsed.Path, "/"), "apps/") {
		return "", fmt.Errorf("cluster GitHub Application URL %q is not a github.com/apps URL", raw)
	}
	slug := strings.TrimPrefix(strings.Trim(parsed.Path, "/"), "apps/")
	if !githubAppSlugPattern.MatchString(slug) {
		return "", fmt.Errorf("cluster GitHub Application URL %q has no valid app slug", raw)
	}
	return slug, nil
}
