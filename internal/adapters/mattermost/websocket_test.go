package mattermost_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/testkit"
	"github.com/sarpaydemir/melinda/internal/testkit/fakeclock"
	"github.com/sarpaydemir/melinda/internal/testkit/mattermostfake"
)

// session is a Listen call in progress.
type session struct {
	// calls receives "connected" and each delivered frame, in callback order.
	calls  chan string
	done   chan struct{}
	err    error // written before done is closed
	cancel context.CancelFunc
}

func listen(t *testing.T, p chat.Platform) *session {
	ctx, cancel := context.WithCancel(t.Context())
	s := &session{calls: make(chan string, 16), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(s.done)
		s.err = p.Listen(ctx, func() { s.calls <- "connected" }, func(_ context.Context, event json.RawMessage) error {
			s.calls <- string(event)
			return nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-s.done
	})
	return s
}

// expect fails t unless the next callbacks are want, in order.
func (s *session) expect(t *testing.T, want ...string) {
	t.Helper()
	for _, w := range want {
		select {
		case got := <-s.calls:
			if got != w {
				t.Fatalf("callback %.120q, want %.120q", got, w)
			}
		case <-s.done:
			t.Fatalf("Listen returned %v while waiting for %.120q", s.err, w)
		case <-time.After(testkit.AwaitTimeout):
			t.Fatalf("timed out waiting for %.120q", w)
		}
	}
}

func (s *session) result(t *testing.T) error {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(testkit.AwaitTimeout):
		t.Fatal("timed out waiting for Listen to return")
	}
	return s.err
}

func (s *session) returned() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func TestListenDeliversOnlyPostedFramesWhole(t *testing.T) {
	mm := newFake(t)
	var logs bytes.Buffer
	p := openWith(t, mm, "", token, clock.Real(), slog.New(slog.NewTextHandler(&logs, nil)))
	first := string(mm.Event(mm.AddPost(mattermostfake.Post{ChannelID: channel, UserID: ayse.ID, Message: "merhaba"})))
	otherReply := `{"event":"posted","seq_reply":3,"seq":3}`
	last := string(mm.Event(mm.AddPost(mattermostfake.Post{ChannelID: id("dm"), UserID: ayse.ID, Message: "görüşürüz"})))
	for _, f := range []string{
		`{"event":"hello","data":{"server_version":"10.5.0"},"broadcast":{"user_id":"` + bot.ID + `"},"seq":0}`,
		`{"event":"typing","data":{"parent_id":""},"broadcast":{"channel_id":"` + channel + `"},"seq":1}`,
		first,
		`{"status":"OK","seq_reply":2}`,
		`{"status":"FAIL","seq_reply":2}`,
		otherReply,
		`{"event":"post_edited","data":{},"seq":4}`,
		`{"event":"posted",`,
		`{"seq_reply":1.0,"status":"OK","event":"posted"}`,
		last,
	} {
		mm.Push([]byte(f))
	}

	s := listen(t, p)
	s.expect(t, "connected", first, otherReply, "connected", last)
	s.cancel()
	if err := s.result(t); err != nil {
		t.Fatalf("Listen after cancellation = %v", err)
	}
	mm.AwaitWebsocket(t, func() bool { return len(mm.Closes()) == 1 })
	if got := mm.Closes(); !slices.Equal(got, []websocket.StatusCode{websocket.StatusNormalClosure}) {
		t.Errorf("close statuses = %v, want a normal closure on cancellation", got)
	}
	if got := strings.Count(logs.String(), "level=WARN"); got != 1 || !strings.Contains(logs.String(), "bytes=18") {
		t.Errorf("logs:\n%s\nwant one warning for the 18-byte frame", logs.String())
	}
}

func TestListenEndsWhenAuthenticationFails(t *testing.T) {
	mm := newFake(t)
	const wrong = "expired-token-123"
	s := listen(t, openWith(t, mm, "", wrong, clock.Real(), discard))
	err := s.result(t)
	if err == nil || !strings.Contains(err.Error(), "FAIL") || strings.Contains(err.Error(), wrong) {
		t.Errorf("Listen = %v, want an error with the reply's status and without the token", err)
	}
	if len(s.calls) != 0 {
		t.Errorf("callbacks after a refused token: %d", len(s.calls))
	}
}

func TestListenEndsWhenAPingGoesUnanswered(t *testing.T) {
	mm := newFake(t)
	clk := fakeclock.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s := listen(t, openWith(t, mm, "", token, clk, discard))
	s.expect(t, "connected")
	mm.WithholdPongs()

	clk.AwaitTimers(t, 1)
	if d, _ := clk.Next(); d != 20*time.Second {
		t.Fatalf("first ping due in %v, want 20s", d)
	}
	clk.Advance(20 * time.Second)
	mm.AwaitWebsocket(t, func() bool { return mm.Pings() == 1 })
	if d, _ := clk.Next(); d != 20*time.Second || s.returned() {
		t.Fatalf("after the ping: pong deadline in %v, Listen returned %v; want 20s and a live session", d, s.returned())
	}
	clk.Advance(20 * time.Second)
	if err := s.result(t); err == nil {
		t.Error("Listen returned nil after a missing pong")
	}
	mm.AwaitWebsocket(t, func() bool { return len(mm.Closes()) == 1 })
}

func TestListenKeepsAnEventItReadThroughAPongTimeout(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		mm := newFake(t)
		clk := fakeclock.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		p := openWith(t, mm, "", token, clk, discard)
		mm.Push([]byte(`{"event":"posted","seq":1}`))
		mm.WithholdPongs()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		held, release, done := make(chan context.Context, 1), make(chan struct{}), make(chan error, 1)
		go func() {
			done <- p.Listen(ctx, func() {}, func(ctx context.Context, _ json.RawMessage) error {
				held <- ctx // intake is full until release
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}()
		var delivering context.Context
		select {
		case delivering = <-held:
		case <-time.After(testkit.AwaitTimeout):
			t.Fatal("timed out waiting for the delivery")
		}
		clk.AwaitTimers(t, 1)
		clk.Advance(20 * time.Second)
		mm.AwaitWebsocket(t, func() bool { return mm.Pings() == 1 })
		clk.Advance(20 * time.Second)
		mm.AwaitWebsocket(t, func() bool { return len(mm.Closes()) == 1 })
		if delivering.Err() != nil || len(done) != 0 {
			t.Fatalf("after the pong timeout: delivery context %v, Listen returned %v; want the delivery still waiting", delivering.Err(), len(done) != 0)
		}
		if shutdown {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Listen cancelled during a held delivery = %v, want nil", err)
			}
		} else {
			close(release)
			if err := <-done; err == nil {
				t.Error("Listen returned nil after the delivery went through on a dead connection")
			}
		}
	}
}

func TestListenAcceptsFramesUpToEightMebibytes(t *testing.T) {
	mm := newFake(t)
	frame := func(size int) string {
		const head, tail = `{"event":"posted","pad":"`, `"}`
		return head + strings.Repeat("x", size-len(head)-len(tail)) + tail
	}
	largest := frame(8 << 20)
	mm.Push([]byte(largest), []byte(frame(8<<20+1)))
	s := listen(t, open(t, mm, ""))
	s.expect(t, "connected", largest)
	if err := s.result(t); err == nil {
		t.Error("Listen returned nil after a frame over 8 MiB")
	}
}
