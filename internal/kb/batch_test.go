package kb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/kvtest"
)

const batchKey = "agent:public_kb_batch"

func batchSettings(learnFromPrivate bool) *config.Settings {
	return &config.Settings{KBBatchKey: batchKey, AssistantLabel: "agent", KBLearnFromPrivateChannels: learnFromPrivate}
}

// answered is a buffered thread reply whose text carries its author's prefix.
func answered() chat.Message {
	return chat.Message{ID: "p1", ConversationID: "c1", ThreadID: "r1", AuthorID: "u1", Text: "u1: hello",
		Created: time.UnixMilli(1780000000123), Kind: chat.Public}
}

// newBatch returns a Batch on an empty store, the store, and the trigger it requests learning runs on.
func newBatch() (Batch, *kvtest.Memory, Trigger) {
	store := &kvtest.Memory{}
	learn := NewTrigger()
	return Batch{Store: store, Learn: learn, Log: discard}, store, learn
}

func put(t *testing.T, store *kvtest.Memory, value string) {
	t.Helper()
	if err := store.Set(context.Background(), batchKey, json.RawMessage(value)); err != nil {
		t.Fatal(err)
	}
}

// stored returns the batch value, or "" when there is none.
func stored(t *testing.T, store *kvtest.Memory) string {
	t.Helper()
	v, _, err := store.Get(context.Background(), batchKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(v)
}

func assertBatch(t *testing.T, store *kvtest.Memory, want string) {
	t.Helper()
	got := stored(t, store)
	if eq, err := testkit.JSONEqual([]byte(got), []byte(want)); err != nil || !eq {
		t.Errorf("batch = %s\nwant    %s (err %v)", got, want, err)
	}
}

// requested reports whether a learning run was requested, and takes the request.
func requested(learn Trigger) bool {
	select {
	case <-learn:
		return true
	default:
		return false
	}
}

// pendingItems returns n items numbered from 0 whose status member is status, or which have none when status is "".
func pendingItems(n int, status string) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"post_id": "x%d"%s}`, i, status)
	}
	return `{"messages": [` + strings.Join(items, ", ") + `]}`
}

func TestAppendQueuesTheTurnAsAPendingItem(t *testing.T) {
	b, store, _ := newBatch()
	if err := b.Append(t.Context(), batchSettings(false), answered(), "hi there"); err != nil {
		t.Fatal(err)
	}
	want := `{"messages":[{"post_id":"p1","channel_id":"c1","user_id":"u1","create_at":1780000000123,"context_id":"r1",` +
		`"root_id":"r1","is_thread":true,"message":"u1: hello\nagent: hi there","status":"pending"}]}`
	if got := stored(t, store); got != want {
		t.Errorf("batch = %s\nwant    %s", got, want)
	}
}

func TestAppendDescribesTopLevelAndUndatedMessages(t *testing.T) {
	tests := []struct {
		name string
		edit func(*chat.Message)
		want string
	}{
		{"top-level message", func(m *chat.Message) { m.ThreadID = "" },
			`{"post_id": "p1", "channel_id": "c1", "user_id": "u1", "create_at": 1780000000123, "context_id": "p1",
			"root_id": "", "is_thread": false, "message": "u1: hello\nagent: ok", "status": "pending"}`},
		{"no creation time", func(m *chat.Message) { m.Created = time.Time{} },
			`{"post_id": "p1", "channel_id": "c1", "user_id": "u1", "create_at": null, "context_id": "r1",
			"root_id": "r1", "is_thread": true, "message": "u1: hello\nagent: ok", "status": "pending"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, store, _ := newBatch()
			m := answered()
			tt.edit(&m)
			if err := b.Append(t.Context(), batchSettings(false), m, "ok"); err != nil {
				t.Fatal(err)
			}
			assertBatch(t, store, `{"messages": [`+tt.want+`]}`)
		})
	}
}

func TestAppendSkipsWhatIsNotSharedKnowledge(t *testing.T) {
	tests := []struct {
		name             string
		kind             chat.Kind
		learnFromPrivate bool
		text, reply      string
		queued           bool
	}{
		{"public conversation", chat.Public, false, "u1: hello", "ok", true},
		{"direct message", chat.Direct, false, "u1: hello", "ok", false},
		{"direct message while private conversations are learned from", chat.Direct, true, "u1: hello", "ok", false},
		{"private conversation", chat.Private, false, "u1: hello", "ok", false},
		{"private conversation while private conversations are learned from", chat.Private, true, "u1: hello", "ok", true},
		{"error reply", chat.Public, false, "u1: hello", "sorry [ERR_X]", false},
		{"empty text", chat.Public, false, "", "ok", false},
		{"whitespace text", chat.Public, false, " \t\n", "ok", false},
		{"ideographic space text", chat.Public, false, "\u3000", "ok", false},
		{"separator control text", chat.Public, false, "\x1c", "ok", false},
		{"zero-width space text", chat.Public, false, "\u200b", "ok", true},
		{"empty reply", chat.Public, false, "u1: hello", "", false},
		{"no-break space reply", chat.Public, false, "u1: hello", "\u00a0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, store, _ := newBatch()
			m := answered()
			m.Kind, m.Text = tt.kind, tt.text
			if err := b.Append(t.Context(), batchSettings(tt.learnFromPrivate), m, tt.reply); err != nil {
				t.Fatal(err)
			}
			if queued := stored(t, store) != ""; queued != tt.queued {
				t.Errorf("queued = %v, want %v", queued, tt.queued)
			}
		})
	}
}

func TestAppendRemovesOneLeadingAuthorPrefix(t *testing.T) {
	tests := []struct {
		name, author, text, want string
	}{
		{"prefix", "u1", "u1: hello", "u1: hello\nagent: ok"},
		{"repeated prefix", "u1", "u1: u1: hello", "u1: u1: hello\nagent: ok"},
		{"prefixes of later lines", "u1", "u1: a\nu1: b", "u1: a\nu1: b\nagent: ok"},
		{"prefix only", "u1", "u1: ", "u1: \nagent: ok"},
		{"no space after the colon", "u1", "u1:hello", "u1: u1:hello\nagent: ok"},
		{"space before the prefix", "u1", " u1: hello", "u1:  u1: hello\nagent: ok"},
		{"another author's prefix", "u1", "u2: hello", "u1: u2: hello\nagent: ok"},
		{"no author", "", ": hello", ": : hello\nagent: ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, store, _ := newBatch()
			m := answered()
			m.AuthorID, m.Text = tt.author, tt.text
			if err := b.Append(t.Context(), batchSettings(false), m, "ok"); err != nil {
				t.Fatal(err)
			}
			var batch struct{ Messages []struct{ Message string } }
			if err := json.Unmarshal([]byte(stored(t, store)), &batch); err != nil || len(batch.Messages) != 1 {
				t.Fatalf("batch = %s (err %v), want one item", stored(t, store), err)
			}
			if got := batch.Messages[0].Message; got != tt.want {
				t.Errorf("message = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAppendLeavesTheBatchUnchangedWhenThePostIsQueued(t *testing.T) {
	b, store, learn := newBatch()
	before := `{"messages":[{"post_id":"x","status":"pending"},{"post_id":"p1","status":"processing","n":1.0}],"other":"kept"}`
	put(t, store, before)
	if err := b.Append(t.Context(), batchSettings(false), answered(), "ok"); err != nil {
		t.Fatal(err)
	}
	if got := stored(t, store); got != before {
		t.Errorf("batch = %s, want it unchanged", got)
	}

	// Well past the learning threshold, a queued post still requests nothing.
	put(t, store, strings.Replace(pendingItems(30, `, "status": "pending"`), `"x0"`, `"p1"`, 1))
	if err := b.Append(t.Context(), batchSettings(false), answered(), "ok"); err != nil {
		t.Fatal(err)
	}
	if requested(learn) {
		t.Error("learning run requested for a post already queued")
	}
}

func TestAppendMatchesQueuedPostsByStringID(t *testing.T) {
	b, store, _ := newBatch()
	put(t, store, `{"messages": [{"post_id": 5, "status": "pending"}]}`)
	m := answered()
	m.ID = "5"
	if err := b.Append(t.Context(), batchSettings(false), m, "ok"); err != nil {
		t.Fatal(err)
	}
	assertBatch(t, store, `{"messages": [{"post_id": 5, "status": "pending"},
		{"post_id": "5", "channel_id": "c1", "user_id": "u1", "create_at": 1780000000123, "context_id": "r1",
		"root_id": "r1", "is_thread": true, "message": "u1: hello\nagent: ok", "status": "pending"}]}`)
}

func TestAppendRequestsLearningFromTwentyFivePendingItems(t *testing.T) {
	tests := []struct {
		name   string
		before string
		want   bool
	}{
		{"24 pending", pendingItems(23, `, "status": "pending"`), false},
		{"25 pending", pendingItems(24, `, "status": "pending"`), true},
		{"26 pending", pendingItems(25, `, "status": "pending"`), true},
		{"items without a status count as pending", pendingItems(24, ""), true},
		{"null status is not pending", pendingItems(24, `, "status": null`), false},
		{"processing items are not pending", pendingItems(24, `, "status": "processing"`), false},
		{"status matches case-sensitively", pendingItems(24, `, "status": "PENDING"`), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, store, learn := newBatch()
			put(t, store, tt.before)
			if err := b.Append(t.Context(), batchSettings(false), answered(), "ok"); err != nil {
				t.Fatal(err)
			}
			if got := requested(learn); got != tt.want {
				t.Errorf("learning run requested = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAppendRequestsAgainForEveryAppendOverTheThreshold(t *testing.T) {
	b, store, learn := newBatch()
	put(t, store, pendingItems(30, ""))
	for _, id := range []string{"p1", "p2"} {
		m := answered()
		m.ID = id
		if err := b.Append(t.Context(), batchSettings(false), m, "ok"); err != nil {
			t.Fatal(err)
		}
		if !requested(learn) {
			t.Errorf("append of %s requested no learning run", id)
		}
	}
}

func TestTriggerHoldsOneRequestWithoutBlocking(t *testing.T) {
	learn := NewTrigger()
	learn.Request()
	learn.Request()
	if !requested(learn) || requested(learn) {
		t.Error("two requests while none is taken did not merge into one")
	}
}

func TestAppendKeepsQueuedItemsVerbatim(t *testing.T) {
	b, store, _ := newBatch()
	queued := `{"post_id": "a", "create_at": 1780000000.0, "big": 12345678901234567890, "f": 1.5,
		"processing_started_at": 1780000000999, "status": "processing", "unknown": {"n": [1, 2.0]}},
		{"post_id": "b", "message": "m"}`
	put(t, store, `{"messages": [`+queued+`], "other": 1}`)
	if err := b.Append(t.Context(), batchSettings(false), answered(), "ok"); err != nil {
		t.Fatal(err)
	}
	assertBatch(t, store, `{"messages": [`+queued+`, {"post_id": "p1", "channel_id": "c1", "user_id": "u1",
		"create_at": 1780000000123, "context_id": "r1", "root_id": "r1", "is_thread": true,
		"message": "u1: hello\nagent: ok", "status": "pending"}]}`)
}

func TestAppendStartsAnEmptyBatch(t *testing.T) {
	item := `{"post_id": "p1", "channel_id": "c1", "user_id": "u1", "create_at": 1780000000123, "context_id": "r1",
		"root_id": "r1", "is_thread": true, "message": "u1: hello\nagent: ok", "status": "pending"}`
	for _, before := range []string{"null", `{}`, `{"messages": null}`, `{"messages": {}}`, `{"messages": ""}`,
		`{"messages": false}`} {
		t.Run(before, func(t *testing.T) {
			b, store, _ := newBatch()
			put(t, store, before)
			if err := b.Append(t.Context(), batchSettings(false), answered(), "ok"); err != nil {
				t.Fatal(err)
			}
			assertBatch(t, store, `{"messages": [`+item+`]}`)
		})
	}
}

func TestAppendRejectsAMalformedBatch(t *testing.T) {
	for _, before := range []string{`[]`, `"x"`, `{"messages": {"a": 1}}`, `{"messages": "ab"}`, `{"messages": true}`,
		`{"messages": [1]}`} {
		t.Run(before, func(t *testing.T) {
			b, store, _ := newBatch()
			put(t, store, before)
			if err := b.Append(t.Context(), batchSettings(false), answered(), "ok"); err == nil {
				t.Error("Append succeeded")
			}
			if got := stored(t, store); got != before {
				t.Errorf("batch = %s, want it unchanged", got)
			}
		})
	}
}

func TestAppendRawStoresTheItemAsGiven(t *testing.T) {
	b, store, learn := newBatch()
	queued := strings.Replace(pendingItems(30, ""), "[", "[1, ", 1) // a non-object item and 30 pending ones
	put(t, store, strings.TrimSuffix(queued, "}")+`, "other": 1}`)
	item := `{"post_id": "auto:1:1780000000", "message": "step\nbody", "channel_id": "automation", "create_at": 1780000000.0}`
	for range 2 {
		if err := b.AppendRaw(t.Context(), batchSettings(false), answered(), json.RawMessage(item)); err != nil {
			t.Fatal(err)
		}
	}
	assertBatch(t, store, strings.TrimSuffix(queued, "]}")+", "+item+", "+item+"]}")
	if requested(learn) {
		t.Error("learning run requested")
	}
}

func TestAppendRawSkipsWhatIsNotSharedKnowledge(t *testing.T) {
	tests := []struct {
		name             string
		kind             chat.Kind
		learnFromPrivate bool
		queued           bool
	}{
		{"private conversation", chat.Private, false, false},
		{"private conversation while private conversations are learned from", chat.Private, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, store, _ := newBatch()
			m := answered()
			m.Kind = tt.kind
			if err := b.AppendRaw(t.Context(), batchSettings(tt.learnFromPrivate), m, json.RawMessage(`{"post_id": "x"}`)); err != nil {
				t.Fatal(err)
			}
			if queued := stored(t, store) != ""; queued != tt.queued {
				t.Errorf("queued = %v, want %v", queued, tt.queued)
			}
		})
	}
}
