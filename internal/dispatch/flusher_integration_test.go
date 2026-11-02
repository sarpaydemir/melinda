//go:build integration

package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/dispatch"
	"github.com/sarpaydemir/melinda/internal/pipeline"
	"github.com/sarpaydemir/melinda/internal/routing"
	"github.com/sarpaydemir/melinda/internal/store/journal"
	"github.com/sarpaydemir/melinda/internal/store/kv"
	"github.com/sarpaydemir/melinda/internal/store/pg"
	"github.com/sarpaydemir/melinda/internal/testkit"
	"github.com/sarpaydemir/melinda/internal/testkit/fakeclock"
	"github.com/sarpaydemir/melinda/internal/testkit/testdb"
)

// scheduler is a pipeline whose capacity wait and job handling a test supplies.
type scheduler struct {
	reserve func(ctx context.Context) error
	accept  func(routing.Job)
}

func (scheduler) Waiting(string) int { return 0 }

func (s scheduler) Reserve(ctx context.Context) (func(routing.Job), func(), error) {
	if s.reserve != nil {
		if err := s.reserve(ctx); err != nil {
			return nil, nil, err
		}
	}
	return s.accept, func() {}, nil
}

// hookedJournal replaces Take when take is set.
type hookedJournal struct {
	*journal.Store
	take func(ctx context.Context, key, generation string) (json.RawMessage, bool, error)
}

func (h hookedJournal) Take(ctx context.Context, key, generation string) (json.RawMessage, bool, error) {
	if h.take != nil {
		return h.take(ctx, key, generation)
	}
	return h.Store.Take(ctx, key, generation)
}

// agent is a database holding one due buffer of p1 and p2, and the jobs answered from it by any process.
type agent struct {
	t     *testing.T
	db    *pg.DB
	conn  *pgx.Conn
	clk   *fakeclock.Clock
	store *kv.Store
	runs  *journal.Store

	mu       sync.Mutex
	answered []routing.Job
	changed  testkit.Notifier
}

func newAgent(t *testing.T) *agent {
	t.Helper()
	dsn := testdb.New(t)
	db, err := pg.Open(context.Background(), dsn, clock.Real(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	clk := fakeclock.New(start)
	a := &agent{t: t, db: db, conn: conn, clk: clk, store: kv.New(db, clk), runs: journal.New(db, agentID, clk)}
	if err := a.store.Set(t.Context(), prefix+"dm:u1", json.RawMessage(stored("g1", "2026-10-31T09:59:00+00:00", "p1", "p2"))); err != nil {
		t.Fatal(err)
	}
	return a
}

// answer does what a run does before its first outbound write: it releases j's record and answers only when the
// record was still there.
func (a *agent) answer(j routing.Job) {
	released, err := a.runs.Release(context.Background(), j.Generation)
	if err != nil {
		a.t.Error(err)
	}
	if released {
		a.reply(j)
	}
}

// reply records that j was answered.
func (a *agent) reply(j routing.Job) {
	a.mu.Lock()
	a.answered = append(a.answered, j)
	a.mu.Unlock()
	a.changed.Notify()
}

func (a *agent) answers() []routing.Job {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.answered)
}

// left reports what the store holds: "buffer", "record", "none", or "unexpected".
func (a *agent) left() string {
	var buffers, records int
	if err := a.conn.QueryRow(a.t.Context(), `SELECT (SELECT count(*) FROM key_value_store), (SELECT count(*) FROM melinda_inflight_runs)`).
		Scan(&buffers, &records); err != nil {
		a.t.Fatal(err)
	}
	switch {
	case buffers == 1 && records == 0:
		return "buffer"
	case buffers == 0 && records == 1:
		return "record"
	case buffers == 0 && records == 0:
		return "none"
	}
	return "unexpected"
}

// flusher returns a flusher for a new process of the agent, with its lock. The lock retries on the real clock: the
// server frees it only once the previous holder's backend has exited.
func (a *agent) flusher(runs hookedJournal, pipe dispatch.Scheduler) (*dispatch.Flusher, *pg.Lock) {
	lock := a.db.NewLock(agentID, clock.Real(), slog.New(slog.DiscardHandler))
	return dispatch.NewFlusher(prefix, a.store, runs, lock, pipe, a.clk, slog.New(slog.DiscardHandler)), lock
}

// restart runs a new process until the agent has answered and nothing is left, then stops it.
func (a *agent) restart() {
	a.t.Helper()
	f, lock := a.flusher(hookedJournal{Store: a.runs}, scheduler{accept: a.answer})
	ctx, cancel := context.WithCancel(a.t.Context())
	defer cancel()
	if err := f.Start(ctx); err != nil {
		a.t.Fatalf("restart: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	a.changed.Await(a.t, "an answer and nothing left", func() bool { return len(a.answers()) > 0 && a.left() == "none" })
	cancel()
	if err := <-done; err != nil {
		a.t.Fatalf("Run after the restart = %v", err)
	}
	lock.Release()
}

func (a *agent) assertAnsweredOnce() {
	a.t.Helper()
	got := a.answers()
	if len(got) != 1 || !slices.Equal(got[0].MessageIDs, []string{"p1", "p2"}) || got[0].Generation != "g1" {
		a.t.Errorf("answered runs = %+v, want one run of g1 with p1 and p2", got)
	}
}

func receiveWithin(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	timeout := time.NewTimer(testkit.AwaitTimeout)
	defer timeout.Stop()
	select {
	case err := <-ch:
		return err
	case <-timeout.C:
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

func TestRestartAfterAStopAtEachStepAnswersEveryMessageOnce(t *testing.T) {
	type take = func(context.Context, string, string) (json.RawMessage, bool, error)
	steps := []struct {
		name  string
		pipe  func(a *agent, stop func()) scheduler
		take  func(a *agent, stop func()) take
		state string // what the stop leaves behind
	}{
		{name: "while waiting for capacity", state: "buffer",
			pipe: func(_ *agent, stop func()) scheduler {
				return scheduler{reserve: func(ctx context.Context) error { stop(); <-ctx.Done(); return ctx.Err() }}
			}},
		{name: "before the take", state: "buffer",
			take: func(a *agent, stop func()) take {
				return func(ctx context.Context, key, generation string) (json.RawMessage, bool, error) {
					stop()
					return a.runs.Take(ctx, key, generation)
				}
			}},
		{name: "after the take committed, before its result arrived", state: "record",
			take: func(a *agent, stop func()) take {
				return func(ctx context.Context, key, generation string) (json.RawMessage, bool, error) {
					if _, _, err := a.runs.Take(ctx, key, generation); err != nil {
						return nil, false, err
					}
					stop()
					return nil, false, errors.New("read tcp 10.0.0.5:5432: connection reset by peer")
				}
			}},
		{name: "after the take, before the run was submitted", state: "record",
			pipe: func(*agent, func()) scheduler { return scheduler{accept: func(routing.Job) {}} },
			take: func(a *agent, stop func()) take {
				return func(ctx context.Context, key, generation string) (json.RawMessage, bool, error) {
					defer stop()
					return a.runs.Take(ctx, key, generation)
				}
			}},
		{name: "after the run was submitted, before its first outbound write", state: "record",
			pipe: func(_ *agent, stop func()) scheduler { return scheduler{accept: func(routing.Job) { stop() }} }},
		{name: "after the first outbound write", state: "none",
			pipe: func(a *agent, stop func()) scheduler {
				return scheduler{accept: func(j routing.Job) {
					a.answer(j)
					stop()
				}}
			}},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			a := newAgent(t)
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			pipe, runs := scheduler{accept: a.answer}, hookedJournal{Store: a.runs}
			if step.pipe != nil {
				pipe = step.pipe(a, stop)
			}
			if step.take != nil {
				runs.take = step.take(a, stop)
			}
			f, lock := a.flusher(runs, pipe)
			if err := f.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- f.Run(ctx) }()
			if err := receiveWithin(t, done, "the first process to stop at the step"); err != nil {
				t.Fatalf("Run = %v", err)
			}
			lock.Release() // the process exits
			if got := a.left(); got != step.state {
				t.Fatalf("stopped %s, the store holds %s, want %s", step.name, got, step.state)
			}

			a.restart()
			a.assertAnsweredOnce()
		})
	}
}

func TestRunOfAProcessThatLostTheLockAnswersNothingOnceAnotherTookOver(t *testing.T) {
	a := newAgent(t)
	inFlight := make(chan routing.Job, 1)
	f, lock := a.flusher(hookedJournal{Store: a.runs}, scheduler{accept: func(j routing.Job) { inFlight <- j }})
	defer lock.Release()
	if err := f.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- f.Run(t.Context()) }()
	timeout := time.NewTimer(testkit.AwaitTimeout)
	defer timeout.Stop()
	var j routing.Job
	select {
	case j = <-inFlight:
	case <-timeout.C:
		t.Fatal("no run submitted")
	}

	var terminated bool
	if err := a.conn.QueryRow(t.Context(), `SELECT bool_and(pg_terminate_backend(pid)) FROM pg_locks
		WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).
		Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate the lock's session: %v, %v", terminated, err)
	}
	if err := receiveWithin(t, done, "Run after the lock was lost"); err == nil {
		t.Error("Run after the lock was lost = nil, want an error")
	}

	a.restart()
	a.answer(j) // the first process's run reaches its first outbound write
	a.assertAnsweredOnce()
}

func TestTakeWhoseResultIsLostIsAnsweredOnceWithoutARestart(t *testing.T) {
	a := newAgent(t)
	taken := make(chan error, 1)
	lost := hookedJournal{Store: a.runs, take: func(ctx context.Context, key, generation string) (json.RawMessage, bool, error) {
		_, _, err := a.runs.Take(ctx, key, generation)
		taken <- err
		return nil, false, errors.New("read tcp 10.0.0.5:5432: connection reset by peer")
	}}
	f, lock := a.flusher(lost, scheduler{accept: a.answer})
	defer lock.Release()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := f.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	if err := receiveWithin(t, taken, "the take"); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got := a.left(); got != "record" {
		t.Fatalf("after the lost take the store holds %s, want record", got)
	}

	a.clk.AwaitTimers(t, 2) // the take's retry and the journal sweep
	a.clk.Advance(time.Minute)
	a.changed.Await(t, "an answer and nothing left", func() bool { return len(a.answers()) > 0 && a.left() == "none" })
	cancel()
	if err := receiveWithin(t, done, "Run"); err != nil {
		t.Fatalf("Run = %v", err)
	}
	a.assertAnsweredOnce()
}

func TestCrashLoopOnOneRunLeavesTheRestartsOfTheRunQueuedBehindIt(t *testing.T) {
	a := newAgent(t)
	ctx, key := t.Context(), prefix+"dm:u1"
	if _, ok, err := a.runs.Take(ctx, key, "g1"); !ok || err != nil {
		t.Fatalf("take g1: %v, %v", ok, err)
	}
	if err := a.store.Set(ctx, key, json.RawMessage(stored("g2", "2026-10-31T09:59:30+00:00", "p3"))); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := a.runs.Take(ctx, key, "g2"); !ok || err != nil {
		t.Fatalf("take g2: %v, %v", ok, err)
	}
	crashed := make(chan error, 1)
	answer := func(ctx context.Context, j routing.Job, beforeWrite func(context.Context) error) error {
		if j.Generation == "g1" { // its input crashes the process
			crashed <- nil
			<-ctx.Done()
			return ctx.Err()
		}
		if err := beforeWrite(ctx); err != nil {
			return err
		}
		a.reply(j)
		return nil
	}

	for crash := 1; crash <= 3; crash++ {
		a.process(answer, func() { _ = receiveWithin(t, crashed, "the run of g1") })
		records, err := a.runs.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		starts := map[string]int{}
		for _, r := range records {
			starts[r.Generation] = r.Starts
		}
		if want := map[string]int{"g1": crash, "g2": 0}; !maps.Equal(starts, want) {
			t.Fatalf("after crash %d the runs have started %v times, want %v", crash, starts, want)
		}
	}
	a.process(answer, func() {
		a.changed.Await(t, "an answer and nothing left", func() bool { return len(a.answers()) > 0 && a.left() == "none" })
	})
	if got := a.answers(); len(got) != 1 || got[0].Generation != "g2" || !slices.Equal(got[0].MessageIDs, []string{"p3"}) {
		t.Errorf("answered runs = %+v, want one run of g2 with p3", got)
	}
}

// process runs a process of the agent whose pipeline answers with answer until until returns, then stops it the way a
// crash does: no run starts or writes after that, and every run keeps its record.
func (a *agent) process(answer func(context.Context, routing.Job, func(context.Context) error) error, until func()) {
	a.t.Helper()
	intake, stopIntake := context.WithCancel(context.Background())
	work, abortWork := context.WithCancel(context.Background())
	running, stopRunning := context.WithCancel(context.Background())
	s := pipeline.NewScheduler(intake, work, answer, a.runs, 2, time.Hour, a.clk, slog.New(slog.DiscardHandler))
	ran := make(chan error, 1)
	go func() { ran <- s.Run(running) }()
	f, lock := a.flusher(hookedJournal{Store: a.runs}, s)
	defer lock.Release()
	if err := f.Start(a.t.Context()); err != nil {
		a.t.Fatalf("Start: %v", err)
	}
	until()
	stopIntake()
	abortWork()
	stopRunning()
	if err := receiveWithin(a.t, ran, "the pipeline to stop"); err != nil {
		a.t.Fatalf("pipeline Run = %v", err)
	}
}
