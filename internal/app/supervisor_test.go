package app_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sarpaydemir/dost/internal/app"
	"github.com/sarpaydemir/dost/internal/clock"
)

type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// fake records "start <name>" and "stop <name>". Without a run func, Run waits for cancellation.
type fake struct {
	name  string
	rec   *recorder
	start func(ctx context.Context) error
	run   func(ctx context.Context) error
	ready func() error
}

func (f *fake) Name() string { return f.name }

func (f *fake) Run(ctx context.Context) error {
	if f.run != nil {
		return f.run(ctx)
	}
	<-ctx.Done()
	f.rec.add("stop " + f.name)
	return ctx.Err()
}

type starter struct{ *fake }

func (s starter) Start(ctx context.Context) error {
	s.rec.add("start " + s.name)
	if s.start != nil {
		return s.start(ctx)
	}
	return nil
}

type readier struct{ *fake }

func (r readier) Ready() error { return r.ready() }

func newSupervisor(timeout time.Duration) *app.Supervisor {
	return app.NewSupervisor(slog.New(slog.DiscardHandler), clock.Real(), timeout)
}

// run starts s in the background and returns a function that waits for its result.
func run(ctx context.Context, s *app.Supervisor, cs ...app.Component) func() error {
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx, cs...) }()
	return func() error { return <-errc }
}

func assertEvents(t *testing.T, rec *recorder, want ...string) {
	t.Helper()
	if got := rec.list(); !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
}

func assertNotReady(t *testing.T, s *app.Supervisor, want ...string) {
	t.Helper()
	if got := s.NotReady(); !slices.Equal(got, want) {
		t.Errorf("NotReady = %q, want %q", got, want)
	}
}

func assertComponentError(t *testing.T, err error, name, msg string) {
	t.Helper()
	var ce *app.ComponentError
	if !errors.As(err, &ce) || ce.Name != name {
		t.Fatalf("Run = %v, want a ComponentError for %s", err, name)
	}
	if msg != "" && err.Error() != msg {
		t.Errorf("Run = %q, want %q", err, msg)
	}
}

func TestSupervisorStartsInOrderAndStopsOneAtATimeInReverse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{}
		release := make(chan struct{})
		c := &fake{name: "c", rec: rec, run: func(ctx context.Context) error {
			<-ctx.Done()
			rec.add("stop c")
			<-release
			return nil
		}}
		s := newSupervisor(time.Minute)
		ctx, cancel := context.WithCancel(t.Context())
		wait := run(ctx, s, starter{&fake{name: "a", rec: rec}}, starter{&fake{name: "b", rec: rec}}, starter{c})
		synctest.Wait()
		assertEvents(t, rec, "start a", "start b", "start c")

		cancel()
		synctest.Wait()
		assertEvents(t, rec, "start a", "start b", "start c", "stop c")
		if s.WorkContext().Err() != nil {
			t.Error("work context cancelled before the shutdown timeout")
		}

		close(release)
		if err := wait(); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
		assertEvents(t, rec, "start a", "start b", "start c", "stop c", "stop b", "stop a")
	})
}

func TestSupervisorStopsAllOnComponentFailure(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name string
		run  func() error
		want string
	}{
		{"error", func() error { return errBoom }, "b: boom"},
		{"early return", func() error { return nil }, "b: exited before shutdown"},
		{"cancellation error before shutdown", func() error { return context.Canceled }, "b: context canceled"},
		{"panic", func() error { panic("bad state") }, "b: panic: bad state"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rec := &recorder{}
				fail := make(chan struct{})
				b := &fake{name: "b", rec: rec, run: func(context.Context) error {
					<-fail
					return tt.run()
				}}
				wait := run(t.Context(), newSupervisor(time.Minute),
					starter{&fake{name: "a", rec: rec}}, starter{b}, starter{&fake{name: "c", rec: rec}})
				synctest.Wait()
				close(fail)

				assertComponentError(t, wait(), "b", tt.want)
				assertEvents(t, rec, "start a", "start b", "start c", "stop c", "stop a")
			})
		})
	}
}

func TestSupervisorStartFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{}
		errSweep := errors.New("sweep failed")
		b := &fake{name: "b", rec: rec, start: func(context.Context) error { return errSweep },
			run: func(context.Context) error { t.Error("Run called after Start failed"); return nil }}
		c := &fake{name: "c", rec: rec, run: func(context.Context) error { t.Error("later component ran"); return nil }}

		err := newSupervisor(time.Minute).Run(t.Context(), starter{&fake{name: "a", rec: rec}}, starter{b}, c)
		assertComponentError(t, err, "b", "b: sweep failed")
		assertEvents(t, rec, "start a", "start b", "stop a")
	})
}

type cancelledStart struct {
	name   string
	start  func(rec *recorder) func(ctx context.Context) error
	events []string
}

// cancelledStarts returns Start funcs that block until cancelled and then give up or succeed. A successful Start
// must be followed by Run, which releases what Start acquired.
func cancelledStarts() []cancelledStart {
	return []cancelledStart{
		{"start gives up", func(rec *recorder) func(ctx context.Context) error {
			return func(ctx context.Context) error {
				<-ctx.Done()
				rec.add("gave up b")
				return ctx.Err()
			}
		}, []string{"gave up b"}},
		{"start succeeds", func(rec *recorder) func(ctx context.Context) error {
			return func(ctx context.Context) error {
				<-ctx.Done()
				rec.add("acquired b")
				return nil
			}
		}, []string{"acquired b", "stop b"}},
	}
}

func TestSupervisorFailureWhileStarting(t *testing.T) {
	for _, tt := range cancelledStarts() {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rec := &recorder{}
				fail := make(chan struct{})
				a := &fake{name: "a", rec: rec, run: func(context.Context) error {
					<-fail
					return errors.New("connection lost")
				}}
				c := &fake{name: "c", rec: rec, run: func(context.Context) error { t.Error("later component ran"); return nil }}
				wait := run(t.Context(), newSupervisor(time.Minute), a, starter{&fake{name: "b", rec: rec, start: tt.start(rec)}}, c)
				synctest.Wait()
				close(fail)

				assertComponentError(t, wait(), "a", "a: connection lost")
				assertEvents(t, rec, append([]string{"start b"}, tt.events...)...)
			})
		})
	}
}

func TestSupervisorCancelledWhileStarting(t *testing.T) {
	for _, tt := range cancelledStarts() {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rec := &recorder{}
				c := &fake{name: "c", rec: rec, run: func(context.Context) error { t.Error("later component ran"); return nil }}
				s := newSupervisor(time.Minute)
				ctx, cancel := context.WithCancel(t.Context())
				wait := run(ctx, s, starter{&fake{name: "a", rec: rec}}, starter{&fake{name: "b", rec: rec, start: tt.start(rec)}}, c)
				synctest.Wait()
				assertNotReady(t, s, "b: starting", "c: not started")
				cancel()

				if err := wait(); err != nil {
					t.Fatalf("Run = %v, want nil", err)
				}
				assertEvents(t, rec, append(append([]string{"start a", "start b"}, tt.events...), "stop a")...)
			})
		})
	}
}

func TestSupervisorFailureWhileStopping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{}
		b := &fake{name: "b", rec: rec, run: func(ctx context.Context) error {
			<-ctx.Done()
			rec.add("stop b")
			return errors.New("flush failed")
		}}
		ctx, cancel := context.WithCancel(t.Context())
		wait := run(ctx, newSupervisor(time.Minute), &fake{name: "a", rec: rec}, b, &fake{name: "c", rec: rec})
		synctest.Wait()
		cancel()

		assertComponentError(t, wait(), "b", "b: flush failed")
		assertEvents(t, rec, "stop c", "stop b", "stop a")
	})
}

func TestSupervisorEarlyExitWhileAnotherStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{}
		exit, release := make(chan struct{}), make(chan struct{})
		a := &fake{name: "a", rec: rec, run: func(context.Context) error {
			<-exit
			return nil
		}}
		b := &fake{name: "b", rec: rec, run: func(ctx context.Context) error {
			<-ctx.Done()
			<-release
			rec.add("stop b")
			return nil
		}}
		ctx, cancel := context.WithCancel(t.Context())
		wait := run(ctx, newSupervisor(time.Minute), a, b)
		synctest.Wait()
		cancel()
		synctest.Wait()
		close(exit)
		synctest.Wait()
		close(release)

		assertComponentError(t, wait(), "a", "a: exited before shutdown")
		assertEvents(t, rec, "stop b")
	})
}

func TestSupervisorShutdownTimeoutKeepsFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{}
		s := newSupervisor(10 * time.Second)
		exit := make(chan struct{})
		a := &fake{name: "a", rec: rec, run: func(context.Context) error {
			<-exit
			return errors.New("db gone")
		}}
		b := &fake{name: "b", rec: rec, run: func(ctx context.Context) error {
			<-ctx.Done()
			<-s.WorkContext().Done()
			return errors.New("persist failed")
		}}
		ctx, cancel := context.WithCancel(t.Context())
		wait := run(ctx, s, a, b)
		synctest.Wait()
		cancel()
		synctest.Wait()
		close(exit)

		err := wait()
		if !errors.Is(err, app.ErrShutdownTimeout) {
			t.Fatalf("Run = %v, want a shutdown timeout", err)
		}
		var ce *app.ComponentError
		if !errors.As(err, &ce) {
			t.Fatalf("Run = %v, want component failures as well", err)
		}
		if want := "shutdown timeout after 10s while stopping b\nb: persist failed\na: db gone"; err.Error() != want {
			t.Errorf("Run = %q, want %q", err, want)
		}
	})
}

func TestSupervisorShutdownTimeoutReportsAnEarlyExitOfAComponentNotYetStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{}
		s := newSupervisor(10 * time.Second)
		exit := make(chan struct{})
		a := &fake{name: "a", rec: rec, run: func(context.Context) error {
			<-exit
			return nil
		}}
		b := &fake{name: "b", rec: rec, run: func(ctx context.Context) error {
			<-ctx.Done()
			<-s.WorkContext().Done()
			return nil
		}}
		ctx, cancel := context.WithCancel(t.Context())
		wait := run(ctx, s, a, b)
		synctest.Wait()
		cancel()
		synctest.Wait()
		close(exit)

		err := wait()
		if !errors.Is(err, app.ErrShutdownTimeout) {
			t.Fatalf("Run = %v, want a shutdown timeout", err)
		}
		assertComponentError(t, err, "a", "shutdown timeout after 10s while stopping b\na: exited before shutdown")
	})
}

func TestSupervisorShutdownTimeoutCancelsWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{}
		s := newSupervisor(10 * time.Second)
		draining := &fake{name: "b", rec: rec, run: func(ctx context.Context) error {
			<-ctx.Done()
			<-s.WorkContext().Done()
			rec.add("aborted b")
			return nil
		}}
		ctx, cancel := context.WithCancel(t.Context())
		wait := run(ctx, s, &fake{name: "a", rec: rec}, draining, &fake{name: "c", rec: rec})
		synctest.Wait()
		cancel()
		begin := time.Now()

		err := wait()
		if !errors.Is(err, app.ErrShutdownTimeout) || err.Error() != "shutdown timeout after 10s while stopping b" {
			t.Fatalf("Run = %v, want a shutdown timeout while stopping b", err)
		}
		if got := time.Since(begin); got != 10*time.Second {
			t.Errorf("shutdown took %v, want 10s", got)
		}
		synctest.Wait()
		// At the deadline a and b are released together, in no particular order.
		got := rec.list()
		slices.Sort(got[1:])
		if want := []string{"stop c", "aborted b", "stop a"}; !slices.Equal(got, want) {
			t.Errorf("events = %q, want %q", got, want)
		}
	})
}

func TestSupervisorGivesUpAfterAbortGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{}
		release := make(chan struct{})
		stuck := &fake{name: "b", rec: rec, run: func(context.Context) error {
			<-release
			return nil
		}}
		ctx, cancel := context.WithCancel(t.Context())
		wait := run(ctx, newSupervisor(10*time.Second), &fake{name: "a", rec: rec}, stuck)
		synctest.Wait()
		cancel()
		begin := time.Now()

		err := wait()
		if want := "shutdown timeout after 10s while stopping b; still running: b"; !errors.Is(err, app.ErrShutdownTimeout) || err.Error() != want {
			t.Fatalf("Run = %v, want %q", err, want)
		}
		if got, want := time.Since(begin), 10*time.Second+app.AbortGrace; got != want {
			t.Errorf("shutdown took %v, want %v", got, want)
		}
		assertEvents(t, rec, "stop a")
		close(release)
	})
}

func TestSupervisorNotReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &recorder{}
		s := newSupervisor(time.Minute)
		ctx, cancel := context.WithCancel(t.Context())
		wait := run(ctx, s,
			&fake{name: "a", rec: rec},
			readier{&fake{name: "b", rec: rec, ready: func() error { return errors.New("not connected") }}})
		synctest.Wait()
		assertNotReady(t, s, "b: not connected")

		cancel()
		if err := wait(); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
		assertNotReady(t, s, "a: stopped", "b: stopped")
		if s.WorkContext().Err() == nil {
			t.Error("work context still open after Run returned")
		}
	})
}
