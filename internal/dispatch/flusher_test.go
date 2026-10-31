package dispatch_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/dispatch"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/routing"
	"github.com/sarpaydemir/dost/internal/store/journal"
	"github.com/sarpaydemir/dost/internal/testkit/kvtest"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

// pipelineFake accepts the flusher's runs. When slots is set, each Reserve waits for a value from it.
type pipelineFake struct {
	slots    chan struct{}
	onSubmit func(routing.Job)

	mu        sync.Mutex
	waiting   map[string]int
	checked   []time.Time // when Waiting was called
	jobs      []routing.Job
	cancelled int
}

func (p *pipelineFake) Waiting(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checked = append(p.checked, time.Now())
	return p.waiting[key]
}

func (p *pipelineFake) Reserve(ctx context.Context) (func(routing.Job), func(), error) {
	if p.slots != nil {
		select {
		case <-p.slots:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	submit := func(j routing.Job) {
		if p.onSubmit != nil {
			p.onSubmit(j)
		}
		p.mu.Lock()
		p.jobs = append(p.jobs, j)
		p.mu.Unlock()
	}
	cancel := func() {
		p.mu.Lock()
		p.cancelled++
		p.mu.Unlock()
	}
	return submit, cancel, nil
}

func (p *pipelineFake) checks() []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.checked)
}

func (p *pipelineFake) cancels() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cancelled
}

func (p *pipelineFake) setWaiting(key string, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.waiting[key] = n
}

func (p *pipelineFake) submitted() []routing.Job {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.jobs)
}

// taken returns the message IDs of each submitted job.
func (p *pipelineFake) taken() [][]string {
	var out [][]string
	for _, j := range p.submitted() {
		out = append(out, j.MessageIDs)
	}
	return out
}

// journalFake takes buffers from an in-memory store; its first failures takes fail as an unreachable database would.
type journalFake struct {
	store             *kvtest.Memory
	failures          int
	resubmit, expired []journal.Record
	resubmitErr       error

	mu          sync.Mutex
	attempts    []time.Time
	records     []journal.Record
	resubmitted bool
}

func (j *journalFake) Take(ctx context.Context, key, generation string) (json.RawMessage, bool, error) {
	j.mu.Lock()
	j.attempts = append(j.attempts, time.Now())
	fail := len(j.attempts) <= j.failures
	j.mu.Unlock()
	if fail {
		return nil, false, errors.New("read tcp 10.0.0.5:5432: connection reset by peer")
	}
	v, ok, err := j.store.DeleteIf(ctx, key, func(cur json.RawMessage) bool {
		var b struct {
			Generation any `json:"generation"`
		}
		return json.Unmarshal(cur, &b) == nil && b.Generation == generation
	})
	if ok {
		j.mu.Lock()
		j.records = append(j.records, journal.Record{Key: key, Generation: generation, Buffer: v})
		j.mu.Unlock()
	}
	return v, ok, err
}

func (j *journalFake) Resubmit(_ context.Context, limit int) ([]journal.Record, []journal.Record, error) {
	j.mu.Lock()
	j.resubmitted = true
	j.mu.Unlock()
	if limit != 2 {
		return nil, nil, fmt.Errorf("resubmission limit %d, want 2", limit)
	}
	return j.resubmit, j.expired, j.resubmitErr
}

func (j *journalFake) attemptTimes() []time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.attempts)
}

func (j *journalFake) journaled() []journal.Record {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.records)
}

// lockFake is held once acquired. When gate is set, Acquire waits until it is closed; a value sent on lost ends Wait
// with that error.
type lockFake struct {
	gate chan struct{}
	lost chan error

	mu   sync.Mutex
	held bool
}

func (l *lockFake) Acquire(ctx context.Context) error {
	if l.gate != nil {
		select {
		case <-l.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held = true
	return nil
}

func (l *lockFake) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-l.lost:
		return err
	}
}

func (l *lockFake) isHeld() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

// flushFixture is a flusher fed by a dispatcher with a 3 s window, both on one in-memory store. It runs inside a
// synctest bubble, where time moves only while every goroutine waits.
type flushFixture struct {
	t        *testing.T
	store    *kvtest.Memory
	runs     *journalFake
	lock     *lockFake
	pipeline *pipelineFake
	logs     bytes.Buffer // read only after shutdown
	f        *dispatch.Flusher
	d        *dispatch.Dispatcher
	stop     context.CancelFunc
	done     chan error
}

func newFlushFixture(t *testing.T) *flushFixture { return build(t, &kvtest.Memory{}) }

// build returns a fixture on store, as a new process would start on it.
func build(t *testing.T, store *kvtest.Memory) *flushFixture {
	ff := &flushFixture{t: t, store: store, runs: &journalFake{store: store}, lock: &lockFake{lost: make(chan error, 1)},
		pipeline: &pipelineFake{waiting: map[string]int{}}}
	ff.f = dispatch.NewFlusher(prefix, store, ff.runs, ff.lock, ff.pipeline, clock.Real(), slog.New(slog.NewTextHandler(&ff.logs, nil)))
	settings := &snapshot{}
	settings.window(3)
	p := &platform{configured: chat.User{ID: agentID, Username: "helper"}}
	open := func(*config.Settings) (chat.Platform, error) { return p, nil }
	discard := slog.New(slog.DiscardHandler)
	identity := routing.NewIdentity(&kvtest.Memory{}, "agent:bot_identity", clock.Real(), discard)
	ff.d = dispatch.NewDispatcher(t.Context(), settings, open, identity, store, ff.f, clock.Real(), discard)
	return ff
}

// start runs Start, then Run and the dispatcher, and waits until they are idle.
func (ff *flushFixture) start() {
	ff.t.Helper()
	if err := ff.f.Start(ff.t.Context()); err != nil {
		ff.t.Fatalf("Start: %v", err)
	}
	ff.run()
}

// run starts Run and the dispatcher after Start has returned, and waits until they are idle.
func (ff *flushFixture) run() {
	ctx, cancel := context.WithCancel(ff.t.Context())
	ff.stop, ff.done = cancel, make(chan error, 1)
	go func() { ff.done <- ff.f.Run(ctx) }()
	go func() { _ = ff.d.Run(ctx) }()
	synctest.Wait()
}

// shutdown stops the flusher and the dispatcher and returns Run's error.
func (ff *flushFixture) shutdown() error {
	ff.stop()
	err := <-ff.done
	synctest.Wait()
	return err
}

// deliver passes messages to the dispatcher and waits until they are buffered and the flusher is idle.
func (ff *flushFixture) deliver(ms ...chat.Message) {
	ff.t.Helper()
	for _, m := range ms {
		if err := ff.d.Enqueue(ff.t.Context(), event(m)); err != nil {
			ff.t.Fatal(err)
		}
	}
	synctest.Wait()
}

// after lets d pass and waits until the flusher is idle.
func after(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

func (ff *flushFixture) notify(key, generation string, due time.Time) {
	ff.t.Helper()
	if err := ff.f.Notify(ff.t.Context(), dispatch.Notice{Key: key, Generation: generation, FlushAfter: due}); err != nil {
		ff.t.Fatal(err)
	}
	synctest.Wait()
}

func (ff *flushFixture) set(key, value string) {
	ff.t.Helper()
	if err := ff.store.Set(ff.t.Context(), key, json.RawMessage(value)); err != nil {
		ff.t.Fatal(err)
	}
}

// value returns the value under key, or "" when there is none.
func (ff *flushFixture) value(key string) string {
	ff.t.Helper()
	v, _, err := ff.store.Get(ff.t.Context(), key)
	if err != nil {
		ff.t.Fatal(err)
	}
	return string(v)
}

func (ff *flushFixture) assertTaken(want ...[]string) {
	ff.t.Helper()
	if got := ff.pipeline.taken(); !slices.EqualFunc(got, want, slices.Equal) {
		ff.t.Errorf("jobs hold the messages %q, want %q", got, want)
	}
}

// stored returns a buffer due at flushAfter holding a message per post ID.
func stored(generation, flushAfter string, postIDs ...string) string {
	var ms []string
	for _, id := range postIDs {
		ms = append(ms, fmt.Sprintf(`{"post_id": %q, "user_id": "u1", "message": "hi", "file_ids": []}`, id))
	}
	return fmt.Sprintf(`{"generation": %q, "flush_after": %q, "payload": {"data": {"post": "{}", "channel_type": "D"}},
		"messages": [%s]}`, generation, flushAfter, strings.Join(ms, ", "))
}

// at formats the time d from now as flush_after values are stored.
func at(d time.Duration) string { return textfmt.FormatTime(time.Now().Add(d)) }

func TestJobCombinesTheBufferedMessagesUnderTheFirstPayload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := newFlushFixture(t)
		const payload = `{"data": {"post": "{\"id\": \"p1\", \"user_id\": \"u1\", \"message\": \"a\", \"is_dm\": true}", "channel_type": "D"}}`
		const buffer = `{"generation": "g1", "flush_after": "1999-12-31T23:59:59.500000+00:00", "payload": ` + payload + `,
			"messages": [
				{"post_id": "p1", "user_id": "u1", "message": "a", "file_ids": ["f1", "f2"]},
				{"post_id": "p2", "user_id": "u1", "message": "b\nc", "file_ids": ["f2", "f3"]},
				{"post_id": "p3", "user_id": "u2", "message": "d"},
				{"post_id": "p4", "user_id": 5, "message": null, "file_ids": null},
				{"post_id": "p5", "user_id": true, "message": 1.0, "file_ids": ["f1"]},
				{"post_id": "p6", "user_id": "u1", "message": ["x", {"k": "v"}], "file_ids": []}]}`
		key := prefix + "dm:u1"
		ff.set(key, buffer)
		ff.start()
		if err := ff.shutdown(); err != nil {
			t.Fatalf("Run = %v", err)
		}

		jobs := ff.pipeline.submitted()
		if len(jobs) != 1 {
			t.Fatalf("%d jobs, want 1", len(jobs))
		}
		j := jobs[0]
		const text = "u1: a\nu1: b\nc\nu2: d\n5: None\nTrue: 1.0\nu1: ['x', {'k': 'v'}]"
		if j.Key != key || j.Generation != "g1" || j.Text != text ||
			!slices.Equal(j.AttachmentIDs, []string{"f1", "f2", "f3"}) ||
			!slices.Equal(j.MessageIDs, []string{"p1", "p2", "p3", "p4", "p5", "p6"}) {
			t.Errorf("job = %+v\nwant key %s, generation g1, text %q, attachments [f1 f2 f3], messages p1 to p6", j, key, text)
		}
		assertJSON(t, "payload", string(j.Payload), payload)
		if v := ff.value(key); v != "" {
			t.Errorf("buffer after the take = %s", v)
		}
		if r := ff.runs.journaled(); len(r) != 1 || r[0].Generation != "g1" {
			t.Fatalf("journal = %+v, want the taken buffer", r)
		} else {
			assertJSON(t, "journaled buffer", string(r[0].Buffer), buffer)
		}
		if logs := ff.logs.String(); !strings.Contains(logs, "key="+key) || !strings.Contains(logs, "generation=g1") ||
			!strings.Contains(logs, "fragments=6") || !strings.Contains(logs, "post_ids=\"[p1 p2 p3 p4 p5 p6]\"") {
			t.Errorf("the take is not logged with key, generation, fragment count, and post IDs:\n%s", logs)
		}
	})
}

func TestNullOrMissingPayloadIsHandedOnAsStored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := newFlushFixture(t)
		ff.set(prefix+"dm:null", `{"generation": "g1", "flush_after": "1999-12-31T23:59:59+00:00", "payload": null,
			"messages": [{"post_id": "p1", "user_id": "u1", "message": "a", "file_ids": []}]}`)
		ff.set(prefix+"dm:missing", `{"generation": "g2", "flush_after": "1999-12-31T23:59:59+00:00",
			"messages": [{"post_id": "p2", "user_id": "u2", "message": "b", "file_ids": []}]}`)
		ff.start()
		if err := ff.shutdown(); err != nil {
			t.Fatalf("Run = %v", err)
		}

		payloads := map[string]string{}
		for _, j := range ff.pipeline.submitted() {
			payloads[j.Key] = string(j.Payload)
		}
		if want := map[string]string{prefix + "dm:null": "null", prefix + "dm:missing": ""}; !maps.Equal(payloads, want) {
			t.Errorf("payloads by key = %q, want %q", payloads, want)
		}
		for _, key := range []string{prefix + "dm:null", prefix + "dm:missing"} {
			if v := ff.value(key); v != "" {
				t.Errorf("%s not taken: %s", key, v)
			}
		}
	})
}

func TestStaleGenerationIsSkippedAndALaterFlushAfterIsAwaited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := newFlushFixture(t)
		ff.start()
		stale, extended := prefix+"dm:u1", prefix+"dm:u2"
		ff.set(stale, stored("g-new", at(0), "p1"))
		ff.notify(stale, "g-old", time.Now().Add(3*time.Second))
		ff.set(extended, stored("g2", at(5*time.Second), "p2"))
		ff.notify(extended, "g2", time.Now().Add(3*time.Second))

		after(3 * time.Second)
		ff.assertTaken()
		if n := ff.pipeline.cancels(); n != 0 {
			t.Errorf("%d reservations for a stale or early timer, want none", n)
		}
		after(2*time.Second - time.Microsecond)
		ff.assertTaken()
		after(time.Microsecond)
		ff.assertTaken([]string{"p2"})
		if v := ff.value(stale); v == "" {
			t.Error("the buffer of a newer generation was taken by an older notice")
		}
		if err := ff.shutdown(); err != nil {
			t.Fatalf("Run = %v", err)
		}
	})
}

func TestMessageAppendedAfterTheTakeGetsItsOwnRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := newFlushFixture(t)
		paused, resume := make(chan struct{}), make(chan struct{})
		ff.pipeline.onSubmit = func(j routing.Job) {
			if slices.Contains(j.MessageIDs, "p1") {
				paused <- struct{}{}
				<-resume
			}
		}
		ff.start()
		ff.deliver(msg("p1", "user-a", "first", direct))
		<-paused
		ff.deliver(msg("p2", "user-a", "second", direct))
		close(resume)
		after(3 * time.Second)

		ff.assertTaken([]string{"p1"}, []string{"p2"})
		jobs := ff.pipeline.submitted()
		if len(jobs) == 2 && (jobs[1].Text != "user-a: second" || len(jobs[1].AttachmentIDs) != 0) {
			t.Errorf("second job = %+v, want only the second message and no attachments", jobs[1])
		}
		if err := ff.shutdown(); err != nil {
			t.Fatalf("Run = %v", err)
		}
	})
}

func TestMessageArrivingDuringACapacityWaitMergesIntoTheNextTake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := newFlushFixture(t)
		ff.pipeline.slots = make(chan struct{})
		ff.start()
		ff.deliver(msg("p1", "user-a", "first", direct))
		after(3 * time.Second)
		ff.deliver(msg("p2", "user-a", "second", direct))
		ff.pipeline.slots <- struct{}{}
		synctest.Wait()
		ff.assertTaken()

		ff.pipeline.slots <- struct{}{}
		synctest.Wait()
		ff.assertTaken([]string{"p1", "p2"})
		if n := ff.pipeline.cancels(); n != 1 {
			t.Errorf("%d reservations cancelled, want the one whose take found a newer generation", n)
		}
		if err := ff.shutdown(); err != nil {
			t.Fatalf("Run = %v", err)
		}
	})
}

func TestBusyConversationIsCheckedAgainEverySecondWhileItKeepsMerging(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := newFlushFixture(t)
		key := prefix + "dm:user-a"
		ff.pipeline.setWaiting(key, 4)
		ff.start()
		begin := time.Now()
		ff.deliver(msg("p1", "user-a", "first", direct))
		after(5500 * time.Millisecond)
		ff.assertTaken()
		ff.deliver(msg("p2", "user-a", "second", direct))
		ff.pipeline.setWaiting(key, 3)
		after(3 * time.Second)

		ff.assertTaken([]string{"p1", "p2"})
		var offsets []time.Duration
		for _, c := range ff.pipeline.checks() {
			offsets = append(offsets, c.Sub(begin))
		}
		if want := []time.Duration{3 * time.Second, 4 * time.Second, 5 * time.Second, 8500 * time.Millisecond}; !slices.Equal(offsets, want) {
			t.Errorf("waiting runs checked at %v, want %v", offsets, want)
		}
		if err := ff.shutdown(); err != nil {
			t.Fatalf("Run = %v", err)
		}
	})
}

func TestFailedTakeIsRetriedWithBackoffAndTheBufferKept(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := newFlushFixture(t)
		ff.runs.failures = 7
		ff.start()
		begin := time.Now()
		ff.deliver(msg("p1", "user-a", "hi", direct))
		after(93 * time.Second)
		if v := ff.value(prefix + "dm:user-a"); v == "" {
			t.Error("buffer dropped while the store fails")
		}
		after(time.Second)

		ff.assertTaken([]string{"p1"})
		var offsets []time.Duration
		for _, a := range ff.runs.attemptTimes() {
			offsets = append(offsets, a.Sub(begin))
		}
		want := []time.Duration{3, 4, 6, 10, 18, 34, 64, 94}
		for i := range want {
			want[i] *= time.Second
		}
		if !slices.Equal(offsets, want) {
			t.Errorf("takes attempted at %v, want %v", offsets, want)
		}
		if n := ff.pipeline.cancels(); n != 7 {
			t.Errorf("%d reservations cancelled, want one per failed take", n)
		}
		if err := ff.shutdown(); err != nil {
			t.Fatalf("Run = %v", err)
		}
	})
}

func TestJournaledRunsAreResubmittedBeforeStoredBuffersAreFlushed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := newFlushFixture(t)
		k1, k2 := prefix+"dm:u1", prefix+"dm:u2"
		ff.runs.resubmit = []journal.Record{
			{Key: k1, Generation: "g-a", Buffer: json.RawMessage(stored("g-a", at(-time.Hour), "p1", "p2"))},
			{Key: k2, Generation: "g-b", Buffer: json.RawMessage(stored("g-b", at(-time.Hour), "p3"))},
		}
		ff.runs.expired = []journal.Record{
			{Key: prefix + "dm:u3", Generation: "g-c", Buffer: json.RawMessage(stored("g-c", at(-time.Hour), "p8", "p9"))},
		}
		ff.set(k1, stored("g-d", at(-time.Minute), "p4"))
		ff.pipeline.setWaiting(k1, 4)
		if err := ff.f.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		ff.assertTaken([]string{"p1", "p2"}, []string{"p3"})
		if v := ff.value(k1); v == "" {
			t.Error("stored buffer taken during Start")
		}

		ff.pipeline.setWaiting(k1, 0)
		ff.run()
		ff.assertTaken([]string{"p1", "p2"}, []string{"p3"}, []string{"p4"})
		if jobs := ff.pipeline.submitted(); jobs[0].Key != k1 || jobs[0].Generation != "g-a" || jobs[1].Key != k2 || jobs[1].Generation != "g-b" {
			t.Errorf("resubmitted jobs %+v, want them under their records' keys and generations", jobs[:2])
		}
		if err := ff.shutdown(); err != nil {
			t.Fatalf("Run = %v", err)
		}
		var dropped string
		for _, line := range strings.Split(ff.logs.String(), "\n") {
			if strings.Contains(line, "level=ERROR") {
				dropped += line
			}
		}
		if !strings.Contains(dropped, "key="+prefix+"dm:u3") || !strings.Contains(dropped, `post_ids="[p8 p9]"`) {
			t.Errorf("expired record not logged at error level with its key and post IDs:\n%s", ff.logs.String())
		}
	})
}

func TestStartupSweepSchedulesBuffersWrittenBeforeStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := newFlushFixture(t)
		buffers := map[string]string{
			"dm:past":        stored("g1", at(-time.Minute), "past"),
			"dm:naive":       stored("g2", strings.TrimSuffix(at(5*time.Second), "+00:00"), "naive"),
			"dm:unparseable": stored("g3", "soon", "unparseable"),
			"dm:malformed":   `["not a buffer"]`,
			"dm:empty":       `{"generation": "g4", "flush_after": "1999-12-31T23:00:00+00:00", "payload": {}, "messages": []}`,
		}
		for key, v := range buffers {
			ff.set(prefix+key, v)
		}
		ff.set("other:buffer:dm:u1", stored("g5", at(-time.Minute), "other"))
		ff.start()

		got := ff.pipeline.taken()
		slices.SortFunc(got, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
		if want := [][]string{{"past"}, {"unparseable"}}; !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("flushed at start: %q, want %q", got, want)
		}
		after(5*time.Second - time.Microsecond)
		if n := len(ff.pipeline.taken()); n != 2 {
			t.Errorf("%d jobs before the naive flush_after, want 2", n)
		}
		after(time.Microsecond)
		if jobs := ff.pipeline.taken(); len(jobs) != 3 || jobs[2][0] != "naive" {
			t.Errorf("jobs %q, want the naive buffer flushed at its time read as UTC", jobs)
		}
		for _, key := range []string{prefix + "dm:malformed", prefix + "dm:empty", "other:buffer:dm:u1"} {
			if v := ff.value(key); v == "" {
				t.Errorf("%s was removed", key)
			}
		}
		if err := ff.shutdown(); err != nil {
			t.Fatalf("Run = %v", err)
		}
		if logs := ff.logs.String(); !strings.Contains(logs, "key="+prefix+"dm:malformed") {
			t.Errorf("malformed buffer not logged:\n%s", logs)
		}
	})
}

func TestShutdownLeavesBuffersForTheNextStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := newFlushFixture(t)
		ff.pipeline.slots = make(chan struct{})
		ff.start()
		ff.deliver(msg("p1", "user-a", "waiting for capacity", direct))
		after(3 * time.Second)
		ff.deliver(msg("p2", "user-b", "notice not yet read", direct), msg("p3", "user-c", "timer pending", direct))
		if err := ff.shutdown(); err != nil {
			t.Fatalf("Run = %v", err)
		}
		if !ff.lock.isHeld() {
			t.Error("lock released by Run; it must stay held until the pipeline has stopped")
		}
		ff.assertTaken()
		if r := ff.runs.journaled(); len(r) > 0 {
			t.Errorf("journal = %+v after shutdown, want nothing taken", r)
		}

		next := build(t, ff.store)
		next.start()
		after(3 * time.Second)
		got := next.pipeline.taken()
		slices.SortFunc(got, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
		if want := [][]string{{"p1"}, {"p2"}, {"p3"}}; !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("after a restart the buffers held %q, want %q", got, want)
		}
		if err := next.shutdown(); err != nil {
			t.Fatalf("Run = %v", err)
		}
	})
}

func TestNotifyQueueHolds1024NoticesAndThenWaitsWithoutDropping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := newFlushFixture(t)
		if err := ff.f.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		const n = 1025
		for i := range n {
			ff.set(fmt.Sprintf("%sdm:u%d", prefix, i), stored("g", at(0), fmt.Sprint(i)))
		}
		notice := func(i int) dispatch.Notice {
			return dispatch.Notice{Key: fmt.Sprintf("%sdm:u%d", prefix, i), Generation: "g", FlushAfter: time.Now()}
		}
		for i := range n - 1 {
			if err := ff.f.Notify(t.Context(), notice(i)); err != nil {
				t.Fatalf("Notify %d: %v", i, err)
			}
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := ff.f.Notify(ctx, notice(n-1)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Notify on a full queue = %v, want it to wait until its context ends", err)
		}
		notified := make(chan error, 1)
		go func() { notified <- ff.f.Notify(t.Context(), notice(n-1)) }()
		ff.run()
		if err := <-notified; err != nil {
			t.Errorf("Notify after the queue drained = %v", err)
		}
		if got := len(ff.pipeline.taken()); got != n {
			t.Errorf("%d buffers flushed, want all %d notified", got, n)
		}
		if err := ff.shutdown(); err != nil {
			t.Fatalf("Run = %v", err)
		}
	})
}

func TestStartHoldsTheLockBeforeTouchingTheStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := newFlushFixture(t)
		ff.lock.gate = make(chan struct{})
		ff.set(prefix+"dm:u1", stored("g1", at(-time.Minute), "p1"))
		started := make(chan error, 1)
		go func() { started <- ff.f.Start(t.Context()) }()
		synctest.Wait()
		if ff.runs.resubmitted {
			t.Error("journal read before the lock was held")
		}
		close(ff.lock.gate)
		if err := <-started; err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !ff.runs.resubmitted || !ff.lock.isHeld() {
			t.Errorf("after Start: journal read %v, lock held %v; want both", ff.runs.resubmitted, ff.lock.isHeld())
		}

		failing := newFlushFixture(t)
		failing.runs.resubmitErr = errors.New("connection refused")
		if err := failing.f.Start(t.Context()); err == nil {
			t.Error("Start with an unreachable journal succeeded")
		}
	})
}

func TestLostLockEndsRunWithTheErrorAndLeavesBuffers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ff := newFlushFixture(t)
		ff.start()
		ff.deliver(msg("p1", "user-a", "hi", direct))
		lost := errors.New("lock lost: connection reset")
		ff.lock.lost <- lost
		if err := <-ff.done; !errors.Is(err, lost) {
			t.Errorf("Run = %v, want the lock error", err)
		}
		ff.stop()
		synctest.Wait()
		after(3 * time.Second)
		ff.assertTaken()
		if v := ff.value(prefix + "dm:user-a"); v == "" {
			t.Error("buffer removed after the lock was lost")
		}
	})
}
