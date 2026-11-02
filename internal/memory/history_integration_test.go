//go:build integration

package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/store/history"
	"github.com/sarpaydemir/melinda/internal/store/pg"
	"github.com/sarpaydemir/melinda/internal/testkit"
	"github.com/sarpaydemir/melinda/internal/testkit/fakeclock"
	"github.com/sarpaydemir/melinda/internal/testkit/testdb"
)

const prefix = "agent"

// newHistory returns a History on a fresh database and the store beneath it, which takes full storage keys.
func newHistory(t *testing.T, now time.Time) (*History, *history.Store) {
	t.Helper()
	db, err := pg.Open(context.Background(), testdb.New(t), clock.Real(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	clk := fakeclock.New(now)
	store := history.New(db, clk)
	return NewHistory(store, clk, prefix), store
}

func mustGet(t *testing.T, h *History, key string, n int) []json.RawMessage {
	t.Helper()
	got, err := h.Get(context.Background(), key, n)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func mustAddTurn(t *testing.T, h *History, key, user, assistant string, metadata map[string]any) int {
	t.Helper()
	count, err := h.AddTurn(context.Background(), key, user, assistant, metadata)
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func mustAdd(t *testing.T, h *History, key, item string) {
	t.Helper()
	if err := h.Add(context.Background(), key, json.RawMessage(item)); err != nil {
		t.Fatal(err)
	}
}

func assertItems(t *testing.T, got []json.RawMessage, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d items %s, want %d", len(got), got, len(want))
	}
	for i := range want {
		if eq, err := testkit.JSONEqual(got[i], []byte(want[i])); err != nil || !eq {
			t.Errorf("item %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func counts(t *testing.T, items []json.RawMessage) []int {
	t.Helper()
	out := make([]int, len(items))
	for i, item := range items {
		var v struct {
			MessageCount int `json:"message_count"`
		}
		if err := json.Unmarshal(item, &v); err != nil {
			t.Fatal(err)
		}
		out[i] = v.MessageCount
	}
	return out
}

func TestAddTurnStoresTheTurnUnderThePrefixedKey(t *testing.T) {
	h, store := newHistory(t, time.Date(2026, 10, 30, 12, 0, 0, 123456789, time.UTC))
	metadata := map[string]any{"scope": "public", "context_id": "root1", "user_id": "u1", "is_dm": false, "thread_id": "root1"}
	if count := mustAddTurn(t, h, "thread:root1", "u1: hello", "hi there", metadata); count != 1 {
		t.Errorf("AddTurn = %d, want 1", count)
	}
	stored, err := store.Get(context.Background(), "agent:thread:root1")
	if err != nil {
		t.Fatal(err)
	}
	assertItems(t, stored, `{"scope": "public", "context_id": "root1", "user_id": "u1", "is_dm": false, "thread_id": "root1",
		"user": "u1: hello", "assistant": "hi there", "ts": "2026-10-30T12:00:00.123456+00:00", "message_count": 1}`)
}

func TestAddTurnWritesWholeSecondsWithoutAFraction(t *testing.T) {
	h, _ := newHistory(t, time.Date(2026, 10, 30, 12, 0, 0, 999, time.UTC))
	mustAddTurn(t, h, "u1", "msg", "reply", nil)
	assertItems(t, mustGet(t, h, "u1", 0), `{"user": "msg", "assistant": "reply", "ts": "2026-10-30T12:00:00+00:00", "message_count": 1}`)
}

func TestMessageCountIncrementsPerKey(t *testing.T) {
	h, _ := newHistory(t, time.Now())
	public := map[string]any{"scope": "public"}
	got := []int{
		mustAddTurn(t, h, "u1", "msg1", "reply1", public),
		mustAddTurn(t, h, "u1", "msg2", "reply2", public),
		mustAddTurn(t, h, "thread:p1", "msg2", "reply2", public),
	}
	if want := []int{1, 2, 1}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("counts = %v, want %v", got, want)
	}
	if got := counts(t, mustGet(t, h, "u1", 10)); fmt.Sprint(got) != "[1 2]" {
		t.Errorf("stored counts = %v, want [1 2]", got)
	}
}

func TestMessageCountRestartsAfterAnItemWithoutOne(t *testing.T) {
	h, _ := newHistory(t, time.Now())
	mustAddTurn(t, h, "u1", "msg1", "reply1", nil)
	mustAddTurn(t, h, "u1", "msg2", "reply2", nil)
	mustAdd(t, h, "u1", `{"user": "/cmd x", "assistant": "result", "ts": "2026-02-01T00:00:00+00:00", "scope": "public", "context_id": "", "step": 1}`)
	if count := mustAddTurn(t, h, "u1", "msg3", "reply3", nil); count != 1 {
		t.Errorf("AddTurn after an item without a count = %d, want 1", count)
	}
}

func TestMessageCountKeepsGrowingAfterTrimming(t *testing.T) {
	h, _ := newHistory(t, time.Now())
	var count int
	for i := range 32 {
		count = mustAddTurn(t, h, "u1", fmt.Sprintf("msg%d", i), "reply", nil)
	}
	if count != 32 {
		t.Errorf("last count = %d, want 32", count)
	}
	stored := counts(t, mustGet(t, h, "u1", 0))
	if len(stored) != 30 || stored[0] != 3 || stored[29] != 32 {
		t.Errorf("stored counts = %v, want 3..32", stored)
	}
}

func TestGetReturnsTheNewestItemsNewestLast(t *testing.T) {
	h, _ := newHistory(t, time.Now())
	for i := range 5 {
		mustAdd(t, h, "u1", fmt.Sprintf(`{"msg": %d}`, i))
	}
	assertItems(t, mustGet(t, h, "u1", 3), `{"msg": 2}`, `{"msg": 3}`, `{"msg": 4}`)
	if got := mustGet(t, h, "u1", 0); len(got) != 5 {
		t.Errorf("Get with n=0 returned %d items, want all 5", len(got))
	}
	if got := mustGet(t, h, "u1", 10); len(got) != 5 {
		t.Errorf("Get with n=10 returned %d items, want all 5", len(got))
	}
}

func TestGetMissingKeyIsEmpty(t *testing.T) {
	h, _ := newHistory(t, time.Now())
	if got := mustGet(t, h, "nobody", 10); len(got) != 0 {
		t.Errorf("Get = %s, want no items", got)
	}
}

func TestAddKeepsTheNewestThirtyItems(t *testing.T) {
	h, _ := newHistory(t, time.Now())
	for i := range 32 {
		mustAdd(t, h, "u1", fmt.Sprintf(`{"msg": %d}`, i))
	}
	got := mustGet(t, h, "u1", 0)
	if len(got) != 30 {
		t.Fatalf("kept %d items, want 30", len(got))
	}
	assertItems(t, []json.RawMessage{got[0], got[29]}, `{"msg": 2}`, `{"msg": 31}`)
}

func TestOlderItemShapesAreKeptVerbatim(t *testing.T) {
	h, _ := newHistory(t, time.Now())
	older := []string{
		`{"ts": "2026-02-01T00:00:00", "user": "old user msg", "scope": "public", "assistant": "old assistant msg"}`,
		`{"user": "/cmd x - step", "assistant": "**title:**\nbody", "ts": "2026-02-01T00:00:00.5+00:00", "scope": "public", "context_id": "", "step": 3}`,
	}
	for _, item := range older {
		mustAdd(t, h, "u1", item)
	}
	assertItems(t, mustGet(t, h, "u1", 10), older...)

	mustAddTurn(t, h, "u1", "new", "reply", map[string]any{"scope": "private"})
	got := mustGet(t, h, "u1", 10)
	assertItems(t, got[:2], older...)
}

func TestHistoryTextOfStoredTurns(t *testing.T) {
	h, _ := newHistory(t, time.Now())
	mustAddTurn(t, h, "u1", "msg1", "reply1", map[string]any{"scope": "private"})
	mustAddTurn(t, h, "u1", "msg2", "reply2", map[string]any{"scope": "private"})
	want := "User: msg1\nAssistant: reply1\nUser: msg2\nAssistant: reply2"
	if got := HistoryText(mustGet(t, h, "u1", 10)); got != want {
		t.Errorf("HistoryText = %q, want %q", got, want)
	}
}

func TestScopedReturnsTheNewestItemsOfExactlyThatScope(t *testing.T) {
	h, _ := newHistory(t, time.Now())
	for _, item := range []string{
		`{"scope": "public", "n": 0}`,
		`{"scope": "private", "n": 1}`,
		`{"n": 2}`,
		`{"scope": "Public", "n": 3}`,
		`{"scope": "public", "n": 4}`,
		`{"scope": "public", "n": 5}`,
	} {
		mustAdd(t, h, "u1", item)
	}
	got, err := h.Scoped(context.Background(), "u1", "public", 2)
	if err != nil {
		t.Fatal(err)
	}
	assertItems(t, got, `{"scope": "public", "n": 4}`, `{"scope": "public", "n": 5}`)

	got, err = h.Scoped(context.Background(), "u1", "public", 0)
	if err != nil {
		t.Fatal(err)
	}
	assertItems(t, got, `{"scope": "public", "n": 0}`, `{"scope": "public", "n": 4}`, `{"scope": "public", "n": 5}`)
}

func TestConcurrentTurnsOnOneKeyAreAllKept(t *testing.T) {
	h, _ := newHistory(t, time.Now())
	const writers = 20
	var wg sync.WaitGroup
	returned := make(chan int, writers)
	for i := range writers {
		wg.Go(func() {
			count, err := h.AddTurn(context.Background(), "thread:t1", fmt.Sprintf("msg%d", i), "reply", map[string]any{"scope": "public"})
			if err != nil {
				t.Error(err)
			}
			returned <- count
		})
	}
	wg.Wait()
	close(returned)

	seen := map[int]bool{}
	for c := range returned {
		seen[c] = true
	}
	stored := counts(t, mustGet(t, h, "thread:t1", 0))
	for i, c := range stored {
		if c != i+1 || !seen[c] {
			t.Fatalf("stored counts = %v and returned %v, want 1..%d in both", stored, seen, writers)
		}
	}
	if len(stored) != writers {
		t.Errorf("stored %d turns, want %d", len(stored), writers)
	}
}
