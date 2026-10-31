// Package fakeclock provides a clock.Clock whose time moves only when a test advances it.
package fakeclock

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/testkit"
)

// Clock is a manually advanced clock.Clock. Its timers follow time.Timer semantics as of Go 1.23: after Stop or
// Reset returns, no stale value can be received.
type Clock struct {
	changed testkit.Notifier

	mu    sync.Mutex
	now   time.Time
	armed []*timer // in arming order
}

var _ clock.Clock = (*Clock)(nil)

// New returns a clock set to now.
func New(now time.Time) *Clock { return &Clock{now: now} }

// Now returns the clock's current time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// NewTimer returns a timer that fires once the clock has advanced by d.
func (c *Clock) NewTimer(d time.Duration) clock.Timer {
	t := &timer{c: c, ch: make(chan time.Time, 1)}
	t.Reset(d)
	return t
}

// Advance moves the time forward by d. Due timers fire in deadline order, each with the time set to its
// deadline; timers armed in reaction to those firings wait for the next Advance.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	end := c.now.Add(d)
	for {
		i := c.next(end)
		if i < 0 {
			break
		}
		t := c.armed[i]
		c.armed = slices.Delete(c.armed, i, i+1)
		c.now = t.when
		t.ch <- t.when
	}
	c.now = end
	c.changed.Notify()
}

// Timers returns the number of armed timers.
func (c *Clock) Timers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.armed)
}

// AwaitTimers blocks until at least n timers are armed, so a test can advance past a timer that another
// goroutine is about to arm.
func (c *Clock) AwaitTimers(t testing.TB, n int) {
	t.Helper()
	c.changed.Await(t, fmt.Sprintf("%d armed timers", n), func() bool { return c.Timers() >= n })
}

// next returns the index of the earliest timer due by end, or -1.
func (c *Clock) next(end time.Time) int {
	best := -1
	for i, t := range c.armed {
		if !t.when.After(end) && (best < 0 || t.when.Before(c.armed[best].when)) {
			best = i
		}
	}
	return best
}

type timer struct {
	c    *Clock
	ch   chan time.Time
	when time.Time // guarded by c.mu
}

func (t *timer) C() <-chan time.Time { return t.ch }

func (t *timer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	return t.stop()
}

func (t *timer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	active := t.stop()
	t.when = t.c.now.Add(d)
	if d <= 0 {
		t.ch <- t.when
	} else {
		t.c.armed = append(t.c.armed, t)
	}
	t.c.mu.Unlock()
	t.c.changed.Notify()
	return active
}

// stop disarms t and drops an unreceived value; it reports whether either was pending.
func (t *timer) stop() bool {
	i := slices.Index(t.c.armed, t)
	if i >= 0 {
		t.c.armed = slices.Delete(t.c.armed, i, i+1)
	}
	select {
	case <-t.ch:
		return true
	default:
		return i >= 0
	}
}
