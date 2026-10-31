package testkit

import (
	"sync"
	"testing"
	"time"
)

// AwaitTimeout bounds every wait in the test kit, so a missing event fails the test instead of hanging it. It sits
// far above what a loaded machine needs: 200 racing appends to Postgres have taken 19 s at load 75.
const AwaitTimeout = 60 * time.Second

// Notifier wakes goroutines that wait for shared state to change. The zero value is ready to use.
type Notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

// C returns a channel that the next Notify closes.
func (n *Notifier) C() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch == nil {
		n.ch = make(chan struct{})
	}
	return n.ch
}

// Notify wakes every current waiter.
func (n *Notifier) Notify() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch != nil {
		close(n.ch)
		n.ch = nil
	}
}

// Await blocks until cond holds, checking it again after each Notify, and fails t after AwaitTimeout.
func (n *Notifier) Await(t testing.TB, what string, cond func() bool) {
	t.Helper()
	timeout := time.NewTimer(AwaitTimeout)
	defer timeout.Stop()
	for {
		changed := n.C()
		if cond() {
			return
		}
		select {
		case <-changed:
		case <-timeout.C:
			t.Fatalf("testkit: timed out after %v waiting for %s", AwaitTimeout, what)
		}
	}
}
