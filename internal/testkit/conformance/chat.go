// Package conformance holds the behavior every adapter of a port must show. An adapter's tests run its port's
// suite with a harness that wires the adapter to a fake.
package conformance

import (
	"bytes"
	"context"
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
}

// prompt is how long a call may take to return once its context ends.
const prompt = 5 * time.Second

// Chat checks a chat platform adapter's lookups and writes, each against a fresh harness from newHarness.
func Chat(t *testing.T, newHarness func(*testing.T) ChatHarness) {
	image := chat.File{Name: "chart.png", MediaType: "image/png", Data: []byte("\x89PNG\r\n\x1a\nchart")}

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

// calls makes every lookup and write that does I/O and returns their errors by method name.
func calls(ctx context.Context, h ChatHarness, fileID string) map[string]error {
	p := h.Platform
	errs := map[string]error{}
	_, errs["Self"] = p.Self(ctx)
	_, errs["User"] = p.User(ctx, h.User.ID)
	_, errs["Attachment"] = p.Attachment(ctx, fileID)
	_, errs["Download"] = p.Download(ctx, fileID, 1<<20)
	_, errs["Send"] = p.Send(ctx, chat.Outgoing{ConversationID: h.Conversation, Text: "hello"})
	_, errs["Upload"] = p.Upload(ctx, h.Conversation, chat.File{Name: "a.txt", MediaType: "text/plain", Data: []byte("a")})
	return errs
}
