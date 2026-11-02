package memory

import (
	"context"
	"encoding/json"
	"maps"
	"strings"

	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/store/history"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

// maxItems is how many of the newest items a history keeps.
const maxItems = 30

// History is the rolling list of conversation items kept under each logical key, stored as <prefix>:<key>.
type History struct {
	store  *history.Store
	clk    clock.Clock
	prefix string
}

// NewHistory returns a History in store under prefix that timestamps turns with clk.
func NewHistory(store *history.Store, clk clock.Clock, prefix string) *History {
	return &History{store: store, clk: clk, prefix: prefix}
}

// HistoryKeys returns the key a conversation's history is read from and the keys its turns are written to, in
// write order. A direct message uses the user's key; elsewhere the history is the thread's and each turn is also
// written to the user's key, so direct-message history includes the user's turns in channels.
func HistoryKeys(userID, contextID string, isDM bool) (read string, write []string) {
	if isDM {
		return userID, []string{userID}
	}
	thread := "thread:" + contextID
	return thread, []string{userID, thread}
}

// Add appends item under key and keeps the newest 30 items.
func (h *History) Add(ctx context.Context, key string, item json.RawMessage) error {
	return h.add(ctx, key, func([]json.RawMessage) (json.RawMessage, error) { return item, nil })
}

// AddTurn appends a turn under key and returns its message_count: one more than the last item's, or 1 when the
// last item has none. The count is read under the same row lock that appends the turn.
func (h *History) AddTurn(ctx context.Context, key, user, assistant string, metadata map[string]any) (int, error) {
	var count int
	err := h.add(ctx, key, func(items []json.RawMessage) (json.RawMessage, error) {
		count = lastCount(items) + 1
		return turn(metadata, user, assistant, textfmt.FormatTime(h.clk.Now()), count)
	})
	return count, err
}

// add appends the item next builds from the current items, under the row lock, and trims to maxItems.
func (h *History) add(ctx context.Context, key string, next func([]json.RawMessage) (json.RawMessage, error)) error {
	return h.store.Update(ctx, h.prefix+":"+key, func(items []json.RawMessage) ([]json.RawMessage, error) {
		item, err := next(items)
		if err != nil {
			return nil, err
		}
		return textfmt.Tail(append(items, item), maxItems), nil
	})
}

// Get returns the last n items under key, or all of them when n <= 0.
func (h *History) Get(ctx context.Context, key string, n int) ([]json.RawMessage, error) {
	items, err := h.store.Get(ctx, h.prefix+":"+key)
	return textfmt.Tail(items, n), err
}

// Scoped returns the last n of the newest 30 items under key whose scope is exactly scope, or all such items
// when n <= 0.
func (h *History) Scoped(ctx context.Context, key, scope string, n int) ([]json.RawMessage, error) {
	items, err := h.Get(ctx, key, maxItems)
	if err != nil {
		return nil, err
	}
	var matched []json.RawMessage
	for _, item := range items {
		if s, ok := members(item)["scope"].(string); ok && s == scope {
			matched = append(matched, item)
		}
	}
	return textfmt.Tail(matched, n), nil
}

// HistoryText renders items as a "User: <user>" line and an "Assistant: <assistant>" line each, joined by
// newlines; a missing value renders empty.
func HistoryText(items []json.RawMessage) string {
	lines := make([]string, 0, 2*len(items))
	for _, item := range items {
		m := members(item)
		lines = append(lines, "User: "+text(m, "user"), "Assistant: "+text(m, "assistant"))
	}
	return strings.Join(lines, "\n")
}

func text(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	return textfmt.Str(v)
}

// turn builds a turn item; user, assistant, ts, and message_count win over metadata members of the same name.
func turn(metadata map[string]any, user, assistant, ts string, count int) (json.RawMessage, error) {
	item := maps.Clone(metadata)
	if item == nil {
		item = map[string]any{}
	}
	item["user"] = user
	item["assistant"] = assistant
	item["ts"] = ts
	item["message_count"] = count
	return json.Marshal(item)
}

// lastCount returns the last item's message_count, or 0 when there are no items or the last has none.
func lastCount(items []json.RawMessage) int {
	if len(items) == 0 {
		return 0
	}
	return textfmt.Int(members(items[len(items)-1])["message_count"], 0)
}

// members decodes the top-level members of an item; an item that is not a JSON object has none.
func members(item json.RawMessage) map[string]any {
	v, _ := textfmt.Decode(item, false)
	obj, _ := v.(textfmt.Object)
	m := make(map[string]any, len(obj))
	for _, f := range obj {
		m[f.Key] = f.Value
	}
	return m
}
