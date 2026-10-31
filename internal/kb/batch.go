package kb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/llm"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/routing"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

// learnAt is how many pending items make an append request a learning run.
const learnAt = 25

// emptyBatch is the value a missing batch starts as.
var emptyBatch = json.RawMessage(`{"messages": []}`)

// Trigger carries requests for a learning run to the learner. It holds one request, into which the requests made
// while it waits merge.
type Trigger chan struct{}

// NewTrigger returns a Trigger with no request waiting.
func NewTrigger() Trigger { return make(Trigger, 1) }

// Request asks for a learning run. It never blocks.
func (t Trigger) Request() {
	select {
	case t <- struct{}{}:
	default:
	}
}

// Batch queues answered conversations under the batch key for the learning run. Every field is required.
type Batch struct {
	Store interface {
		Update(ctx context.Context, key string, seed json.RawMessage, fn func(json.RawMessage) (json.RawMessage, error)) error
	}
	// Learn is requested whenever an append leaves enough items pending.
	Learn Trigger
	Log   *slog.Logger
}

// item is a queued turn.
type item struct {
	PostID    string `json:"post_id"`
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	// CreateAt is in epoch milliseconds, and nil when the platform did not report it.
	CreateAt  *int64 `json:"create_at"`
	ContextID string `json:"context_id"`
	RootID    string `json:"root_id"`
	IsThread  bool   `json:"is_thread"`
	Message   string `json:"message"`
	Status    string `json:"status"`
}

// Append queues the turn of m, a flushed message with the combined text, and reply as a pending item. It skips a
// conversation that is not learnable, an error reply, and a blank text or reply, and leaves the batch unchanged
// when m is already queued. The item's text has one leading author prefix removed. The batch keeps only its
// messages. When the append leaves 25 or more items pending, it requests a learning run.
func (b Batch) Append(ctx context.Context, s *config.Settings, m chat.Message, reply string) error {
	if !learnable(s, m) || llm.IsErrorReply(reply) || textfmt.Strip(m.Text) == "" || textfmt.Strip(reply) == "" {
		return nil
	}
	text := m.Text
	if m.AuthorID != "" {
		text = strings.TrimPrefix(text, m.AuthorID+": ")
	}
	it := item{PostID: m.ID, ChannelID: m.ConversationID, UserID: m.AuthorID, ContextID: routing.ContextID(m),
		RootID: m.ThreadID, IsThread: m.ThreadID != "", Status: "pending",
		Message: m.AuthorID + ": " + text + "\n" + s.AssistantLabel + ": " + reply}
	if !m.Created.IsZero() {
		ms := m.Created.UnixMilli()
		it.CreateAt = &ms
	}
	raw, _ := json.Marshal(it) // strings, a bool, and an integer always marshal
	pending := 0               // stays 0 when m is already queued
	err := b.Store.Update(ctx, s.KBBatchKey, emptyBatch, func(cur json.RawMessage) (json.RawMessage, error) {
		items, err := queued(cur)
		if err != nil {
			return nil, err
		}
		n := 1
		for _, q := range items {
			v, _ := textfmt.Decode(q, false)
			obj, ok := v.(textfmt.Object)
			if !ok {
				return nil, errors.New("queued item is not an object")
			}
			if id, ok := obj.Get("post_id").(string); ok && id == m.ID {
				return cur, nil
			}
			if isPending(obj) {
				n++
			}
		}
		pending = n
		return json.Marshal(map[string][]json.RawMessage{"messages": append(items, raw)})
	})
	if err != nil || pending == 0 {
		return err
	}
	b.Log.DebugContext(ctx, "turn queued for learning", "post_id", m.ID, "pending", pending)
	if pending >= learnAt {
		b.Learn.Request()
	}
	return nil
}

// AppendRaw queues item as given, unless m's conversation is not learnable. It neither checks for a queued
// duplicate nor requests a learning run. The batch keeps only its messages.
func (b Batch) AppendRaw(ctx context.Context, s *config.Settings, m chat.Message, item json.RawMessage) error {
	if !learnable(s, m) {
		return nil
	}
	return b.Store.Update(ctx, s.KBBatchKey, emptyBatch, func(cur json.RawMessage) (json.RawMessage, error) {
		items, err := queued(cur)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string][]json.RawMessage{"messages": append(items, item)})
	})
}

// learnable reports whether m's conversation may feed the shared knowledge: never a direct message, and a private
// conversation only when s allows it.
func learnable(s *config.Settings, m chat.Message) bool {
	switch m.Kind {
	case chat.Direct:
		return false
	case chat.Private:
		return s.KBLearnFromPrivateChannels
	}
	return true
}

// queued returns the items of a batch value: its messages list, where a missing or falsy one has none.
func queued(batch json.RawMessage) ([]json.RawMessage, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(batch, &members); err != nil {
		return nil, fmt.Errorf("batch is not an object: %w", err)
	}
	raw, ok := members["messages"]
	if !ok {
		return nil, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		return items, nil
	}
	if v, err := textfmt.Decode(raw, false); err == nil && !textfmt.Truthy(v) {
		return nil, nil
	}
	return nil, errors.New("batch messages are not a list")
}

// isPending reports whether a queued item has no status or the status "pending".
func isPending(obj textfmt.Object) bool {
	for _, m := range obj {
		if m.Key == "status" {
			return m.Value == "pending"
		}
	}
	return true
}
