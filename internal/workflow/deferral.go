package workflow

import (
	"errors"
	"time"

	"github.com/redhat-appstudio/lkmabt/internal/model"
)

// ErrDeferred marks an error that defers a cluster attempt instead of failing
// it. A deferred attempt is retried as a fresh run after the first pass.
var ErrDeferred = errors.New("cluster deferred")

// IsDeferred reports whether err represents a cluster deferral. Deferral is
// distinct from failure: the workflow must not record PhaseFailed or collect
// failure artifacts, and the scheduler may retry the cluster.
func IsDeferred(err error) bool {
	return errors.Is(err, ErrDeferred)
}

// DeferralReason returns the short machine-readable reason persisted with a
// deferral.
func DeferralReason(err error) string {
	var admission *AdmissionPendingError
	if errors.As(err, &admission) {
		return "admission pending"
	}
	return "deferred"
}

// deferralFor records a deferred stage error in the manifest.
func deferralFor(err error, phase model.Phase, now time.Time) model.Deferral {
	return model.Deferral{
		Phase:      phase,
		Reason:     DeferralReason(err),
		Message:    err.Error(),
		DeferredAt: now.UTC(),
	}
}
