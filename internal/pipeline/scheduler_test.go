package pipeline_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/pipeline"
	"github.com/sarpaydemir/dost/internal/routing"
)

const runTimeout = 30 * time.Minute

// script is what a fake run does; it returns the run's result.
type script func(ctx context.Context, beforeWrite func(context.Context) error) error

// recordsFake holds in-flight records by generation and the starts counted in them. Its next failures releases fail
// as an unreachable database would, and every count fails with countErr when it is set. A slow release yields to
// other goroutines many times before it answers.
type recordsFake struct {
	trace    *trace
	failures int
	slow     bool
	countErr error

	mu     sync.Mutex
	held   map[string]bool
	starts []string // generations whose starts were counted, in order
}

func (r *recordsFake) Started(_ context.Context, generation string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.countErr != nil {
		return r.countErr
	}
	r.starts = append(r.starts, generation)
	return nil
}

func (r *recordsFake) counted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.starts)
}

func (r *recordsFake) Release(ctx context.Context, generation string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if r.slow {
		for range 1000 {
			runtime.Gosched()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trace.add("release " + generation)
	if r.failures > 0 {
		r.failures--
		return false, errors.New("read tcp 10.0.0.5:5432: connection reset by peer")
	}
	ok := r.held[generation]
	delete(r.held, generation)
	return ok, nil
}

func (r *recordsFake) holds(generation string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held[generation]
}

// trace is the order in which runs started, wrote, and released their records.
type trace struct {
	mu     sync.Mutex
	events []string
}

func (t *trace) add(e string) {
	t.mu.Lock()
	t.events = append(t.events, e)
	t.mu.Unlock()
}

func (t *trace) list() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.events)
}

// harness is a running scheduler whose runs, by default, start and then wait until the test finishes them.
type harness struct {
	t       *testing.T
	s       *pipeline.Scheduler
	records *recordsFake
	trace   *trace
	logs    *syncBuffer

	stopIntake, abortWork, stopRun context.CancelFunc
	ran                            chan struct{}

	mu      sync.Mutex
	scripts map[string]script
	gates   map[string]chan struct{}
}

func start(t *testing.T, workers int) *harness {
	tr := &trace{}
	h := &harness{
		t: t, records: &recordsFake{trace: tr, held: map[string]bool{}}, trace: tr, logs: &syncBuffer{},
		ran: make(chan struct{}), scripts: map[string]script{}, gates: map[string]chan struct{}{},
	}
	intake, stopIntake := context.WithCancel(context.Background())
	work, abortWork := context.WithCancel(context.Background())
	runCtx, stopRun := context.WithCancel(context.Background())
	h.stopIntake, h.abortWork, h.stopRun = stopIntake, abortWork, stopRun
	log := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.s = pipeline.NewScheduler(intake, work, h.answer, h.records, workers, runTimeout, clock.Real(), log)
	go func() {
		defer close(h.ran)
		if err := h.s.Run(runCtx); err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	}()
	t.Cleanup(func() {
		stopRun()
		abortWork()
		stopIntake()
		<-h.ran
	})
	return h
}

func (h *harness) answer(ctx context.Context, j routing.Job, beforeWrite func(context.Context) error) error {
	h.trace.add("start " + j.Generation)
	h.mu.Lock()
	run, ok := h.scripts[j.Generation]
	h.mu.Unlock()
	if ok {
		return run(ctx, beforeWrite)
	}
	select {
	case <-h.gate(j.Generation):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *harness) gate(generation string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gates[generation] == nil {
		h.gates[generation] = make(chan struct{})
	}
	return h.gates[generation]
}

func (h *harness) script(generation string, s script) {
	h.mu.Lock()
	h.scripts[generation] = s
	h.mu.Unlock()
}

// finish lets the run of generation return and waits until every goroutine has settled.
func (h *harness) finish(generation string) {
	close(h.gate(generation))
	synctest.Wait()
}

// submit hands the scheduler a journaled run of generation under key, for messages p<generation>.
func (h *harness) submit(key, generation string) {
	h.t.Helper()
	h.records.mu.Lock()
	h.records.held[generation] = true
	h.records.mu.Unlock()
	h.accept(routing.Job{Key: key, Generation: generation, MessageIDs: []string{"p" + generation}})
}

// accept reserves a place for j, submits it, and waits until every goroutine has settled.
func (h *harness) accept(j routing.Job) {
	h.t.Helper()
	submit, _, err := h.s.Reserve(context.Background())
	if err != nil {
		h.t.Fatalf("Reserve = %v", err)
	}
	submit(j)
	synctest.Wait()
}

// started returns the generations that started, in order.
func (h *harness) started() []string {
	var out []string
	for _, e := range h.trace.list() {
		if g, ok := strings.CutPrefix(e, "start "); ok {
			out = append(out, g)
		}
	}
	return out
}

func (h *harness) wantStarted(want ...string) {
	h.t.Helper()
	if got := h.started(); !slices.Equal(got, want) {
		h.t.Fatalf("started %q, want %q", got, want)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// line returns the log line containing msg, or "" when there is none.
func (s *syncBuffer) line(msg string) string {
	for l := range strings.Lines(s.String()) {
		if strings.Contains(l, msg) {
			return l
		}
	}
	return ""
}

func TestRunsOfAConversationStartOneAtATimeInAcceptanceOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 8)
		h.submit("dm:u1", "1")
		h.submit("dm:u1", "2")
		h.submit("dm:u1", "3")
		h.wantStarted("1")
		if got := h.s.Waiting("dm:u1"); got != 2 {
			t.Errorf("Waiting = %d, want 2", got)
		}
		h.finish("1")
		h.wantStarted("1", "2")
		h.finish("2")
		h.wantStarted("1", "2", "3")
		if got := h.s.Waiting("dm:u1"); got != 0 {
			t.Errorf("Waiting = %d, want 0", got)
		}
	})
}

func TestRunsOfDifferentConversationsRunConcurrently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 8)
		h.submit("thread:t1:u1", "1")
		h.submit("thread:t1:u2", "2")
		h.submit("dm:u1", "3")
		h.wantStarted("1", "2", "3")
	})
}

func TestARunWaitingForItsConversationHoldsNoWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 2)
		h.submit("dm:u1", "1")
		h.submit("dm:u1", "2")
		h.submit("dm:u2", "3")
		h.wantStarted("1", "3")
	})
}

func TestReadyRunsStartInTheOrderTheyBecameReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		h.submit("dm:u1", "1")
		h.submit("dm:u1", "2") // ready once 1 returns
		h.submit("dm:u2", "3") // ready now
		h.finish("1")
		h.wantStarted("1", "3")
		h.finish("3")
		h.wantStarted("1", "3", "2")
	})
}

func TestAcceptedRunsAreBoundedAndReservationsWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		h.submit("dm:busy", "busy") // started, so no longer waiting
		for i := range 255 {
			h.submit("dm:u1", fmt.Sprint(i))
		}
		_, cancelLast, err := h.s.Reserve(context.Background())
		if err != nil {
			t.Fatalf("256th Reserve = %v", err)
		}

		ctx, stop := context.WithCancel(context.Background())
		reserved := reserve(h.s, ctx)
		synctest.Wait()
		select {
		case err := <-reserved:
			t.Fatalf("Reserve with 256 runs accepted returned %v, want it to wait", err)
		default:
		}
		if got := h.s.Waiting("dm:u1"); got != 255 {
			t.Errorf("Waiting while Reserve waits = %d, want 255", got)
		}
		stop()
		if err := <-reserved; !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled Reserve = %v, want context.Canceled", err)
		}

		cancelLast()
		if err := <-reserve(h.s, context.Background()); err != nil {
			t.Fatalf("Reserve after a cancelled reservation = %v", err)
		}
		reserved = reserve(h.s, context.Background())
		synctest.Wait()
		h.finish("busy") // the first run of dm:u1 starts and frees a place
		select {
		case err := <-reserved:
			if err != nil {
				t.Errorf("Reserve after a run started = %v", err)
			}
		default:
			t.Error("Reserve still waits after a run started")
		}
	})
}

// reserve calls s.Reserve in a goroutine and sends its error.
func reserve(s *pipeline.Scheduler, ctx context.Context) <-chan error {
	errc := make(chan error, 1)
	go func() {
		_, _, err := s.Reserve(ctx)
		errc <- err
	}()
	return errc
}

func TestWaitingCountsRunsPastTheConversationLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 8)
		for i := range 6 {
			h.submit("dm:u1", fmt.Sprint(i))
		}
		if got := h.s.Waiting("dm:u1"); got != 5 {
			t.Errorf("Waiting = %d, want 5", got)
		}
		if got := h.s.Waiting("dm:u2"); got != 0 {
			t.Errorf("Waiting of an idle conversation = %d, want 0", got)
		}
	})
}

func TestStartIsCountedInTheRecordJustBeforeTheRunAndNotWhileItWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		var countedAtStart []string
		h.script("1", func(context.Context, func(context.Context) error) error {
			countedAtStart = h.records.counted()
			<-h.gate("1")
			return nil
		})
		h.submit("dm:u1", "1")
		h.submit("dm:u1", "2")
		h.submit("dm:u2", "3")
		if !slices.Equal(countedAtStart, []string{"1"}) {
			t.Errorf("starts counted when run 1 began = %q, want its own", countedAtStart)
		}
		if got := h.records.counted(); !slices.Equal(got, []string{"1"}) {
			t.Errorf("starts counted while runs 2 and 3 wait = %q, want only run 1's", got)
		}
		h.finish("1")
		h.finish("3")
		if got := h.records.counted(); !slices.Equal(got, []string{"1", "3", "2"}) {
			t.Errorf("starts counted = %q, want each run's when it started", got)
		}
	})
}

func TestRunGoesAheadWhenItsStartCannotBeCounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		h.records.countErr = errors.New("read tcp 10.0.0.5:5432: connection reset by peer")
		h.submit("dm:u1", "1")
		h.wantStarted("1")
		line := h.logs.line("start not counted")
		if !strings.Contains(line, "level=WARN") || !strings.Contains(line, "connection reset") || !strings.Contains(line, "conversation_key=dm:u1") {
			t.Errorf("count failure log = %q, want a warning with the error and the conversation key", line)
		}
	})
}

func TestRunPastItsDeadlineIsCancelledReleasedAndTheConversationMovesOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 8)
		var cause error
		h.script("1", func(ctx context.Context, _ func(context.Context) error) error {
			<-ctx.Done()
			cause = context.Cause(ctx)
			<-h.gate("1") // slow to notice the cancellation
			return ctx.Err()
		})
		h.submit("dm:u1", "1")
		h.submit("dm:u1", "2")
		time.Sleep(runTimeout - time.Nanosecond)
		synctest.Wait()
		if h.logs.line("run timed out") != "" {
			t.Fatal("run logged as timed out before its deadline")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		line := h.logs.line("run timed out")
		if !strings.Contains(line, "level=ERROR") || !strings.Contains(line, "conversation_key=dm:u1") || !strings.Contains(line, "post_ids=[p1]") {
			t.Errorf("timeout log at the deadline = %q, want an error with the conversation key and post IDs", line)
		}
		if cause == nil || errors.Is(cause, context.Canceled) {
			t.Errorf("cancellation cause = %v, want the run deadline", cause)
		}
		h.wantStarted("1") // the conversation waits until the cancelled run has returned
		h.finish("1")
		h.wantStarted("1", "2")
		if h.records.holds("1") {
			t.Error("timed-out run kept its in-flight record")
		}
	})
}

func TestPanickingRunIsLoggedWithItsStackAndTheWorkerKeepsServing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		h.script("1", func(context.Context, func(context.Context) error) error { panic("index out of range") })
		h.submit("dm:u1", "1")
		h.submit("dm:u2", "2")
		h.wantStarted("1", "2")
		line := h.logs.line("run failed")
		if !strings.Contains(line, "index out of range") || !strings.Contains(line, "scheduler_test.go") || !strings.Contains(line, "post_ids=[p1]") {
			t.Errorf("panic log = %q, want the panic, its stack, and the post IDs", line)
		}
		if h.records.holds("1") {
			t.Error("panicked run kept its in-flight record")
		}
	})
}

func TestFailedRunIsLoggedAndReleased(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		h.script("1", func(context.Context, func(context.Context) error) error {
			return errors.New("load context: read history thread:p1: connection refused")
		})
		h.submit("thread:p1:u1", "1")
		line := h.logs.line("run failed")
		if !strings.Contains(line, "level=ERROR") || !strings.Contains(line, "load context") || !strings.Contains(line, "conversation_key=thread:p1:u1") {
			t.Errorf("failure log = %q, want an error with the cause and the conversation key", line)
		}
		if h.records.holds("1") {
			t.Error("failed run kept its in-flight record")
		}
	})
}

func TestRecordIsReleasedOnceJustBeforeTheFirstWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		h.script("1", func(ctx context.Context, beforeWrite func(context.Context) error) error {
			for _, w := range []string{"progress", "reply"} {
				if err := beforeWrite(ctx); err != nil {
					return err
				}
				h.trace.add(w)
			}
			return nil
		})
		h.submit("dm:u1", "1")
		want := []string{"start 1", "release 1", "progress", "reply"}
		if got := h.trace.list(); !slices.Equal(got, want) {
			t.Errorf("events = %q, want %q", got, want)
		}
	})
}

func TestConcurrentFirstWritesWaitForOneRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		h.records.slow = true
		h.script("1", func(ctx context.Context, beforeWrite func(context.Context) error) error {
			var wg sync.WaitGroup
			for range 2 {
				wg.Go(func() {
					if err := beforeWrite(ctx); err != nil {
						h.trace.add("refused")
						return
					}
					h.trace.add("write")
				})
			}
			wg.Wait()
			return nil
		})
		h.submit("dm:u1", "1")
		want := []string{"start 1", "release 1", "write", "write"}
		if got := h.trace.list(); !slices.Equal(got, want) {
			t.Errorf("events = %q, want %q", got, want)
		}
	})
}

func TestRunWithoutWritesIsReleasedWhenItReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		h.submit("dm:u1", "1")
		if !h.records.holds("1") {
			t.Fatal("record released while the run is going")
		}
		h.finish("1")
		if h.records.holds("1") {
			t.Error("record kept after the run returned")
		}
	})
}

func TestRunWritesNothingWhenItsRecordIsAlreadyGone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		var errs []error
		h.script("1", func(ctx context.Context, beforeWrite func(context.Context) error) error {
			errs = append(errs, beforeWrite(ctx), beforeWrite(ctx))
			return errs[0]
		})
		h.accept(routing.Job{Key: "dm:u1", Generation: "1"}) // another process released it
		if len(errs) != 2 || errs[0] == nil || errs[1] == nil {
			t.Errorf("beforeWrite errors = %v, want an error for every write", errs)
		}
		want := []string{"start 1", "release 1"}
		if got := h.trace.list(); !slices.Equal(got, want) {
			t.Errorf("events = %q, want %q", got, want)
		}
	})
}

func TestFailedReleaseLetsTheWriteProceedAndIsRetriedOnceWhenTheRunReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		h.records.failures = 1
		h.script("1", func(ctx context.Context, beforeWrite func(context.Context) error) error {
			if err := beforeWrite(ctx); err != nil {
				return err
			}
			h.trace.add("reply")
			if err := beforeWrite(ctx); err != nil {
				return err
			}
			h.trace.add("upload")
			return nil
		})
		h.submit("dm:u1", "1")
		want := []string{"start 1", "release 1", "reply", "upload", "release 1"}
		if got := h.trace.list(); !slices.Equal(got, want) {
			t.Errorf("events = %q, want %q", got, want)
		}
		if h.records.holds("1") {
			t.Error("record kept after the retried release")
		}
		if line := h.logs.line("in-flight record not released"); !strings.Contains(line, "connection reset") {
			t.Errorf("release error log = %q, want the error", line)
		}
	})
}

func TestReleaseFailingAtReturnIsRetriedOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		h.records.failures = 2
		h.submit("dm:u1", "1")
		h.finish("1")
		want := []string{"start 1", "release 1", "release 1"}
		if got := h.trace.list(); !slices.Equal(got, want) {
			t.Errorf("events = %q, want %q", got, want)
		}
	})
}

func TestShutdownStartsNoRunAndKeepsTheRecordsOfRunsNotStarted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		h.submit("dm:u1", "1")
		h.submit("dm:u2", "2")
		h.stopIntake()
		synctest.Wait()
		for range 10 { // places are free, so each call could take one
			if _, _, err := h.s.Reserve(context.Background()); err == nil {
				t.Fatal("Reserve after the shutdown signal succeeded")
			}
		}
		h.finish("1")
		h.wantStarted("1")
		select {
		case <-h.ran:
			t.Fatal("Run returned before its own context ended")
		default:
		}
		h.stopRun()
		<-h.ran
		if !h.records.holds("2") {
			t.Error("run that never started lost its in-flight record")
		}
		if h.records.holds("1") {
			t.Error("run that finished kept its in-flight record")
		}
	})
}

func TestRunCutAtTheShutdownTimeoutBeforeWritingRunsAgainAtTheNextStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		h.submit("dm:u1", "1")
		h.stopIntake()
		h.stopRun()
		synctest.Wait()
		h.abortWork()
		<-h.ran
		if !h.records.holds("1") {
			t.Error("run cut before writing lost its in-flight record")
		}
		if slices.Contains(h.trace.list(), "release 1") {
			t.Error("run cut before writing released its record")
		}
		line := h.logs.line("run interrupted before writing")
		if !strings.Contains(line, "conversation_key=dm:u1") || !strings.Contains(line, "post_ids=[p1]") {
			t.Errorf("interruption log = %q, want the conversation key and post IDs", line)
		}
	})
}

func TestRunCutAtTheShutdownTimeoutAfterAProgressPostIsNotRepeated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		h.script("1", func(ctx context.Context, beforeWrite func(context.Context) error) error {
			if err := beforeWrite(ctx); err != nil {
				return err
			}
			h.trace.add("progress")
			<-ctx.Done() // still generating the reply
			return ctx.Err()
		})
		h.submit("dm:u1", "1")
		h.stopIntake()
		h.stopRun()
		synctest.Wait()
		h.abortWork()
		<-h.ran
		if h.records.holds("1") {
			t.Error("run cut after a progress post kept its record, so the next start would post again")
		}
		line := h.logs.line("run interrupted after writing")
		if !strings.Contains(line, "conversation_key=dm:u1") || !strings.Contains(line, "post_ids=[p1]") {
			t.Errorf("interruption log = %q, want the conversation key and post IDs", line)
		}
	})
}

func TestRunCutAfterWritingDespiteAFailedReleaseIsNotRepeated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		h.records.failures = 1
		h.script("1", func(ctx context.Context, beforeWrite func(context.Context) error) error {
			if err := beforeWrite(ctx); err != nil {
				return err
			}
			h.trace.add("progress")
			<-ctx.Done()
			return ctx.Err()
		})
		h.submit("dm:u1", "1")
		h.stopIntake()
		h.stopRun()
		synctest.Wait()
		h.abortWork()
		<-h.ran
		if h.records.holds("1") {
			t.Error("run that wrote kept its record after its release failed, so the next start would post again")
		}
	})
}

func TestRunLogsItsWaitsAndDuration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := start(t, 1)
		h.submit("dm:u1", "1")
		h.submit("dm:u1", "2")
		h.submit("dm:u2", "3")
		time.Sleep(31 * time.Second)
		h.finish("1")
		time.Sleep(2 * time.Second)
		h.finish("3")
		time.Sleep(time.Second)
		h.finish("2")

		first := h.logs.line("post_ids=[p1]")
		if !strings.Contains(first, "level=DEBUG") || !strings.Contains(first, "queue_wait=0s") ||
			!strings.Contains(first, "conversation_wait=0s") || !strings.Contains(first, "duration=31s") {
			t.Errorf("first run log = %q", first)
		}
		third := h.logs.line("post_ids=[p3]")
		if !strings.Contains(third, "level=DEBUG") || !strings.Contains(third, "queue_wait=31s") || !strings.Contains(third, "conversation_wait=0s") {
			t.Errorf("run that waited for a worker logs %q", third)
		}
		second := h.logs.line("post_ids=[p2]")
		if !strings.Contains(second, "level=INFO") || !strings.Contains(second, "conversation_wait=31s") ||
			!strings.Contains(second, "queue_wait=2s") || !strings.Contains(second, "duration=1s") {
			t.Errorf("run that waited over 30 s for its conversation logs %q", second)
		}
	})
}
