package mattermostfake

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

const wsPath = "/api/v4/websocket"

// wsState is guarded by Fake.mu.
type wsState struct {
	outbox      [][]byte
	seq         int
	conns       map[*websocket.Conn]struct{}
	connections int
	pings       int
	noPongs     bool
	closes      []websocket.StatusCode
}

// Push queues websocket frames. They are delivered in order to authenticated connections; a frame stays
// queued until one is connected.
func (f *Fake) Push(frames ...[]byte) {
	f.update(func() { f.ws.outbox = append(f.ws.outbox, frames...) })
}

// Posted adds p as AddPost does and pushes the posted event Mattermost sends for it.
func (f *Fake) Posted(p Post) Post {
	f.t.Helper()
	p = f.AddPost(p)
	f.Push(f.Event(p))
	return p
}

// Event returns the posted event Mattermost sends for p, which must be in a channel added to the fake, without
// storing or pushing anything.
func (f *Fake) Event(p Post) []byte {
	f.t.Helper()
	c, ok := f.channel(p.ChannelID)
	if !ok {
		f.t.Fatalf("mattermostfake: post to unknown channel %q", p.ChannelID)
	}
	post, err := json.Marshal(p)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.ws.seq++
	frame, err := json.Marshal(map[string]any{
		"event":     "posted",
		"data":      map[string]string{"channel_type": c.Type, "post": string(post)},
		"broadcast": map[string]string{"channel_id": c.ID},
		"seq":       f.ws.seq,
	})
	f.mu.Unlock()
	if err != nil {
		f.t.Fatal(err)
	}
	return frame
}

// Connections returns the number of websocket connections accepted so far.
func (f *Fake) Connections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ws.connections
}

// Pings returns the number of websocket pings received so far.
func (f *Fake) Pings() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ws.pings
}

// WithholdPongs makes the fake stop answering pings; they are still counted.
func (f *Fake) WithholdPongs() {
	f.update(func() { f.ws.noPongs = true })
}

// Closes returns, for each websocket connection that has ended, the close status the client sent, or -1 when it
// sent none.
func (f *Fake) Closes() []websocket.StatusCode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ws.closes)
}

// AwaitWebsocket blocks until cond holds, checking it after every websocket connection, ping, and push.
func (f *Fake) AwaitWebsocket(t testing.TB, cond func() bool) {
	t.Helper()
	f.changed.Await(t, "websocket condition", cond)
}

// Drop closes every websocket connection without a close handshake, as a network failure would.
func (f *Fake) Drop() {
	f.mu.Lock()
	var conns []*websocket.Conn
	for c := range f.ws.conns {
		conns = append(conns, c)
	}
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.CloseNow()
	}
}

func (f *Fake) update(change func()) {
	f.mu.Lock()
	change()
	f.mu.Unlock()
	f.changed.Notify()
}

func (f *Fake) serveWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		OnPingReceived: func(context.Context, []byte) bool {
			answer := false
			f.update(func() {
				f.ws.pings++
				answer = !f.ws.noPongs
			})
			return answer
		},
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(f.Context())
	f.update(func() {
		f.ws.conns[c] = struct{}{}
		f.ws.connections++
	})
	var delivering sync.WaitGroup
	authenticated := false
	var status websocket.StatusCode
	for {
		_, frame, err := c.Read(ctx)
		if err != nil {
			status = websocket.CloseStatus(err)
			break
		}
		if !authenticated && f.authenticate(ctx, c, frame) {
			authenticated = true
			delivering.Go(func() { f.deliver(ctx, c) })
		}
	}
	cancel()
	delivering.Wait()
	_ = c.CloseNow()
	f.update(func() {
		delete(f.ws.conns, c)
		f.ws.closes = append(f.ws.closes, status)
	})
}

// authenticate answers an authentication challenge and reports whether its token was accepted.
func (f *Fake) authenticate(ctx context.Context, c *websocket.Conn, frame []byte) bool {
	var msg struct {
		Seq    int64  `json:"seq"`
		Action string `json:"action"`
		Data   struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if json.Unmarshal(frame, &msg) != nil || msg.Action != "authentication_challenge" {
		return false
	}
	status := "OK"
	if msg.Data.Token != f.token {
		status = "FAIL"
	}
	reply, _ := json.Marshal(map[string]any{"status": status, "seq_reply": msg.Seq})
	return c.Write(ctx, websocket.MessageText, reply) == nil && status == "OK"
}

// deliver writes queued frames to c until ctx ends; a frame that fails to write goes back to the queue.
func (f *Fake) deliver(ctx context.Context, c *websocket.Conn) {
	for {
		changed := f.changed.C()
		var frame []byte
		f.mu.Lock()
		if len(f.ws.outbox) > 0 {
			frame, f.ws.outbox = f.ws.outbox[0], f.ws.outbox[1:]
		}
		f.mu.Unlock()
		if frame == nil {
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return
			}
		}
		if err := c.Write(ctx, websocket.MessageText, frame); err != nil {
			f.update(func() { f.ws.outbox = append([][]byte{frame}, f.ws.outbox...) })
			return
		}
	}
}
