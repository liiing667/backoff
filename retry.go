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
	BackOff             BackOff         // Strategy for calculating backoff periods.
	Timer               timer           // Timer to manage retry delays.
	Notify              Notify          // Optional function called before each backoff wait.
	MaxTries            uint            // Maximum number of retry attempts.
	MaxElapsedTime      time.Duration   // Maximum total time for all retries.
	Budget              *TimeBudget     // Optional budget shared across nested Retry calls.
	Classify            ErrorClassifier // Optional per-error-class retry allowances.
	RandomizationFactor float64         // Optional jitter for the default backoff; <0 means unset.
	defaultBackOff      bool            // Whether BackOff is the default exponential policy.
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
		args.defaultBackOff = false
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

// WithTimeBudget attaches a TimeBudget shared across Retry calls that belong
// to the same request. Before each retry, Retry charges the time already spent
// (the failed attempt plus any wait already endured) against the budget, and
// when the remaining budget cannot cover the next backoff wait it stops
// immediately — without sleeping — returning a *RetryError with Cause
// ErrMaxElapsedTime and LastErr set to the last operation error.
//
// This complements WithMaxElapsedTime: WithMaxElapsedTime bounds a single
// Retry call measured from its own start, whereas a shared TimeBudget spans
// nested or sequential calls, so time burned by lower layers leaves less
// budget for the layers above.
//
// The budget is checked only between attempts, like WithMaxElapsedTime; it
// never interrupts a running operation or a wait in progress. Passing nil
// disables the budget (the default).
func WithTimeBudget(b *TimeBudget) RetryOption {
	return func(args *retryOptions) {
		args.Budget = b
	}
}

// ErrorClassifier maps an operation error to a retry class and the maximum
// number of retries allowed for errors of that class. It is invoked with the
// error returned by each failed attempt. Return ok=false for errors the
// classifier does not cover; they are retried without a class-specific limit
// (but WithMaxTries, the budget, and the context still apply).
//
// Allowances are counted as retries, not total attempts, independently per
// class: WithMaxTries counts every attempt, while a class allowance counts
// only retries made for that class. When an allowance is exhausted, Retry
// returns a *RetryError with Cause ErrExhausted and LastErr set to the last
// operation error.
//
// For example, with gRPC status codes, one might allow more retries for
// codes.Unavailable than for codes.ResourceExhausted:
//
//	classify := func(err error) (string, uint, bool) {
//		st, ok := status.FromError(err)
//		if !ok {
//			return "", 0, false
//		}
//		switch st.Code() {
//		case codes.Unavailable:
//			return "unavailable", 5, true
//		case codes.ResourceExhausted:
//			return "rate_limited", 2, true
//		default:
//			return "", 0, false
//		}
//	}
type ErrorClassifier func(err error) (class string, maxRetries uint, ok bool)

// WithErrorClassifier limits how many times each error class may be retried,
// counted independently per class; see ErrorClassifier.
func WithErrorClassifier(c ErrorClassifier) RetryOption {
	return func(args *retryOptions) {
		args.Classify = c
	}
}

// WithRandomizationFactor sets the jitter ratio of the default exponential
// backoff policy: NextBackOff ranges within 1±factor of the current interval,
// so 0 disables jitter entirely and 0.5 (the default) allows ±50%. Values
// outside [0, 1] are clamped to that range.
//
// It applies only to the default ExponentialBackOff: combining it with
// WithBackOff gives the supplied policy priority, so configure a custom
// policy's jitter directly instead.
func WithRandomizationFactor(factor float64) RetryOption {
	return func(args *retryOptions) {
		args.RandomizationFactor = factor
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
		BackOff:             NewExponentialBackOff(),
		Timer:               &defaultTimer{},
		MaxElapsedTime:      DefaultMaxElapsedTime,
		RandomizationFactor: -1,
		defaultBackOff:      true,
	}

	// Apply user-provided options to the default settings.
	for _, opt := range opts {
		opt(args)
	}

	// Apply the jitter override to the default exponential policy.
	if args.defaultBackOff && args.RandomizationFactor >= 0 {
		if exp, ok := args.BackOff.(*ExponentialBackOff); ok {
			exp.RandomizationFactor = min(max(args.RandomizationFactor, 0), 1)
		}
	}

	defer args.Timer.Stop()

	startedAt := time.Now()
	var charged time.Duration
	var classRetries map[string]uint
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

		// Charge the time spent since the last charge against the shared budget.
		if args.Budget != nil {
			spent := time.Since(startedAt) - charged
			args.Budget.spend(spent)
			charged += spent
		}

		// Stop retrying if maximum tries exceeded.
		if args.MaxTries > 0 && numTries >= args.MaxTries {
			return res, &RetryError{LastErr: lastErr, Cause: ErrExhausted}
		}

		// Stop retrying if this error's class used up its retry allowance.
		if args.Classify != nil {
			if class, maxRetries, ok := args.Classify(err); ok {
				if classRetries == nil {
					classRetries = make(map[string]uint)
				}
				if classRetries[class] >= maxRetries {
					return res, &RetryError{LastErr: lastErr, Cause: ErrExhausted}
				}
				classRetries[class]++
			}
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

		// Stop retrying if the shared budget cannot cover the next wait, and
		// charge the wait up front so nested Retry calls see it as spent.
		if args.Budget != nil {
			if args.Budget.Remaining() < next {
				return res, &RetryError{LastErr: lastErr, Cause: ErrMaxElapsedTime}
			}
			args.Budget.spend(next)
			charged += next
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
