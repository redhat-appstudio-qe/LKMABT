package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/cleanup"
	"github.com/redhat-appstudio/lkmabt/internal/workflow"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// pendingNamespaceDeletion is one run-owned namespace awaiting the aggregate
// cleanup prompt.
type pendingNamespaceDeletion struct {
	cluster    string
	namespace  cleanup.NamespaceInfo
	service    cleanup.NamespaceService
	lockServer string
	out        io.Writer
}

// NamespaceDeletions queues run-owned namespaces during an aggregate run so the
// operator answers every deletion prompt after the last cluster.
type NamespaceDeletions struct {
	mu      sync.Mutex
	pending []pendingNamespaceDeletion
}

// Add queues a namespace for the aggregate cleanup prompt. Ownership is
// re-checked when the prompt is answered, so a namespace reused by another run
// is never deleted.
func (d *NamespaceDeletions) Add(cluster, lockServer string, service cleanup.NamespaceService, info cleanup.NamespaceInfo, out io.Writer) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending = append(d.pending, pendingNamespaceDeletion{cluster: cluster, namespace: info, service: service, lockServer: lockServer, out: out})
}

// Finalize prompts once for every queued namespace and deletes approved ones.
// Declines and unavailable prompts retain the namespace without an error.
func (d *NamespaceDeletions) Finalize(ctx context.Context, prompt workflow.Confirmation, log io.Writer) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	pending := d.pending
	d.pending = nil
	d.mu.Unlock()
	if len(pending) == 0 || prompt == nil {
		return nil
	}
	var errs []error
	for _, item := range pending {
		approved, err := prompt.Confirm(ctx, fmt.Sprintf("Delete owned tenant namespace %s from run %s (cluster %s)", item.namespace.Name, item.namespace.RunID, item.cluster))
		switch {
		case errors.Is(err, ErrPromptUnavailable):
			noticeDeferred(item, log, "retained tenant namespace %s on cluster %s: cleanup prompt unavailable (%v)", item.namespace.Name, item.cluster, err)
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("cluster %s: %w", item.cluster, err))
			continue
		case !approved:
			noticeDeferred(item, log, "retained tenant namespace %s on cluster %s", item.namespace.Name, item.cluster)
			continue
		}
		release, lockErr := acquireTenantNamespaceLock(item.lockServer, item.namespace.Name, tenantLockHolder(item.namespace.RunID, time.Now()))
		if lockErr != nil {
			noticeDeferred(item, log, "retained tenant namespace %s on cluster %s: %v", item.namespace.Name, item.cluster, lockErr)
			errs = append(errs, lockErr)
			continue
		}
		uid, deleteErr := item.service.Delete(ctx, item.namespace.Name, item.namespace.RunID)
		release()
		if apierrors.IsNotFound(deleteErr) {
			continue
		}
		if deleteErr != nil {
			errs = append(errs, fmt.Errorf("cluster %s: %w", item.cluster, deleteErr))
			continue
		}
		if waitErr := waitForNamespaceDeletion(ctx, item.service, item.namespace.Name, uid); waitErr != nil {
			errs = append(errs, fmt.Errorf("cluster %s: %w", item.cluster, waitErr))
		}
	}
	return errors.Join(errs...)
}

// noticeDeferred writes one deferred cleanup decision to the console and the
// aggregate run log.
func noticeDeferred(item pendingNamespaceDeletion, log io.Writer, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if item.out != nil {
		fmt.Fprintln(item.out, line)
	}
	if log != nil {
		fmt.Fprintln(log, line)
	}
}
