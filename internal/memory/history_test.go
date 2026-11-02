package memory

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sarpaydemir/melinda/internal/testkit"
)

func TestHistoryKeys(t *testing.T) {
	cases := []struct {
		name      string
		contextID string
		isDM      bool
		read      string
		write     []string
	}{
		{"direct message", "p1", true, "u1", []string{"u1"}},
		{"direct message thread", "root1", true, "u1", []string{"u1"}},
		{"channel post", "p1", false, "thread:p1", []string{"u1", "thread:p1"}},
		{"channel thread reply", "root1", false, "thread:root1", []string{"u1", "thread:root1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			read, write := HistoryKeys("u1", c.contextID, c.isDM)
			if read != c.read || !slices.Equal(write, c.write) {
				t.Errorf("HistoryKeys = %q, %q; want %q, %q", read, write, c.read, c.write)
			}
		})
	}
}

func items(raw ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(raw))
	for i, r := range raw {
		out[i] = json.RawMessage(r)
	}
	return out
}

func TestHistoryText(t *testing.T) {
	cases := []struct {
		name  string
		items []json.RawMessage
		want  string
	}{
		{"no items", nil, ""},
		{
			"turns in order without a trailing newline",
			items(`{"user": "msg1", "assistant": "reply1", "message_count": 1}`, `{"user": "msg2", "assistant": "reply2"}`),
			"User: msg1\nAssistant: reply1\nUser: msg2\nAssistant: reply2",
		},
		{"missing values render empty", items(`{"scope": "public"}`), "User: \nAssistant: "},
		{"multi-line values stay as they are", items(`{"user": "a\nb", "assistant": "c"}`), "User: a\nb\nAssistant: c"},
		{"non-string values render as literals", items(`{"user": null, "assistant": 5}`), "User: None\nAssistant: 5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HistoryText(c.items); got != c.want {
				t.Errorf("HistoryText = %q, want %q", got, c.want)
			}
		})
	}
}

func TestTurnPutsItsOwnFieldsOverMetadata(t *testing.T) {
	got, err := turn(map[string]any{"scope": "public", "user": "meta", "ts": "meta", "is_dm": false}, "msg", "reply", "2026-10-30T12:00:00+00:00", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"scope": "public", "is_dm": false, "user": "msg", "assistant": "reply", "ts": "2026-10-30T12:00:00+00:00", "message_count": 3}`
	if eq, err := testkit.JSONEqual(got, []byte(want)); err != nil || !eq {
		t.Errorf("turn = %s, want %s", got, want)
	}
}

func TestLastCount(t *testing.T) {
	cases := []struct {
		name  string
		items []json.RawMessage
		want  int
	}{
		{"no items", nil, 0},
		{"last item has a count", items(`{"message_count": 1}`, `{"message_count": 7}`), 7},
		{"last item has none", items(`{"message_count": 7}`, `{"user": "x", "step": 1}`), 0},
		{"last item is not an object", items(`{"message_count": 7}`, `"text"`), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lastCount(c.items); got != c.want {
				t.Errorf("lastCount = %d, want %d", got, c.want)
			}
		})
	}
}
