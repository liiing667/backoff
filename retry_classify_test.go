package backoff

import (
	"context"
	"errors"
	"testing"
)

func TestRetryErrorClassifierLimit(t *testing.T) {
	// maxRetries counts retries, so 2 allowed retries mean 3 total attempts.
	flaky := errors.New("flaky")
	calls := 0

	_, err := Retry(context.Background(),
		func() (int, error) {
			calls++
			return 0, flaky
		},
		WithErrorClassifier(func(error) (string, uint, bool) {
			return "flaky", 2, true
		}),
		WithBackOff(&ZeroBackOff{}),
		WithMaxElapsedTime(0),
		withTimer(&testTimer{}),
	)
	if calls != 3 {
		t.Errorf("operation called %d times, want 3", calls)
	}
	if !errors.Is(err, ErrExhausted) {
		t.Errorf("Cause = %v, want ErrExhausted", err)
	}
	if !errors.Is(err, flaky) {
		t.Errorf("operation error not in result: %v", err)
	}
	if re := AsRetryError(err); re == nil || re.LastErr != flaky {
		t.Errorf("LastErr = %v, want %v", re, flaky)
	}
}

func TestRetryErrorClassifierPerClass(t *testing.T) {
	// Each class has an independent allowance: A and B each get one retry, so
	// A, B, A exhausts A on the third attempt before B is ever exhausted.
	errA := errors.New("a")
	errB := errors.New("b")
	seq := []error{errA, errB, errA, errB, errA}
	calls := 0

	_, err := Retry(context.Background(),
		func() (int, error) {
			err := seq[calls]
			calls++
			return 0, err
		},
		WithErrorClassifier(func(err error) (string, uint, bool) {
			switch {
			case errors.Is(err, errA):
				return "A", 1, true
			case errors.Is(err, errB):
				return "B", 1, true
			default:
				return "", 0, false
			}
		}),
		WithBackOff(&ZeroBackOff{}),
		WithMaxElapsedTime(0),
		withTimer(&testTimer{}),
	)
	if calls != 3 {
		t.Errorf("operation called %d times, want 3", calls)
	}
	if !errors.Is(err, ErrExhausted) {
		t.Errorf("Cause = %v, want ErrExhausted", err)
	}
	if !errors.Is(err, errA) {
		t.Errorf("LastErr class = %v, want errA", err)
	}
}

func TestRetryErrorClassifierUnmatchedNotLimited(t *testing.T) {
	// ok=false means no class limit applies; WithMaxTries still governs.
	calls := 0
	_, err := Retry(context.Background(),
		func() (int, error) {
			calls++
			return 0, errors.New("unclassified")
		},
		WithErrorClassifier(func(error) (string, uint, bool) {
			return "", 0, false
		}),
		WithBackOff(&ZeroBackOff{}),
		WithMaxTries(3),
		WithMaxElapsedTime(0),
		withTimer(&testTimer{}),
	)
	if calls != 3 {
		t.Errorf("operation called %d times, want 3", calls)
	}
	if !errors.Is(err, ErrExhausted) {
		t.Errorf("Cause = %v, want ErrExhausted", err)
	}
}

func TestRetryErrorClassifierPermanentWins(t *testing.T) {
	// A permanent error stops before any class allowance is consulted.
	stop := errors.New("stop")
	calls := 0
	_, err := Retry(context.Background(),
		func() (int, error) {
			calls++
			return 0, Permanent(stop)
		},
		WithErrorClassifier(func(error) (string, uint, bool) {
			return "anything", 99, true
		}),
		WithBackOff(&ZeroBackOff{}),
		WithMaxElapsedTime(0),
		withTimer(&testTimer{}),
	)
	if calls != 1 {
		t.Errorf("operation called %d times, want 1", calls)
	}
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("Cause = %v, want ErrPermanent", err)
	}
	if !errors.Is(err, stop) {
		t.Errorf("permanent error not in result: %v", err)
	}
}
