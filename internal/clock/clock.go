// Package clock provides the current time and timers behind an interface so tests can control them.
package clock

import (
	"context"
	"time"
)

// Clock is the source of time for code that reads the time or waits.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is a single-shot timer with the semantics of time.Timer.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// Real returns the system clock.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }

func (r realTimer) Stop() bool { return r.t.Stop() }

func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }

// Sleep waits for d on c and returns ctx.Err() if ctx ends first.
func Sleep(ctx context.Context, c Clock, d time.Duration) error {
	t := c.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
