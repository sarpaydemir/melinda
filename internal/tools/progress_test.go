package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/tools"
)

// outbound records every message sent through it and answers each send with the next of replies, then with a
// message ID.
type outbound struct {
	mu      sync.Mutex
	sent    []chat.Outgoing
	replies []reply
}

type reply struct {
	id  string
	err error
}

func (o *outbound) Send(_ context.Context, m chat.Outgoing) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sent = append(o.sent, m)
	if len(o.replies) == 0 {
		return "post", nil
	}
	r := o.replies[0]
	o.replies = o.replies[1:]
	return r.id, r.err
}

// progressSet offers the tools of a reply in conversation that goes to thread "root", allowing 2 updates of at
// most 500 code points through o.
func progressSet(t *testing.T, o *outbound, conversation string) tools.Set {
	t.Helper()
	b := tools.Builtin{Chat: o, ReplyThread: "root", MaxProgressUpdates: 2, ProgressUpdateMaxChars: 500, Log: slog.New(slog.DiscardHandler)}
	set, err := tools.Offer(tool.Call{ConversationID: conversation}, nil, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func sendProgress(set tools.Set, args string) string {
	return set.Execute(context.Background(), "send_progress_update", json.RawMessage(args))
}

const (
	posted       = "Progress update posted to the user. Continue the task."
	limitReached = "Error: Progress update limit reached for this request. Continue the task and give the final answer."
	notPosted    = "Error: Progress update could not be posted"
)

func TestProgressUpdatesPostToTheReplyThreadUpToTheLimit(t *testing.T) {
	o := &outbound{}
	set := progressSet(t, o, "town-square")
	for _, c := range []struct{ args, want string }{
		{`{"message": "  working on it\n"}`, posted},
		{`{"message": "almost there"}`, posted},
		{`{"message": "one more"}`, limitReached},
	} {
		if got := sendProgress(set, c.args); got != c.want {
			t.Errorf("%s: got %q, want %q", c.args, got, c.want)
		}
	}
	want := []chat.Outgoing{
		{ConversationID: "town-square", ThreadID: "root", Text: "working on it"},
		{ConversationID: "town-square", ThreadID: "root", Text: "almost there"},
	}
	if !reflect.DeepEqual(o.sent, want) {
		t.Errorf("sent %+v, want %+v", o.sent, want)
	}
}

func TestProgressUpdateChecksTheMessageBeforeTheConversation(t *testing.T) {
	for _, c := range []struct{ conversation, args, want string }{
		{"town-square", `{"message": "   "}`, "Error: Missing progress message"},
		{"town-square", `{}`, "Error: Missing progress message"},
		{"town-square", `{"message": 5}`, "Error: Could not execute send_progress_update: 'int' object has no attribute 'strip'"},
		{"", `{"message": " "}`, "Error: Missing progress message"},
		{"", `{"message": "working on it"}`, "Error: send_progress_update requires the current conversation"},
	} {
		o := &outbound{}
		if got := sendProgress(progressSet(t, o, c.conversation), c.args); got != c.want || len(o.sent) != 0 {
			t.Errorf("%q %s: got %q after %d sends, want %q after none", c.conversation, c.args, got, len(o.sent), c.want)
		}
	}
}

func TestProgressUpdateIsClippedToTheLimitInCodePoints(t *testing.T) {
	o := &outbound{}
	sendProgress(progressSet(t, o, "town-square"), `{"message": "  `+strings.Repeat("ğ", 600)+`  "}`)
	if len(o.sent) != 1 || o.sent[0].Text != strings.Repeat("ğ", 500) {
		t.Errorf("sent %+v, want one update of 500 code points", o.sent)
	}
}

func TestProgressUpdateThatIsNotPostedIsNotCounted(t *testing.T) {
	o := &outbound{replies: []reply{{err: errors.New("service unavailable")}, {id: ""}}}
	set := progressSet(t, o, "town-square")
	for i, want := range []string{notPosted, notPosted, posted, posted, limitReached} {
		if got := sendProgress(set, `{"message": "working on it"}`); got != want {
			t.Errorf("call %d: got %q, want %q", i+1, got, want)
		}
	}
	if len(o.sent) != 4 {
		t.Errorf("sent %d updates, want each of the 4 allowed calls sent once", len(o.sent))
	}
}

func TestProgressUpdateLimitHoldsForConcurrentCalls(t *testing.T) {
	o := &outbound{}
	set := progressSet(t, o, "town-square")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { sendProgress(set, `{"message": "working on it"}`) })
	}
	wg.Wait()
	if len(o.sent) != 2 {
		t.Errorf("sent %d updates, want 2", len(o.sent))
	}
}

func TestEachReplyHasItsOwnProgressUpdates(t *testing.T) {
	o := &outbound{}
	for range 2 {
		set := progressSet(t, o, "town-square")
		sendProgress(set, `{"message": "working on it"}`)
		sendProgress(set, `{"message": "working on it"}`)
	}
	if len(o.sent) != 4 {
		t.Errorf("sent %d updates, want 2 for each reply", len(o.sent))
	}
}
