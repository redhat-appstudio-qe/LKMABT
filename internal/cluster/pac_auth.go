package cluster

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
)

const (
	// PacGitAuthSecretAnnotation names the git authentication Secret PaC generates for a PipelineRun.
	PacGitAuthSecretAnnotation = "pipelinesascode.tekton.dev/git-auth-secret"
	// PacAuthSecretGracePeriod tolerates a PipelineRun that has not created its Secret yet.
	PacAuthSecretGracePeriod = 2 * time.Minute
)

// MissingPacAuthSecretError reports a pending PipelineRun whose PaC git authentication Secret is gone.
type MissingPacAuthSecretError struct {
	Namespace   string
	PipelineRun string
	Component   string
	Secret      string
	Pending     time.Duration
}

// Error explains that PaC removed the Secret with its PipelineRun, which a concurrent run explains.
func (e *MissingPacAuthSecretError) Error() string {
	return fmt.Sprintf("PipelineRun %s/%s for component %s has been pending for %s and its PaC git authentication Secret %q is missing; PaC deletes that Secret with its PipelineRun, so check for a concurrent LKMABT run using namespace %s",
		e.Namespace, e.PipelineRun, e.Component, e.Pending.Round(time.Second), e.Secret, e.Namespace)
}

// PacAuthInspector detects PipelineRuns that can never start because PaC's git Secret disappeared.
type PacAuthInspector struct {
	Kubernetes kubernetes.Interface
	Now        func() time.Time
	Grace      time.Duration
}

// Missing returns a typed error for the first non-terminal annotated PipelineRun
// past its grace period whose Secret is absent. A failed Secret Get is reported
// as an inspection error so the caller can warn without failing the wait.
func (i PacAuthInspector) Missing(ctx context.Context, namespace string, pipelineRuns []unstructured.Unstructured) (*MissingPacAuthSecretError, error) {
	if i.Kubernetes == nil {
		return nil, fmt.Errorf("kubernetes client is required to inspect PaC git authentication Secrets")
	}
	now := time.Now
	if i.Now != nil {
		now = i.Now
	}
	grace := i.Grace
	if grace == 0 {
		grace = PacAuthSecretGracePeriod
	}
	for index := range pipelineRuns {
		pipelineRun := &pipelineRuns[index]
		secret := pipelineRun.GetAnnotations()[PacGitAuthSecretAnnotation]
		if secret == "" {
			continue
		}
		if status := conditionStatus(pipelineRun); status == "True" || status == "False" {
			continue
		}
		created := pipelineRun.GetCreationTimestamp().Time
		if created.IsZero() {
			continue
		}
		pending := now().Sub(created)
		if pending <= grace {
			continue
		}
		_, err := i.Kubernetes.CoreV1().Secrets(namespace).Get(ctx, secret, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return &MissingPacAuthSecretError{
				Namespace:   namespace,
				PipelineRun: pipelineRun.GetName(),
				Component:   pipelineRun.GetLabels()["appstudio.openshift.io/component"],
				Secret:      secret,
				Pending:     pending,
			}, nil
		case err != nil:
			return nil, fmt.Errorf("inspect PaC git authentication Secret %q for PipelineRun %s/%s: %w", secret, namespace, pipelineRun.GetName(), err)
		}
	}
	return nil, nil
}
