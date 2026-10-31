// Package conformance holds the behavior every adapter of a port must show. An adapter's tests run its port's
// suite with a harness that wires the adapter to a fake.
package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/port/chat"
)

// ChatHarness is a chat platform adapter wired to a fake platform that has the agent's account, one other
// account, and one conversation the agent can post in.
type ChatHarness struct {
	Platform chat.Platform
	// Agent is the agent's account as the fake reports it.
	Agent chat.User
	// User is another account as the fake reports it.
	User         chat.User
	Conversation string
	// Sent returns the messages the fake holds in the conversation, oldest first.
	Sent func() []chat.Outgoing
	// Requests returns how many requests the fake has received.
	Requests func() int
	// Fail makes the fake fail every later request.
	Fail func()
	// Stall makes the fake hold every later request until its caller gives up.
	Stall func()
	// Event has User write text in a conversation of kind, in the thread rooted at thread or at the conversation
	// root when thread is "", and returns the event the platform delivers for it. Each message is created after
	// the ones before it.
	Event func(kind chat.Kind, thread, text string) json.RawMessage
	// SystemEvents are events for messages that are not a person's, such as a deleted message or a join notice.
	SystemEvents []json.RawMessage
	// NonMessages are events of the platform that carry no message.
	NonMessages []json.RawMessage
}

// prompt is how long a call may take to return once its context ends.
const prompt = 5 * time.Second

// Chat checks a chat platform adapter's decoding, lookups, and writes, each against a fresh harness from
// newHarness.
func Chat(t *testing.T, newHarness func(*testing.T) ChatHarness) {
	image := chat.File{Name: "chart.png", MediaType: "image/png", Data: []byte("\x89PNG\r\n\x1a\nchart")}

	t.Run("DecodeReadsEachConversationKind", func(t *testing.T) {
		h := newHarness(t)
		for _, kind := range []chat.Kind{chat.Public, chat.Private, chat.Direct} {
			root := decode(t, h.Platform, h.Event(kind, "", "status?"))
			reply := decode(t, h.Platform, h.Event(kind, root.ID, "all green"))
			for _, c := range []struct {
				m            chat.Message
				thread, text string
			}{{root, "", "status?"}, {reply, root.ID, "all green"}} {
				if m := c.m; m.ID == "" || m.ConversationID == "" || m.ConversationID != root.ConversationID ||
					m.ThreadID != c.thread || m.AuthorID != h.User.ID || m.Text != c.text || m.Kind != kind || m.System ||
					m.Created.IsZero() {
					t.Errorf("kind %v: Decode = %+v; want a message by %q in thread %q with text %q", kind, m, h.User.ID, c.thread, c.text)
				}
			}
			if reply.ID == root.ID {
				t.Errorf("kind %v: the reply has its root's ID %q", kind, root.ID)
			}
		}
		if n := h.Requests(); n != 0 {
			t.Errorf("Decode made %d requests", n)
		}
	})

	t.Run("DecodeMarksSystemMessages", func(t *testing.T) {
		h := newHarness(t)
		if len(h.SystemEvents) == 0 {
			t.Fatal("the harness has no system events")
		}
		for _, ev := range h.SystemEvents {
			if m := decode(t, h.Platform, ev); !m.System {
				t.Errorf("Decode(%s) = %+v, want a system message", ev, m)
			}
		}
	})

	t.Run("DecodeReportsNoMessageForOtherEvents", func(t *testing.T) {
		h := newHarness(t)
		events := []json.RawMessage{nil, []byte("{"), []byte("null"), []byte("[]"), []byte(`"text"`), []byte("{}")}
		for _, ev := range append(events, h.NonMessages...) {
			if m, ok := h.Platform.Decode(ev); ok {
				t.Errorf("Decode(%s) = %+v, want no message", ev, m)
			}
		}
	})

	t.Run("IdentityIsTheAgentWithoutIO", func(t *testing.T) {
		h := newHarness(t)
		id := h.Platform.Identity()
		if id.ID != h.Agent.ID || (id.Username != "" && id.Username != h.Agent.Username) {
			t.Errorf("Identity() = %+v, want ID %q and username empty or %q", id, h.Agent.ID, h.Agent.Username)
		}
		if n := h.Requests(); n != 0 {
			t.Errorf("Identity made %d requests", n)
		}
	})

	t.Run("SelfIsTheAgent", func(t *testing.T) {
		h := newHarness(t)
		if got, err := h.Platform.Self(t.Context()); err != nil || got != h.Agent {
			t.Errorf("Self() = %+v, %v; want %+v", got, err, h.Agent)
		}
	})

	t.Run("UserLooksUpAnAccount", func(t *testing.T) {
		h := newHarness(t)
		if got, err := h.Platform.User(t.Context(), h.User.ID); err != nil || got != h.User {
			t.Errorf("User(%q) = %+v, %v; want %+v", h.User.ID, got, err, h.User)
		}
	})

	t.Run("SendAtTheRootAndInAThread", func(t *testing.T) {
		h := newHarness(t)
		root, err := h.Platform.Send(t.Context(), chat.Outgoing{ConversationID: h.Conversation, Text: "status?"})
		if err != nil || root == "" {
			t.Fatalf("Send at the root = %q, %v", root, err)
		}
		reply, err := h.Platform.Send(t.Context(), chat.Outgoing{ConversationID: h.Conversation, ThreadID: root, Text: "all green"})
		if err != nil || reply == "" || reply == root {
			t.Fatalf("Send in the thread = %q, %v", reply, err)
		}
		want := []chat.Outgoing{
			{ConversationID: h.Conversation, Text: "status?"},
			{ConversationID: h.Conversation, ThreadID: root, Text: "all green"},
		}
		if got := h.Sent(); !reflect.DeepEqual(got, want) {
			t.Errorf("sent %+v, want %+v", got, want)
		}
	})

	t.Run("UploadThenSendWithTheFile", func(t *testing.T) {
		h := newHarness(t)
		fileID, err := h.Platform.Upload(t.Context(), h.Conversation, image)
		if err != nil || fileID == "" {
			t.Fatalf("Upload = %q, %v", fileID, err)
		}
		if _, err := h.Platform.Send(t.Context(), chat.Outgoing{ConversationID: h.Conversation, Text: "chart", FileIDs: []string{fileID}}); err != nil {
			t.Fatal(err)
		}
		want := []chat.Outgoing{{ConversationID: h.Conversation, Text: "chart", FileIDs: []string{fileID}}}
		if got := h.Sent(); !reflect.DeepEqual(got, want) {
			t.Errorf("sent %+v, want %+v", got, want)
		}
	})

	t.Run("AttachmentAndDownloadReadAnUploadedFile", func(t *testing.T) {
		h := newHarness(t)
		fileID, err := h.Platform.Upload(t.Context(), h.Conversation, image)
		if err != nil {
			t.Fatal(err)
		}
		a, err := h.Platform.Attachment(t.Context(), fileID)
		if err != nil || a.Name != image.Name || a.MediaType != image.MediaType || a.Size != int64(len(image.Data)) {
			t.Errorf("Attachment = %+v, %v; want name %q, media type %q, size %d", a, err, image.Name, image.MediaType, len(image.Data))
		}
		size := int64(len(image.Data))
		if data, err := h.Platform.Download(t.Context(), fileID, size); err != nil || !bytes.Equal(data, image.Data) {
			t.Errorf("Download at the limit = %q, %v; want %q", data, err, image.Data)
		}
		if data, err := h.Platform.Download(t.Context(), fileID, size-1); !errors.Is(err, chat.ErrTooLarge) || data != nil {
			t.Errorf("Download past the limit = %q, %v; want ErrTooLarge", data, err)
		}
	})

	t.Run("ReplayDeliversEachLaterMessageOnceOldestFirst", func(t *testing.T) {
		h := newHarness(t)
		seen := decode(t, h.Platform, h.Event(chat.Public, "", "seen"))
		var want []chat.Message
		for _, kind := range []chat.Kind{chat.Public, chat.Direct, chat.Private} {
			root := decode(t, h.Platform, h.Event(kind, "", "missed"))
			want = append(want, root, decode(t, h.Platform, h.Event(kind, root.ID, "missed reply")))
		}
		want = append(want, decode(t, h.Platform, h.Event(chat.Public, seen.ID, "late reply")))
		var got []chat.Message
		err := h.Platform.Replay(t.Context(), seen.Created, func(_ context.Context, ev json.RawMessage) error {
			got = append(got, decode(t, h.Platform, ev))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if g, w := idsByConversation(got), idsByConversation(want); !reflect.DeepEqual(g, w) {
			t.Errorf("replayed %v, want %v", g, w)
		}
		live := map[string]chat.Message{}
		for _, m := range want {
			m.Payload = nil
			live[m.ID] = m
		}
		for _, m := range got {
			m.Payload = nil
			if !reflect.DeepEqual(m, live[m.ID]) {
				t.Errorf("replayed %+v, want the facts of the live event %+v", m, live[m.ID])
			}
		}
	})

	t.Run("FailuresAreErrors", func(t *testing.T) {
		h := newHarness(t)
		fileID, err := h.Platform.Upload(t.Context(), h.Conversation, image)
		if err != nil {
			t.Fatal(err)
		}
		h.Fail()
		for name, err := range calls(t.Context(), h, fileID) {
			if err == nil {
				t.Errorf("%s: no error", name)
			}
		}
	})

	t.Run("CancellationReturnsPromptly", func(t *testing.T) {
		h := newHarness(t)
		fileID, err := h.Platform.Upload(t.Context(), h.Conversation, image)
		if err != nil {
			t.Fatal(err)
		}
		h.Stall()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		start := time.Now()
		for name, err := range calls(ctx, h, fileID) {
			if err == nil {
				t.Errorf("%s: no error", name)
			}
		}
		if d := time.Since(start); d > prompt {
			t.Errorf("calls returned %v after their context ended", d)
		}
	})
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

// idsByConversation lists message IDs per conversation in the order given.
func idsByConversation(ms []chat.Message) map[string][]string {
	ids := map[string][]string{}
	for _, m := range ms {
		ids[m.ConversationID] = append(ids[m.ConversationID], m.ID)
	}
	return ids
}

// calls makes every call that does I/O and returns their errors by method name.
func calls(ctx context.Context, h ChatHarness, fileID string) map[string]error {
	p := h.Platform
	errs := map[string]error{}
	_, errs["Self"] = p.Self(ctx)
	_, errs["User"] = p.User(ctx, h.User.ID)
	_, errs["Attachment"] = p.Attachment(ctx, fileID)
	_, errs["Download"] = p.Download(ctx, fileID, 1<<20)
	_, errs["Send"] = p.Send(ctx, chat.Outgoing{ConversationID: h.Conversation, Text: "hello"})
	_, errs["Upload"] = p.Upload(ctx, h.Conversation, chat.File{Name: "a.txt", MediaType: "text/plain", Data: []byte("a")})
	errs["Replay"] = p.Replay(ctx, time.UnixMilli(0), func(context.Context, json.RawMessage) error { return nil })
	return errs
}
