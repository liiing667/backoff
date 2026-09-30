package backoff

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWithRandomizationFactor(t *testing.T) {
	t.Run("zero disables jitter", func(t *testing.T) {
		// Default policy: InitialInterval 500ms, Multiplier 1.5. With no jitter
		// the captured waits are the exact exponential intervals.
		tm := &spyTimer{}
		calls := 0
		_, err := Retry(context.Background(),
			func() (int, error) {
				calls++
				if calls == 3 {
					return 1, nil
				}
				return 0, errors.New("boom")
			},
			WithRandomizationFactor(0),
			WithMaxElapsedTime(0),
			withTimer(tm),
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []time.Duration{500 * time.Millisecond, 750 * time.Millisecond}
		if len(tm.starts) != len(want) {
			t.Fatalf("waits = %v, want %v", tm.starts, want)
		}
		for i := range want {
			if tm.starts[i] != want[i] {
				t.Errorf("wait %d = %v, want %v", i, tm.starts[i], want[i])
			}
		}
	})

	t.Run("waits stay inside the jitter band", func(t *testing.T) {
		tm := &spyTimer{}
		calls := 0
		_, err := Retry(context.Background(),
			func() (int, error) {
				calls++
				if calls == 6 {
					return 1, nil
				}
				return 0, errors.New("boom")
			},
			WithRandomizationFactor(0.5),
			WithMaxElapsedTime(0),
			withTimer(tm),
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		base := 500 * time.Millisecond
		for i, got := range tm.starts {
			low := base / 2
			high := base + base/2 // getRandomValueFromInterval rounds up by 1ns
			if got < low || got > high+1 {
				t.Errorf("wait %d = %v, want within [%v, %v]", i, got, low, high)
			}
			base = time.Duration(float64(base) * 1.5)
		}
	})

	t.Run("ignored with a custom backoff policy", func(t *testing.T) {
		tm := &spyTimer{}
		calls := 0
		_, err := Retry(context.Background(),
			func() (int, error) {
				calls++
				if calls == 3 {
					return 1, nil
				}
				return 0, errors.New("boom")
			},
			WithBackOff(NewConstantBackOff(time.Second)),
			WithRandomizationFactor(0),
			WithMaxElapsedTime(0),
			withTimer(tm),
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for i, got := range tm.starts {
			if got != time.Second {
				t.Errorf("wait %d = %v, want 1s", i, got)
			}
		}
	})

	t.Run("out-of-range factor is clamped", func(t *testing.T) {
		tm := &spyTimer{}
		_, err := Retry(context.Background(),
			func() (int, error) { return 0, errors.New("boom") },
			WithRandomizationFactor(5),
			WithMaxTries(2),
			WithMaxElapsedTime(0),
			withTimer(tm),
		)
		if !errors.Is(err, ErrExhausted) {
			t.Fatalf("Cause = %v, want ErrExhausted", err)
		}
		if len(tm.starts) != 1 {
			t.Fatalf("waits = %v, want one wait", tm.starts)
		}
		// Factor 5 clamps to 1: the 500ms interval is jittered within [0, 1s].
		if got := tm.starts[0]; got < 0 || got > time.Second+1 {
			t.Errorf("wait = %v, want within [0, 1s]", got)
		}
	})
}
