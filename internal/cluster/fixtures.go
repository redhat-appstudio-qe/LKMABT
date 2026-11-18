package cluster

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/redhat-appstudio/lkmabt/internal/cleanup"
)

var (
	NamespaceGVR   = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	ApplicationGVR = schema.GroupVersionResource{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "applications"}
	ComponentGVR   = schema.GroupVersionResource{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "components"}
	SecretGVR      = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
)

const (
	componentPaCRequestAnnotation = "build.appstudio.openshift.io/request"
	imageGenerationAnnotation     = "image.redhat.com/generate"
	// GitLabCredentialSecretName is the harness-owned GitLab SCM Secret.
	GitLabCredentialSecretName    = "gitlab-build-secret"
	scmCredentialsLabel           = "appstudio.redhat.com/credentials"
	scmHostnameLabel              = "appstudio.redhat.com/scm.host"
	scmSecretRepositoryAnnotation = "appstudio.redhat.com/scm.repository"
)

type ComponentSpec struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Context    string `json:"context"`
	Dockerfile string `json:"dockerfile"`
}

type FixtureSpec struct {
	Namespace            string
	RunID                string
	Application          string
	Provider             string
	GitLabToken          string
	GitLabTokenEphemeral bool
	Repository           string
	Components           []ComponentSpec
}

type FixtureResult struct {
	Namespace   string
	Application string
	Components  []string
}

type FixtureService struct {
	Dynamic dynamic.Interface
}

// EnsureFixture creates missing tenant resources and rejects drift in existing components.
func (s FixtureService) EnsureFixture(ctx context.Context, spec FixtureSpec) (FixtureResult, error) {
	if s.Dynamic == nil {
		return FixtureResult{}, fmt.Errorf("dynamic client is required")
	}
	if spec.Namespace == "" || spec.RunID == "" || spec.Application == "" || len(spec.Components) != 3 {
		return FixtureResult{}, fmt.Errorf("fixture requires namespace, application, and exactly three components")
	}
	namespaces := s.Dynamic.Resource(NamespaceGVR)
	if _, err := namespaces.Get(ctx, spec.Namespace, metav1.GetOptions{}); errors.IsNotFound(err) {
		object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": spec.Namespace, "labels": map[string]any{"app.konflux-ci.org/managed-by": "lkmabt", "app.konflux.org/run-id": spec.RunID, "konflux-ci.dev/type": "tenant", "cost-center": "670"}}}}
		if _, err := namespaces.Create(ctx, object, metav1.CreateOptions{}); err != nil {
			return FixtureResult{}, fmt.Errorf("create namespace: %w", err)
		}
	} else if err != nil {
		return FixtureResult{}, fmt.Errorf("get namespace: %w", err)
	}
	if spec.Provider == "gitlab" {
		if err := s.ensureGitLabCredentialSecret(ctx, spec.Namespace, spec.RunID, spec.GitLabToken, spec.Repository, spec.GitLabTokenEphemeral); err != nil {
			return FixtureResult{}, err
		}
	}
	applications := s.Dynamic.Resource(ApplicationGVR).Namespace(spec.Namespace)
	application, err := applications.Get(ctx, spec.Application, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		application = &unstructured.Unstructured{Object: map[string]any{"apiVersion": "appstudio.redhat.com/v1alpha1", "kind": "Application", "metadata": map[string]any{"name": spec.Application, "namespace": spec.Namespace}, "spec": map[string]any{"displayName": spec.Application}}}
		if application, err = applications.Create(ctx, application, metav1.CreateOptions{}); err != nil {
			return FixtureResult{}, fmt.Errorf("create application: %w", err)
		}
	} else if err != nil {
		return FixtureResult{}, fmt.Errorf("get application: %w", err)
	}
	if application.GetNamespace() != spec.Namespace {
		return FixtureResult{}, fmt.Errorf("application drift: namespace %q", application.GetNamespace())
	}
	components := s.Dynamic.Resource(ComponentGVR).Namespace(spec.Namespace)
	result := FixtureResult{Namespace: spec.Namespace, Application: spec.Application}
	for _, component := range spec.Components {
		if strings.TrimSpace(component.Name) == "" || strings.TrimSpace(component.Repository) == "" || strings.TrimSpace(component.Branch) == "" {
			return FixtureResult{}, fmt.Errorf("component %q is incomplete", component.Name)
		}
		object, getErr := components.Get(ctx, component.Name, metav1.GetOptions{})
		if errors.IsNotFound(getErr) {
			object = &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "appstudio.redhat.com/v1alpha1", "kind": "Component",
				"metadata": map[string]any{
					"name": component.Name, "namespace": spec.Namespace,
					"annotations": map[string]any{
						componentPaCRequestAnnotation: "configure-pac",
						imageGenerationAnnotation:     `{"visibility":"public"}`,
					},
				},
				"spec": map[string]any{"application": spec.Application, "source": map[string]any{"git": map[string]any{"url": component.Repository, "revision": component.Branch, "context": component.Context, "dockerfileUrl": component.Dockerfile}}},
			}}
			if _, getErr = components.Create(ctx, object, metav1.CreateOptions{}); getErr != nil {
				return FixtureResult{}, fmt.Errorf("create component %s: %w", component.Name, getErr)
			}
		} else if getErr != nil {
			return FixtureResult{}, fmt.Errorf("get component %s: %w", component.Name, getErr)
		} else if err := validateComponent(object, component, spec.Application); err != nil {
			return FixtureResult{}, err
		}
		result.Components = append(result.Components, component.Name)
	}
	return result, nil
}

// ensureGitLabCredentialSecret manages the harness-owned GitLab SCM basic-auth Secret.
// It never lists or mutates unowned Secrets unless the API permits it for reuse or refresh.
func (s FixtureService) ensureGitLabCredentialSecret(ctx context.Context, namespace, runID, token, repository string, ephemeral bool) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("GitLab token is required to create a GitLab SCM Secret")
	}
	repositoryPath, err := gitLabRepositoryPath(repository)
	if err != nil {
		return err
	}
	secrets := s.Dynamic.Resource(SecretGVR).Namespace(namespace)
	existing, err := secrets.Get(ctx, GitLabCredentialSecretName, metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("get GitLab SCM Secret %q in namespace %s: %w", GitLabCredentialSecretName, namespace, err)
	}
	if err == nil && !managedByLKMABT(existing) {
		return unmanagedGitLabSecretError(namespace)
	}
	if ephemeral {
		if err := s.refreshRepositoryScopedSecrets(ctx, secrets, repositoryPath, token); err != nil {
			return err
		}
		return s.writeGitLabCredentialSecret(ctx, secrets, existing, namespace, runID, token, repositoryPath)
	}
	if existing != nil {
		return s.writeGitLabCredentialSecret(ctx, secrets, existing, namespace, runID, token, repositoryPath)
	}
	if reused, err := s.reusableGitLabCredentialSecret(ctx, secrets, namespace); err != nil {
		return err
	} else if reused {
		return nil
	}
	return s.writeGitLabCredentialSecret(ctx, secrets, nil, namespace, runID, token, repositoryPath)
}

// reusableGitLabCredentialSecret reports whether a usable, repository-scoped
// GitLab Secret exists. A Forbidden list skips reuse; other list errors fail.
func (s FixtureService) reusableGitLabCredentialSecret(ctx context.Context, secrets dynamic.ResourceInterface, namespace string) (bool, error) {
	list, err := secrets.List(ctx, metav1.ListOptions{LabelSelector: scmCredentialsLabel + "=scm," + scmHostnameLabel + "=gitlab.com"})
	if err != nil {
		if errors.IsForbidden(err) {
			return false, nil
		}
		return false, fmt.Errorf("list GitLab SCM Secrets in namespace %s: %w", namespace, err)
	}
	for index := range list.Items {
		if usableBasicAuthSecret(&list.Items[index]) {
			return true, nil
		}
	}
	return false, nil
}

// refreshRepositoryScopedSecrets replaces stale passwords in basic-auth Secrets
// that build-service would prefer for the fixture repository. It is best effort:
// a Forbidden list skips refresh, and a Forbidden update skips that Secret.
func (s FixtureService) refreshRepositoryScopedSecrets(ctx context.Context, secrets dynamic.ResourceInterface, repositoryPath, token string) error {
	list, err := secrets.List(ctx, metav1.ListOptions{LabelSelector: scmCredentialsLabel + "=scm," + scmHostnameLabel + "=gitlab.com"})
	if err != nil {
		if errors.IsForbidden(err) {
			return nil
		}
		return fmt.Errorf("list GitLab SCM Secrets: %w", err)
	}
	for index := range list.Items {
		secret := &list.Items[index]
		if secret.GetName() == GitLabCredentialSecretName || !directRepositoryMatch(secret, repositoryPath) || !basicAuthSecret(secret) {
			continue
		}
		updateBasicAuthPassword(secret, token)
		if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			if errors.IsForbidden(err) {
				continue
			}
			return fmt.Errorf("update GitLab SCM Secret %q: %w", secret.GetName(), err)
		}
	}
	return nil
}

// writeGitLabCredentialSecret creates or updates the harness-owned GitLab Secret.
// It refuses to modify a Secret of the same name that belongs to someone else.
func (s FixtureService) writeGitLabCredentialSecret(ctx context.Context, secrets dynamic.ResourceInterface, secret *unstructured.Unstructured, namespace, runID, token, repositoryPath string) error {
	if secret == nil {
		return s.createGitLabCredentialSecret(ctx, secrets, namespace, runID, token, repositoryPath)
	}
	if !managedByLKMABT(secret) {
		return unmanagedGitLabSecretError(namespace)
	}
	updateBasicAuthPassword(secret, token)
	applyGitLabSecretOwnership(secret, runID)
	if repositoryPath != "" {
		annotations := secret.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[scmSecretRepositoryAnnotation] = repositoryPath
		secret.SetAnnotations(annotations)
	}
	if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update GitLab SCM Secret %q: %w", GitLabCredentialSecretName, err)
	}
	return nil
}

// managedByLKMABT reports whether the Secret carries the harness ownership label.
func managedByLKMABT(secret *unstructured.Unstructured) bool {
	return secret.GetLabels()[cleanup.ManagedByLabel] == cleanup.ManagedByValue
}

// unmanagedGitLabSecretError explains how to clear a name collision the harness must not touch.
func unmanagedGitLabSecretError(namespace string) error {
	return fmt.Errorf("GitLab SCM Secret %q in namespace %s exists but is not managed by lkmabt: delete it or use a namespace where it is absent", GitLabCredentialSecretName, namespace)
}

// applyGitLabSecretOwnership marks a Secret as managed by this harness run.
func applyGitLabSecretOwnership(secret *unstructured.Unstructured, runID string) {
	labels := secret.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[cleanup.ManagedByLabel] = cleanup.ManagedByValue
	labels[cleanup.RunIDLabel] = runID
	secret.SetLabels(labels)
}

// gitLabRepositoryPath returns the owner/repo path build-service uses for matching.
func gitLabRepositoryPath(repository string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(repository))
	if err != nil || parsed.Path == "" {
		return "", fmt.Errorf("invalid GitLab repository URL %q", repository)
	}
	repositoryPath := strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git")
	if repositoryPath == "" {
		return "", fmt.Errorf("GitLab repository URL %q has no path", repository)
	}
	return repositoryPath, nil
}

// directRepositoryMatch reports whether a Secret directly names the repository.
func directRepositoryMatch(secret *unstructured.Unstructured, repositoryPath string) bool {
	for _, candidate := range strings.Split(secret.GetAnnotations()[scmSecretRepositoryAnnotation], ",") {
		if strings.Trim(strings.TrimSpace(candidate), "/") == repositoryPath {
			return true
		}
	}
	return false
}

// basicAuthSecret reports whether a Secret is a basic-auth credential.
func basicAuthSecret(secret *unstructured.Unstructured) bool {
	secretType, found, err := unstructured.NestedString(secret.Object, "type")
	return err == nil && found && secretType == string(corev1.SecretTypeBasicAuth)
}

// updateBasicAuthPassword writes the token without discarding other Secret data.
func updateBasicAuthPassword(secret *unstructured.Unstructured, token string) {
	data, _, _ := unstructured.NestedMap(secret.Object, "data")
	if data == nil {
		data = map[string]any{}
	}
	data["password"] = base64.StdEncoding.EncodeToString([]byte(token))
	secret.Object["type"] = string(corev1.SecretTypeBasicAuth)
	secret.Object["data"] = data
	delete(secret.Object, "stringData")
	labels := secret.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[scmCredentialsLabel] = "scm"
	labels[scmHostnameLabel] = "gitlab.com"
	secret.SetLabels(labels)
}

// createGitLabCredentialSecret creates the harness-owned GitLab Secret.
func (s FixtureService) createGitLabCredentialSecret(ctx context.Context, secrets dynamic.ResourceInterface, namespace, runID, token, repositoryPath string) error {
	metadata := map[string]any{
		"name":      GitLabCredentialSecretName,
		"namespace": namespace,
		"labels": map[string]any{
			scmCredentialsLabel:    "scm",
			scmHostnameLabel:       "gitlab.com",
			cleanup.ManagedByLabel: cleanup.ManagedByValue,
			cleanup.RunIDLabel:     runID,
		},
	}
	if repositoryPath != "" {
		metadata["annotations"] = map[string]any{scmSecretRepositoryAnnotation: repositoryPath}
	}
	secret := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   metadata,
		"type":       string(corev1.SecretTypeBasicAuth),
		"stringData": map[string]any{
			"password": token,
		},
	}}
	if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if errors.IsAlreadyExists(err) {
			return fmt.Errorf("GitLab SCM Secret %q already exists but is not usable", GitLabCredentialSecretName)
		}
		return fmt.Errorf("create GitLab SCM Secret: %w", err)
	}
	return nil
}

// usableBasicAuthSecret reports whether a Secret contains a non-empty basic-auth password.
func usableBasicAuthSecret(secret *unstructured.Unstructured) bool {
	secretType, found, err := unstructured.NestedString(secret.Object, "type")
	if err != nil || !found || secretType != string(corev1.SecretTypeBasicAuth) {
		return false
	}
	for _, field := range []string{"data", "stringData"} {
		values, found, err := unstructured.NestedMap(secret.Object, field)
		password, hasPassword := values["password"].(string)
		if err == nil && found && hasPassword && strings.TrimSpace(password) != "" {
			return true
		}
	}
	return false
}

// validateComponent checks that an existing component matches its expected source configuration.
func validateComponent(object *unstructured.Unstructured, expected ComponentSpec, application string) error {
	actualApplication, _, _ := unstructured.NestedString(object.Object, "spec", "application")
	if actualApplication != application {
		return fmt.Errorf("component %s drift: application %q", expected.Name, actualApplication)
	}
	for _, field := range []struct {
		name     string
		path     []string
		expected string
	}{
		{name: "repository", path: []string{"spec", "source", "git", "url"}, expected: expected.Repository},
		{name: "branch", path: []string{"spec", "source", "git", "revision"}, expected: expected.Branch},
		{name: "dockerfile", path: []string{"spec", "source", "git", "dockerfileUrl"}, expected: expected.Dockerfile},
	} {
		actual, found, err := unstructured.NestedString(object.Object, field.path...)
		if err != nil {
			return fmt.Errorf("component %s %s drift: %w", expected.Name, field.name, err)
		}
		if !found || actual != field.expected {
			return fmt.Errorf("component %s %s drift: %q", expected.Name, field.name, actual)
		}
	}
	if expected.Context != "" {
		actual, found, err := unstructured.NestedString(object.Object, "spec", "source", "git", "context")
		if err != nil {
			return fmt.Errorf("component %s context drift: %w", expected.Name, err)
		}
		if !found || actual != expected.Context {
			return fmt.Errorf("component %s context drift: %q", expected.Name, actual)
		}
	}
	return nil
}
