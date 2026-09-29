package backoff

import (
	"context"
	"errors"
	"time"
)

// DefaultMaxElapsedTime sets a default limit for the total retry duration.
const DefaultMaxElapsedTime = 15 * time.Minute

// Operation is the function Retry calls. It is invoked at least once and may be
// retried on error. Return a Permanent error to stop retrying immediately, or a
// RetryAfterError to control the delay before the next attempt.
type Operation[T any] func() (T, error)

// Notify is called after a failed attempt that will be retried, with the
// operation error and the backoff duration before the next attempt. It is
// called once per retry, not for the final error that stops Retry (a permanent
// error, an exhausted limit, or a cancelled context).
type Notify func(error, time.Duration)

// retryOptions holds configuration settings for the retry mechanism.
type retryOptions struct {
	BackOff        BackOff       // Strategy for calculating backoff periods.
	Timer          timer         // Timer to manage retry delays.
	Notify         Notify        // Optional function called before each backoff wait.
	MaxTries       uint          // Maximum number of retry attempts.
	MaxElapsedTime time.Duration // Maximum total time for all retries.
	Budget         *Budget       // Optional shared remaining-time budget.
	Limits         []retryLimit  // Per-error-class attempt limits.
}

// retryLimit caps the number of attempts whose error matches a class.
type retryLimit struct {
	match    func(error) bool
	maxTries uint
	tries    uint
}

// RetryOption configures the behavior of Retry.
type RetryOption func(*retryOptions)

// WithBackOff configures the backoff policy used between attempts. The default
// is NewExponentialBackOff.
//
// Retry calls Reset on the policy before the first attempt, so a previously
// used policy may be passed. A BackOff is stateful and not safe for concurrent
// use: give each concurrent Retry call its own BackOff rather than sharing one.
func WithBackOff(b BackOff) RetryOption {
	return func(args *retryOptions) {
		args.BackOff = b
	}
}

// withTimer sets a custom timer for managing delays between retries.
func withTimer(t timer) RetryOption {
	return func(args *retryOptions) {
		args.Timer = t
	}
}

// WithNotify sets a function called after each failed attempt that will be
// retried. See Notify for exactly when it fires.
func WithNotify(n Notify) RetryOption {
	return func(args *retryOptions) {
		args.Notify = n
	}
}

// WithMaxTries limits the total number of attempts, not retries: WithMaxTries(1)
// runs the operation once and does not retry. When the limit is reached, Retry
// returns a *RetryError with Cause ErrExhausted. The default, 0, means no limit.
func WithMaxTries(n uint) RetryOption {
	return func(args *retryOptions) {
		args.MaxTries = n
	}
}

// WithMaxElapsedTime limits the total wall-clock time spent retrying, measured
// from when Retry is called. When the limit is reached, Retry returns a
// *RetryError with Cause ErrMaxElapsedTime.
//
// The limit is checked only between attempts: it gates whether another attempt
// is scheduled. It does not interrupt an operation that is already running, nor
// a backoff wait already in progress, and Retry stops early rather than
// starting a backoff that would overrun the limit.
//
// This differs from bounding Retry with a context deadline (e.g.
// context.WithTimeout): a context deadline is reactive — it interrupts the
// backoff wait and, if the operation observes the context, can abort an
// in-flight attempt — and Retry reports it with Cause context.DeadlineExceeded.
//
// The default is DefaultMaxElapsedTime (15 minutes), so both limits are active
// at once unless overridden. Pass 0 to disable the elapsed-time limit and rely
// on the context (or WithMaxTries) instead.
func WithMaxElapsedTime(d time.Duration) RetryOption {
	return func(args *retryOptions) {
		args.MaxElapsedTime = d
	}
}

// WithBudget bounds retrying by a remaining-time budget shared across Retry
// calls — typically one per logical request, created at its entry point and
// handed down through nested layers. Before scheduling another attempt, Retry
// checks the budget's remaining time (which already reflects everything spent
// so far, by any layer); when it cannot cover the next backoff window, Retry
// stops immediately instead of waiting, and returns a *RetryError with Cause
// ErrMaxElapsedTime and LastErr preserved.
//
// Unlike WithMaxElapsedTime, which measures from the moment each Retry call
// starts, a Budget measures from when the request started, so nested Retry
// loops cannot collectively overrun it. The two compose: the first limit
// reached stops the loop, and both report ErrMaxElapsedTime.
//
// Like WithMaxElapsedTime, the budget is checked only between attempts: it
// gates whether another attempt is scheduled and never interrupts a running
// operation or a wait already in progress. Use a context deadline for that.
func WithBudget(b *Budget) RetryOption {
	return func(args *retryOptions) {
		args.Budget = b
	}
}

// WithMaxTriesFor limits the total number of attempts whose error matches
// match, counted independently from WithMaxTries and from other classes.
// Like WithMaxTries, it counts attempts, not retries: WithMaxTriesFor(m, 1)
// stops on the first matching error without retrying it. When the limit is
// reached, Retry returns a *RetryError with Cause ErrExhausted and LastErr
// set to the matching error. A maxTries of 0 means no limit for that class.
//
// The option may be supplied multiple times to give different error classes
// different limits; an error can match several classes and counts against
// each of them. match receives the error exactly as the operation returned
// it, so use errors.Is or errors.As inside it to see through wrapping:
//
//	backoff.Retry(ctx, op,
//		backoff.WithMaxTriesFor(func(err error) bool {
//			return errors.Is(err, syscall.ECONNRESET)
//		}, 5),
//		backoff.WithMaxTriesFor(func(err error) bool {
//			return errors.Is(err, errRateLimited)
//		}, 2),
//	)
//
// Errors matched by no class are unlimited (beyond WithMaxTries and the
// other limits). A permanent error still stops Retry immediately, before any
// class limit is consulted.
func WithMaxTriesFor(match func(error) bool, maxTries uint) RetryOption {
	return func(args *retryOptions) {
		args.Limits = append(args.Limits, retryLimit{match: match, maxTries: maxTries})
	}
}

// WithRandomizationFactor sets the jitter ratio of the backoff policy Retry
// uses: each wait is randomized within ±f of its interval, so f == 0 disables
// jitter. It applies to the policy configured at the point the option is
// applied — the default NewExponentialBackOff, or one supplied earlier with
// WithBackOff — and only when that policy is an *ExponentialBackOff; other
// policies are left untouched.
func WithRandomizationFactor(f float64) RetryOption {
	return func(args *retryOptions) {
		if exp, ok := args.BackOff.(*ExponentialBackOff); ok {
			exp.RandomizationFactor = f
		}
	}
}

// Retry attempts the operation until it succeeds, returns a Permanent error,
// or backoff completes. It ensures the operation is executed at least once.
//
// On success it returns the operation result and a nil error. On any failure
// it returns the last result and a *RetryError whose Cause reports why it
// stopped — ErrPermanent, ErrExhausted, ErrMaxElapsedTime, or the context
// cancellation cause — and whose LastErr holds the last operation error.
// See RetryError and AsRetryError.
//
// ctx bounds the retry loop: its cancellation or deadline stops further
// attempts and interrupts the wait between them. The operation receives no
// context, so capture ctx inside the operation if you want cancellation to
// abort an in-flight attempt. To bound only how long backoff keeps retrying,
// without affecting in-flight attempts, use WithMaxElapsedTime instead.
func Retry[T any](ctx context.Context, operation Operation[T], opts ...RetryOption) (T, error) {
	// Initialize default retry options.
	args := &retryOptions{
		BackOff:        NewExponentialBackOff(),
		Timer:          &defaultTimer{},
		MaxElapsedTime: DefaultMaxElapsedTime,
	}

	// Apply user-provided options to the default settings.
	for _, opt := range opts {
		opt(args)
	}

	defer args.Timer.Stop()

	startedAt := time.Now()
	args.BackOff.Reset()
	for numTries := uint(1); ; numTries++ {
		// Execute the operation.
		res, err := operation()
		if err == nil {
			return res, nil
		}

		// Stop immediately on a permanent error; surface it as a RetryError.
		var perm *permanent
		if errors.As(err, &perm) {
			return res, &RetryError{LastErr: perm.err, Cause: ErrPermanent}
		}

		// A RetryAfterError carries the delay before the next attempt; if it also
		// carries an underlying error, that is the meaningful error to report as
		// LastErr should retrying stop (mirrors how a permanent error surfaces its
		// inner error). errors.As matches it whether returned directly or wrapped.
		lastErr := err
		var retryAfter *RetryAfterError
		if errors.As(err, &retryAfter) && retryAfter.err != nil {
			lastErr = retryAfter.err
		}

		// Stop retrying if a per-error-class attempt limit is reached.
		exhausted := false
		for i := range args.Limits {
			l := &args.Limits[i]
			if l.match != nil && l.match(err) {
				l.tries++
				if l.maxTries > 0 && l.tries >= l.maxTries {
					exhausted = true
				}
			}
		}
		if exhausted {
			return res, &RetryError{LastErr: lastErr, Cause: ErrExhausted}
		}

		// Stop retrying if maximum tries exceeded.
		if args.MaxTries > 0 && numTries >= args.MaxTries {
			return res, &RetryError{LastErr: lastErr, Cause: ErrExhausted}
		}

		// Stop retrying if context is cancelled.
		if cerr := context.Cause(ctx); cerr != nil {
			return res, &RetryError{LastErr: lastErr, Cause: cerr}
		}

		// Calculate next backoff duration.
		next := args.BackOff.NextBackOff()
		if next == Stop {
			return res, &RetryError{LastErr: lastErr, Cause: ErrExhausted}
		}

		// Reset backoff if a RetryAfterError requested a specific delay.
		if retryAfter != nil {
			next = retryAfter.Duration
			args.BackOff.Reset()
		}

		// Stop retrying if maximum elapsed time exceeded.
		if args.MaxElapsedTime > 0 && time.Since(startedAt)+next > args.MaxElapsedTime {
			return res, &RetryError{LastErr: lastErr, Cause: ErrMaxElapsedTime}
		}

		// Stop retrying if the shared budget cannot cover the next wait.
		if args.Budget != nil {
			if remaining := args.Budget.Remaining(); remaining <= 0 || remaining < next {
				return res, &RetryError{LastErr: lastErr, Cause: ErrMaxElapsedTime}
			}
		}

		// Notify on error if a notifier function is provided.
		if args.Notify != nil {
			args.Notify(err, next)
		}

		// Wait for the next backoff period or context cancellation.
		args.Timer.Start(next)
		select {
		case <-args.Timer.C():
		case <-ctx.Done():
			return res, &RetryError{LastErr: lastErr, Cause: context.Cause(ctx)}
		}
	}
}
