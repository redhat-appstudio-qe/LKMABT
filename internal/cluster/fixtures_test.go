package cluster

import (
	"context"
	"encoding/base64"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/cleanup"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

// testFixtureSpec builds a fixture specification with three components for a provider.
func testFixtureSpec(provider, token string) FixtureSpec {
	return FixtureSpec{
		Namespace:   "tenant",
		RunID:       "run-1",
		Application: "app",
		Provider:    provider,
		GitLabToken: token,
		Repository:  "https://gitlab.com/o/r",
		Components: []ComponentSpec{
			{Name: "one", Repository: "https://gitlab.com/o/r", Branch: "main"},
			{Name: "two", Repository: "https://gitlab.com/o/r", Branch: "main"},
			{Name: "three", Repository: "https://gitlab.com/o/r", Branch: "main"},
		},
	}
}

// fixtureTestClient creates a fake dynamic client that simulates the OpenShift
// project controller creating a Namespace for each ProjectRequest.
func fixtureTestClient(objects ...runtime.Object) *fake.FakeDynamicClient {
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		SecretGVR:    "SecretList",
		NamespaceGVR: "NamespaceList",
	}, objects...)
	client.PrependReactor("create", ProjectRequestGVR.Resource, func(action kubetesting.Action) (bool, runtime.Object, error) {
		create := action.(kubetesting.CreateAction)
		request := create.GetObject().(*unstructured.Unstructured)
		uid := types.UID("project-" + request.GetName())
		request.SetUID(uid)
		namespace := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": request.GetName()}}}
		namespace.SetUID(uid)
		if err := client.Tracker().Create(NamespaceGVR, namespace, ""); err != nil {
			return true, nil, err
		}
		return true, request, nil
	})
	return client
}

// fixtureTestKubernetes returns a fake clientset that allows every Secret verb.
func fixtureTestKubernetes() *kubernetesfake.Clientset {
	client := kubernetesfake.NewSimpleClientset()
	respondSSAR(client, authorizationv1.SubjectAccessReviewStatus{Allowed: true}, nil)
	return client
}

// fixtureTestService wires the clients EnsureFixture needs.
func fixtureTestService(client *fake.FakeDynamicClient) FixtureService {
	return FixtureService{Dynamic: client, Kubernetes: fixtureTestKubernetes()}
}

// forbidSecretList makes every Secret list fail with Forbidden.
func forbidSecretList(client *fake.FakeDynamicClient) {
	client.PrependReactor("list", "secrets", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "secrets", fmt.Errorf("not allowed"))
	})
}

var _ = Describe("FixtureService", func() {
	It("splits component path into build context and Dockerfile", func() {
		client := fixtureTestClient()
		spec := testFixtureSpec("github", "token")
		spec.Components[0].Context = "web-server"
		spec.Components[0].Dockerfile = "Dockerfile"
		spec.Components[1].Context = "history-worker"
		spec.Components[1].Dockerfile = "Dockerfile"
		spec.Components[2].Context = "frontend"
		spec.Components[2].Dockerfile = "Dockerfile"

		_, err := (fixtureTestService(client)).EnsureFixture(context.Background(), spec)
		Expect(err).NotTo(HaveOccurred())
		for _, expected := range []struct {
			name    string
			context string
		}{
			{name: "one", context: "web-server"},
			{name: "two", context: "history-worker"},
			{name: "three", context: "frontend"},
		} {
			component, err := client.Resource(ComponentGVR).Namespace("tenant").Get(context.Background(), expected.name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			gotContext, found, err := unstructured.NestedString(component.Object, "spec", "source", "git", "context")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(gotContext).To(Equal(expected.context))
			gotDockerfile, found, err := unstructured.NestedString(component.Object, "spec", "source", "git", "dockerfileUrl")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(gotDockerfile).To(Equal("Dockerfile"))
		}
	})

	It("does not create GitLab Secret for GitHub", func() {
		client := fixtureTestClient()

		if _, err := (fixtureTestService(client)).EnsureFixture(context.Background(), testFixtureSpec("github", "token")); err != nil {
			Expect(err).NotTo(HaveOccurred())
		}

		secrets, err := client.Resource(SecretGVR).Namespace("tenant").List(context.Background(), metav1.ListOptions{})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(secrets.Items).To(BeEmpty())
	})

	It("creates GitLab SCM Secret", func() {
		client := fixtureTestClient()

		if _, err := (fixtureTestService(client)).EnsureFixture(context.Background(), testFixtureSpec("gitlab", "token")); err != nil {
			Expect(err).NotTo(HaveOccurred())
		}

		secret, err := client.Resource(SecretGVR).Namespace("tenant").Get(context.Background(), GitLabCredentialSecretName, metav1.GetOptions{})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		got := secret.GetLabels()
		Expect(got).To(SatisfyAll(HaveKeyWithValue(scmCredentialsLabel, "scm"), HaveKeyWithValue(scmHostnameLabel, "gitlab.com")))
		if got, found, _ := unstructured.NestedString(secret.Object, "type"); !found || got != string(corev1.SecretTypeBasicAuth) {
			Expect(found).To(BeTrue())
			Expect(got).To(Equal(string(corev1.SecretTypeBasicAuth)))
		}
		password, found, err := unstructured.NestedString(secret.Object, "stringData", "password")
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(found).To(BeTrue())
		Expect(password).To(Equal("token"))
	})

	It("updates the run-scoped GitLab SCM Secret for an ephemeral token", func() {
		client := fixtureTestClient(
			&unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "Secret",
				"metadata": map[string]any{"name": "existing-gitlab-secret", "namespace": "tenant", "labels": map[string]any{scmCredentialsLabel: "scm", scmHostnameLabel: "gitlab.com"}},
				"type":     "kubernetes.io/basic-auth", "data": map[string]any{"password": "existing-token"},
			}},
			&unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "Secret",
				"metadata": map[string]any{
					"name": GitLabCredentialSecretName, "namespace": "tenant",
					"labels":      map[string]any{scmCredentialsLabel: "scm", scmHostnameLabel: "gitlab.com", cleanup.ManagedByLabel: cleanup.ManagedByValue, cleanup.RunIDLabel: "old-run"},
					"annotations": map[string]any{scmSecretRepositoryAnnotation: "o/r"},
				},
				"type": "kubernetes.io/basic-auth", "data": map[string]any{"password": "old-token"},
			}},
			&unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "Secret",
				"metadata": map[string]any{
					"name": "repo-gitlab-secret", "namespace": "tenant",
					"labels":      map[string]any{scmCredentialsLabel: "scm", scmHostnameLabel: "gitlab.com"},
					"annotations": map[string]any{scmSecretRepositoryAnnotation: "o/r"},
				},
				"type": "kubernetes.io/basic-auth", "data": map[string]any{"password": "stale-token"},
			}},
		)
		spec := testFixtureSpec("gitlab", "new-token")
		spec.GitLabTokenEphemeral = true

		if _, err := (fixtureTestService(client)).EnsureFixture(context.Background(), spec); err != nil {
			Expect(err).NotTo(HaveOccurred())
		}

		secret, err := client.Resource(SecretGVR).Namespace("tenant").Get(context.Background(), GitLabCredentialSecretName, metav1.GetOptions{})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		password, found, err := unstructured.NestedString(secret.Object, "data", "password")
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(found).To(BeTrue())
		Expect(password).To(Equal(base64.StdEncoding.EncodeToString([]byte("new-token"))))
		Expect(secret.GetLabels()).To(SatisfyAll(HaveKeyWithValue(scmCredentialsLabel, "scm"), HaveKeyWithValue(scmHostnameLabel, "gitlab.com"), HaveKeyWithValue(cleanup.ManagedByLabel, cleanup.ManagedByValue), HaveKeyWithValue(cleanup.RunIDLabel, "run-1")))
		Expect(secret.GetAnnotations()).To(HaveKeyWithValue(scmSecretRepositoryAnnotation, "o/r"))

		repositoryScoped, err := client.Resource(SecretGVR).Namespace("tenant").Get(context.Background(), "repo-gitlab-secret", metav1.GetOptions{})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		repositoryPassword, _, _ := unstructured.NestedString(repositoryScoped.Object, "data", "password")
		Expect(repositoryPassword).To(Equal(base64.StdEncoding.EncodeToString([]byte("new-token"))))

		other, err := client.Resource(SecretGVR).Namespace("tenant").Get(context.Background(), "existing-gitlab-secret", metav1.GetOptions{})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		otherPassword, _, _ := unstructured.NestedString(other.Object, "data", "password")
		Expect(otherPassword).To(Equal("existing-token"))
	})

	It("reuses existing GitLab SCM Secret", func() {
		client := fixtureTestClient(&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      "existing-gitlab-secret",
				"namespace": "tenant",
				"labels": map[string]any{
					scmCredentialsLabel: "scm",
					scmHostnameLabel:    "gitlab.com",
				},
			},
			"type": "kubernetes.io/basic-auth",
			"data": map[string]any{"password": "existing-token"},
		}})

		if _, err := (fixtureTestService(client)).EnsureFixture(context.Background(), testFixtureSpec("gitlab", "new-token")); err != nil {
			Expect(err).NotTo(HaveOccurred())
		}

		if _, err := client.Resource(SecretGVR).Namespace("tenant").Get(context.Background(), GitLabCredentialSecretName, metav1.GetOptions{}); err == nil {
			Expect(err).To(HaveOccurred())
		}
		secret, err := client.Resource(SecretGVR).Namespace("tenant").Get(context.Background(), "existing-gitlab-secret", metav1.GetOptions{})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		password, found, err := unstructured.NestedString(secret.Object, "data", "password")
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(found).To(BeTrue())
		Expect(password).To(Equal("existing-token"))
	})

	It("preserves unrelated existing Secret", func() {
		client := fixtureTestClient(&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      "pipelines-as-code-secret",
				"namespace": "tenant",
			},
			"type": "Opaque",
			"data": map[string]any{"password": "unrelated-token"},
		}})

		if _, err := (fixtureTestService(client)).EnsureFixture(context.Background(), testFixtureSpec("gitlab", "token")); err != nil {
			Expect(err).NotTo(HaveOccurred())
		}

		if _, err := client.Resource(SecretGVR).Namespace("tenant").Get(context.Background(), "pipelines-as-code-secret", metav1.GetOptions{}); err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		_, err := client.Resource(SecretGVR).Namespace("tenant").Get(context.Background(), GitLabCredentialSecretName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sets application display name", func() {
		client := fixtureTestClient()
		service := fixtureTestService(client)
		_, err := service.EnsureFixture(context.Background(), FixtureSpec{
			Namespace:   "tenant",
			RunID:       "run-1",
			Application: "app",
			Components: []ComponentSpec{
				{Name: "one", Repository: "https://gitlab.com/o/r", Branch: "main"},
				{Name: "two", Repository: "https://gitlab.com/o/r", Branch: "main"},
				{Name: "three", Repository: "https://gitlab.com/o/r", Branch: "main"},
			},
		})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}

		application, err := client.Resource(ApplicationGVR).Namespace("tenant").Get(context.Background(), "app", metav1.GetOptions{})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		displayName, found, err := unstructured.NestedString(application.Object, "spec", "displayName")
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(found).To(BeTrue())
		Expect(displayName).To(Equal("app"))
	})

	It("configures build and image controllers", func() {
		client := fixtureTestClient()
		service := fixtureTestService(client)
		_, err := service.EnsureFixture(context.Background(), FixtureSpec{
			Namespace:   "tenant",
			RunID:       "run-1",
			Application: "app",
			Components: []ComponentSpec{
				{Name: "one", Repository: "https://gitlab.com/o/r", Branch: "main"},
				{Name: "two", Repository: "https://gitlab.com/o/r", Branch: "main"},
				{Name: "three", Repository: "https://gitlab.com/o/r", Branch: "main"},
			},
		})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}

		for _, name := range []string{"one", "two", "three"} {
			component, err := client.Resource(ComponentGVR).Namespace("tenant").Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				Expect(err).NotTo(HaveOccurred())
			}
			annotations := component.GetAnnotations()
			Expect(annotations).To(HaveKeyWithValue("build.appstudio.openshift.io/request", "configure-pac"))
			Expect(annotations).To(HaveKeyWithValue("image.redhat.com/generate", `{"visibility":"public"}`))
		}
	})

	It("rejects drift", func() {
		client := fixtureTestClient()
		service := fixtureTestService(client)
		spec := FixtureSpec{Namespace: "tenant", RunID: "run-1", Application: "app", Components: []ComponentSpec{{Name: "one", Repository: "https://gitlab.com/o/r", Branch: "main", Dockerfile: "one/Dockerfile"}, {Name: "two", Repository: "https://gitlab.com/o/r", Branch: "main", Dockerfile: "two/Dockerfile"}, {Name: "three", Repository: "https://gitlab.com/o/r", Branch: "main", Dockerfile: "three/Dockerfile"}}}
		_, err := service.EnsureFixture(context.Background(), spec)
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		spec.Components[0].Repository = "https://gitlab.com/other/repository"
		_, err = service.EnsureFixture(context.Background(), spec)
		Expect(err).To(MatchError(ContainSubstring("repository drift")))
	})

	It("labels created namespace with ownership", func() {
		client := fixtureTestClient()
		_, err := (fixtureTestService(client)).EnsureFixture(context.Background(), FixtureSpec{
			Namespace: "tenant", RunID: "run-1", Application: "app",
			Components: []ComponentSpec{{Name: "one", Repository: "https://gitlab.com/o/r", Branch: "main"}, {Name: "two", Repository: "https://gitlab.com/o/r", Branch: "main"}, {Name: "three", Repository: "https://gitlab.com/o/r", Branch: "main"}},
		})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		namespace, err := client.Resource(NamespaceGVR).Get(context.Background(), "tenant", metav1.GetOptions{})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(namespace.GetLabels()).To(SatisfyAll(HaveKeyWithValue("app.konflux-ci.org/managed-by", "lkmabt"), HaveKeyWithValue("app.konflux.org/run-id", "run-1"), HaveKeyWithValue("konflux-ci.dev/type", "tenant"), HaveKeyWithValue("cost-center", "670")))
	})
})

var _ = Describe("tenant namespace creation", func() {
	It("requests the project, labels the created namespace, and authorizes the GitLab Secret", func() {
		client := fixtureTestClient()
		kubernetes := fixtureTestKubernetes()
		spec := testFixtureSpec("gitlab", "token")
		spec.GitLabTokenEphemeral = true

		_, err := (FixtureService{Dynamic: client, Kubernetes: kubernetes}).EnsureFixture(context.Background(), spec)
		Expect(err).ShouldNot(HaveOccurred())

		var requests []string
		for _, action := range client.Actions() {
			create, ok := action.(kubetesting.CreateAction)
			if ok && action.GetResource() == ProjectRequestGVR {
				requests = append(requests, create.GetObject().(metav1.Object).GetName())
			}
		}
		Expect(requests).Should(ConsistOf("tenant"))

		namespace, err := client.Resource(NamespaceGVR).Get(context.Background(), "tenant", metav1.GetOptions{})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(namespace.GetLabels()).Should(SatisfyAll(HaveKeyWithValue(cleanup.ManagedByLabel, cleanup.ManagedByValue), HaveKeyWithValue(cleanup.RunIDLabel, "run-1"), HaveKeyWithValue("konflux-ci.dev/type", "tenant"), HaveKeyWithValue("cost-center", "670")))

		var verbs []string
		for _, action := range kubernetes.Actions() {
			create, ok := action.(kubetesting.CreateAction)
			if !ok || action.GetResource().Resource != "selfsubjectaccessreviews" {
				continue
			}
			review, ok := create.GetObject().(*authorizationv1.SelfSubjectAccessReview)
			Expect(ok).Should(BeTrue())
			verbs = append(verbs, review.Spec.ResourceAttributes.Verb)
		}
		Expect(verbs).Should(ConsistOf("get", "create", "update", "delete"))
	})

	DescribeTable("falls back to a bare Namespace when the ProjectRequest API is unavailable",
		func(unavailable error) {
			client := fixtureTestClient()
			client.PrependReactor("create", ProjectRequestGVR.Resource, func(kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, unavailable
			})

			_, err := fixtureTestService(client).EnsureFixture(context.Background(), testFixtureSpec("gitlab", "token"))
			Expect(err).ShouldNot(HaveOccurred())

			namespace, err := client.Resource(NamespaceGVR).Get(context.Background(), "tenant", metav1.GetOptions{})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(namespace.GetLabels()).Should(SatisfyAll(HaveKeyWithValue(cleanup.ManagedByLabel, cleanup.ManagedByValue), HaveKeyWithValue(cleanup.RunIDLabel, "run-1"), HaveKeyWithValue("konflux-ci.dev/type", "tenant"), HaveKeyWithValue("cost-center", "670")))
		},
		Entry("not found", apierrors.NewNotFound(schema.GroupResource{Group: "project.openshift.io", Resource: "projectrequests"}, "tenant")),
		Entry("forbidden", apierrors.NewForbidden(schema.GroupResource{Group: "project.openshift.io", Resource: "projectrequests"}, "tenant", fmt.Errorf("not allowed"))),
	)

	It("fails before writing the Secret when Secret authorization is denied", func() {
		client := fixtureTestClient()
		kubernetes := kubernetesfake.NewSimpleClientset()
		respondSSAR(kubernetes, authorizationv1.SubjectAccessReviewStatus{Reason: "rbac denied"}, nil)

		_, err := (FixtureService{Dynamic: client, Kubernetes: kubernetes}).EnsureFixture(context.Background(), testFixtureSpec("gitlab", "token"))
		Expect(err).Should(MatchError(ContainSubstring("get secrets in namespace tenant denied: rbac denied")))
		Expect(err.Error()).Should(ContainSubstring("get, create, update"))
		Expect(err.Error()).Should(ContainSubstring("oc adm policy add-role-to-user edit <user> -n tenant"))

		_, getErr := client.Resource(SecretGVR).Namespace("tenant").Get(context.Background(), GitLabCredentialSecretName, metav1.GetOptions{})
		Expect(apierrors.IsNotFound(getErr)).Should(BeTrue())
	})

	It("does not attempt Secret authorization before the namespace exists", func() {
		client := fixtureTestClient()
		client.PrependReactor("create", ProjectRequestGVR.Resource, func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "project.openshift.io", Resource: "projectrequests"}, "tenant", fmt.Errorf("not allowed"))
		})
		client.PrependReactor("create", "namespaces", func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "tenant", fmt.Errorf("not allowed"))
		})
		kubernetes := fixtureTestKubernetes()

		_, err := (FixtureService{Dynamic: client, Kubernetes: kubernetes}).EnsureFixture(context.Background(), testFixtureSpec("gitlab", "token"))
		Expect(err).Should(MatchError(ContainSubstring("create namespace")))
		Expect(kubernetes.Actions()).Should(BeEmpty())
	})

	It("refuses to label a namespace created by another actor", func() {
		client := fixtureTestClient()
		client.PrependReactor("create", ProjectRequestGVR.Resource, func(action kubetesting.Action) (bool, runtime.Object, error) {
			create := action.(kubetesting.CreateAction)
			request := create.GetObject().(*unstructured.Unstructured)
			request.SetUID(types.UID("project-request-uid"))
			foreign := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": request.GetName()}}}
			foreign.SetUID(types.UID("foreign-uid"))
			if err := client.Tracker().Create(NamespaceGVR, foreign, ""); err != nil {
				return true, nil, err
			}
			return true, request, nil
		})

		_, err := fixtureTestService(client).EnsureFixture(context.Background(), testFixtureSpec("gitlab", "token"))
		Expect(err).Should(MatchError(ContainSubstring("refusing to label a namespace this run did not create")))

		namespace, getErr := client.Resource(NamespaceGVR).Get(context.Background(), "tenant", metav1.GetOptions{})
		Expect(getErr).ShouldNot(HaveOccurred())
		Expect(namespace.GetLabels()).ShouldNot(HaveKey(cleanup.ManagedByLabel))
	})

	It("explains the orphaned namespace when labels cannot be applied", func() {
		client := fixtureTestClient()
		client.PrependReactor("update", "namespaces", func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "tenant", fmt.Errorf("not allowed"))
		})

		_, err := fixtureTestService(client).EnsureFixture(context.Background(), testFixtureSpec("gitlab", "token"))
		Expect(err).Should(MatchError(ContainSubstring("cluster-scoped update on namespaces")))
		Expect(err.Error()).Should(ContainSubstring("delete namespace tenant manually"))
	})

	It("requires a Kubernetes client to authorize the GitLab Secret", func() {
		_, err := (FixtureService{Dynamic: fixtureTestClient()}).EnsureFixture(context.Background(), testFixtureSpec("gitlab", "token"))
		Expect(err).Should(MatchError(ContainSubstring("kubernetes client is required")))
	})
})

var _ = Describe("GitLab Secret management", func() {
	DescribeTable("creates the managed Secret when listing is forbidden",
		func(ephemeral bool) {
			client := fixtureTestClient()
			forbidSecretList(client)
			spec := testFixtureSpec("gitlab", "new-token")
			spec.GitLabTokenEphemeral = ephemeral

			_, err := (fixtureTestService(client)).EnsureFixture(context.Background(), spec)
			Expect(err).ShouldNot(HaveOccurred())

			secret, err := client.Resource(SecretGVR).Namespace("tenant").Get(context.Background(), GitLabCredentialSecretName, metav1.GetOptions{})
			Expect(err).ShouldNot(HaveOccurred())
			password, found, err := unstructured.NestedString(secret.Object, "stringData", "password")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(found).Should(BeTrue())
			Expect(password).Should(Equal("new-token"))
			Expect(secret.GetLabels()).Should(SatisfyAll(
				HaveKeyWithValue(scmCredentialsLabel, "scm"),
				HaveKeyWithValue(scmHostnameLabel, "gitlab.com"),
				HaveKeyWithValue(cleanup.ManagedByLabel, cleanup.ManagedByValue),
				HaveKeyWithValue(cleanup.RunIDLabel, "run-1"),
			))
			Expect(secret.GetAnnotations()).Should(HaveKeyWithValue(scmSecretRepositoryAnnotation, "o/r"))
		},
		Entry("for an ephemeral token", true),
		Entry("for a long-lived token", false),
	)

	When("a Secret with the managed name belongs to someone else", func() {
		It("should fail without touching it", func() {
			client := fixtureTestClient(&unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "Secret",
				"metadata": map[string]any{"name": GitLabCredentialSecretName, "namespace": "tenant"},
				"type":     "kubernetes.io/basic-auth", "data": map[string]any{"password": "foreign-token"},
			}})

			_, err := (fixtureTestService(client)).EnsureFixture(context.Background(), testFixtureSpec("gitlab", "new-token"))

			Expect(err).Should(MatchError(ContainSubstring("is not owned by lkmabt")))
			Expect(err.Error()).Should(ContainSubstring(GitLabCredentialSecretName))
			Expect(err.Error()).Should(ContainSubstring("tenant"))
			secret, err := client.Resource(SecretGVR).Namespace("tenant").Get(context.Background(), GitLabCredentialSecretName, metav1.GetOptions{})
			Expect(err).ShouldNot(HaveOccurred())
			password, _, _ := unstructured.NestedString(secret.Object, "data", "password")
			Expect(password).Should(Equal("foreign-token"))
			Expect(secret.GetLabels()).ShouldNot(HaveKey(cleanup.ManagedByLabel))
		})
	})

	When("a same-name Secret has the managed-by label but no run ID", func() {
		It("should fail without touching it", func() {
			client := fixtureTestClient(&unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "Secret",
				"metadata": map[string]any{
					"name": GitLabCredentialSecretName, "namespace": "tenant",
					"labels": map[string]any{scmCredentialsLabel: "scm", scmHostnameLabel: "gitlab.com", cleanup.ManagedByLabel: cleanup.ManagedByValue},
				},
				"type": "kubernetes.io/basic-auth", "data": map[string]any{"password": "partial-token"},
			}})

			_, err := (fixtureTestService(client)).EnsureFixture(context.Background(), testFixtureSpec("gitlab", "new-token"))

			Expect(err).Should(MatchError(ContainSubstring("is not owned by lkmabt")))
			secret, err := client.Resource(SecretGVR).Namespace("tenant").Get(context.Background(), GitLabCredentialSecretName, metav1.GetOptions{})
			Expect(err).ShouldNot(HaveOccurred())
			password, _, _ := unstructured.NestedString(secret.Object, "data", "password")
			Expect(password).Should(Equal("partial-token"))
			Expect(secret.GetLabels()).ShouldNot(HaveKey(cleanup.RunIDLabel))
		})
	})

	When("the managed Secret already exists for an earlier run", func() {
		It("should update the token, repository annotation, and run ID", func() {
			client := fixtureTestClient(&unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "Secret",
				"metadata": map[string]any{
					"name": GitLabCredentialSecretName, "namespace": "tenant",
					"labels":      map[string]any{scmCredentialsLabel: "scm", scmHostnameLabel: "gitlab.com", cleanup.ManagedByLabel: cleanup.ManagedByValue, cleanup.RunIDLabel: "old-run"},
					"annotations": map[string]any{scmSecretRepositoryAnnotation: "old/repo"},
				},
				"type": "kubernetes.io/basic-auth", "data": map[string]any{"password": "old-token"},
			}})

			_, err := (fixtureTestService(client)).EnsureFixture(context.Background(), testFixtureSpec("gitlab", "new-token"))
			Expect(err).ShouldNot(HaveOccurred())

			secret, err := client.Resource(SecretGVR).Namespace("tenant").Get(context.Background(), GitLabCredentialSecretName, metav1.GetOptions{})
			Expect(err).ShouldNot(HaveOccurred())
			password, found, err := unstructured.NestedString(secret.Object, "data", "password")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(found).Should(BeTrue())
			Expect(password).Should(Equal(base64.StdEncoding.EncodeToString([]byte("new-token"))))
			Expect(secret.GetLabels()).Should(SatisfyAll(
				HaveKeyWithValue(scmCredentialsLabel, "scm"),
				HaveKeyWithValue(scmHostnameLabel, "gitlab.com"),
				HaveKeyWithValue(cleanup.ManagedByLabel, cleanup.ManagedByValue),
				HaveKeyWithValue(cleanup.RunIDLabel, "run-1"),
			))
			Expect(secret.GetAnnotations()).Should(HaveKeyWithValue(scmSecretRepositoryAnnotation, "o/r"))
		})
	})
})

var _ = Describe("ProjectRequest ownership verification", func() {
	It("refuses to label a namespace when the request returns no UID", func() {
		client := fixtureTestClient()
		client.PrependReactor("create", ProjectRequestGVR.Resource, func(action kubetesting.Action) (bool, runtime.Object, error) {
			request := action.(kubetesting.CreateAction).GetObject().(*unstructured.Unstructured)
			if err := client.Tracker().Create(NamespaceGVR, &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": request.GetName()},
			}}, ""); err != nil {
				return true, nil, err
			}
			return true, request, nil
		})

		_, err := fixtureTestService(client).EnsureFixture(context.Background(), testFixtureSpec("gitlab", "token"))

		Expect(err).Should(MatchError(ContainSubstring("returned no UID")))
	})
})
