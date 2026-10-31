package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/logging"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/routing"
)

const (
	// maxAccepted is how many runs may be accepted or reserved and not yet started.
	maxAccepted = 256
	// slowConversation is the wait for a conversation's earlier runs above which a run is logged at info.
	slowConversation = 30 * time.Second
	// releaseAttempts is how often a run's in-flight record is released before the run gives up on it.
	releaseAttempts = 2
)

var (
	errStopped    = errors.New("pipeline stopped accepting runs")
	errRunTimeout = errors.New("run deadline exceeded")
)

type inflight interface {
	// Started counts one more start of the run of generation's in-flight record.
	Started(ctx context.Context, generation string) error
	// Release deletes the in-flight record of generation and reports whether it was there.
	Release(ctx context.Context, generation string) (bool, error)
}

// Scheduler runs the jobs the flusher hands over: the runs of one conversation key one at a time in acceptance
// order, runs of different keys concurrently on a fixed number of workers.
type Scheduler struct {
	intake  context.Context
	work    context.Context
	answer  func(ctx context.Context, j routing.Job, beforeWrite func(context.Context) error) error
	records inflight
	workers int
	timeout time.Duration
	clk     clock.Clock
	log     *slog.Logger

	slots chan struct{} // one per run accepted or reserved and not started
	ready chan accepted // runs whose conversation is free, in the order they became so

	mu    sync.Mutex
	convs map[string]*conversation
}

// accepted is a run that has not started.
type accepted struct {
	job        routing.Job
	acceptedAt time.Time
	readyAt    time.Time
}

// conversation is the runs of one key that are accepted or running.
type conversation struct {
	queued  []accepted // behind the run that is ready or running
	waiting int        // accepted and not started
	active  bool       // a run is ready or running
}

// NewScheduler returns a scheduler that answers each job with answer on one of workers goroutines, each run on a
// context derived from work and cancelled after timeout. answer calls beforeWrite, and returns its error, before
// each write outside the engine, and returns nil when the job is answered or needs no answer. Once intake ends,
// Reserve fails and no further run starts; runs accepted and not started keep their in-flight records in records.
func NewScheduler(intake, work context.Context, answer func(ctx context.Context, j routing.Job, beforeWrite func(context.Context) error) error,
	records inflight, workers int, timeout time.Duration, clk clock.Clock, log *slog.Logger) *Scheduler {
	return &Scheduler{
		intake: intake, work: work, answer: answer, records: records, workers: workers, timeout: timeout, clk: clk,
		log: log, slots: make(chan struct{}, maxAccepted), ready: make(chan accepted, maxAccepted),
		convs: map[string]*conversation{},
	}
}

// Name identifies the component.
func (s *Scheduler) Name() string { return "pipeline" }

// Waiting returns the number of runs of key that were accepted and have not started.
func (s *Scheduler) Waiting(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.convs[key]; c != nil {
		return c.waiting
	}
	return 0
}

// Reserve waits until fewer than 256 runs are accepted or reserved and not started, and reserves a place for one.
// It returns ctx.Err() if ctx ends first, and an error once intake has ended.
func (s *Scheduler) Reserve(ctx context.Context) (submit func(routing.Job), cancel func(), err error) {
	if s.intake.Err() != nil {
		return nil, nil, errStopped
	}
	select {
	case s.slots <- struct{}{}:
		return s.accept, func() { <-s.slots }, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-s.intake.Done():
		return nil, nil, errStopped
	}
}

// accept queues j behind the runs of its conversation, or makes it ready when there are none.
func (s *Scheduler) accept(j routing.Job) {
	now := s.clk.Now()
	a := accepted{job: j, acceptedAt: now}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.convs[j.Key]
	if c == nil {
		c = &conversation{}
		s.convs[j.Key] = c
	}
	c.waiting++
	if c.active {
		c.queued = append(c.queued, a)
		return
	}
	c.active = true
	a.readyAt = now
	s.ready <- a // never blocks: every run in it holds a slot
}

// Run starts ready runs on the workers until ctx or intake ends, and returns once ctx has ended and the running
// runs have returned.
func (s *Scheduler) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for range s.workers {
		wg.Go(func() { s.serve(ctx) })
	}
	wg.Wait()
	<-ctx.Done()
	return nil
}

func (s *Scheduler) serve(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-s.ready:
			if ctx.Err() != nil || s.intake.Err() != nil {
				return
			}
			s.execute(a)
		}
	}
}

// execute runs a on the calling worker, settles its in-flight record, and makes the conversation's next run ready.
func (s *Scheduler) execute(a accepted) {
	started := s.clk.Now()
	s.mu.Lock()
	s.convs[a.job.Key].waiting--
	s.mu.Unlock()
	<-s.slots

	log := s.log.With(logging.KeyConversation, a.job.Key, "post_ids", a.job.MessageIDs)
	rec := &record{records: s.records, generation: a.job.Generation, log: log}
	ctx, cancel := context.WithCancelCause(s.work)
	done := make(chan error, 1)
	go func() {
		rec.start(ctx)
		done <- s.call(ctx, a.job, rec.beforeWrite)
	}()
	deadline := s.clk.NewTimer(s.timeout)
	var err error
	timedOut := false
	select {
	case err = <-done:
	case <-deadline.C():
		timedOut = true
		log.Error("run timed out; cancelling it", "timeout", s.timeout)
		cancel(errRunTimeout)
		err = <-done
	}
	deadline.Stop()
	cancel(nil)

	interrupted := err != nil && !timedOut && s.work.Err() != nil
	wrote := rec.written()
	switch {
	case err == nil, timedOut:
	case interrupted && !wrote:
		log.Info("run interrupted before writing; it runs again at the next start", "error", err)
	case interrupted:
		log.Warn("run interrupted after writing", "error", err)
	default:
		log.Error("run failed", "error", err)
	}
	if !interrupted || wrote {
		// The work context may be over; a run that wrote must still not be re-run.
		rec.settle(context.WithoutCancel(s.work))
	}

	waited := a.readyAt.Sub(a.acceptedAt)
	level := slog.LevelDebug
	if waited > slowConversation {
		level = slog.LevelInfo
	}
	log.Log(s.work, level, "run finished", "queue_wait", started.Sub(a.readyAt), "conversation_wait", waited,
		"duration", s.clk.Now().Sub(started))
	s.next(a.job.Key)
}

// call answers j, turning a panic into an error that carries the stack.
func (s *Scheduler) call(ctx context.Context, j routing.Job, beforeWrite func(context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	return s.answer(ctx, j, beforeWrite)
}

// next makes the next accepted run of key ready, or marks the conversation idle.
func (s *Scheduler) next(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.convs[key]
	if len(c.queued) == 0 {
		delete(s.convs, key)
		return
	}
	a := c.queued[0]
	c.queued = c.queued[1:]
	a.readyAt = s.clk.Now()
	s.ready <- a
}

// record is a run's in-flight record. The run's start is counted in it before the run, and it is released just before
// the run's first write, or when the run returns without one.
type record struct {
	records    inflight
	generation string
	log        *slog.Logger

	mu       sync.Mutex
	attempts int
	settled  bool // released, or found gone
	gone     bool // another process released it
	wrote    bool // a write was attempted
}

// start counts the run's start, which bounds how often a run that crashes the process is resubmitted. A failure is
// logged and the run goes ahead.
func (r *record) start(ctx context.Context) {
	if err := r.records.Started(ctx, r.generation); err != nil {
		r.log.Warn("run start not counted in its in-flight record", "error", err)
	}
}

// beforeWrite releases the record before the run's first write. A failed release is logged and the write goes
// ahead; a record that is already gone stops every write.
func (r *record) beforeWrite(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wrote = true
	if r.attempts == 0 {
		r.release(ctx)
	}
	if r.gone {
		return chat.ErrTakenOver
	}
	return nil
}

// written reports whether the run attempted a write.
func (r *record) written() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.wrote
}

// settle releases the record if it is still held, retrying a release that failed once.
func (r *record) settle(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for !r.settled && r.attempts < releaseAttempts {
		r.release(ctx)
	}
}

func (r *record) release(ctx context.Context) {
	r.attempts++
	ok, err := r.records.Release(ctx, r.generation)
	switch {
	case err != nil:
		r.log.Error("in-flight record not released", "attempt", r.attempts, "error", err)
	case !ok:
		r.settled, r.gone = true, true
		r.log.Warn("in-flight record was already released")
	default:
		r.settled = true
	}
}
