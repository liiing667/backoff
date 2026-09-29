package backoff

import "time"

// Budget is a remaining-time budget for one logical request, shared across
// Retry calls. Create it once at the entry point of a request and pass it to
// every nested Retry with WithBudget: each Retry loop then checks the same
// remaining time before scheduling another attempt, so a lower layer that
// burns through the budget makes upper layers stop immediately instead of
// waiting out their own backoff windows.
//
// The budget is wall-clock based: Remaining decreases as time passes, which
// is how time already spent — by any layer — is deducted from it. When the
// remaining time cannot cover the next backoff window, Retry stops and
// returns a *RetryError with Cause ErrMaxElapsedTime, the same cause used by
// WithMaxElapsedTime.
//
// A Budget is immutable after creation and safe to share across goroutines.
type Budget struct {
	deadline time.Time
}

// NewBudget returns a Budget with d remaining. d <= 0 yields an already
// exhausted budget: the first attempt still runs, but no retry is scheduled.
func NewBudget(d time.Duration) *Budget {
	return &Budget{deadline: time.Now().Add(d)}
}

// Remaining returns the time left in the budget. It is <= 0 once the budget
// is exhausted.
func (b *Budget) Remaining() time.Duration {
	return time.Until(b.deadline)
}
