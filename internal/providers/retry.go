package providers

import (
	"context"
	"fmt"
	"time"
)

const (
	// DefaultMax5xxRetries is how many times the default policy retries an
	// operation after the first attempt fails with a provider 5xx response.
	DefaultMax5xxRetries = 3
	// Default5xxRetryDelay is the fixed wait before each default-policy retry.
	Default5xxRetryDelay = 3 * time.Minute
)

// RetryPolicy controls retries of provider 5xx responses. The delay is fixed;
// there is no exponential backoff.
type RetryPolicy struct {
	// MaxRetries is the number of retries after the first attempt, so the
	// operation runs at most MaxRetries+1 times. Negative values mean no retries.
	MaxRetries int
	// Delay is the wait before each retry.
	Delay time.Duration
	// Sleep waits for Delay and reports context cancellation. Tests replace it;
	// nil uses a timer that returns the context error when ctx ends.
	Sleep func(context.Context, time.Duration) error
}

// DefaultRetryPolicy returns the default maximum retry count and delay for
// provider 5xx errors. The policy is a value; mutating it does not affect later
// calls.
//
// One attempt plus three retries, three minutes apart: patient enough for a
// provider hiccup, short enough that a wedged run still looks wedged. Rate-limit
// responses (429) are not 5xx and get no retry—they need a different
// conversation.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: DefaultMax5xxRetries, Delay: Default5xxRetryDelay}
}

// RetryOn5xx calls operation and retries while is5xx classifies the error and
// fewer than policy.MaxRetries retries have been made. It returns the first
// success, the first error is5xx rejects, the last 5xx error once retries are
// exhausted, or the sleep error when ctx ends during a wait. Errors are
// returned unwrapped.
//
// operation and is5xx are required. Delay is applied unchanged before each
// retry; a non-positive Delay retries immediately unless ctx is already done.
func RetryOn5xx(ctx context.Context, policy RetryPolicy, operation func() error, is5xx func(error) bool) error {
	if operation == nil {
		return fmt.Errorf("retry operation is required")
	}
	if is5xx == nil {
		return fmt.Errorf("5xx classifier is required")
	}
	if policy.MaxRetries < 0 {
		policy.MaxRetries = 0
	}
	sleep := policy.Sleep
	if sleep == nil {
		sleep = waitRetryDelay
	}

	for attempt := 0; ; attempt++ {
		err := operation()
		if err == nil || !is5xx(err) || attempt >= policy.MaxRetries {
			return err
		}
		if err := sleep(ctx, policy.Delay); err != nil {
			return err
		}
	}
}

// waitRetryDelay waits for the retry delay or returns when the context is canceled.
func waitRetryDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
