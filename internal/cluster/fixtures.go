package cluster

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/redhat-appstudio/lkmabt/internal/cleanup"
)

var (
	// NamespaceGVR is the core Namespace resource, read and created cluster-scoped.
	NamespaceGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	// ApplicationGVR is the Konflux Application resource fixtures create.
	ApplicationGVR = schema.GroupVersionResource{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "applications"}
	// ComponentGVR is the Konflux Component resource fixtures create and validate.
	ComponentGVR = schema.GroupVersionResource{Group: "appstudio.redhat.com", Version: "v1alpha1", Resource: "components"}
	// SecretGVR is the core Secret resource used for GitLab SCM credentials.
	SecretGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	// ProjectRequestGVR is the OpenShift self-service namespace API. A
	// ProjectRequest makes the requester project admin.
	ProjectRequestGVR = schema.GroupVersionResource{Group: "project.openshift.io", Version: "v1", Resource: "projectrequests"}
)

// projectCreationInterval bounds how often fixture setup rechecks for a requested project.
var projectCreationInterval = time.Second

// projectCreationTimeout bounds how long the OpenShift project controller may
// take to create the Namespace for a ProjectRequest.
const projectCreationTimeout = 30 * time.Second

const (
	componentPaCRequestAnnotation = "build.appstudio.openshift.io/request"
	imageGenerationAnnotation     = "image.redhat.com/generate"
	// GitLabCredentialSecretName is the harness-owned GitLab SCM Secret.
	GitLabCredentialSecretName    = "gitlab-build-secret"
	scmCredentialsLabel           = "appstudio.redhat.com/credentials"
	scmHostnameLabel              = "appstudio.redhat.com/scm.host"
	scmSecretRepositoryAnnotation = "appstudio.redhat.com/scm.repository"
)

// ComponentSpec describes one Konflux Component fixture. Name, Repository, and
// Branch are required; Context and Dockerfile are optional.
type ComponentSpec struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Context    string `json:"context"`
	Dockerfile string `json:"dockerfile"`
}

// FixtureSpec describes the tenant resources EnsureFixture creates. Namespace,
// RunID, and Application are required, together with Components matching
// ExpectedComponents exactly. Provider "gitlab" enables
// GitLab SCM credential handling, using GitLabToken and Repository; any other
// Provider skips it. GitLabTokenEphemeral marks a token revoked at run end,
// which forces the Secret write and requires delete permission.
type FixtureSpec struct {
	Namespace            string
	RunID                string
	Application          string
	Provider             string
	GitLabToken          string
	GitLabTokenEphemeral bool
	Repository           string
	Components           []ComponentSpec
	// ExpectedComponents is the number of Components this fixture must
	// contain. It is required and must be positive; zero is not "any count".
	ExpectedComponents int
}

// FixtureResult reports the namespace, application, and component names that
// now exist, with components in FixtureSpec order.
type FixtureResult struct {
	Namespace   string
	Application string
	Components  []string
}

// FixtureService creates tenant fixtures through the dynamic API. Dynamic is
// required; Kubernetes is used only by GitLab provider runs to check Secret
// authorization.
type FixtureService struct {
	Dynamic    dynamic.Interface
	Kubernetes kubernetes.Interface
}

// EnsureNamespace creates or adopts the tenant namespace without creating the
// fixture resources, so a preflight probe can run in it before the components
// exist. It is idempotent and applies the same ownership labels as
// EnsureFixture: an existing namespace is adopted without relabeling, while a
// requested project is labeled with runID.
func (s FixtureService) EnsureNamespace(ctx context.Context, namespace, runID string) error {
	if s.Dynamic == nil {
		return fmt.Errorf("dynamic client is required")
	}
	if namespace == "" || runID == "" {
		return fmt.Errorf("namespace and run ID are required")
	}
	return s.ensureNamespace(ctx, s.Dynamic.Resource(NamespaceGVR), namespace, runID)
}

// EnsureFixture creates missing tenant resources and rejects drift in existing
// components. Namespace, RunID, and Application are required, as is a positive
// ExpectedComponents matching len(Components), with unique component names;
// those checks run before any cluster resource is created. Existing
// namespaces are adopted without relabeling, an existing Application must
// live in the requested namespace, and existing Components must match their
// expected source configuration. The first failure aborts the call;
// resources created before it remain for cleanup.
func (s FixtureService) EnsureFixture(ctx context.Context, spec FixtureSpec) (FixtureResult, error) {
	if s.Dynamic == nil {
		return FixtureResult{}, fmt.Errorf("dynamic client is required")
	}
	if spec.Namespace == "" || spec.RunID == "" || spec.Application == "" {
		return FixtureResult{}, fmt.Errorf("fixture requires namespace, run ID, and application")
	}
	if spec.ExpectedComponents <= 0 {
		return FixtureResult{}, fmt.Errorf("fixture requires a positive expected component count; got %d", spec.ExpectedComponents)
	}
	if len(spec.Components) != spec.ExpectedComponents {
		return FixtureResult{}, fmt.Errorf("fixture expects %d components but got %d", spec.ExpectedComponents, len(spec.Components))
	}
	if duplicate, found := duplicateComponentName(spec.Components); found {
		return FixtureResult{}, fmt.Errorf("fixture lists component %q more than once", duplicate)
	}
	namespaces := s.Dynamic.Resource(NamespaceGVR)
	if err := s.ensureNamespace(ctx, namespaces, spec.Namespace, spec.RunID); err != nil {
		return FixtureResult{}, err
	}
	if spec.Provider == "gitlab" {
		if err := s.authorizeGitLabSecretAccess(ctx, spec.Namespace, spec.GitLabTokenEphemeral); err != nil {
			return FixtureResult{}, err
		}
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
		} else {
			// Duplicate finalizers are corruption, not drift; report them
			// before validateComponent so the failure names the problem.
			if duplicates := duplicateFinalizers(object); len(duplicates) > 0 {
				return FixtureResult{}, fmt.Errorf("component %s lists duplicate finalizers (%s); finalizers: %s", component.Name, strings.Join(duplicates, ", "), strings.Join(object.GetFinalizers(), ", "))
			}
			if err := validateComponent(object, component, spec.Application); err != nil {
				return FixtureResult{}, err
			}
		}
		result.Components = append(result.Components, component.Name)
	}
	return result, nil
}

// ensureNamespace makes the tenant namespace exist, preferring an OpenShift
// ProjectRequest so the requester becomes project admin. An existing namespace
// is accepted as-is, without ownership labels. Otherwise it falls back to a
// bare Namespace when the ProjectRequest API is unavailable, forbidden, or
// reports NotFound; other ProjectRequest errors are returned unchanged.
func (s FixtureService) ensureNamespace(ctx context.Context, namespaces dynamic.ResourceInterface, namespace, runID string) error {
	if _, err := namespaces.Get(ctx, namespace, metav1.GetOptions{}); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("get namespace: %w", err)
	}
	if uid, err := s.createProject(ctx, namespaces, namespace); err == nil {
		return s.labelNamespace(ctx, namespaces, namespace, runID, uid)
	} else if !errors.IsNotFound(err) && !errors.IsForbidden(err) && !meta.IsNoMatchError(err) {
		return err
	}
	object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": namespace}}}
	object.SetLabels(tenantNamespaceLabels(runID))
	if _, err := namespaces.Create(ctx, object, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create namespace: %w", err)
	}
	return nil
}

// createProject requests a namespace from the OpenShift project controller,
// waits up to projectCreationTimeout for the Namespace to appear, and returns
// the created project's UID so fixture setup can reject a namespace another
// actor created in the same window. It fails when the request already exists or
// carries no UID; callers treat NotFound, Forbidden, and a missing API as
// permission to fall back to a bare Namespace.
func (s FixtureService) createProject(ctx context.Context, namespaces dynamic.ResourceInterface, namespace string) (string, error) {
	request := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "project.openshift.io/v1", "kind": "ProjectRequest", "metadata": map[string]any{"name": namespace},
	}}
	created, err := s.Dynamic.Resource(ProjectRequestGVR).Create(ctx, request, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			return "", fmt.Errorf("project request %s already exists; refusing to adopt a namespace this run did not request", namespace)
		}
		return "", fmt.Errorf("create project request %s: %w", namespace, err)
	}
	uid := string(created.GetUID())
	if uid == "" {
		return "", fmt.Errorf("project request %s returned no UID; refusing to label a namespace this run cannot verify", namespace)
	}
	err = wait.PollUntilContextTimeout(ctx, projectCreationInterval, projectCreationTimeout, true, func(ctx context.Context) (bool, error) {
		_, err := namespaces.Get(ctx, namespace, metav1.GetOptions{})
		if err == nil {
			return true, nil
		}
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	})
	if err != nil {
		return "", fmt.Errorf("wait for project %s: %w", namespace, err)
	}
	return uid, nil
}

// namespaceOwnershipError reports a namespace that appeared without the UID
// returned by this run's ProjectRequest.
type namespaceOwnershipError struct {
	Namespace string
	Actual    string
	Expected  string
}

// Error names the mismatched UIDs and refuses to take ownership.
func (e *namespaceOwnershipError) Error() string {
	return fmt.Sprintf("namespace %s has UID %q, not the requested project UID %q; refusing to label a namespace this run did not create", e.Namespace, e.Actual, e.Expected)
}

// labelNamespace applies the harness ownership labels that a ProjectRequest
// does not guarantee the project controller copies. It retries update conflicts
// and only labels a namespace whose UID matches the project this run requested;
// a mismatch returns namespaceOwnershipError unchanged, while other update
// failures add guidance about the required cluster-scoped permission.
func (s FixtureService) labelNamespace(ctx context.Context, namespaces dynamic.ResourceInterface, namespace, runID, expectedUID string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		object, err := namespaces.Get(ctx, namespace, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get requested namespace: %w", err)
		}
		if actual := string(object.GetUID()); actual != expectedUID {
			return &namespaceOwnershipError{Namespace: namespace, Actual: actual, Expected: expectedUID}
		}
		labels := object.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		for key, value := range tenantNamespaceLabels(runID) {
			labels[key] = value
		}
		object.SetLabels(labels)
		_, err = namespaces.Update(ctx, object, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		if _, ok := err.(*namespaceOwnershipError); ok {
			return err
		}
		return fmt.Errorf("label requested namespace %s: %w (a ProjectRequest does not copy ownership labels, so the run identity needs cluster-scoped update on namespaces to mark it for cleanup; grant that permission or delete namespace %s manually)", namespace, err, namespace)
	}
	return nil
}

// tenantNamespaceLabels returns the labels cleanup requires to own a namespace.
func tenantNamespaceLabels(runID string) map[string]string {
	return map[string]string{
		cleanup.ManagedByLabel: cleanup.ManagedByValue,
		cleanup.RunIDLabel:     runID,
		"konflux-ci.dev/type":  "tenant",
		"cost-center":          "670",
	}
}

// authorizeGitLabSecretAccess verifies Secret permissions in the tenant
// namespace, including delete when ephemeral revokes the token at run end. It
// runs at fixture setup because a ProjectRequest grants the requester project
// admin only once the namespace exists, and it requires Kubernetes.
func (s FixtureService) authorizeGitLabSecretAccess(ctx context.Context, namespace string, ephemeral bool) error {
	if s.Kubernetes == nil {
		return fmt.Errorf("kubernetes client is required to authorize GitLab SCM Secret access in namespace %s", namespace)
	}
	verbs := []string{"get", "create", "update"}
	if ephemeral {
		verbs = append(verbs, "delete")
	}
	check := AccessCheck{
		Name:      "GitLab SCM Secret",
		Resource:  SecretGVR,
		Namespace: namespace,
		Verbs:     verbs,
		Guidance:  fmt.Sprintf("GitLab runs require %s on Secrets in namespace %s; ask a namespace administrator to grant them, for example: oc adm policy add-role-to-user edit <user> -n %s", strings.Join(verbs, ", "), namespace, namespace),
	}
	if details := authorizeVerbs(ctx, s.Kubernetes, check); details != "" {
		return fmt.Errorf("GitLab SCM Secret authorization failed: %s", details)
	}
	return nil
}

// ensureGitLabCredentialSecret manages the harness-owned GitLab SCM basic-auth
// Secret. An ephemeral token is always written because it is revoked at run
// end; otherwise an existing harness-owned Secret is refreshed, any usable
// GitLab SCM Secret is reused, and only then is a new Secret created. A
// same-name Secret that lkmabt does not own is an error.
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
		// A run-acquired token is revoked at the end of the run, so reuse is
		// unsafe: write this run's own Secret instead of adopting one scoped to
		// another repository.
		return s.writeGitLabCredentialSecret(ctx, secrets, existing, namespace, runID, token, repositoryPath)
	}
	if existing != nil {
		return s.writeGitLabCredentialSecret(ctx, secrets, existing, namespace, runID, token, repositoryPath)
	}
	if reused, err := s.reusableGitLabCredentialSecret(ctx, secrets, namespace, repositoryPath); err != nil {
		return err
	} else if reused {
		return nil
	}
	return s.writeGitLabCredentialSecret(ctx, secrets, nil, namespace, runID, token, repositoryPath)
}

// reusableGitLabCredentialSecret reports whether the namespace already holds a
// usable GitLab SCM basic-auth Secret for repositoryPath. Reuse requires the SCM
// labels, a non-empty password, and a matching appstudio.redhat.com/scm.repository
// annotation. A Forbidden list means no reuse; other list errors fail.
func (s FixtureService) reusableGitLabCredentialSecret(ctx context.Context, secrets dynamic.ResourceInterface, namespace, repositoryPath string) (bool, error) {
	list, err := secrets.List(ctx, metav1.ListOptions{LabelSelector: scmCredentialsLabel + "=scm," + scmHostnameLabel + "=gitlab.com"})
	if err != nil {
		if errors.IsForbidden(err) {
			return false, nil
		}
		return false, fmt.Errorf("list GitLab SCM Secrets in namespace %s: %w", namespace, err)
	}
	for index := range list.Items {
		secret := &list.Items[index]
		if secret.GetAnnotations()[scmSecretRepositoryAnnotation] != repositoryPath {
			continue
		}
		if usableBasicAuthSecret(secret) {
			return true, nil
		}
	}
	return false, nil
}

// writeGitLabCredentialSecret creates the Secret when secret is nil, or updates
// an existing harness-owned one with the new token, this run's ownership
// labels, and the repository annotation when repositoryPath is non-empty. It
// refuses to modify a Secret that is not harness-owned.
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

// managedByLKMABT reports whether the Secret carries both harness ownership labels.
// A Secret created by any LKMABT run is adoptable; one missing either label is foreign.
func managedByLKMABT(secret *unstructured.Unstructured) bool {
	labels := secret.GetLabels()
	return labels[cleanup.ManagedByLabel] == cleanup.ManagedByValue && strings.TrimSpace(labels[cleanup.RunIDLabel]) != ""
}

// unmanagedGitLabSecretError explains how to clear a name collision the harness must not touch.
func unmanagedGitLabSecretError(namespace string) error {
	return fmt.Errorf("GitLab SCM Secret %q in namespace %s exists but is not owned by lkmabt (it needs %s=%s and a non-empty %s): delete it or use a namespace where it is absent", GitLabCredentialSecretName, namespace, cleanup.ManagedByLabel, cleanup.ManagedByValue, cleanup.RunIDLabel)
}

func applyGitLabSecretOwnership(secret *unstructured.Unstructured, runID string) {
	labels := secret.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[cleanup.ManagedByLabel] = cleanup.ManagedByValue
	labels[cleanup.RunIDLabel] = runID
	secret.SetLabels(labels)
}

// gitLabRepositoryPath returns the owner/repo path build-service matches, taken
// from the URL path with any .git suffix removed. It validates the path, not
// the host.
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

// updateBasicAuthPassword writes the token without discarding other Secret data.
// It also forces the basic-auth type, drops any stringData, and adds the SCM
// credential labels.
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

// duplicateComponentName reports whether a component name repeats. The bool
// result distinguishes "no duplicate" from a repeated empty name, which is
// a duplicate too. When found is true, name is the first repeated name.
func duplicateComponentName(components []ComponentSpec) (name string, found bool) {
	seen := make(map[string]struct{}, len(components))
	for _, component := range components {
		if _, exists := seen[component.Name]; exists {
			return component.Name, true
		}
		seen[component.Name] = struct{}{}
	}
	return "", false
}

// duplicateFinalizers returns the finalizer strings an object lists more
// than once, in first-seen order. Several distinct controller finalizers are
// legitimate; a repeated string is not.
func duplicateFinalizers(object *unstructured.Unstructured) []string {
	seen := make(map[string]struct{})
	reported := make(map[string]struct{})
	duplicates := make([]string, 0)
	for _, finalizer := range object.GetFinalizers() {
		_, isDuplicate := seen[finalizer]
		_, alreadyReported := reported[finalizer]
		if isDuplicate && !alreadyReported {
			duplicates = append(duplicates, finalizer)
			reported[finalizer] = struct{}{}
		} else if !isDuplicate {
			seen[finalizer] = struct{}{}
		}
	}
	return duplicates
}

// validateComponent checks that an existing component belongs to application
// and matches the expected repository, branch, and Dockerfile, plus Context
// when expected.Context is non-empty. Context is ignored when the expectation
// leaves it empty, so an existing component may carry any context.
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
