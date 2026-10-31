package mattermostfake_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/mattermostfake"
)

func dial(t *testing.T, mm *mattermostfake.Fake) (*websocket.Conn, error) {
	t.Helper()
	c, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(mm.URL(), "http")+"/api/v4/websocket", nil)
	if err == nil {
		t.Cleanup(func() { _ = c.CloseNow() })
	}
	return c, err
}

func authenticate(t *testing.T, mm *mattermostfake.Fake, tok string) *websocket.Conn {
	t.Helper()
	c, err := dial(t, mm)
	if err != nil {
		t.Fatal(err)
	}
	challenge := `{"seq":1,"action":"authentication_challenge","data":{"token":"` + tok + `"}}`
	if err := c.Write(t.Context(), websocket.MessageText, []byte(challenge)); err != nil {
		t.Fatal(err)
	}
	return c
}

func read(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testkit.AwaitTimeout)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("frame %s: %v", data, err)
	}
	return frame
}

func TestWebsocketAuthenticatesThenDeliversQueuedEvents(t *testing.T) {
	mm := mattermostfake.New(t, token, bot)
	mm.AddChannel(mattermostfake.Channel{ID: "dm", Type: "D"})
	post := mm.Posted(mattermostfake.Post{ChannelID: "dm", UserID: "u1", Message: "merhaba İlker"})
	mm.Push([]byte(`{"event":"typing","data":{},"seq":9}`))

	c := authenticate(t, mm, token)
	if reply := read(t, c); reply["status"] != "OK" || reply["seq_reply"] != 1.0 {
		t.Errorf("auth reply = %v, want status OK and seq_reply 1", reply)
	}
	posted := read(t, c)
	data, _ := posted["data"].(map[string]any)
	if posted["event"] != "posted" || data["channel_type"] != "D" {
		t.Errorf("posted frame = %v", posted)
	}
	raw, _ := data["post"].(string)
	var got mattermostfake.Post
	if err := json.Unmarshal([]byte(raw), &got); err != nil || got.ID != post.ID || got.Message != post.Message {
		t.Errorf("data.post = %q (%v), want the posted post %+v", raw, err, post)
	}
	if typing := read(t, c); typing["event"] != "typing" {
		t.Errorf("third frame = %v, want the pushed typing event", typing)
	}
	if got := mm.Posts("dm"); len(got) != 1 {
		t.Errorf("stored posts = %d, want 1", len(got))
	}
}

func TestEventReturnsThePostedFrameWithoutStoringOrPushing(t *testing.T) {
	mm := mattermostfake.New(t, token, bot)
	mm.AddChannel(mattermostfake.Channel{ID: "ops", Type: "P"})
	p := mattermostfake.Post{ID: "p1", ChannelID: "ops", UserID: "u1", RootID: "r1", Message: "selam", Type: "system_join_channel"}
	var frame struct {
		Event string `json:"event"`
		Data  struct {
			ChannelType string `json:"channel_type"`
			Post        string `json:"post"`
		} `json:"data"`
	}
	if err := json.Unmarshal(mm.Event(p), &frame); err != nil {
		t.Fatal(err)
	}
	var got mattermostfake.Post
	if err := json.Unmarshal([]byte(frame.Data.Post), &got); err != nil || frame.Event != "posted" || frame.Data.ChannelType != "P" ||
		!reflect.DeepEqual(got, p) {
		t.Errorf("frame = %+v, post %+v (%v); want a posted event in a P channel carrying %+v", frame, got, err, p)
	}
	if posts := mm.Posts("ops"); len(posts) != 0 {
		t.Errorf("stored posts = %+v, want none", posts)
	}
	c := authenticate(t, mm, token)
	read(t, c)
	mm.Push([]byte(`{"event":"typing","data":{},"seq":9}`))
	if next := read(t, c); next["event"] != "typing" {
		t.Errorf("first frame after authentication = %v, want only the pushed typing event", next)
	}
}

func TestWebsocketRejectsWrongToken(t *testing.T) {
	mm := mattermostfake.New(t, token, bot)
	c := authenticate(t, mm, "wrong")
	if reply := read(t, c); reply["status"] != "FAIL" || reply["seq_reply"] != 1.0 {
		t.Errorf("auth reply = %v, want status FAIL and seq_reply 1", reply)
	}
}

func TestWebsocketRecordsPingsAndSurvivesDrops(t *testing.T) {
	mm := mattermostfake.New(t, token, bot)
	mm.On("GET", "/api/v4/websocket").Reply(testkit.Status(503))
	if _, err := dial(t, mm); err == nil {
		t.Fatal("scripted 503: dial succeeded")
	}

	first := authenticate(t, mm, token)
	read(t, first)
	pinging := first.CloseRead(t.Context())
	if err := first.Ping(pinging); err != nil {
		t.Fatal(err)
	}
	if n := mm.Pings(); n != 1 {
		t.Errorf("pings = %d, want 1", n)
	}

	mm.Drop()
	<-pinging.Done() // the client notices the drop
	mm.Push([]byte(`{"event":"after_drop","seq":1}`))
	second := authenticate(t, mm, token)
	read(t, second)
	if frame := read(t, second); frame["event"] != "after_drop" {
		t.Errorf("frame after reconnect = %v", frame)
	}
	mm.AwaitWebsocket(t, func() bool { return mm.Connections() == 2 })
	mm.AssertCalls(t, "GET /api/v4/websocket", "GET /api/v4/websocket", "GET /api/v4/websocket")
}
