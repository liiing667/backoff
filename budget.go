package backoff

import (
	"sync"
	"time"
)

// TimeBudget is a wall-clock budget shared by every Retry call that belongs to
// the same request. When several layers of a request each wrap their work in
// Retry (e.g. handler → service → gRPC client), passing the same budget to all
// of them lets the time spent by lower layers count against the upper layers:
// once the budget is spent, every layer stops instead of waiting out another
// backoff window for a deadline the request has already effectively missed.
//
// Before each retry, Retry charges the time elapsed since the previous charge
// (the failed attempt, plus any wait already endured) against the budget, and
// stops with a *RetryError whose Cause is ErrMaxElapsedTime — the same cause
// WithMaxElapsedTime uses — when the remaining budget cannot cover the next
// backoff wait. The wait itself is charged up front, so nested Retry calls
// observe it as spent while it happens.
//
// Like WithMaxElapsedTime, the budget gates whether another attempt is
// scheduled: it never interrupts a running operation or a wait in progress.
//
// A TimeBudget is safe for concurrent use and may be shared by any number of
// Retry calls, sequentially or concurrently.
type TimeBudget struct {
	mu        sync.Mutex
	remaining time.Duration
}

// NewTimeBudget returns a TimeBudget holding d. A zero or negative d leaves no
// room for any retry wait: the first failure stops Retry immediately.
func NewTimeBudget(d time.Duration) *TimeBudget {
	return &TimeBudget{remaining: d}
}

// Remaining reports the unspent budget. It never goes below zero.
func (b *TimeBudget) Remaining() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.remaining
}

// spend charges d against the budget, flooring the remainder at zero.
func (b *TimeBudget) spend(d time.Duration) {
	if d <= 0 {
		return
	}
	b.mu.Lock()
	b.remaining -= d
	if b.remaining < 0 {
		b.remaining = 0
	}
	b.mu.Unlock()
}
