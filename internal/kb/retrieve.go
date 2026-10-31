package kb

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/llm"
	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/store/vector"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

const (
	// entryType is the metadata type of a shared-knowledge entry.
	entryType = "public_kb"
	// Entries is how many entries a reply's prompt shows, and how many a knowledge search tool returns by default.
	Entries = 4
	// candidates is how many of the nearest entries the recency ranking chooses from.
	candidates = 5 * Entries
	// msEpochAbove marks epoch numbers in milliseconds; smaller ones are seconds.
	msEpochAbove = 10_000_000_000
	// firstSecond and lastSecond bound the epoch seconds of the years 1 to 9999.
	firstSecond = -62135596800
	lastSecond  = 253402300799
)

var (
	// ErrNoEmbeddingModel reports a search while no embedding model is set.
	ErrNoEmbeddingModel = errors.New("no embedding model is set")
	// ErrNotEmbedded reports a search whose query could not be embedded.
	ErrNotEmbedded = errors.New("the query could not be embedded")
)

// Retriever finds the shared knowledge relevant to a message, with one settings snapshot. Every field is required.
type Retriever struct {
	Settings *config.Settings
	// Model writes the search query.
	Model interface {
		GenerateJSON(ctx context.Context, r model.Request) any
	}
	Embedder interface {
		Embed(ctx context.Context, model, text string) ([]float32, error)
	}
	Store interface {
		Search(ctx context.Context, collection string, vec []float32, filter map[string]string, limit int) ([]vector.Hit, error)
	}
	// QueryPrompt is the catalog entry kb.query.
	QueryPrompt string
	Clock       clock.Clock
	Log         *slog.Logger
}

// Preload returns the entries nearest to a search query the fact model writes for text, preferring recent ones
// among near-equal matches, as "- <document>" lines. It is empty when no embedding model is set, when there is no
// query or query embedding, and when the search fails, which is logged.
func (r Retriever) Preload(ctx context.Context, text string) string {
	s := r.Settings
	if s.EmbeddingModel == "" {
		return ""
	}
	query := searchQuery(r.Model.GenerateJSON(ctx, model.Request{
		Model:    s.RoleModel(s.FactModel),
		Messages: llm.Messages(r.QueryPrompt, nil, text),
	}))
	r.Log.DebugContext(ctx, "knowledge query", "query", query)
	if query == "" {
		return ""
	}
	hits, err := r.Search(ctx, query, candidates)
	if err != nil {
		if !errors.Is(err, ErrNotEmbedded) && ctx.Err() == nil {
			r.Log.ErrorContext(ctx, "knowledge search failed", "collection", s.KBCollection, "error", err)
		}
		return ""
	}
	var lines []string
	for _, h := range rerank(hits, r.Clock.Now(), Entries) {
		doc := textfmt.Str(nil) // an entry without a document shows as None
		if h.Document != nil {
			doc = *h.Document
		}
		lines = append(lines, "- "+doc)
	}
	return strings.Join(lines, "\n")
}

// Search returns the limit entries nearest to query, nearest first. Without an embedding model or a query
// embedding it fails with ErrNoEmbeddingModel or ErrNotEmbedded.
func (r Retriever) Search(ctx context.Context, query string, limit int) ([]vector.Hit, error) {
	s := r.Settings
	if s.EmbeddingModel == "" {
		return nil, ErrNoEmbeddingModel
	}
	vec := llm.Embed(ctx, r.Embedder, s, r.Log, query)
	if vec == nil {
		return nil, ErrNotEmbedded
	}
	return r.Store.Search(ctx, s.KBCollection, vec, map[string]string{"type": entryType}, limit)
}

// searchQuery reads the query from the model's answer: an object's truthy query member, or a string other than
// NULL, stripped.
func searchQuery(answer any) string {
	switch a := answer.(type) {
	case textfmt.Object:
		if q := a.Get("query"); textfmt.Truthy(q) {
			return textfmt.Strip(textfmt.Str(q))
		}
	case string:
		if !strings.EqualFold(a, "null") {
			return textfmt.Strip(a)
		}
	}
	return ""
}

// rerank returns the first limit hits by distance less a recency boost, which only the hits within a relevance
// band of the best distance get; equal scores keep the search order.
func rerank(hits []vector.Hit, now time.Time, limit int) []vector.Hit {
	if len(hits) == 0 {
		return nil
	}
	best := hits[0].Distance
	for _, h := range hits[1:] {
		if h.Distance < best {
			best = h.Distance
		}
	}
	band := max(0.08, best*0.15)
	type scored struct {
		hit   vector.Hit
		score float64
	}
	ranked := make([]scored, len(hits))
	for i, h := range hits {
		ranked[i] = scored{h, h.Distance}
		if h.Distance <= best+band {
			if made, ok := entryTime(h.Metadata); ok {
				ranked[i].score -= boost(now, made)
			}
		}
	}
	// A NaN score compares as neither less nor greater, so it keeps its place instead of sorting first.
	slices.SortStableFunc(ranked, func(a, b scored) int {
		switch {
		case a.score < b.score:
			return -1
		case a.score > b.score:
			return 1
		}
		return 0
	})
	ranked = ranked[:min(limit, len(ranked))]
	out := make([]vector.Hit, len(ranked))
	for i, s := range ranked {
		out[i] = s.hit
	}
	return out
}

// boost is 0.05 for an entry made at or after now, falling to half of that at 30 days old.
func boost(now, made time.Time) float64 {
	age := float64(now.UnixMicro()-made.UnixMicro()) / 1e6
	days := max(0, age/86400)
	return 0.05 / (1 + days/30)
}

// entryTime returns when an entry was made: its metadata's source_create_at_max when truthy, else its timestamp.
// A string is an ISO 8601 time, in UTC when it has no offset. A number (a boolean counts as 0 or 1) is epoch
// milliseconds above 10^10 and epoch seconds otherwise. It reports false for anything else and for a time outside
// the years 1 to 9999.
func entryTime(metadata json.RawMessage) (time.Time, bool) {
	v, _ := textfmt.Decode(metadata, true)
	obj, _ := v.(textfmt.Object)
	value := obj.Get("source_create_at_max")
	if !textfmt.Truthy(value) {
		value = obj.Get("timestamp")
	}
	var secs float64
	switch value := value.(type) {
	case string:
		t, _, err := textfmt.ParseTime(value)
		return t, err == nil
	case bool:
		if value {
			secs = 1
		}
	case int64:
		secs = float64(value) // exact for every value within the years 1 to 9999, in seconds or milliseconds
	case float64:
		secs = value
	default:
		return time.Time{}, false // integers beyond int64 are far outside the years 1 to 9999
	}
	if secs > msEpochAbove {
		secs /= 1000
	}
	return epoch(secs)
}

// epoch returns the time secs seconds after the Unix epoch, rounded half to even to the microsecond, and whether
// it falls within the years 1 to 9999.
func epoch(secs float64) (time.Time, bool) {
	whole, frac := math.Modf(secs)
	us := math.RoundToEven(frac * 1e6)
	switch {
	case us >= 1e6:
		whole, us = whole+1, us-1e6
	case us < 0:
		whole, us = whole-1, us+1e6
	}
	if !(whole >= firstSecond && whole <= lastSecond) { // NaN fails too
		return time.Time{}, false
	}
	return time.Unix(int64(whole), int64(us)*1000).UTC(), true
}
