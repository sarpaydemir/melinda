package memory

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/llm"
	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/routing"
	"github.com/sarpaydemir/melinda/internal/store/vector"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

const (
	// extractEvery is how many turns of a user's history pass between fact extractions, and how many of the newest
	// turns an extraction reads.
	extractEvery = 5
	// recalled is how many facts a reply's prompt shows.
	recalled = 10
	// duplicateBelow is the cosine distance under which a fact repeats a stored one.
	duplicateBelow = 0.15
)

// Facts keeps durable facts about each user in the facts collection, with one settings snapshot. Every field is
// required.
type Facts struct {
	Settings *config.Settings
	// Model finds the facts in a user's recent turns.
	Model interface {
		GenerateJSON(ctx context.Context, r model.Request) any
	}
	Embedder interface {
		Embed(ctx context.Context, model, text string) ([]float32, error)
	}
	Store interface {
		Search(ctx context.Context, collection string, vec []float32, filter map[string]string, limit int) ([]vector.Hit, error)
		Add(ctx context.Context, collection string, docs []vector.Doc) error
	}
	History interface {
		Scoped(ctx context.Context, key, scope string, n int) ([]json.RawMessage, error)
	}
	// Prompt is the catalog entry memory.facts.
	Prompt string
	Log    *slog.Logger
}

// metadata is a stored fact's metadata.
type metadata struct {
	Key       string `json:"key"`
	Scope     string `json:"scope"`
	Type      string `json:"type"`
	UserID    string `json:"user_id"`
	IsDM      bool   `json:"is_dm"`
	ContextID string `json:"context_id"`
	ThreadID  string `json:"thread_id,omitempty"`
}

// Recall returns the author's facts nearest to m's text, one per line, nearest first; outside a direct message
// only facts of m's scope are eligible. A fact without a document adds no line. It is empty when no embedding model
// is set, when the text cannot be embedded, and when the search fails, which is logged.
func (f Facts) Recall(ctx context.Context, m chat.Message) string {
	s := f.Settings
	vec := llm.Embed(ctx, f.Embedder, s, f.Log, m.Text)
	if vec == nil {
		return ""
	}
	filter := map[string]string{"key": f.key(m)}
	if m.Kind != chat.Direct {
		filter["scope"] = routing.Scope(m)
	}
	hits, err := f.Store.Search(ctx, s.FactsCollection, vec, filter, recalled)
	if err != nil {
		if ctx.Err() == nil {
			f.Log.ErrorContext(ctx, "fact search failed", "collection", s.FactsCollection, "error", err)
		}
		return ""
	}
	var lines []string
	for _, h := range hits {
		if h.Document != nil {
			lines = append(lines, *h.Document)
		}
	}
	return strings.Join(lines, "\n")
}

// Extract stores the new facts the fact model finds in the author's newest turns of m's scope, once every fifth
// turn: count is the message_count of the turn just saved under the author's key. It does nothing without an
// embedding model or after a failure reply. A fact near one already stored for the author in that scope is
// skipped. A failure is logged and ends the extraction.
func (f Facts) Extract(ctx context.Context, m chat.Message, reply string, count int) {
	s := f.Settings
	if s.EmbeddingModel == "" || llm.IsErrorReply(reply) || count%extractEvery != 0 {
		return
	}
	scope := routing.Scope(m)
	turns, err := f.History.Scoped(ctx, m.AuthorID, scope, extractEvery)
	if err != nil {
		f.Log.ErrorContext(ctx, "fact extraction failed", "error", err)
		return
	}
	if len(turns) == 0 {
		return
	}
	answer := f.Model.GenerateJSON(ctx, model.Request{
		Model:    s.RoleModel(s.FactModel),
		Messages: llm.Messages(f.Prompt, nil, HistoryText(turns)),
	})
	meta := metadata{Key: f.key(m), Scope: scope, Type: "user_fact", UserID: m.AuthorID, IsDM: m.Kind == chat.Direct,
		ContextID: routing.ContextID(m)}
	if m.ThreadID != "" {
		meta.ThreadID = meta.ContextID
	}
	for _, text := range f.texts(ctx, answer) {
		if err := f.store(ctx, text, meta); err != nil {
			f.Log.ErrorContext(ctx, "fact extraction failed", "error", err)
			return
		}
	}
}

// texts reads the facts from the fact model's answer, a list or one object: an object gives its fact member when
// that is a string, anything else its text. Empty texts are dropped; an answer of another kind gives none.
func (f Facts) texts(ctx context.Context, answer any) []string {
	if !textfmt.Truthy(answer) {
		return nil
	}
	var items []any
	switch a := answer.(type) {
	case textfmt.Object:
		items = []any{a}
	case []any:
		items = a
	default:
		f.Log.WarnContext(ctx, "fact model answer is neither a list nor an object", "answer", textfmt.Clip(textfmt.Repr(a), 200))
		return nil
	}
	var texts []string
	for _, item := range items {
		var text string
		if obj, ok := item.(textfmt.Object); ok {
			text, _ = obj.Get("fact").(string)
		} else {
			text = textfmt.Str(item)
		}
		if text != "" {
			texts = append(texts, text)
		}
	}
	return texts
}

// store adds text as a fact unless it cannot be embedded or repeats a fact stored with the same key and scope.
func (f Facts) store(ctx context.Context, text string, meta metadata) error {
	s := f.Settings
	vec := llm.Embed(ctx, f.Embedder, s, f.Log, text)
	if vec == nil {
		return nil
	}
	nearest, err := f.Store.Search(ctx, s.FactsCollection, vec, map[string]string{"key": meta.Key, "scope": meta.Scope}, 1)
	if err != nil {
		return err
	}
	if len(nearest) > 0 && nearest[0].Distance < duplicateBelow {
		f.Log.DebugContext(ctx, "duplicate fact skipped", "distance", nearest[0].Distance, "fact", textfmt.Clip(text, 60))
		return nil
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return f.Store.Add(ctx, s.FactsCollection, []vector.Doc{{Document: text, Embedding: vec, Metadata: b}})
}

// key is the metadata key of the facts about m's author.
func (f Facts) key(m chat.Message) string { return f.Settings.MemoryKeyPrefix + ":" + m.AuthorID }
