package dispatch_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/dispatch"
	"github.com/sarpaydemir/melinda/internal/gateway"
	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/testkit"
)

// recovery runs a recovery that replays into f's dispatcher until stop is called.
type recovery struct {
	*dispatch.Recovery
	f      *fixture
	cancel context.CancelFunc
	done   <-chan error
}

func (f *fixture) startRecovery() *recovery {
	rec := dispatch.NewRecovery(f.d, f.store, f.log)
	ctx, cancel := context.WithCancel(f.t.Context())
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()
	return &recovery{Recovery: rec, f: f, cancel: cancel, done: done}
}

// stop stops intake and waits for Run to return.
func (r *recovery) stop() {
	r.f.t.Helper()
	r.cancel()
	r.f.wait(r.done)
}

// awaitReplays waits until n replays have returned.
func (f *fixture) awaitReplays(n int32) {
	f.t.Helper()
	f.platform.replayed.Await(f.t, fmt.Sprintf("%d replays", n), func() bool { return f.platform.replays.Load() >= n })
}

// recoverOnce runs one recovery and returns once it has finished.
func (f *fixture) recoverOnce() {
	f.t.Helper()
	r := f.startRecovery()
	r.Trigger()
	f.awaitReplays(1)
	r.stop()
}

// replaying makes the platform deliver ms on every replay.
func replaying(ms ...chat.Message) func(context.Context, time.Time, chat.Deliver) error {
	return func(ctx context.Context, _ time.Time, deliver chat.Deliver) error {
		for _, m := range ms {
			if err := deliver(ctx, event(m)); err != nil {
				return err
			}
		}
		return nil
	}
}

// gate holds each replay until the test opens it, counting the replays that started.
type gate struct {
	open    chan struct{}
	started atomic.Int32
	changed testkit.Notifier
}

func newGate() *gate { return &gate{open: make(chan struct{})} }

func (g *gate) wait(ctx context.Context) error {
	g.started.Add(1)
	g.changed.Notify()
	select {
	case <-g.open:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gate) awaitStarted(t *testing.T, n int32) {
	t.Helper()
	g.changed.Await(t, fmt.Sprintf("%d replays started", n), func() bool { return g.started.Load() >= n })
}

func TestRunReplaysAfterTheMarkerOrFromNowWithoutOne(t *testing.T) {
	for _, tt := range []struct {
		name, marker string
		want         time.Time
	}{
		{"no marker", "", start},
		{"marker", `{"create_at": 1767225600000}`, time.UnixMilli(1767225600000)},
		{"marker without a time", `{"create_at": null}`, start},
		{"marker that is not an object", `"text"`, start},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.marker != "" {
				f.set(marker, tt.marker)
			}
			var after time.Time
			f.platform.replay = func(_ context.Context, a time.Time, _ chat.Deliver) error {
				after = a
				return nil
			}
			f.recoverOnce()
			if !after.Equal(tt.want) {
				t.Errorf("Replay after %v, want %v", after, tt.want)
			}
		})
	}
}

func TestEachRunResolvesTheAgentIdentity(t *testing.T) {
	f := newFixture(t)
	f.platform.configured = chat.User{ID: agentID}
	f.platform.reported = chat.User{ID: agentID, Username: "helper"}
	f.platform.replay = replaying()
	f.recoverOnce()
	if n := f.platform.lookups.Load(); n != 1 {
		t.Errorf("%d identity lookups, want 1 with nothing to replay", n)
	}
}

func TestMessageSeenLiveAndReplayedIsBufferedOnce(t *testing.T) {
	f := newFixture(t)
	live := msg("p1", "user-a", "hi", direct, createdAt(2000))
	f.deliver(live)
	f.platform.replay = replaying(live, msg("p2", "user-a", "are you there?", direct, createdAt(3000)))
	f.recoverOnce()
	f.stop()
	if got := postIDs(f.value(prefix + "dm:user-a")); !slices.Equal(got, []string{"p1", "p2"}) {
		t.Errorf("buffer holds %q, want p1 then p2", got)
	}
	if n := len(f.flusher.received()); n != 2 {
		t.Errorf("%d notices, want one per message", n)
	}
}

func TestTriggersDuringARunCoalesceIntoOneMoreRun(t *testing.T) {
	f := newFixture(t)
	g := newGate()
	f.platform.replay = func(ctx context.Context, _ time.Time, _ chat.Deliver) error { return g.wait(ctx) }
	r := f.startRecovery()
	r.Trigger()
	g.awaitStarted(t, 1)
	r.Trigger()
	r.Trigger()
	r.Trigger()
	g.open <- struct{}{}
	g.awaitStarted(t, 2)
	g.open <- struct{}{}
	f.awaitReplays(2)
	r.stop()
	if n := g.started.Load(); n != 2 {
		t.Errorf("%d runs, want the first and one more for the triggers during it", n)
	}
}

func TestFullIntakeMakesTheRunWaitWithoutDropping(t *testing.T) {
	f := newFixture(t)
	var missed []chat.Message
	for i := range 300 {
		missed = append(missed, msg(fmt.Sprintf("p%d", i), fmt.Sprintf("user-%d", i), "hi", direct))
	}
	var delivering atomic.Int32
	var changed testkit.Notifier
	f.platform.replay = func(ctx context.Context, _ time.Time, deliver chat.Deliver) error {
		for _, m := range missed {
			delivering.Add(1)
			changed.Notify()
			if err := deliver(ctx, event(m)); err != nil {
				return err
			}
		}
		return nil
	}
	r := f.startRecovery()
	r.Trigger()
	changed.Await(t, "the delivery past a full intake", func() bool { return delivering.Load() == 257 })

	ctx, cancel := context.WithCancel(t.Context())
	done := f.run(ctx)
	f.flusher.await(t, 300)
	f.awaitReplays(1)
	r.stop()
	cancel()
	f.wait(done)
	if n := len(f.flusher.received()); n != 300 {
		t.Errorf("%d messages buffered, want all 300 replayed", n)
	}
	if logs := f.logs.String(); !strings.Contains(logs, "replayed=300") {
		t.Errorf("logs do not report the 300 replayed messages:\n%s", logs)
	}
}

func TestShutdownLetsTheRunningRunFinishAndDropsThePendingOne(t *testing.T) {
	f := newFixture(t)
	g := newGate()
	var mu sync.Mutex
	var deliverErr error
	finished := false
	f.platform.replay = func(ctx context.Context, _ time.Time, deliver chat.Deliver) error {
		if err := g.wait(ctx); err != nil {
			return err
		}
		err := deliver(ctx, event(msg("p1", "user-a", "hi", direct)))
		mu.Lock()
		deliverErr, finished = err, true
		mu.Unlock()
		return err
	}
	r := f.startRecovery()
	r.Trigger()
	g.awaitStarted(t, 1)
	r.Trigger()
	r.cancel()
	close(g.open)
	f.wait(r.done)

	mu.Lock()
	defer mu.Unlock()
	if !finished || deliverErr != nil {
		t.Errorf("run in progress at shutdown: finished %v, delivery error %v; want it finished with its delivery", finished, deliverErr)
	}
	if n := g.started.Load(); n != 1 {
		t.Errorf("%d runs, want the pending one dropped at shutdown", n)
	}
	f.stop()
	if f.value(prefix+"dm:user-a") == "" {
		t.Error("the message replayed during shutdown was not buffered")
	}
}

func TestReplayErrorIsLoggedWithTheReplayedCount(t *testing.T) {
	f := newFixture(t)
	f.platform.replay = func(ctx context.Context, _ time.Time, deliver chat.Deliver) error {
		if err := deliver(ctx, event(msg("p1", "user-a", "hi", direct))); err != nil {
			return err
		}
		return errors.New("mattermost GET /users/me/teams: 502 Bad Gateway")
	}
	f.recoverOnce()
	logs := f.logs.String()
	if !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "replayed=1") ||
		!strings.Contains(logs, "502 Bad Gateway") {
		t.Errorf("logs do not report the failed replay with its count:\n%s", logs)
	}
}

// server makes the platform replay the messages in ms created after the replay's start, recording each start.
type server struct {
	mu     sync.Mutex
	ms     []chat.Message
	starts []time.Time
	fail   error
}

func (f *fixture) serve(ms ...chat.Message) *server {
	s := &server{ms: ms}
	f.platform.replay = func(ctx context.Context, after time.Time, deliver chat.Deliver) error {
		s.mu.Lock()
		s.starts = append(s.starts, after)
		fail := s.fail
		s.mu.Unlock()
		for _, m := range s.ms {
			if m.Created.After(after) {
				if err := deliver(ctx, event(m)); err != nil {
					return err
				}
			}
		}
		return fail
	}
	return s
}

// lastStart returns where the latest replay started, in Unix milliseconds.
func (s *server) lastStart() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts[len(s.starts)-1].UnixMilli()
}

// reconnect triggers a run and waits until replay n has returned.
func (r *recovery) reconnect(n int32) {
	r.f.t.Helper()
	r.Trigger()
	r.f.awaitReplays(n)
}

func TestMessageMissedWhileDisconnectedIsBufferedOnceAfterALivePostMovesTheMarker(t *testing.T) {
	f := newFixture(t)
	before := msg("p1", "user-a", "before", direct, createdAt(1000))
	missed := msg("p2", "user-b", "missed", direct, createdAt(2000))
	live := msg("p3", "user-c", "live", direct, createdAt(3000))
	f.serve(before, missed, live)
	f.deliver(before)
	r := f.startRecovery()
	r.Disconnected()
	f.deliver(live)
	r.reconnect(1)
	r.stop()
	f.stop()
	for user, want := range map[string]string{"user-a": "p1", "user-b": "p2", "user-c": "p3"} {
		if got := postIDs(f.value(prefix + "dm:" + user)); !slices.Equal(got, []string{want}) {
			t.Errorf("buffer of %s holds %q, want %s once", user, got, want)
		}
	}
	if n := len(f.flusher.received()); n != 3 {
		t.Errorf("%d notices, want one per message", n)
	}
}

func TestReplayStartsFromTheEarliestDisconnectUntilAReplayAfterReconnectingSucceeds(t *testing.T) {
	f := newFixture(t)
	s := f.serve()
	r := f.startRecovery()
	defer r.stop()
	f.deliver(msg("p1", "user-a", "hi", createdAt(1000)))
	r.Disconnected()
	f.deliver(msg("p2", "user-a", "hi", createdAt(2000)))
	r.Disconnected()
	f.deliver(msg("p3", "user-a", "hi", createdAt(3000)))
	r.reconnect(1)
	if got := s.lastStart(); got != 1000 {
		t.Errorf("replay after two disconnects starts at %d, want the first disconnect's 1000", got)
	}

	f.deliver(msg("p4", "user-a", "hi", createdAt(4000)))
	r.Disconnected()
	f.deliver(msg("p5", "user-a", "hi", createdAt(5000)))
	r.reconnect(2)
	if got := s.lastStart(); got != 4000 {
		t.Errorf("replay after a successful replay and one more disconnect starts at %d, want 4000", got)
	}
}

func TestFailedReplayKeepsItsStartForTheNextOne(t *testing.T) {
	f := newFixture(t)
	s := f.serve()
	s.fail = errors.New("mattermost GET /users/me/teams: 502 Bad Gateway")
	r := f.startRecovery()
	defer r.stop()
	f.deliver(msg("p1", "user-a", "hi", createdAt(1000)))
	r.Disconnected()
	f.deliver(msg("p2", "user-a", "hi", createdAt(2000)))
	r.reconnect(1)
	s.mu.Lock()
	s.fail = nil
	s.mu.Unlock()
	f.deliver(msg("p3", "user-a", "hi", createdAt(3000)))
	r.Disconnected()
	r.reconnect(2)
	if got := s.lastStart(); got != 1000 {
		t.Errorf("replay after a failed one starts at %d, want the failed replay's 1000", got)
	}
}

func TestStartupReplayFailureKeepsTheStartupMarker(t *testing.T) {
	f := newFixture(t)
	f.set(marker, `{"create_at": 1000}`)
	s := f.serve()
	s.fail = errors.New("mattermost GET /users/me/teams: 502 Bad Gateway")
	r := f.startRecovery()
	defer r.stop()
	r.reconnect(1)
	s.mu.Lock()
	s.fail = nil
	s.mu.Unlock()
	f.deliver(msg("p2", "user-a", "hi", createdAt(2000)))
	r.Disconnected()
	r.reconnect(2)
	if got := s.lastStart(); got != 1000 {
		t.Errorf("replay after a failed startup replay starts at %d, want the startup marker 1000", got)
	}
}

func TestDisconnectDuringAReplayKeepsItsStart(t *testing.T) {
	f := newFixture(t)
	g := newGate()
	s := f.serve()
	serve := f.platform.replay
	f.platform.replay = func(ctx context.Context, after time.Time, deliver chat.Deliver) error {
		if err := g.wait(ctx); err != nil {
			return err
		}
		return serve(ctx, after, deliver)
	}
	r := f.startRecovery()
	defer r.stop()
	f.deliver(msg("p1", "user-a", "hi", createdAt(1000)))
	r.Disconnected()
	r.Trigger()
	g.awaitStarted(t, 1)
	f.deliver(msg("p2", "user-a", "hi", createdAt(2000)))
	r.Disconnected()
	g.open <- struct{}{}
	f.awaitReplays(1)
	close(g.open)
	r.reconnect(2)
	if got := s.lastStart(); got != 1000 {
		t.Errorf("replay after a disconnect during the previous one starts at %d, want that one's 1000", got)
	}
}

func TestReplayWhileDisconnectedKeepsItsStart(t *testing.T) {
	f := newFixture(t)
	g := newGate()
	s := f.serve()
	serve := f.platform.replay
	f.platform.replay = func(ctx context.Context, after time.Time, deliver chat.Deliver) error {
		if err := g.wait(ctx); err != nil {
			return err
		}
		return serve(ctx, after, deliver)
	}
	f.set(marker, `{"create_at": 500}`)
	r := f.startRecovery()
	defer r.stop()
	r.Trigger()
	g.awaitStarted(t, 1)
	f.deliver(msg("p1", "user-a", "hi", createdAt(1000)))
	r.Disconnected()
	r.Trigger()
	f.deliver(msg("p2", "user-a", "hi", createdAt(1500)))
	r.Disconnected()
	close(g.open)
	f.awaitReplays(2)
	f.deliver(msg("p3", "user-a", "hi", createdAt(2000)))
	r.reconnect(3)
	if got := s.lastStart(); got != 500 {
		t.Errorf("replay after reconnecting starts at %d, want 500 despite the replay that ran while disconnected", got)
	}
}

// listener is a platform whose one intake session connects, delivers live, and stays open until ctx ends.
type listener struct {
	*platform
	live chat.Message
}

func (l *listener) Listen(ctx context.Context, connected func(), deliver chat.Deliver) error {
	connected()
	if err := deliver(ctx, event(l.live)); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func TestMessageMissedWhileStoppedIsBufferedOnceAfterALivePostAtStartupMovesTheMarker(t *testing.T) {
	f := newFixture(t)
	f.set(marker, `{"create_at": 1000}`)
	missed := msg("p2", "user-b", "missed", direct, createdAt(2000))
	live := msg("p3", "user-c", "live", direct, createdAt(3000))
	f.serve(missed, live)
	rec := dispatch.NewRecovery(f.d, f.store, f.log)
	open := func(*config.Settings) (chat.Platform, error) { return &listener{platform: f.platform, live: live}, nil }
	gw := gateway.New(f.settings, open, f.d.Enqueue, rec.Trigger, rec.Disconnected, f.clk, f.log)
	dispatching, stopDispatch := context.WithCancel(t.Context())
	dispatched := f.run(dispatching)
	connected, disconnect := context.WithCancel(t.Context())
	defer disconnect()
	go func() { _ = gw.Run(connected) }()

	// The live post moves the marker before the startup run, which the connection triggered, starts.
	f.flusher.await(t, 1)
	recovering, stopRecovery := context.WithCancel(t.Context())
	recovered := make(chan error, 1)
	go func() { recovered <- rec.Run(recovering) }()
	f.awaitReplays(1)
	stopRecovery()
	f.wait(recovered)
	stopDispatch()
	f.wait(dispatched)
	for user, want := range map[string]string{"user-b": "p2", "user-c": "p3"} {
		if got := postIDs(f.value(prefix + "dm:" + user)); !slices.Equal(got, []string{want}) {
			t.Errorf("buffer of %s holds %q, want %s once", user, got, want)
		}
	}
	if n := len(f.flusher.received()); n != 2 {
		t.Errorf("%d notices, want one per message", n)
	}
}
