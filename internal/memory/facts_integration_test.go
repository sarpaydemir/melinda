//go:build integration

package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/llm"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/store/history"
	"github.com/sarpaydemir/dost/internal/store/pg"
	"github.com/sarpaydemir/dost/internal/store/vector"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/fakeclock"
	"github.com/sarpaydemir/dost/internal/testkit/testdb"
)

// vectors embeds each text it knows as its vector.
type vectors map[string][]float32

func (v vectors) Embed(_ context.Context, _, text string) ([]float32, error) {
	if vec, ok := v[text]; ok {
		return vec, nil
	}
	return nil, errors.New("unknown text")
}

// point returns an embedding with the given leading components.
func point(xs ...float32) []float32 {
	v := make([]float32, pg.EmbeddingDims)
	copy(v, xs)
	return v
}

func TestExtractedFactsAreRecalledWithOlderFacts(t *testing.T) {
	ctx := context.Background()
	db, err := pg.Open(ctx, testdb.New(t), clock.Real(), discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	clk := fakeclock.New(time.Now())
	h := NewHistory(history.New(db, clk), clk, "agent")
	vs := vector.New(db)
	older := []vector.Doc{
		{Document: "Likes tea.", Embedding: point(1), Metadata: json.RawMessage(`{"key": "agent:u1", "type": "user_fact", "scope": "public"}`)},
		{Document: "Lives in Izmir.", Embedding: point(0, 0, 1), Metadata: json.RawMessage(`{"key": "agent:u1", "type": "user_fact", "scope": "private"}`)},
		{Document: "Works on billing too.", Embedding: point(0, 1), Metadata: json.RawMessage(`{"key": "agent:u2", "type": "user_fact", "scope": "public"}`)},
	}
	if err := vs.Add(ctx, "agent_memory", older); err != nil {
		t.Fatal(err)
	}
	var count int
	for i := range 5 {
		count = mustAddTurn(t, h, "u1", fmt.Sprint("u1: message ", i), "Noted.", map[string]any{"scope": "public"})
	}
	const question = "u1: what do you know about me?"
	f := Facts{
		Settings: settings(),
		Model:    llm.Client{Provider: &provider{answer: `[{"fact": "Likes tea."}, {"fact": "Works on billing."}]`}, Log: discard},
		Embedder: vectors{"Likes tea.": point(1), "Works on billing.": point(0, 1), question: point(0.1, 1, 0.05)},
		Store:    vs,
		History:  h,
		Prompt:   factsPrompt,
		Log:      discard,
	}

	f.Extract(ctx, channel, "Noted.", count)

	hits, err := vs.Search(ctx, "agent_memory", point(0, 1), map[string]string{"key": "agent:u1", "scope": "public"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || *hits[0].Document != "Works on billing." || *hits[1].Document != "Likes tea." {
		t.Fatalf("public facts of u1 = %+v, want the new fact and the older one it repeated", hits)
	}
	want := `{"key": "agent:u1", "scope": "public", "type": "user_fact", "user_id": "u1", "is_dm": false, "context_id": "p1"}`
	if eq, err := testkit.JSONEqual(hits[0].Metadata, []byte(want)); err != nil || !eq {
		t.Errorf("new fact metadata = %s, want %s", hits[0].Metadata, want)
	}

	for kind, want := range map[chat.Kind]string{
		chat.Direct: "Works on billing.\nLikes tea.\nLives in Izmir.",
		chat.Public: "Works on billing.\nLikes tea.",
	} {
		if got := f.Recall(ctx, chat.Message{ID: "p9", AuthorID: "u1", Kind: kind, Text: question}); got != want {
			t.Errorf("Recall in conversation kind %d = %q, want %q", kind, got, want)
		}
	}
}
