package clock_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
)

func TestSleep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if err := clock.Sleep(t.Context(), clock.Real(), 5*time.Second); err != nil {
			t.Fatalf("Sleep: %v", err)
		}
		if got := time.Since(start); got != 5*time.Second {
			t.Errorf("slept %v, want 5s", got)
		}
	})
}

func TestSleepCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		start := time.Now()
		if err := clock.Sleep(ctx, clock.Real(), time.Hour); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Sleep error = %v, want deadline exceeded", err)
		}
		if got := time.Since(start); got != time.Second {
			t.Errorf("returned after %v, want 1s", got)
		}
	})
}
