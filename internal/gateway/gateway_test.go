package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/gateway"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/fakeclock"
)

// session is an intake session the test drives: it connects when the test calls connected and ends with the
// error the test sends on end, or with nil once its context is cancelled.
type session struct {
	ctx       context.Context
	settings  *config.Settings
	connected func()
	deliver   chat.Deliver
	end       chan error
}

// platform hands each Listen call to the test as a session.
type platform struct {
	chat.Platform
	settings *config.Settings
	sessions chan<- *session
}

func (p *platform) Listen(ctx context.Context, connected func(), deliver chat.Deliver) error {
	s := &session{ctx: ctx, settings: p.settings, connected: connected, deliver: deliver, end: make(chan error)}
	select {
	case p.sessions <- s:
	case <-ctx.Done():
		return nil
	}
	select {
	case err := <-s.end:
		return err
	case <-ctx.Done():
		return nil
	}
}

// snapshots returns a new settings snapshot on every call and remembers them in order.
type snapshots struct {
	mu    sync.Mutex
	taken []*config.Settings
}

func (s *snapshots) Current(context.Context) *config.Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.taken = append(s.taken, &config.Settings{})
	return s.taken[len(s.taken)-1]
}

type fixture struct {
	gw         *gateway.Gateway
	clk        *fakeclock.Clock
	settings   *snapshots
	sessions   chan *session
	delivered  chan json.RawMessage
	recoveries atomic.Int32
	offline    atomic.Int32
	logs       bytes.Buffer // read only after Run has returned
	cancel     context.CancelFunc
	done       chan error
}

func start(t *testing.T) *fixture {
	f := &fixture{
		clk:       fakeclock.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		settings:  &snapshots{},
		sessions:  make(chan *session),
		delivered: make(chan json.RawMessage, 1),
		done:      make(chan error, 1),
	}
	open := func(s *config.Settings) (chat.Platform, error) {
		return &platform{settings: s, sessions: f.sessions}, nil
	}
	deliver := func(_ context.Context, event json.RawMessage) error {
		f.delivered <- event
		return nil
	}
	f.gw = gateway.New(f.settings, open, deliver, func() { f.recoveries.Add(1) }, func() { f.offline.Add(1) }, f.clk,
		slog.New(slog.NewJSONHandler(&f.logs, nil)))
	ctx, cancel := context.WithCancel(t.Context())
	f.cancel = cancel
	go func() { f.done <- f.gw.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-f.done
	})
	return f
}

// next returns the session the gateway starts next.
func (f *fixture) next(t *testing.T) *session {
	t.Helper()
	select {
	case s := <-f.sessions:
		return s
	case <-time.After(testkit.AwaitTimeout):
		t.Fatal("timed out waiting for the gateway to start a session")
	}
	return nil
}

// waits fails t unless the gateway waits d before starting its next session, then lets that time pass.
func (f *fixture) waits(t *testing.T, d time.Duration) {
	t.Helper()
	f.clk.AwaitTimers(t, 1)
	if got, _ := f.clk.Next(); got != d {
		t.Fatalf("the gateway waits %v, want %v", got, d)
	}
	f.clk.Advance(d)
}

func (f *fixture) stop(t *testing.T) error {
	t.Helper()
	f.cancel()
	select {
	case err := <-f.done:
		f.done <- err
		return err
	case <-time.After(testkit.AwaitTimeout):
		t.Fatal("timed out waiting for Run to return")
	}
	return nil
}

var errRefused = errors.New("connection refused")

func TestReconnectDelaysDoubleUpToAMinute(t *testing.T) {
	f := start(t)
	for _, d := range []time.Duration{1, 2, 4, 8, 16, 32, 60, 60} {
		f.next(t).end <- errRefused
		f.waits(t, d*time.Second)
	}
}

func TestReconnectDelayResetsAfterThirtySecondsConnected(t *testing.T) {
	f := start(t)
	f.next(t).end <- errRefused
	f.waits(t, time.Second)

	short := f.next(t)
	short.connected()
	f.clk.Advance(30*time.Second - time.Nanosecond)
	short.end <- errRefused
	f.waits(t, 2*time.Second)

	healthy := f.next(t)
	healthy.connected()
	f.clk.Advance(30 * time.Second)
	healthy.end <- errRefused
	f.waits(t, time.Second)

	f.next(t).end <- errRefused
	f.waits(t, 2*time.Second)
}

func TestEachSessionOpensThePlatformFromCurrentSettingsAndFeedsIntake(t *testing.T) {
	f := start(t)
	first := f.next(t)
	first.end <- errRefused
	f.waits(t, time.Second)
	second := f.next(t)
	f.settings.mu.Lock()
	taken := f.settings.taken
	f.settings.mu.Unlock()
	if len(taken) != 2 || first.settings != taken[0] || second.settings != taken[1] {
		t.Errorf("sessions opened from %p and %p, want the snapshots taken for them: %v", first.settings, second.settings, taken)
	}
	event := json.RawMessage(`{"event":"posted"}`)
	if err := second.deliver(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if got := <-f.delivered; !bytes.Equal(got, event) {
		t.Errorf("intake received %s, want %s", got, event)
	}
}

func TestReadyAndRecoveryFollowEachConnectionAndOfflineIsReportedBeforeEachSession(t *testing.T) {
	f := start(t)
	s := f.next(t)
	if f.gw.Ready() == nil || f.offline.Load() != 1 {
		t.Errorf("before connecting: ready %v, offline reports = %d; want not ready and one for startup", f.gw.Ready() == nil, f.offline.Load())
	}
	s.connected()
	if err := f.gw.Ready(); err != nil || f.recoveries.Load() != 1 {
		t.Errorf("after connecting: Ready() = %v, recoveries = %d; want ready and one recovery", err, f.recoveries.Load())
	}
	s.end <- errRefused
	f.clk.AwaitTimers(t, 1)
	if f.gw.Ready() == nil || f.offline.Load() != 2 {
		t.Errorf("after the session ended: ready %v, offline reports = %d; want not ready and one more", f.gw.Ready() == nil, f.offline.Load())
	}
	f.clk.Advance(time.Second)
	f.next(t).end <- errRefused
	f.waits(t, 2*time.Second)
	f.next(t).connected()
	if err := f.gw.Ready(); err != nil || f.recoveries.Load() != 2 || f.offline.Load() != 2 {
		t.Errorf("after reconnecting: Ready() = %v, recoveries = %d, offline reports = %d; want ready, two recoveries, and none for the session that never connected",
			err, f.recoveries.Load(), f.offline.Load())
	}
	if err := f.stop(t); err != nil || f.offline.Load() != 2 {
		t.Errorf("after shutdown: Run = %v, offline reports = %d; want nil and none for the session shutdown ended", err, f.offline.Load())
	}
}

func TestShutdownEndsTheSessionOrTheWait(t *testing.T) {
	connected := start(t)
	s := connected.next(t)
	s.connected()
	if err := connected.stop(t); err != nil || s.ctx.Err() == nil || connected.gw.Ready() == nil {
		t.Errorf("stopping a session: Run = %v, session context %v, ready %v; want nil, cancelled, not ready", err, s.ctx.Err(), connected.gw.Ready() == nil)
	}

	waiting := start(t)
	waiting.next(t).end <- errRefused
	waiting.clk.AwaitTimers(t, 1)
	if err := waiting.stop(t); err != nil {
		t.Errorf("stopping during the reconnect delay: Run = %v, want nil", err)
	}
}

func TestConnectionsAndDisconnectsAreLogged(t *testing.T) {
	f := start(t)
	f.next(t).end <- errRefused
	f.waits(t, time.Second)
	s := f.next(t)
	s.connected()
	s.end <- errors.New("EOF")
	f.clk.AwaitTimers(t, 1)
	if err := f.stop(t); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	for line := range bytes.Lines(f.logs.Bytes()) {
		var r map[string]any
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		delete(r, "time")
		got = append(got, r)
	}
	want := []map[string]any{
		{"level": "INFO", "msg": "connecting"},
		{"level": "WARN", "msg": "disconnected", "err": "connection refused", "retry_in": float64(time.Second)},
		{"level": "INFO", "msg": "connecting"},
		{"level": "INFO", "msg": "connected"},
		{"level": "WARN", "msg": "disconnected", "err": "EOF", "retry_in": float64(2 * time.Second)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("logs:\n%v\nwant:\n%v", got, want)
	}
}
