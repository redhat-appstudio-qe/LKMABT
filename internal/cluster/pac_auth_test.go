package cluster_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/lkmabt/internal/cluster"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

// pacAuthPipelineRun creates an annotated PipelineRun created at the supplied time.
func pacAuthPipelineRun(name, component, status, secret string, created time.Time) unstructured.Unstructured {
	object := pipelineRunObject(name, "tenant", "uid-"+name, status)
	object.SetLabels(map[string]string{"appstudio.openshift.io/component": component})
	object.SetAnnotations(map[string]string{cluster.PacGitAuthSecretAnnotation: secret})
	object.SetCreationTimestamp(metav1.NewTime(created))
	return *object
}

// pacAuthSecret creates the named tenant Secret.
func pacAuthSecret(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant"}}
}

// pacAuthCreated marks the PipelineRun as created by a PaC watcher that reconciled it.
func pacAuthCreated(object unstructured.Unstructured) unstructured.Unstructured {
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[cluster.PacSecretCreatedAnnotation] = "true"
	object.SetAnnotations(annotations)
	object.SetFinalizers([]string{cluster.PacFinalizer})
	return object
}

// forbiddenSecretGet makes every Secret Get fail with Forbidden.
func forbiddenSecretGet(client *kubernetesfake.Clientset) {
	client.PrependReactor("get", "secrets", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "git-auth-secret", fmt.Errorf("not allowed"))
	})
}

var _ = Describe("PacAuthInspector", func() {
	var now time.Time

	BeforeEach(func() {
		now = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	})

	When("a PipelineRun PaC created is pending past its grace period without its Secret", func() {
		It("should return a typed error reporting the Secret was deleted and naming a concurrent run", func() {
			inspector := cluster.PacAuthInspector{Kubernetes: kubernetesfake.NewSimpleClientset(), Now: func() time.Time { return now }}
			pending := pacAuthCreated(pacAuthPipelineRun("run-build", "mathwizz-web-server", "Unknown", "git-auth-secret", now.Add(-5*time.Minute)))

			missing, err := inspector.Missing(context.Background(), "tenant", []unstructured.Unstructured{pending})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(missing).ShouldNot(BeNil())
			Expect(missing.Namespace).Should(Equal("tenant"))
			Expect(missing.PipelineRun).Should(Equal("run-build"))
			Expect(missing.Component).Should(Equal("mathwizz-web-server"))
			Expect(missing.Secret).Should(Equal("git-auth-secret"))
			Expect(missing.Pending).Should(BeNumerically("~", 5*time.Minute, time.Second))
			Expect(missing.Created).Should(BeTrue())
			Expect(missing.Reconciled).Should(BeTrue())
			Expect(missing.Error()).Should(ContainSubstring("run-build"))
			Expect(missing.Error()).Should(ContainSubstring("mathwizz-web-server"))
			Expect(missing.Error()).Should(ContainSubstring("git-auth-secret"))
			Expect(missing.Error()).Should(ContainSubstring("5m0s"))
			Expect(missing.Error()).Should(ContainSubstring("deleted after PaC created it"))
			Expect(missing.Error()).Should(ContainSubstring("concurrent"))
		})
	})

	When("PaC never reconciled the PipelineRun", func() {
		It("should report a Secret that was never created and name the missing finalizer", func() {
			inspector := cluster.PacAuthInspector{Kubernetes: kubernetesfake.NewSimpleClientset(), Now: func() time.Time { return now }}
			pending := pacAuthPipelineRun("run-build", "mathwizz-web-server", "Unknown", "git-auth-secret", now.Add(-5*time.Minute))

			missing, err := inspector.Missing(context.Background(), "tenant", []unstructured.Unstructured{pending})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(missing).ShouldNot(BeNil())
			Expect(missing.Created).Should(BeFalse())
			Expect(missing.Reconciled).Should(BeFalse())
			Expect(missing.Error()).Should(ContainSubstring("was never created"))
			Expect(missing.Error()).Should(ContainSubstring("did not reconcile"))
			Expect(missing.Error()).Should(ContainSubstring(cluster.PacFinalizer))
			Expect(missing.Error()).ShouldNot(ContainSubstring("concurrent"))
		})
	})

	When("PaC reconciled the PipelineRun but has not created the Secret", func() {
		It("should report that Secret creation has not finished", func() {
			inspector := cluster.PacAuthInspector{Kubernetes: kubernetesfake.NewSimpleClientset(), Now: func() time.Time { return now }}
			pending := pacAuthPipelineRun("run-build", "mathwizz-web-server", "Unknown", "git-auth-secret", now.Add(-5*time.Minute))
			pending.SetAnnotations(map[string]string{
				cluster.PacGitAuthSecretAnnotation: "git-auth-secret",
				cluster.PacSecretCreatedAnnotation: "false",
			})
			pending.SetFinalizers([]string{cluster.PacFinalizer})

			missing, err := inspector.Missing(context.Background(), "tenant", []unstructured.Unstructured{pending})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(missing).ShouldNot(BeNil())
			Expect(missing.Created).Should(BeFalse())
			Expect(missing.Reconciled).Should(BeTrue())
			Expect(missing.Error()).Should(ContainSubstring("was not created"))
			Expect(missing.Error()).Should(ContainSubstring("has not finished creating"))
			Expect(missing.Error()).ShouldNot(ContainSubstring("concurrent"))
		})
	})

	When("the Secret exists", func() {
		It("should report nothing", func() {
			inspector := cluster.PacAuthInspector{Kubernetes: kubernetesfake.NewSimpleClientset(pacAuthSecret("git-auth-secret")), Now: func() time.Time { return now }}
			pending := pacAuthPipelineRun("run-build", "mathwizz-web-server", "Unknown", "git-auth-secret", now.Add(-5*time.Minute))

			missing, err := inspector.Missing(context.Background(), "tenant", []unstructured.Unstructured{pending})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(missing).Should(BeNil())
		})
	})

	When("the PipelineRun is inside the grace period", func() {
		It("should report nothing", func() {
			inspector := cluster.PacAuthInspector{Kubernetes: kubernetesfake.NewSimpleClientset(), Now: func() time.Time { return now }}
			pending := pacAuthPipelineRun("run-build", "mathwizz-web-server", "Unknown", "git-auth-secret", now.Add(-30*time.Second))

			missing, err := inspector.Missing(context.Background(), "tenant", []unstructured.Unstructured{pending})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(missing).Should(BeNil())
		})
	})

	When("the PipelineRun is terminal or unannotated", func() {
		It("should skip it without reading a Secret", func() {
			inspector := cluster.PacAuthInspector{Kubernetes: kubernetesfake.NewSimpleClientset(), Now: func() time.Time { return now }}
			failed := pacAuthPipelineRun("run-failed", "mathwizz-web-server", "False", "git-auth-secret", now.Add(-5*time.Minute))
			succeeded := pacAuthPipelineRun("run-succeeded", "mathwizz-web-server", "True", "git-auth-secret", now.Add(-5*time.Minute))
			unannotated := pipelineRunObject("run-plain", "tenant", "uid-plain", "Unknown")
			unannotated.SetCreationTimestamp(metav1.NewTime(now.Add(-5 * time.Minute)))
			uncreated := pacAuthPipelineRun("run-uncreated", "mathwizz-web-server", "Unknown", "git-auth-secret", time.Time{})

			missing, err := inspector.Missing(context.Background(), "tenant", []unstructured.Unstructured{failed, succeeded, *unannotated, uncreated})
			Expect(err).ShouldNot(HaveOccurred())
			Expect(missing).Should(BeNil())
		})
	})

	When("the Secret read is forbidden", func() {
		It("should return an inspection error instead of a typed error", func() {
			kube := kubernetesfake.NewSimpleClientset()
			forbiddenSecretGet(kube)
			inspector := cluster.PacAuthInspector{Kubernetes: kube, Now: func() time.Time { return now }}
			pending := pacAuthPipelineRun("run-build", "mathwizz-web-server", "Unknown", "git-auth-secret", now.Add(-5*time.Minute))

			missing, err := inspector.Missing(context.Background(), "tenant", []unstructured.Unstructured{pending})
			Expect(err).Should(HaveOccurred())
			Expect(missing).Should(BeNil())
			var typed *cluster.MissingPacAuthSecretError
			Expect(errors.As(err, &typed)).Should(BeFalse())
			Expect(err.Error()).Should(ContainSubstring("git-auth-secret"))
			Expect(err.Error()).Should(ContainSubstring("run-build"))
		})
	})

	When("the Kubernetes client is missing", func() {
		It("should report an inspection error", func() {
			inspector := cluster.PacAuthInspector{Now: func() time.Time { return now }}

			missing, err := inspector.Missing(context.Background(), "tenant", nil)
			Expect(err).Should(HaveOccurred())
			Expect(missing).Should(BeNil())
		})
	})
})
