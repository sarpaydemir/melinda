package fakeclock_test

import (
	"context"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/testkit/fakeclock"
)

var start = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func received(tm clock.Timer) (time.Time, bool) {
	select {
	case v := <-tm.C():
		return v, true
	default:
		return time.Time{}, false
	}
}

func TestAdvanceFiresDueTimersInDeadlineOrder(t *testing.T) {
	c := fakeclock.New(start)
	late := c.NewTimer(3 * time.Second)
	early := c.NewTimer(time.Second)
	never := c.NewTimer(time.Hour)

	c.Advance(999 * time.Millisecond)
	if _, ok := received(early); ok {
		t.Fatal("timer fired before its deadline")
	}
	c.Advance(5 * time.Second)
	if v, ok := received(early); !ok || !v.Equal(start.Add(time.Second)) {
		t.Errorf("early fired %v, %v; want its deadline", v, ok)
	}
	if v, ok := received(late); !ok || !v.Equal(start.Add(3*time.Second)) {
		t.Errorf("late fired %v, %v; want its deadline", v, ok)
	}
	if _, ok := received(never); ok {
		t.Error("timer fired an hour early")
	}
	if got, want := c.Now(), start.Add(5999*time.Millisecond); !got.Equal(want) {
		t.Errorf("Now = %v, want %v", got, want)
	}
	if n := c.Timers(); n != 1 {
		t.Errorf("armed timers = %d, want 1", n)
	}
}

func TestStopAndResetFollowTimerSemantics(t *testing.T) {
	c := fakeclock.New(start)
	tm := c.NewTimer(time.Second)
	if !tm.Stop() {
		t.Error("Stop of an armed timer = false")
	}
	if tm.Stop() {
		t.Error("second Stop = true")
	}
	c.Advance(time.Hour)
	if _, ok := received(tm); ok {
		t.Error("stopped timer fired")
	}

	if tm.Reset(time.Second) {
		t.Error("Reset of a stopped timer = true")
	}
	c.Advance(time.Second)
	// Fired but not received: Reset drops the stale value and reports the timer as active.
	if !tm.Reset(time.Minute) {
		t.Error("Reset of a fired, unreceived timer = false")
	}
	if _, ok := received(tm); ok {
		t.Error("stale value received after Reset")
	}
	c.Advance(time.Minute)
	if v, ok := received(tm); !ok || !v.Equal(c.Now()) {
		t.Errorf("reset timer fired %v, %v; want %v", v, ok, c.Now())
	}
}

func TestNonPositiveDurationFiresImmediately(t *testing.T) {
	c := fakeclock.New(start)
	if v, ok := received(c.NewTimer(0)); !ok || !v.Equal(start) {
		t.Errorf("zero timer fired %v, %v; want %v", v, ok, start)
	}
}

func TestSleepWakesWhenAdvanced(t *testing.T) {
	c := fakeclock.New(start)
	done := make(chan error)
	go func() { done <- clock.Sleep(context.Background(), c, time.Minute) }()

	c.AwaitTimers(t, 1)
	c.Advance(time.Minute)
	if err := <-done; err != nil {
		t.Fatalf("Sleep = %v", err)
	}
	if n := c.Timers(); n != 0 {
		t.Errorf("armed timers after Sleep = %d, want 0", n)
	}
}

func TestNextIsTheWaitForTheEarliestTimer(t *testing.T) {
	c := fakeclock.New(start)
	if d, ok := c.Next(); ok {
		t.Errorf("Next with no timer = %v, true", d)
	}
	c.NewTimer(time.Hour)
	c.NewTimer(3 * time.Second)
	c.Advance(time.Second)
	if d, ok := c.Next(); !ok || d != 2*time.Second {
		t.Errorf("Next = %v, %v; want 2s", d, ok)
	}
}
