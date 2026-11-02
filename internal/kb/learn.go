package kb

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/llm"
	"github.com/sarpaydemir/melinda/internal/logging"
	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/prompt"
	"github.com/sarpaydemir/melinda/internal/store/vector"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

const (
	// staleAfter is how long a run holds its items before a later run may take them back.
	staleAfter = time.Hour
	// duplicateBelow is the cosine distance under which a new entry repeats a stored one.
	duplicateBelow = 0.15
	// minConfidence is the confidence an entry needs, and factConfidence the one an external fact needs.
	minConfidence  = 0.65
	factConfidence = 0.75
	// threadSeparator separates the threads in the fact model's input.
	threadSeparator = "\n\n====================\n\n"
	// debugClip bounds each long value in debug logs, in characters.
	debugClip = 4000
	// rejectedClip bounds the summary and rationale of a rejected item in logs, in characters.
	rejectedClip = 80
)

// memoryTypes are the kinds of knowledge an entry may hold.
var memoryTypes = []string{"assistant_insight", "team_context", "project_context", "process", "decision",
	"brand_strategy", "external_fact"}

var (
	pendingStatus    = json.RawMessage(`"pending"`)
	processingStatus = json.RawMessage(`"processing"`)
)

// Learner turns queued conversations into shared-knowledge entries, one run at a time, whenever Learn asks for a
// run. Every field is required.
type Learner struct {
	Settings interface {
		Current(ctx context.Context) *config.Settings
	}
	// Model opens the model provider from a settings snapshot.
	Model func(*config.Settings) (model.Provider, error)
	// Batch holds the queue under the batch key.
	Batch interface {
		Update(ctx context.Context, key string, seed json.RawMessage, fn func(json.RawMessage) (json.RawMessage, error)) error
	}
	// Store holds the shared knowledge entries.
	Store interface {
		Search(ctx context.Context, collection string, vec []float32, filter map[string]string, limit int) ([]vector.Hit, error)
		Add(ctx context.Context, collection string, docs []vector.Doc) error
	}
	// Prompts is the catalog that each run's settings override.
	Prompts *prompt.Catalog
	Learn   Trigger
	// Work is the parent of every run's context. It outlives Run's context so that a run in progress at shutdown
	// can finish.
	Work  context.Context
	Clock clock.Clock
	Log   *slog.Logger
}

// Name identifies the component.
func (l *Learner) Name() string { return "kb-learner" }

// Run starts a run for each request until ctx ends. A run in progress then finishes; a waiting request is dropped.
func (l *Learner) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-l.Learn:
		}
		if ctx.Err() != nil {
			return nil
		}
		if err := l.learn(l.Work); err != nil {
			l.Log.Error("learning run failed", "error", err)
		}
	}
}

// learn takes the first 25 pending items, once that many are queued, and stores the knowledge the fact model finds
// in them. It removes the items from the queue once done, and returns them to pending when it fails.
func (l *Learner) learn(ctx context.Context) error {
	s := l.Settings.Current(ctx)
	if s.EmbeddingModel == "" {
		l.Log.InfoContext(ctx, "learning skipped: no embedding model is set")
		return nil
	}
	taken, err := l.claim(ctx, s)
	if err != nil || len(taken) == 0 {
		return err
	}
	l.Log.InfoContext(ctx, "learning from queued conversations", "items", len(taken))
	ids := map[string]bool{}
	for _, q := range taken {
		if id := q.id(); id != "" {
			ids[id] = true
		}
	}
	finish := context.WithoutCancel(ctx)
	written, err := l.write(ctx, s, taken, ids)
	if err != nil {
		l.Log.InfoContext(ctx, "learning failed; taken items returned to pending", "items", len(taken))
		return errors.Join(err, l.rewrite(finish, s, func(items []queuedItem) []queuedItem {
			for _, q := range items {
				if ids[q.id()] && q.get("status") == "processing" {
					q["status"] = pendingStatus
					delete(q, "processing_started_at")
				}
			}
			return items
		}))
	}
	if err := l.rewrite(finish, s, func(items []queuedItem) []queuedItem {
		return slices.DeleteFunc(items, func(q queuedItem) bool { return ids[q.id()] })
	}); err != nil {
		return err
	}
	l.Log.InfoContext(ctx, "learning finished", "written", written, "purged", len(taken))
	return nil
}

// claim returns stale processing items to pending and marks every other item pending. When 25 or more items are
// then pending, it takes the first 25, marks them processing since now, and moves them behind the other pending
// items; in either case the items still processing move to the end.
func (l *Learner) claim(ctx context.Context, s *config.Settings) ([]queuedItem, error) {
	now := l.Clock.Now().UnixMilli()
	cutoff := now - staleAfter.Milliseconds()
	var taken []queuedItem
	var pending, processing, stale int
	err := l.rewrite(ctx, s, func(items []queuedItem) []queuedItem {
		var waiting, busy []queuedItem
		stale = 0
		for _, q := range items {
			switch {
			case q.get("status") != "processing":
				q["status"] = pendingStatus
				waiting = append(waiting, q)
			case isStale(q.get("processing_started_at"), cutoff):
				q["status"] = pendingStatus
				delete(q, "processing_started_at")
				stale++
				waiting = append(waiting, q)
			default:
				busy = append(busy, q)
			}
		}
		pending, processing = len(waiting), len(busy)
		if len(waiting) < batchSize {
			taken = nil
			return slices.Concat(waiting, busy)
		}
		taken = waiting[:batchSize]
		for _, q := range taken {
			q["status"] = processingStatus
			q["processing_started_at"] = json.RawMessage(strconv.FormatInt(now, 10))
		}
		return slices.Concat(waiting[batchSize:], taken, busy)
	})
	if err != nil {
		return nil, err
	}
	l.Log.DebugContext(ctx, "learning queue", "pending", pending, "processing", processing, "stale_recovered", stale,
		"batch_size", batchSize, "selected", len(taken))
	if stale > 0 {
		l.Log.InfoContext(ctx, "stale processing items returned to pending", "items", stale)
	}
	if len(taken) == 0 {
		l.Log.InfoContext(ctx, "learning skipped: not enough pending items", "pending", pending)
	}
	return taken, nil
}

// write asks the fact model about taken, whose post IDs are ids, and adds the accepted items that no stored entry
// nearly repeats, all at once. It returns how many it added.
func (l *Learner) write(ctx context.Context, s *config.Settings, taken []queuedItem, ids map[string]bool) (int, error) {
	cat, err := l.Prompts.With(s.Prompts)
	if err != nil {
		return 0, fmt.Errorf("apply prompt overrides: %w", err)
	}
	provider, err := l.Model(s)
	if err != nil {
		return 0, fmt.Errorf("open model provider: %w", err)
	}
	failure := cat.Text("error.generic")
	client := llm.Client{Provider: provider, Failure: failure, CutOffNote: cat.Text("reply.truncated_note"), Log: l.Log}
	answer := client.Generate(ctx, model.Request{
		Model:     s.RoleModel(s.FactModel),
		Messages:  llm.Messages(cat.Text("kb.extract"), nil, sourceText(cat, taken)),
		MaxTokens: s.PublicKBTokens(),
	})
	l.Log.DebugContext(ctx, "learning model answer", "answer", logging.Clip(answer, debugClip))
	if strings.HasPrefix(answer, failure) {
		return 0, fmt.Errorf("model request failed: %s", answer)
	}
	items, err := parseAnswer(answer)
	if err != nil {
		return 0, err
	}
	var docs []vector.Doc
	for _, it := range items {
		reason := decide(it, ids)
		l.Log.DebugContext(ctx, "knowledge item", "reason", cmp.Or(reason, "accepted"),
			"item", logging.Clip(textfmt.Repr(it), debugClip))
		if reason == "non_dict" {
			continue
		}
		obj := it.(textfmt.Object)
		if reason != "" {
			l.Log.InfoContext(ctx, "knowledge item rejected", "reason", reason, "summary", shown(obj, "summary"),
				"rationale", shown(obj, "rationale"))
			continue
		}
		summary := textfmt.Strip(obj.Get("summary").(string))
		vec := llm.Embed(ctx, provider, s, l.Log, summary)
		if vec == nil {
			return 0, fmt.Errorf("embedding failed for the item %q", textfmt.Clip(summary, 60))
		}
		nearest, err := l.Store.Search(ctx, s.KBCollection, vec, map[string]string{"type": entryType}, 1)
		if err != nil {
			return 0, err
		}
		if len(nearest) > 0 && nearest[0].Distance < duplicateBelow {
			l.Log.InfoContext(ctx, "duplicate knowledge skipped", "distance", nearest[0].Distance,
				"summary", textfmt.Clip(summary, 60))
			continue
		}
		meta, err := metadata(obj, taken, l.Clock.Now())
		if err != nil {
			return 0, err
		}
		docs = append(docs, vector.Doc{Document: summary, Embedding: vec, Metadata: meta})
	}
	if len(docs) == 0 {
		l.Log.InfoContext(ctx, "no new knowledge found")
		return 0, nil
	}
	return len(docs), l.Store.Add(ctx, s.KBCollection, docs)
}

// rewrite replaces the queue's items with change's result, keeping no other member of the batch.
func (l *Learner) rewrite(ctx context.Context, s *config.Settings, change func([]queuedItem) []queuedItem) error {
	return l.Batch.Update(ctx, s.KBBatchKey, nil, func(cur json.RawMessage) (json.RawMessage, error) {
		raws, err := queued(cur)
		if err != nil {
			return nil, err
		}
		items := make([]queuedItem, len(raws))
		for i, raw := range raws {
			if json.Unmarshal(raw, &items[i]) != nil || items[i] == nil {
				return nil, errors.New("queued item is not an object")
			}
		}
		items = change(items)
		if items == nil {
			items = []queuedItem{}
		}
		return json.Marshal(map[string][]queuedItem{"messages": items})
	})
}

// queuedItem is a queued item's members as stored.
type queuedItem map[string]json.RawMessage

// get returns the member key decoded, or nil when there is none.
func (q queuedItem) get(key string) any {
	v, _ := textfmt.Decode(q[key], false)
	return v
}

// text returns the member key as text, or "" when it is falsy.
func (q queuedItem) text(key string) string { return orEmpty(q.get(key)) }

// id returns the post ID, or "" when there is none.
func (q queuedItem) id() string { return q.text("post_id") }

// isStale reports whether a processing start time is a nonzero number before cutoff; a boolean counts as 0 or 1.
func isStale(started any, cutoff int64) bool {
	switch v := started.(type) {
	case bool:
		return v && 1 < cutoff
	case int64:
		return v != 0 && v < cutoff
	case float64:
		return v != 0 && math.Trunc(v) < float64(cutoff)
	case *big.Int:
		return v.Sign() < 0
	}
	return false
}

// sourceText lays out taken for the fact model: one block per channel and thread, in the order first seen, each a
// header and the thread's items with their post IDs.
func sourceText(cat *prompt.Catalog, taken []queuedItem) string {
	type thread struct{ channel, context string }
	var order []thread
	members := map[thread][]queuedItem{}
	for _, q := range taken {
		t := thread{cmp.Or(q.text("channel_id"), "unknown"),
			cmp.Or(q.text("context_id"), q.text("root_id"), q.id(), "unknown")}
		if _, seen := members[t]; !seen {
			order = append(order, t)
		}
		members[t] = append(members[t], q)
	}
	blocks := make([]string, len(order))
	for i, t := range order {
		items := members[t]
		entries := make([]string, len(items))
		for j, q := range items {
			entries[j] = "source_id: " + cmp.Or(q.id(), "unknown") + "\n" + q.text("message")
		}
		header := cat.Render("kb.group_header",
			map[string]string{"channel_id": t.channel, "context_id": t.context, "root_id": items[0].text("root_id")})
		blocks[i] = header + strings.Join(entries, "\n")
	}
	return strings.Join(blocks, threadSeparator)
}

// parseAnswer reads the fact model's items from its answer, with a leading and a trailing code fence removed. An
// empty answer, NULL in any case, and [] give none; an object is one item. Other JSON and invalid JSON are errors.
func parseAnswer(answer string) ([]any, error) {
	text := textfmt.Strip(answer)
	if rest, ok := strings.CutPrefix(text, "```"); ok {
		text = strings.TrimPrefix(rest, "json")
	}
	if rest, ok := strings.CutSuffix(text, "```"); ok {
		text = rest
	}
	text = textfmt.Strip(text)
	if text == "" || strings.EqualFold(text, "null") || text == "[]" {
		return nil, nil
	}
	v, err := textfmt.Decode([]byte(text), true)
	if err != nil {
		return nil, fmt.Errorf("model answer is not JSON: %w", err)
	}
	switch v := v.(type) {
	case textfmt.Object:
		return []any{v}, nil
	case []any:
		return v, nil
	}
	return nil, fmt.Errorf("model answer is %s, not a list or an object", textfmt.Clip(textfmt.Repr(v), 60))
}

// decide returns why item, one of the fact model's items, may not be stored, or "" when it may. taken holds the
// post IDs it may cite.
func decide(item any, taken map[string]bool) string {
	obj, ok := item.(textfmt.Object)
	if !ok {
		return "non_dict"
	}
	store := obj.Get("store")
	if s, ok := store.(string); ok {
		store = slices.Contains([]string{"true", "1", "yes"}, textfmt.Lower(textfmt.Strip(s)))
	}
	if !textfmt.Truthy(store) {
		return "store_false"
	}
	conf := confidence(obj)
	if conf < minConfidence {
		return "confidence_below_threshold"
	}
	kind := memoryType(obj)
	if !slices.Contains(memoryTypes, kind) {
		return "invalid_memory_type"
	}
	if kind == "external_fact" && conf < factConfidence {
		return "external_fact_confidence_below_threshold"
	}
	if summary, _ := obj.Get("summary").(string); textfmt.Strip(summary) == "" {
		return "missing_summary"
	}
	ids := cleanList(obj.Get("source_ids"))
	if len(ids) == 0 {
		return "missing_source_ids"
	}
	for _, id := range ids {
		if !taken[id] {
			return "invalid_source_ids"
		}
	}
	if textfmt.Strip(orEmpty(obj.Get("rationale"))) == "" {
		return "missing_rationale"
	}
	return ""
}

// entryMetadata is a stored entry's metadata.
type entryMetadata struct {
	Type       string   `json:"type"`
	Timestamp  string   `json:"timestamp"`
	Topic      string   `json:"topic"`
	Tags       []string `json:"tags"`
	MemoryType string   `json:"memory_type"`
	// Confidence is a float literal, such as 1.0.
	Confidence json.RawMessage   `json:"confidence"`
	SourceIDs  []string          `json:"source_ids"`
	Channels   []json.RawMessage `json:"source_channel_ids"`
	Contexts   []json.RawMessage `json:"source_context_ids"`
	Roots      []json.RawMessage `json:"source_root_ids"`
	CreatedMin *int64            `json:"source_create_at_min,omitempty"`
	CreatedMax *int64            `json:"source_create_at_max,omitempty"`
}

// metadata describes the entry of item, made at now from the taken items it cites: their distinct truthy channel,
// thread, and root IDs as stored, and the range of their numeric creation times.
func metadata(item textfmt.Object, taken []queuedItem, now time.Time) (json.RawMessage, error) {
	byID := map[string]queuedItem{}
	for _, q := range taken {
		if id := q.id(); id != "" {
			byID[id] = q
		}
	}
	m := entryMetadata{Type: entryType, Timestamp: textfmt.FormatTime(now), Topic: textfmt.Strip(orEmpty(item.Get("topic"))),
		Tags: cleanList(item.Get("tags")), MemoryType: memoryType(item), SourceIDs: []string{},
		Confidence: json.RawMessage(textfmt.Repr(confidence(item))),
		Channels:   []json.RawMessage{}, Contexts: []json.RawMessage{}, Roots: []json.RawMessage{}}
	for _, id := range cleanList(item.Get("source_ids")) {
		q, ok := byID[id]
		if !ok {
			continue
		}
		m.SourceIDs = append(m.SourceIDs, id)
		m.Channels = appendNew(m.Channels, q, "channel_id")
		m.Contexts = appendNew(m.Contexts, q, "context_id")
		m.Roots = appendNew(m.Roots, q, "root_id")
		if t, ok := whole(q.get("create_at")); ok {
			if m.CreatedMin == nil {
				m.CreatedMin, m.CreatedMax = &t, &t
			}
			m.CreatedMin, m.CreatedMax = ptr(min(*m.CreatedMin, t)), ptr(max(*m.CreatedMax, t))
		}
	}
	return json.Marshal(m)
}

// appendNew appends q's member key as stored unless it is falsy or already in list.
func appendNew(list []json.RawMessage, q queuedItem, key string) []json.RawMessage {
	raw := q[key]
	if !textfmt.Truthy(q.get(key)) || slices.ContainsFunc(list, func(r json.RawMessage) bool { return string(r) == string(raw) }) {
		return list
	}
	return append(list, raw)
}

// whole returns a number truncated to an integer, a boolean as 0 or 1; it reports false for other values.
func whole(v any) (int64, bool) {
	switch v := v.(type) {
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	}
	return 0, false
}

func ptr(n int64) *int64 { return &n }

// confidence returns item's confidence as a number, or 0 when it is not one.
func confidence(item textfmt.Object) float64 {
	f, _ := textfmt.Float(item.Get("confidence"))
	return f
}

// memoryType returns item's memory type, stripped and lowercased.
func memoryType(item textfmt.Object) string {
	return textfmt.Lower(textfmt.Strip(orEmpty(item.Get("memory_type"))))
}

// cleanList returns the truthy members of a list as stripped text, without empty texts and repeats, in order. A
// value that is not a list gives none.
func cleanList(v any) []string {
	out := []string{}
	list, _ := v.([]any)
	for _, x := range list {
		if text := textfmt.Strip(orEmpty(x)); text != "" && !slices.Contains(out, text) {
			out = append(out, text)
		}
	}
	return out
}

// orEmpty returns v as text, or "" when it is falsy.
func orEmpty(v any) string {
	if !textfmt.Truthy(v) {
		return ""
	}
	return textfmt.Str(v)
}

// shown returns item's member key as a rejected item's log shows it: as text clipped to 80 characters, or "" when
// item has none.
func shown(item textfmt.Object, key string) string {
	for _, m := range item {
		if m.Key == key {
			return textfmt.Clip(textfmt.Str(m.Value), rejectedClip)
		}
	}
	return ""
}
