package mattermost_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/adapters/mattermost"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/port/chat"
)

// decoder returns the platform for decoding only; it never reaches a server.
func decoder(t testing.TB) chat.Platform {
	t.Helper()
	p, err := mattermost.Open(settings(t, `{"model":"m","mattermost_endpoint":"https://chat.example","mattermost_token":"t","mattermost_bot_id":"b"}`), http.DefaultTransport, clock.Real(), discard)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// event returns a frame whose data holds post, JSON-encoded as a string, and the other data fields.
func event(t testing.TB, outer map[string]any, post any, data map[string]any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(post)
	if err != nil {
		t.Fatal(err)
	}
	d := maps.Clone(data)
	if d == nil {
		d = map[string]any{}
	}
	d["post"] = string(encoded)
	f := maps.Clone(outer)
	if f == nil {
		f = map[string]any{}
	}
	f["data"] = d
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// decode decodes ev, requires a message, and checks that decoding its payload yields the same message.
func decode(t *testing.T, p chat.Platform, ev json.RawMessage) chat.Message {
	t.Helper()
	m, ok := p.Decode(ev)
	if !ok {
		t.Fatalf("Decode(%s) reported no message", ev)
	}
	if again, ok := p.Decode(m.Payload); !ok || !reflect.DeepEqual(again, m) {
		t.Errorf("Decode(Payload) = %+v, %v\nwant %+v", again, ok, m)
	}
	return m
}

// payload splits a payload into its frame fields, its data fields, and its post fields.
func payload(t *testing.T, raw json.RawMessage) (frame, data, post map[string]any) {
	t.Helper()
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("payload %s: %v", raw, err)
	}
	data, _ = frame["data"].(map[string]any)
	s, _ := data["post"].(string)
	if err := json.Unmarshal([]byte(s), &post); err != nil {
		t.Fatalf("payload post %q: %v", s, err)
	}
	return frame, data, post
}

func TestDecodeReadsAPostedEvent(t *testing.T) {
	post := map[string]any{
		"id": "p1", "create_at": 1767225600123, "update_at": 1767225600123, "edit_at": 0, "delete_at": 0,
		"is_pinned": false, "user_id": "u1", "channel_id": "c1", "root_id": "", "original_id": "", "message": "@helper rapor <hazır> mı?",
		"type": "", "props": map[string]any{"disable_group_highlight": true}, "hashtags": "", "file_ids": []string{"f1", "f2"},
		"pending_post_id": "u1:1767225600000", "reply_count": 0, "metadata": map[string]any{},
	}
	outer := map[string]any{"event": "posted", "seq": 7, "broadcast": map[string]any{"omit_users": nil, "channel_id": "c1", "team_id": ""}}
	data := map[string]any{"channel_display_name": "Town", "channel_name": "town", "channel_type": "O", "mentions": `["b"]`,
		"sender_name": "@ayse", "set_online": true, "team_id": "t1"}

	m := decode(t, decoder(t), event(t, outer, post, data))
	want := chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "@helper rapor <hazır> mı?",
		AttachmentIDs: []string{"f1", "f2"}, Created: time.UnixMilli(1767225600123), Kind: chat.Public}
	got := m
	got.Payload = nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Decode = %+v\nwant %+v", got, want)
	}

	frame, gotData, gotPost := payload(t, m.Payload)
	wantFrame := map[string]any{"event": "posted", "seq": 7.0, "broadcast": map[string]any{"omit_users": nil, "channel_id": "c1", "team_id": ""}}
	delete(frame, "data")
	if !reflect.DeepEqual(frame, wantFrame) {
		t.Errorf("payload frame fields = %v, want %v", frame, wantFrame)
	}
	delete(gotData, "post")
	if !reflect.DeepEqual(gotData, data) {
		t.Errorf("payload data fields = %v, want %v", gotData, data)
	}
	var wantPost map[string]any
	b, _ := json.Marshal(post)
	_ = json.Unmarshal(b, &wantPost)
	maps.Copy(wantPost, map[string]any{"channel_type": "O", "post_id": "p1", "is_dm": false, "is_thread": false,
		"scope": "public", "root_id": "", "context_id": "p1", "reply_root_id": "p1"})
	if !reflect.DeepEqual(gotPost, wantPost) {
		t.Errorf("payload post = %v\nwant %v", gotPost, wantPost)
	}
}

func TestDecodeConversationKindsAndRoutingFields(t *testing.T) {
	type fields struct {
		channelType          string
		isDM, isThread       bool
		scope, root          string
		context, replyThread string
	}
	tests := []struct {
		name                string
		frameType, postType string
		root                any
		kind                chat.Kind
		thread              string
		want                fields
	}{
		{"public root", "O", "", "", chat.Public, "", fields{"O", false, false, "public", "", "p1", "p1"}},
		{"public thread", "O", "", "r1", chat.Public, "r1", fields{"O", false, true, "public", "r1", "r1", "r1"}},
		{"private channel", "P", "", "", chat.Private, "", fields{"P", false, false, "public", "", "p1", "p1"}},
		{"private thread", "P", "", "r1", chat.Private, "r1", fields{"P", false, true, "public", "r1", "r1", "r1"}},
		{"group conversation", "G", "", "", chat.Private, "", fields{"G", false, false, "public", "", "p1", "p1"}},
		{"group thread", "G", "", "r1", chat.Private, "r1", fields{"G", false, true, "public", "r1", "r1", "r1"}},
		{"direct root", "D", "", "", chat.Direct, "", fields{"D", true, false, "private", "", "p1", ""}},
		{"direct thread", "D", "", "r1", chat.Direct, "r1", fields{"D", true, true, "private", "r1", "r1", ""}},
		{"null root", "O", "", nil, chat.Public, "", fields{"O", false, false, "public", "", "p1", "p1"}},
		{"type from the post", "", "D", "", chat.Direct, "", fields{"D", true, false, "private", "", "p1", ""}},
		{"frame type wins over the post's", "O", "D", "", chat.Public, "", fields{"O", false, false, "public", "", "p1", "p1"}},
		{"no type", "", "", "", chat.Public, "", fields{"", false, false, "public", "", "p1", "p1"}},
		{"unknown type", "X", "", "", chat.Public, "", fields{"X", false, false, "public", "", "p1", "p1"}},
	}
	p := decoder(t)
	for _, tt := range tests {
		post := map[string]any{"id": "p1", "user_id": "u1", "channel_id": "c1", "root_id": tt.root, "message": "hi"}
		if tt.postType != "" {
			post["channel_type"] = tt.postType
		}
		data := map[string]any{}
		if tt.frameType != "" {
			data["channel_type"] = tt.frameType
		}
		m := decode(t, p, event(t, nil, post, data))
		if m.Kind != tt.kind || m.ThreadID != tt.thread {
			t.Errorf("%s: kind %v, thread %q; want %v, %q", tt.name, m.Kind, m.ThreadID, tt.kind, tt.thread)
		}
		_, _, got := payload(t, m.Payload)
		want := map[string]any{"id": "p1", "user_id": "u1", "channel_id": "c1", "message": "hi",
			"channel_type": tt.want.channelType, "post_id": "p1", "is_dm": tt.want.isDM, "is_thread": tt.want.isThread,
			"scope": tt.want.scope, "root_id": tt.want.root, "context_id": tt.want.context, "reply_root_id": tt.want.replyThread}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: payload post = %v\nwant %v", tt.name, got, want)
		}
	}
}

func TestDecodeMarksDeletedAndTypedPostsAsSystem(t *testing.T) {
	tests := []struct {
		name   string
		extra  map[string]any
		system bool
	}{
		{"person's post", map[string]any{"delete_at": 0, "type": ""}, false},
		{"absent fields", map[string]any{}, false},
		{"null fields", map[string]any{"delete_at": nil, "type": nil}, false},
		{"negative delete time", map[string]any{"delete_at": -1}, false},
		{"deleted post", map[string]any{"delete_at": 1767225600999}, true},
		{"join notice", map[string]any{"type": "system_join_channel"}, true},
		{"integration post type", map[string]any{"type": "custom_poll"}, true},
	}
	p := decoder(t)
	for _, tt := range tests {
		post := map[string]any{"id": "p1", "user_id": "u1", "channel_id": "c1", "message": "hi"}
		maps.Copy(post, tt.extra)
		if m := decode(t, p, event(t, nil, post, map[string]any{"channel_type": "D"})); m.System != tt.system {
			t.Errorf("%s: System = %v, want %v", tt.name, m.System, tt.system)
		}
	}
}

func TestDecodeCreationTime(t *testing.T) {
	tests := []struct {
		name string
		at   any
		want time.Time
	}{
		{"absent", nil, time.Time{}},
		{"zero", 0, time.Time{}},
		{"milliseconds", 1767225600123, time.UnixMilli(1767225600123)},
	}
	p := decoder(t)
	for _, tt := range tests {
		post := map[string]any{"id": "p1"}
		if tt.at != nil {
			post["create_at"] = tt.at
		}
		if m := decode(t, p, event(t, nil, post, nil)); !m.Created.Equal(tt.want) || m.Created.IsZero() != tt.want.IsZero() {
			t.Errorf("%s: Created = %v, want %v", tt.name, m.Created, tt.want)
		}
	}
}

func TestDecodeReadsAStoredPayload(t *testing.T) {
	stored := `{"event": "posted", "seq": 3, "data": {"channel_type": "O", "team_id": "t1", "post": ` +
		`"{\"id\": \"p2\", \"create_at\": 1767225600500, \"user_id\": \"u1\", \"channel_id\": \"c1\", \"root_id\": \"r1\", ` +
		`\"message\": \"@helper \\u015funu yap\", \"type\": \"\", \"file_ids\": [\"f1\"], \"post_id\": \"p2\", ` +
		`\"channel_type\": \"O\", \"is_dm\": false, \"is_thread\": true, \"scope\": \"public\", \"reply_root_id\": \"r1\", ` +
		`\"context_id\": \"r1\"}"}}`
	replayed := `{"data": {"post": "{\"id\": \"p3\", \"create_at\": 1767225600600, \"user_id\": \"u2\", \"channel_id\": \"d1\", ` +
		`\"root_id\": \"\", \"message\": \"selam\", \"props\": {}}", "channel_type": "D"}}`
	p := decoder(t)
	tests := []struct {
		raw  string
		want chat.Message
	}{
		{stored, chat.Message{ID: "p2", ConversationID: "c1", ThreadID: "r1", AuthorID: "u1", Text: "@helper şunu yap",
			AttachmentIDs: []string{"f1"}, Created: time.UnixMilli(1767225600500), Kind: chat.Public}},
		{replayed, chat.Message{ID: "p3", ConversationID: "d1", AuthorID: "u2", Text: "selam",
			Created: time.UnixMilli(1767225600600), Kind: chat.Direct}},
	}
	for _, tt := range tests {
		m := decode(t, p, json.RawMessage(tt.raw))
		m.Payload = nil
		if !reflect.DeepEqual(m, tt.want) {
			t.Errorf("Decode(%s) = %+v\nwant %+v", tt.raw, m, tt.want)
		}
	}
}

func TestDecodeReportsNoMessageWithoutAPostObject(t *testing.T) {
	events := []string{
		`{"data":null}`,
		`{"data":[]}`,
		`{"data":"post"}`,
		`{"data":{}}`,
		`{"data":{"post":null}}`,
		`{"data":{"post":""}}`,
		`{"data":{"post":5}}`,
		`{"data":{"post":true}}`,
		`{"data":{"post":{"id":"p1","message":"hi"}}}`,
		`{"data":{"post":["{}"]}}`,
		`{"data":{"post":"not json"}}`,
		`{"data":{"post":"null"}}`,
		`{"data":{"post":"[]"}}`,
		`{"data":{"post":"\"text\""}}`,
		`{"data":{"post":"5"}}`,
		`{"data":{"post":"{\"id\":\"p1\"} trailing"}}`,
		`{"data":{"post":"{\"id\":\"p1\",\"message\":5}"}}`,
		`{"data":{"post":"{\"id\":\"p1\",\"create_at\":\"soon\"}"}}`,
		`{"data":{"post":"{\"id\":\"p1\",\"file_ids\":\"f1\"}"}}`,
	}
	p := decoder(t)
	for _, ev := range events {
		if m, ok := p.Decode(json.RawMessage(ev)); ok {
			t.Errorf("Decode(%s) = %+v, want no message", ev, m)
		}
	}
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte(`{"event":"posted","data":{"channel_type":"D","post":"{\"id\":\"p1\",\"root_id\":\"r\",\"create_at\":5}"},"seq":1}`))
	f.Add([]byte(`{"data":{"post":"{\"id\":\"p1\",\"type\":\"system_join_channel\",\"channel_type\":\"G\"}"}}`))
	f.Add([]byte(`{"data":{"post":"{}","channel_type":"P"},"data":{"post":"{\"delete_at\":1}"}}`))
	f.Add([]byte(`{"data":{"post":"{\"message\":\"\\ud800 <&>\",\"file_ids\":null}"}}`))
	p := decoder(f)
	f.Fuzz(func(t *testing.T, ev []byte) {
		m, ok := p.Decode(ev)
		if !ok {
			return
		}
		again, ok := p.Decode(m.Payload)
		if !ok || !reflect.DeepEqual(again, m) {
			t.Errorf("Decode(%s) = %+v\nDecode(Payload) = %+v, %v", ev, m, again, ok)
		}
	})
}
