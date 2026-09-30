package backoff

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTimeBudgetSpend(t *testing.T) {
	b := NewTimeBudget(100 * time.Millisecond)
	if got := b.Remaining(); got != 100*time.Millisecond {
		t.Errorf("Remaining = %v, want 100ms", got)
	}

	b.spend(30 * time.Millisecond)
	if got := b.Remaining(); got != 70*time.Millisecond {
		t.Errorf("Remaining = %v, want 70ms", got)
	}

	// Non-positive spends are ignored.
	b.spend(0)
	b.spend(-5 * time.Millisecond)
	if got := b.Remaining(); got != 70*time.Millisecond {
		t.Errorf("Remaining = %v, want 70ms", got)
	}

	// Overspend floors at zero rather than going negative.
	b.spend(time.Hour)
	if got := b.Remaining(); got != 0 {
		t.Errorf("Remaining = %v, want 0", got)
	}
}

func TestRetryTimeBudgetStopsInsteadOfWaiting(t *testing.T) {
	// The budget cannot cover the first backoff wait, so Retry must stop
	// immediately with ErrMaxElapsedTime (the existing time-exhaustion cause)
	// and never start a wait.
	opErr := errors.New("boom")
	tm := &spyTimer{}
	calls := 0

	_, err := Retry(context.Background(),
		func() (int, error) {
			calls++
			return 0, opErr
		},
		WithBackOff(NewConstantBackOff(time.Minute)),
		WithTimeBudget(NewTimeBudget(time.Second)),
		WithMaxElapsedTime(0),
		withTimer(tm),
	)
	if calls != 1 {
		t.Errorf("operation called %d times, want 1", calls)
	}
	if !errors.Is(err, ErrMaxElapsedTime) {
		t.Errorf("Cause = %v, want ErrMaxElapsedTime", err)
	}
	if !errors.Is(err, opErr) {
		t.Errorf("operation error not in result: %v", err)
	}
	if re := AsRetryError(err); re == nil || re.LastErr != opErr {
		t.Errorf("LastErr = %v, want %v", re, opErr)
	}
	if len(tm.starts) != 0 {
		t.Errorf("timer waited %v, want no waits", tm.starts)
	}
}

func TestRetryTimeBudgetChargesWaits(t *testing.T) {
	// Waits Retry actually schedules are charged up front, so repeated failures
	// drain the budget even when the test timer fires instantly.
	budget := NewTimeBudget(10 * time.Second)
	tm := &spyTimer{}

	_, err := Retry(context.Background(),
		func() (int, error) { return 0, errors.New("boom") },
		WithBackOff(NewConstantBackOff(2*time.Second)),
		WithTimeBudget(budget),
		WithMaxTries(5),
		WithMaxElapsedTime(0),
		withTimer(tm),
	)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("Cause = %v, want ErrExhausted", err)
	}
	if len(tm.starts) != 4 {
		t.Errorf("timer waited %d times, want 4", len(tm.starts))
	}
	// Four waits of 2s were charged, leaving 2s at most.
	if got := budget.Remaining(); got > 2*time.Second {
		t.Errorf("Remaining = %v, want <= 2s", got)
	}
}

func TestRetryTimeBudgetSharedAcrossLayers(t *testing.T) {
	// The inner layer burns most of the shared budget with a real attempt; the
	// outer layer must then stop immediately instead of waiting out its own
	// (much longer) backoff window.
	budget := NewTimeBudget(120 * time.Millisecond)
	innerErr := errors.New("inner boom")

	inner := func() (int, error) {
		return Retry(context.Background(),
			func() (int, error) {
				time.Sleep(80 * time.Millisecond) // simulate an RPC attempt
				return 0, innerErr
			},
			WithBackOff(NewConstantBackOff(60*time.Millisecond)),
			WithTimeBudget(budget),
			WithMaxElapsedTime(0),
			withTimer(&testTimer{}),
		)
	}

	outerTimer := &spyTimer{}
	outerCalls := 0
	start := time.Now()
	_, err := Retry(context.Background(),
		func() (int, error) {
			outerCalls++
			return inner()
		},
		WithBackOff(NewConstantBackOff(time.Hour)),
		WithTimeBudget(budget),
		WithMaxElapsedTime(0),
		withTimer(outerTimer),
	)
	elapsed := time.Since(start)

	if outerCalls != 1 {
		t.Errorf("outer operation called %d times, want 1", outerCalls)
	}
	if !errors.Is(err, ErrMaxElapsedTime) {
		t.Errorf("Cause = %v, want ErrMaxElapsedTime", err)
	}
	if !errors.Is(err, innerErr) {
		t.Errorf("inner error not in result: %v", err)
	}
	if len(outerTimer.starts) != 0 {
		t.Errorf("outer timer waited %v, want no waits", outerTimer.starts)
	}
	// The whole request finished promptly instead of waiting an hour.
	if elapsed > 5*time.Second {
		t.Errorf("elapsed = %v, expected the outer wait to be skipped", elapsed)
	}
}

func TestRetryTimeBudgetAllowsRetriesWithinBudget(t *testing.T) {
	// A generous budget does not change normal retry behavior.
	calls := 0
	_, err := Retry(context.Background(),
		func() (int, error) {
			calls++
			if calls == 3 {
				return 42, nil
			}
			return 0, errors.New("boom")
		},
		WithBackOff(&ZeroBackOff{}),
		WithTimeBudget(NewTimeBudget(time.Minute)),
		WithMaxElapsedTime(0),
		withTimer(&testTimer{}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Errorf("operation called %d times, want 3", calls)
	}
}

func TestRetryTimeBudgetNilDisables(t *testing.T) {
	// WithTimeBudget(nil) leaves Retry governed by the other limits as before.
	calls := 0
	_, err := Retry(context.Background(),
		func() (int, error) {
			calls++
			return 0, errors.New("boom")
		},
		WithBackOff(&ZeroBackOff{}),
		WithTimeBudget(nil),
		WithMaxTries(2),
		WithMaxElapsedTime(0),
		withTimer(&testTimer{}),
	)
	if calls != 2 {
		t.Errorf("operation called %d times, want 2", calls)
	}
	if !errors.Is(err, ErrExhausted) {
		t.Errorf("Cause = %v, want ErrExhausted", err)
	}
}
