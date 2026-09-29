package backoff

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestBudgetRemaining(t *testing.T) {
	b := NewBudget(time.Minute)
	if r := b.Remaining(); r <= 0 || r > time.Minute {
		t.Errorf("Remaining() = %v, want in (0, 1m]", r)
	}

	// A non-positive budget is exhausted from the start.
	if r := NewBudget(0).Remaining(); r > 0 {
		t.Errorf("Remaining() = %v, want <= 0 for a zero budget", r)
	}

	// Remaining decreases as time passes: spent time is deducted.
	b = NewBudget(time.Hour)
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got, want := b.Remaining(), time.Hour-50*time.Millisecond; got >= want {
		t.Errorf("Remaining() = %v, want < %v after 50ms", got, want)
	}
}

func TestRetryBudgetExhausted(t *testing.T) {
	// The budget cannot cover the next backoff window: Retry stops at once
	// with ErrMaxElapsedTime instead of waiting, and never starts the timer.
	opErr := errors.New("operation error")
	tm := &spyTimer{}
	calls := 0

	_, err := Retry(context.Background(),
		func() (int, error) {
			calls++
			return 0, opErr
		},
		WithBackOff(NewConstantBackOff(time.Hour)),
		WithBudget(NewBudget(time.Millisecond)),
		withTimer(tm),
	)
	if !errors.Is(err, ErrMaxElapsedTime) {
		t.Errorf("ErrMaxElapsedTime not in result: %v", err)
	}
	if !errors.Is(err, opErr) {
		t.Errorf("operation error not in result: %v", err)
	}
	re := AsRetryError(err)
	if re == nil || re.Cause != ErrMaxElapsedTime || re.LastErr != opErr {
		t.Errorf("RetryError = %+v, want Cause ErrMaxElapsedTime and LastErr %v", re, opErr)
	}
	if calls != 1 {
		t.Errorf("operation called %d times, want 1 (no retry without budget)", calls)
	}
	if len(tm.starts) != 0 {
		t.Errorf("timer waits = %v, want none: the backoff window must not be waited out", tm.starts)
	}
}

func TestRetryBudgetSufficient(t *testing.T) {
	// A comfortable budget does not change the happy path.
	const successOn = 3
	calls := 0

	res, err := Retry(context.Background(),
		func() (int, error) {
			calls++
			if calls == successOn {
				return 42, nil
			}
			return 0, errors.New("error")
		},
		WithBackOff(&ZeroBackOff{}),
		WithBudget(NewBudget(time.Minute)),
		withTimer(&testTimer{}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != 42 || calls != successOn {
		t.Errorf("res = %d, calls = %d, want 42 and %d", res, calls, successOn)
	}
}

func TestRetryBudgetSharedAcrossLayers(t *testing.T) {
	// One budget for the whole request: the inner layer retries until the
	// budget runs out, then the outer layer stops immediately instead of
	// waiting out its own (much longer) backoff window.
	budget := NewBudget(150 * time.Millisecond)
	opErr := errors.New("operation error")

	inner := func() (int, error) { return 0, opErr }

	outerTimer := &spyTimer{}
	outerCalls := 0
	_, err := Retry(context.Background(),
		func() (int, error) {
			outerCalls++
			// The nested call shares the budget and burns it with real waits.
			_, err := Retry(context.Background(), inner,
				WithBackOff(NewConstantBackOff(40*time.Millisecond)),
				WithBudget(budget),
			)
			return 0, err
		},
		WithBackOff(NewConstantBackOff(time.Hour)),
		WithBudget(budget),
		withTimer(outerTimer),
	)
	if !errors.Is(err, ErrMaxElapsedTime) {
		t.Errorf("ErrMaxElapsedTime not in result: %v", err)
	}
	if !errors.Is(err, opErr) {
		t.Errorf("inner operation error not in result: %v", err)
	}
	if outerCalls != 1 {
		t.Errorf("outer operation called %d times, want 1 (budget already spent)", outerCalls)
	}
	if len(outerTimer.starts) != 0 {
		t.Errorf("outer timer waits = %v, want none: the outer layer must not wait", outerTimer.starts)
	}
}

func TestRetryMaxTriesFor(t *testing.T) {
	errA := errors.New("error a")
	errB := errors.New("error b")
	isA := func(err error) bool { return errors.Is(err, errA) }
	isB := func(err error) bool { return errors.Is(err, errB) }

	t.Run("matching class stops at its own limit", func(t *testing.T) {
		calls := 0
		_, err := Retry(context.Background(),
			func() (int, error) {
				calls++
				return 0, errA
			},
			WithBackOff(&ZeroBackOff{}),
			WithMaxTriesFor(isA, 3),
			withTimer(&testTimer{}),
		)
		if !errors.Is(err, ErrExhausted) {
			t.Errorf("ErrExhausted not in result: %v", err)
		}
		if !errors.Is(err, errA) {
			t.Errorf("operation error not in result: %v", err)
		}
		if re := AsRetryError(err); re == nil || re.LastErr != errA {
			t.Errorf("LastErr = %v, want %v", re, errA)
		}
		if calls != 3 {
			t.Errorf("operation called %d times, want 3", calls)
		}
	})

	t.Run("classes are counted independently", func(t *testing.T) {
		// errA is allowed two attempts, errB only one: the first errB stops
		// Retry even though the errA budget is not exhausted.
		errs := []error{errA, errB, errA}
		calls := 0
		_, err := Retry(context.Background(),
			func() (int, error) {
				err := errs[calls]
				calls++
				return 0, err
			},
			WithBackOff(&ZeroBackOff{}),
			WithMaxTriesFor(isA, 2),
			WithMaxTriesFor(isB, 1),
			withTimer(&testTimer{}),
		)
		if !errors.Is(err, ErrExhausted) {
			t.Errorf("ErrExhausted not in result: %v", err)
		}
		if re := AsRetryError(err); re == nil || re.LastErr != errB {
			t.Errorf("LastErr = %v, want %v", re, errB)
		}
		if calls != 2 {
			t.Errorf("operation called %d times, want 2", calls)
		}
	})

	t.Run("match sees through wrapping", func(t *testing.T) {
		calls := 0
		_, err := Retry(context.Background(),
			func() (int, error) {
				calls++
				return 0, fmt.Errorf("wrapped: %w", errA)
			},
			WithBackOff(&ZeroBackOff{}),
			WithMaxTriesFor(isA, 2),
			withTimer(&testTimer{}),
		)
		if !errors.Is(err, ErrExhausted) {
			t.Errorf("ErrExhausted not in result: %v", err)
		}
		if !errors.Is(err, errA) {
			t.Errorf("wrapped operation error not in result: %v", err)
		}
		if calls != 2 {
			t.Errorf("operation called %d times, want 2", calls)
		}
	})

	t.Run("unmatched errors are not limited", func(t *testing.T) {
		const successOn = 3
		calls := 0
		res, err := Retry(context.Background(),
			func() (int, error) {
				calls++
				if calls == successOn {
					return 1, nil
				}
				return 0, errB
			},
			WithBackOff(&ZeroBackOff{}),
			WithMaxTriesFor(isA, 1),
			withTimer(&testTimer{}),
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res != 1 || calls != successOn {
			t.Errorf("res = %d, calls = %d, want 1 and %d", res, calls, successOn)
		}
	})

	t.Run("permanent errors still win over class limits", func(t *testing.T) {
		calls := 0
		_, err := Retry(context.Background(),
			func() (int, error) {
				calls++
				return 0, Permanent(errA)
			},
			WithBackOff(&ZeroBackOff{}),
			WithMaxTriesFor(isA, 5),
			withTimer(&testTimer{}),
		)
		if !errors.Is(err, ErrPermanent) {
			t.Errorf("ErrPermanent not in result: %v", err)
		}
		if calls != 1 {
			t.Errorf("operation called %d times, want 1", calls)
		}
	})
}

func TestRetryRandomizationFactor(t *testing.T) {
	t.Run("zero disables jitter on the default policy", func(t *testing.T) {
		tm := &spyTimer{}
		calls := 0
		_, err := Retry(context.Background(),
			func() (int, error) {
				calls++
				if calls == 3 {
					return 1, nil
				}
				return 0, errors.New("error")
			},
			WithRandomizationFactor(0),
			withTimer(tm),
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []time.Duration{500 * time.Millisecond, 750 * time.Millisecond}
		if len(tm.starts) != len(want) {
			t.Fatalf("timer waits = %v, want %v", tm.starts, want)
		}
		for i, w := range want {
			if tm.starts[i] != w {
				t.Errorf("wait %d = %v, want exactly %v with jitter disabled", i, tm.starts[i], w)
			}
		}
	})

	t.Run("applies to a policy supplied with WithBackOff", func(t *testing.T) {
		exp := NewExponentialBackOff()
		calls := 0
		_, err := Retry(context.Background(),
			func() (int, error) {
				calls++
				if calls == 2 {
					return 1, nil
				}
				return 0, errors.New("error")
			},
			WithBackOff(exp),
			WithRandomizationFactor(0.25),
			withTimer(&testTimer{}),
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if exp.RandomizationFactor != 0.25 {
			t.Errorf("RandomizationFactor = %v, want 0.25", exp.RandomizationFactor)
		}
	})

	t.Run("ignored by non-exponential policies", func(t *testing.T) {
		tm := &spyTimer{}
		calls := 0
		_, err := Retry(context.Background(),
			func() (int, error) {
				calls++
				if calls == 2 {
					return 1, nil
				}
				return 0, errors.New("error")
			},
			WithBackOff(NewConstantBackOff(time.Second)),
			WithRandomizationFactor(0.9),
			withTimer(tm),
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(tm.starts) != 1 || tm.starts[0] != time.Second {
			t.Errorf("timer waits = %v, want a single exact 1s wait", tm.starts)
		}
	})
}
