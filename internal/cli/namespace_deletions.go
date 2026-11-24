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
	// release frees the tenant namespace lock this run acquired. The queue owns
	// it until the operator answers the deferred prompt, so a concurrent run
	// cannot treat the namespace as stale and delete it first.
	release func()
}

// finish releases this entry's tenant namespace lock, if it holds one.
func (item pendingNamespaceDeletion) finish() {
	if item.release != nil {
		item.release()
	}
}

// NamespaceDeletions queues run-owned namespaces during an aggregate run so the
// operator answers every deletion prompt after the last cluster.
type NamespaceDeletions struct {
	mu      sync.Mutex
	pending []pendingNamespaceDeletion
}

// Add queues a namespace for the aggregate cleanup prompt. Ownership is
// re-checked when the prompt is answered, so a namespace reused by another run
// is never deleted. A non-nil release transfers the tenant namespace lock to
// the queue until that prompt is answered.
func (d *NamespaceDeletions) Add(cluster, lockServer string, service cleanup.NamespaceService, info cleanup.NamespaceInfo, out io.Writer, release func()) {
	if d == nil {
		if release != nil {
			release()
		}
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending = append(d.pending, pendingNamespaceDeletion{cluster: cluster, namespace: info, service: service, lockServer: lockServer, out: out, release: release})
}

// Finalize prompts once for every queued namespace and deletes approved ones.
// Declines and unavailable prompts retain the namespace without an error. A
// canceled run still records one retention notice per queued namespace instead
// of dropping them silently, and every queued lock is released.
func (d *NamespaceDeletions) Finalize(ctx context.Context, prompt workflow.Confirmation, log io.Writer) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	pending := d.pending
	d.pending = nil
	d.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	if prompt == nil {
		for _, item := range pending {
			item.finish()
		}
		return nil
	}
	var errs []error
	for _, item := range pending {
		if err := d.finalizeOne(ctx, prompt, log, item); err != nil {
			errs = append(errs, err)
		}
		item.finish()
	}
	return errors.Join(errs...)
}

// finalizeOne prompts for and deletes one queued namespace. It does not release
// an already-held lock; Finalize does that after every outcome.
func (d *NamespaceDeletions) finalizeOne(ctx context.Context, prompt workflow.Confirmation, log io.Writer, item pendingNamespaceDeletion) error {
	if item.release == nil {
		release, lockErr := acquireTenantNamespaceLock(item.lockServer, item.namespace.Name, tenantLockHolder(item.namespace.RunID, time.Now()))
		if lockErr != nil {
			noticeDeferred(item, log, "retained tenant namespace %s on cluster %s: %v", item.namespace.Name, item.cluster, lockErr)
			return fmt.Errorf("cluster %s: %w", item.cluster, lockErr)
		}
		defer release()
	}
	approved, err := prompt.Confirm(ctx, fmt.Sprintf("Delete owned tenant namespace %s from run %s (cluster %s)", item.namespace.Name, item.namespace.RunID, item.cluster))
	switch {
	case errors.Is(err, ErrPromptUnavailable):
		noticeDeferred(item, log, "retained tenant namespace %s on cluster %s: cleanup prompt unavailable (%v)", item.namespace.Name, item.cluster, err)
		return nil
	case err != nil:
		noticeDeferred(item, log, "retained tenant namespace %s on cluster %s: cleanup prompt failed (%v)", item.namespace.Name, item.cluster, err)
		return fmt.Errorf("cluster %s: %w", item.cluster, err)
	case !approved:
		noticeDeferred(item, log, "retained tenant namespace %s on cluster %s", item.namespace.Name, item.cluster)
		return nil
	}
	uid, deleteErr := item.service.Delete(ctx, item.namespace.Name, item.namespace.RunID)
	if apierrors.IsNotFound(deleteErr) {
		return nil
	}
	if deleteErr != nil {
		return fmt.Errorf("cluster %s: %w", item.cluster, deleteErr)
	}
	if waitErr := waitForNamespaceDeletion(ctx, item.service, item.namespace.Name, uid); waitErr != nil {
		return fmt.Errorf("cluster %s: %w", item.cluster, waitErr)
	}
	return nil
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
