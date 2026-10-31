package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
)

// AbortGrace is how long components get to return after in-flight work is cancelled at the shutdown deadline.
const AbortGrace = 5 * time.Second

// ErrShutdownTimeout reports that stopping took longer than the shutdown timeout and in-flight work was cancelled.
var ErrShutdownTimeout = errors.New("shutdown timeout")

var errEarlyExit = errors.New("exited before shutdown")

// ComponentError is a component failure: a Start or Run error, a panic, or a return before shutdown.
type ComponentError struct {
	Name string
	Err  error
}

func (e *ComponentError) Error() string { return e.Name + ": " + e.Err.Error() }

func (e *ComponentError) Unwrap() error { return e.Err }

type phase int

const (
	pending phase = iota
	starting
	running
	stopped
)

// Supervisor runs components: it starts them in order and stops them in reverse order.
type Supervisor struct {
	log     *slog.Logger
	clock   clock.Clock
	timeout time.Duration
	work    context.Context
	abort   context.CancelFunc

	mu         sync.Mutex
	components []Component
	phases     []phase
}

// NewSupervisor returns a supervisor whose stop sequence may take shutdownTimeout before in-flight work is cancelled.
func NewSupervisor(log *slog.Logger, clk clock.Clock, shutdownTimeout time.Duration) *Supervisor {
	work, abort := context.WithCancel(context.Background())
	return &Supervisor{log: log, clock: clk, timeout: shutdownTimeout, work: work, abort: abort}
}

// WorkContext is the parent context for in-flight work. It is cancelled when the shutdown timeout expires or Run
// returns, not when a component is asked to stop.
func (s *Supervisor) WorkContext() context.Context { return s.work }

type handle struct {
	name     string
	cancel   context.CancelFunc
	done     chan struct{}
	err      error // written before done is closed
	early    bool  // Run had returned before it was cancelled
	reported bool
}

// Run runs components, given in start order, and must be called once. Each component starts after the previous
// one's Start has returned. Run blocks until ctx is cancelled or a component fails, then stops the started
// components in reverse order, cancelling one at a time and waiting for its Run to return. If that takes longer
// than the shutdown timeout, Run cancels the work context and all remaining components, waits up to AbortGrace,
// and returns ErrShutdownTimeout joined with any component failures. It returns nil after a shutdown requested
// through ctx in which every component stopped cleanly.
func (s *Supervisor) Run(ctx context.Context, components ...Component) error {
	defer s.abort()
	s.mu.Lock()
	s.components, s.phases = components, make([]phase, len(components))
	s.mu.Unlock()

	exited := make(chan *handle, len(components))
	started, failure := s.startAll(ctx, components, exited)
	if failure == nil {
		select {
		case <-ctx.Done():
		case h := <-exited:
			failure = s.failed(h)
		}
	}
	return errors.Join(failure, s.stopAll(started))
}

func (s *Supervisor) startAll(ctx context.Context, components []Component, exited chan *handle) ([]*handle, error) {
	var started []*handle
	for i, c := range components {
		if ctx.Err() != nil {
			return started, nil
		}
		st, ok := c.(Starter)
		if !ok {
			started = append(started, s.launch(ctx, i, c, exited))
			continue
		}
		s.setPhase(i, starting)
		failedRunning, err := s.start(ctx, c.Name(), st, exited)
		if err == nil {
			// A successful Start is always followed by Run, so Run can release what Start acquired.
			started = append(started, s.launch(ctx, i, c, exited))
		} else {
			s.setPhase(i, stopped)
		}
		switch {
		case failedRunning != nil:
			return started, s.failed(failedRunning)
		case err != nil && ctx.Err() != nil:
			return started, nil
		case err != nil:
			s.log.Error("component failed to start", "component", c.Name(), "err", err)
			return started, &ComponentError{Name: c.Name(), Err: err}
		}
	}
	return started, nil
}

// start runs st.Start. If a running component exits meanwhile, it cancels Start, waits for it, and also returns
// that component.
func (s *Supervisor) start(ctx context.Context, name string, st Starter, exited <-chan *handle) (*handle, error) {
	startCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- s.guard(name, func() error { return st.Start(startCtx) }) }()
	select {
	case err := <-errc:
		return nil, err
	case h := <-exited:
		cancel()
		return h, <-errc
	}
}

func (s *Supervisor) launch(ctx context.Context, i int, c Component, exited chan<- *handle) *handle {
	// Run contexts are detached from ctx so that stopAll can cancel them one at a time.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h := &handle{name: c.Name(), cancel: cancel, done: make(chan struct{})}
	s.setPhase(i, running)
	s.log.Info("component started", "component", h.name)
	go func() {
		h.err = s.guard(h.name, func() error { return c.Run(runCtx) })
		s.setPhase(i, stopped)
		exited <- h
		close(h.done)
	}()
	return h
}

func (s *Supervisor) stopAll(started []*handle) error {
	deadline := s.clock.NewTimer(s.timeout)
	defer deadline.Stop()
	var errs []error
	for i := len(started) - 1; i >= 0; i-- {
		h := started[i]
		h.early = isClosed(h.done)
		h.cancel()
		select {
		case <-h.done:
			errs = append(errs, s.result(h))
		case <-deadline.C():
			return errors.Join(append(errs, s.abortAll(started[:i+1])...)...)
		}
	}
	return errors.Join(errs...)
}

// abortAll handles the shutdown timeout. remaining is in start order; its last component was being stopped.
// It cancels in-flight work and the other components, waits up to AbortGrace for all of them to return, and
// returns the timeout followed by the failures of those that returned.
func (s *Supervisor) abortAll(remaining []*handle) []error {
	stopping := remaining[len(remaining)-1]
	s.log.Error("shutdown timeout, cancelling in-flight work", "component", stopping.name, "timeout", s.timeout)
	s.abort()
	for _, h := range remaining[:len(remaining)-1] {
		h.early = isClosed(h.done)
		h.cancel()
	}
	grace := s.clock.NewTimer(AbortGrace)
	defer grace.Stop()
	expired := false
	var stuck []string
	var failures []error
	for i := len(remaining) - 1; i >= 0; i-- {
		h := remaining[i]
		if !expired {
			select {
			case <-h.done:
			case <-grace.C():
				expired = true
			}
		}
		if isClosed(h.done) {
			failures = append(failures, s.result(h))
		} else {
			stuck = append(stuck, h.name)
		}
	}
	err := fmt.Errorf("%w after %s while stopping %s", ErrShutdownTimeout, s.timeout, stopping.name)
	if len(stuck) > 0 {
		s.log.Error("components still running after cancellation", "components", stuck)
		err = fmt.Errorf("%w; still running: %s", err, strings.Join(stuck, ", "))
	}
	return append([]error{err}, failures...)
}

// result classifies how a component's Run ended once the supervisor stopped it, logging and returning failures.
func (s *Supervisor) result(h *handle) error {
	switch {
	case h.reported:
		// Already returned as the failure that started the shutdown.
		return nil
	case h.early:
		return s.failed(h)
	case h.err != nil && !errors.Is(h.err, context.Canceled):
		s.log.Error("component failed to stop", "component", h.name, "err", h.err)
		return &ComponentError{Name: h.name, Err: h.err}
	}
	s.log.Info("component stopped", "component", h.name)
	return nil
}

// failed reports a component whose Run returned before it was cancelled.
func (s *Supervisor) failed(h *handle) error {
	h.reported = true
	err := h.err
	if err == nil {
		err = errEarlyExit
	}
	s.log.Error("component failed", "component", h.name, "err", err)
	return &ComponentError{Name: h.name, Err: err}
}

func (s *Supervisor) guard(name string, f func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("component panicked", "component", name, "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return f()
}

// NotReady returns "name: reason" for every component that is not ready, in start order.
func (s *Supervisor) NotReady() []string {
	s.mu.Lock()
	components, phases := s.components, append([]phase(nil), s.phases...)
	s.mu.Unlock()
	var out []string
	for i, c := range components {
		switch phases[i] {
		case pending:
			out = append(out, c.Name()+": not started")
		case starting:
			out = append(out, c.Name()+": starting")
		case stopped:
			out = append(out, c.Name()+": stopped")
		default:
			if r, ok := c.(Readier); ok {
				if err := r.Ready(); err != nil {
					out = append(out, c.Name()+": "+err.Error())
				}
			}
		}
	}
	return out
}

func (s *Supervisor) setPhase(i int, p phase) {
	s.mu.Lock()
	s.phases[i] = p
	s.mu.Unlock()
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
