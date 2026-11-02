package kb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/llm"
	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/store/pg"
	"github.com/sarpaydemir/melinda/internal/store/vector"
	"github.com/sarpaydemir/melinda/internal/testkit/fakeclock"
)

const queryPrompt = `Write a search query. Output JSON: {"query": "..."} or NULL.`

var (
	now     = time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	discard = slog.New(slog.DiscardHandler)
)

// provider answers every chat request with one text, after calling onChat when set, and embeds every text as one
// vector, or fails to embed when it has none; it records the requests and the embedded texts.
type provider struct {
	answer   string
	vector   []float32
	onChat   func()
	requests []model.Request
	embedded []string
}

func (p *provider) Chat(_ context.Context, r model.Request) (model.Response, error) {
	p.requests = append(p.requests, r)
	if p.onChat != nil {
		p.onChat()
	}
	return model.Response{Message: model.Message{Role: model.RoleAssistant, Content: p.answer}}, nil
}

func (p *provider) Image(context.Context, string, string) (model.Image, error) {
	return model.Image{}, errors.New("no images")
}

func (p *provider) Embed(_ context.Context, m, text string) ([]float32, error) {
	p.embedded = append(p.embedded, m+": "+text)
	if p.vector == nil {
		return nil, errors.New("embedding unavailable")
	}
	return p.vector, nil
}

type search struct {
	collection string
	vec        []float32
	filter     map[string]string
	limit      int
}

// store answers every search with the same hits or error, and every add with addErr; it records the searches and
// the adds.
type store struct {
	hits     []vector.Hit
	err      error
	addErr   error
	searches []search
	added    []add
}

type add struct {
	collection string
	docs       []vector.Doc
}

func (s *store) Search(_ context.Context, collection string, vec []float32, filter map[string]string, limit int) ([]vector.Hit, error) {
	s.searches = append(s.searches, search{collection, vec, filter, limit})
	return s.hits, s.err
}

func (s *store) Add(_ context.Context, collection string, docs []vector.Doc) error {
	s.added = append(s.added, add{collection, docs})
	return s.addErr
}

func queryVector() []float32 {
	v := make([]float32, pg.EmbeddingDims)
	v[0] = 1
	return v
}

func settings() *config.Settings {
	return &config.Settings{Model: "main", FactModel: "fact", EmbeddingModel: "embed", KBCollection: "agent_public_kb"}
}

func retriever(s *config.Settings, p *provider, st *store, log *slog.Logger) Retriever {
	return Retriever{
		Settings:    s,
		Model:       llm.Client{Provider: p, Failure: "Something went wrong.", Log: log},
		Embedder:    p,
		Store:       st,
		QueryPrompt: queryPrompt,
		Clock:       fakeclock.New(now),
		Log:         log,
	}
}

func hit(doc, metadata string, distance float64) vector.Hit {
	return vector.Hit{ID: doc, Document: &doc, Metadata: json.RawMessage(metadata), Distance: distance}
}

func TestPreloadShowsTheBestRankedEntriesForTheGeneratedQuery(t *testing.T) {
	for _, factModel := range []string{"fact", ""} {
		t.Run("fact_model "+factModel, func(t *testing.T) {
			s := settings()
			s.FactModel = factModel
			p := &provider{answer: `{"query": "release notes"}`, vector: queryVector()}
			st := &store{hits: []vector.Hit{
				hit("Deploys run on Tuesdays.", `{"type": "public_kb"}`, 0.10),
				hit("Release notes live in the wiki.", `{"type": "public_kb", "timestamp": "2026-01-05T09:00:00+00:00"}`, 0.12),
				hit("The release checklist changed in June.", `{"type": "public_kb", "source_create_at_max": 1781600000000}`, 0.13),
				hit("Hotfixes skip the freeze.", `{"type": "public_kb"}`, 0.20),
				hit("Old naming rules.", `{"type": "public_kb", "timestamp": "2019-03-01T00:00:00+00:00"}`, 0.21),
				hit("Unrelated entry.", `{"type": "public_kb"}`, 0.22),
			}}

			got := retriever(s, p, st, discard).Preload(context.Background(), "user_id: u1\nwhere are the release notes?")

			want := "- The release checklist changed in June.\n- Deploys run on Tuesdays.\n" +
				"- Release notes live in the wiki.\n- Hotfixes skip the freeze."
			if got != want {
				t.Errorf("Preload = %q, want %q", got, want)
			}
			wantRequests := []model.Request{{
				Model: map[string]string{"fact": "fact", "": "main"}[factModel],
				Messages: []model.Message{
					{Role: model.RoleSystem, Content: queryPrompt},
					{Role: model.RoleUser, Content: "user_id: u1\nwhere are the release notes?"},
				},
			}}
			if !reflect.DeepEqual(p.requests, wantRequests) {
				t.Errorf("model requests = %+v, want %+v", p.requests, wantRequests)
			}
			if want := []string{"embed: release notes"}; !reflect.DeepEqual(p.embedded, want) {
				t.Errorf("embedded %q, want %q", p.embedded, want)
			}
			wantSearch := []search{{"agent_public_kb", queryVector(), map[string]string{"type": "public_kb"}, 20}}
			if !reflect.DeepEqual(st.searches, wantSearch) {
				t.Errorf("searches = %+v, want %+v", st.searches, wantSearch)
			}
		})
	}
}

func TestPreloadShowsAnEntryWithoutADocumentAsNone(t *testing.T) {
	p := &provider{answer: `{"query": "release notes"}`, vector: queryVector()}
	st := &store{hits: []vector.Hit{
		hit("first entry", `{"type": "public_kb"}`, 0.1),
		{ID: "no document", Metadata: json.RawMessage(`{"type": "public_kb"}`), Distance: 0.1},
		hit("", `{"type": "public_kb"}`, 0.1),
	}}

	got := retriever(settings(), p, st, discard).Preload(context.Background(), "message")
	if want := "- first entry\n- None\n- "; got != want {
		t.Errorf("Preload = %q, want %q", got, want)
	}
}

func TestPreloadReadsTheQueryFromTheModelAnswer(t *testing.T) {
	cases := []struct{ answer, query string }{
		{`{"query": "  release notes  "}`, "release notes"},
		{`{"query": 5}`, "5"},
		{`{"query": true}`, "True"},
		{`{"query": ["a", "b"]}`, "['a', 'b']"},
		{`{"query": null}`, ""},
		{`{"query": 0}`, ""},
		{`{"query": ""}`, ""},
		{`{"query": " \t "}`, ""},
		{`{"topic": "x"}`, ""},
		{`"  vacation policy "`, "vacation policy"},
		{`"null"`, ""},
		{`"Null"`, ""},
		{`" NULL "`, "NULL"},
		{`NULL`, ""},
		{`["a"]`, ""},
		{`42`, ""},
	}
	for _, c := range cases {
		t.Run(c.answer, func(t *testing.T) {
			p := &provider{answer: c.answer, vector: queryVector()}
			st := &store{}
			retriever(settings(), p, st, discard).Preload(context.Background(), "message")

			var want []string
			if c.query != "" {
				want = []string{"embed: " + c.query}
			}
			if !reflect.DeepEqual(p.embedded, want) {
				t.Errorf("embedded %q, want %q", p.embedded, want)
			}
			if len(st.searches) != len(want) {
				t.Errorf("%d searches, want %d", len(st.searches), len(want))
			}
		})
	}
}

func TestPreloadNeedsAnEmbeddingModel(t *testing.T) {
	s := settings()
	s.EmbeddingModel = ""
	p := &provider{answer: `{"query": "release notes"}`, vector: queryVector()}
	st := &store{hits: []vector.Hit{hit("entry", `{}`, 0.1)}}

	if got := retriever(s, p, st, discard).Preload(context.Background(), "message"); got != "" {
		t.Errorf("Preload = %q, want nothing", got)
	}
	if len(p.requests)+len(p.embedded)+len(st.searches) != 0 {
		t.Errorf("made %d model requests, %d embeddings, %d searches; want none", len(p.requests), len(p.embedded),
			len(st.searches))
	}
}

func TestPreloadSearchesNothingWithoutAQueryEmbedding(t *testing.T) {
	p := &provider{answer: `{"query": "release notes"}`}
	st := &store{hits: []vector.Hit{hit("entry", `{}`, 0.1)}}

	if got := retriever(settings(), p, st, discard).Preload(context.Background(), "message"); got != "" {
		t.Errorf("Preload = %q, want nothing", got)
	}
	if len(st.searches) != 0 {
		t.Errorf("searched %+v, want no search", st.searches)
	}
}

func TestPreloadLogsAFailedSearchAndShowsNothing(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	p := &provider{answer: `{"query": "release notes"}`, vector: queryVector()}
	st := &store{err: errors.New("connection refused")}

	if got := retriever(settings(), p, st, log).Preload(context.Background(), "message"); got != "" {
		t.Errorf("Preload = %q, want nothing", got)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "connection refused") {
		t.Errorf("log lacks the search error:\n%s", logs.String())
	}
}

func TestPreloadLogsTheQueryAtDebug(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := &provider{answer: `{"query": "release notes"}`, vector: queryVector()}

	retriever(settings(), p, &store{}, log).Preload(context.Background(), "message")

	if !strings.Contains(logs.String(), `query="release notes"`) {
		t.Errorf("debug log lacks the query:\n%s", logs.String())
	}
}

func TestSearchReturnsTheNearestEntriesWithoutRecencyRanking(t *testing.T) {
	p := &provider{vector: queryVector()}
	nearest := []vector.Hit{
		hit("Deploys run on Tuesdays.", `{"type": "public_kb"}`, 0.10),
		hit("The release checklist changed in June.", `{"type": "public_kb", "source_create_at_max": 1781600000000}`, 0.13),
	}
	st := &store{hits: nearest}

	got, err := retriever(settings(), p, st, discard).Search(context.Background(), "release notes", 3)

	if err != nil || !reflect.DeepEqual(got, nearest) {
		t.Errorf("Search = %+v, %v; want the entries in search order", got, err)
	}
	if want := []string{"embed: release notes"}; !reflect.DeepEqual(p.embedded, want) || len(p.requests) != 0 {
		t.Errorf("embedded %q after %d model requests, want %q and no requests", p.embedded, len(p.requests), want)
	}
	wantSearch := []search{{"agent_public_kb", queryVector(), map[string]string{"type": "public_kb"}, 3}}
	if !reflect.DeepEqual(st.searches, wantSearch) {
		t.Errorf("searches = %+v, want %+v", st.searches, wantSearch)
	}
}

func TestSearchReportsWhyItFoundNothing(t *testing.T) {
	refused := errors.New("connection refused")
	noModel := settings()
	noModel.EmbeddingModel = ""
	cases := []struct {
		name     string
		settings *config.Settings
		vector   []float32
		err      error
		want     error
		searches int
	}{
		{"no embedding model", noModel, queryVector(), nil, ErrNoEmbeddingModel, 0},
		{"no query embedding", settings(), nil, nil, ErrNotEmbedded, 0},
		{"a failed search", settings(), queryVector(), refused, refused, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &store{err: c.err}
			got, err := retriever(c.settings, &provider{vector: c.vector}, st, discard).Search(context.Background(), "q", 4)
			if got != nil || !errors.Is(err, c.want) || len(st.searches) != c.searches {
				t.Errorf("Search = %+v, %v after %d searches; want %v after %d", got, err, len(st.searches), c.want, c.searches)
			}
		})
	}
}

func TestRerankPrefersRecentEntriesAmongNearEqualMatches(t *testing.T) {
	fresh := `{"timestamp": "2026-06-17T12:00:00+00:00"}`
	cases := []struct {
		name  string
		hits  []vector.Hit
		limit int
		want  []string
	}{
		{"a 0.05 match stays ahead of a fresh 0.35 match",
			[]vector.Hit{hit("best", `{}`, 0.05), hit("worst", fresh, 0.35)}, 2, []string{"best", "worst"}},
		{"at equal distance the newer entry comes first",
			[]vector.Hit{hit("old", `{"timestamp": "2020-01-01T00:00:00+00:00"}`, 0.05), hit("new", fresh, 0.05)}, 2,
			[]string{"new", "old"}},
		{"the limit is honoured", tenEntries(), 3, []string{"doc0", "doc1", "doc2"}},
		{"an entry 0.08 from the best match is boosted",
			[]vector.Hit{hit("a", `{}`, 0.10), hit("b", `{}`, 0.15), hit("c", fresh, 0.18)}, 4, []string{"a", "c", "b"}},
		{"an entry just beyond 0.08 from the best match is not",
			[]vector.Hit{hit("a", `{}`, 0.10), hit("b", `{}`, 0.15), hit("c", fresh, 0.18000000000000002)}, 4,
			[]string{"a", "b", "c"}},
		{"the band is 15% of a far best match",
			[]vector.Hit{hit("a", `{}`, 0.80), hit("b", `{}`, 0.90), hit("c", fresh, 0.92)}, 4, []string{"a", "c", "b"}},
		{"an entry just beyond 15% of a far best match is not boosted",
			[]vector.Hit{hit("a", `{}`, 0.80), hit("b", `{}`, 0.90), hit("c", fresh, 0.9200000000000002)}, 4,
			[]string{"a", "b", "c"}},
		{"a fresh entry 0.05 behind overtakes",
			[]vector.Hit{hit("t", `{}`, 0.10), hit("s", fresh, 0.15)}, 4, []string{"s", "t"}},
		{"millisecond creation times count",
			[]vector.Hit{hit("a", `{}`, 0.10), hit("m", `{"source_create_at_max": 1781697600000}`, 0.14)}, 4,
			[]string{"m", "a"}},
		{"an entry with an unusable time is not boosted",
			[]vector.Hit{hit("a", `{}`, 0.10), hit("o", `{"timestamp": 1e20}`, 0.10)}, 4, []string{"a", "o"}},
		{"ties keep the search order",
			[]vector.Hit{hit("p", `{}`, 0.2), hit("q", `{}`, 0.2), hit("r", `{}`, 0.2)}, 4, []string{"p", "q", "r"}},
		{"an entry without a distance stays last",
			[]vector.Hit{hit("a", `{}`, 0.1), hit("b", `{}`, 0.2), hit("n", fresh, math.NaN())}, 4, []string{"a", "b", "n"}},
		{"no entries", nil, 4, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, h := range rerank(c.hits, now, c.limit) {
				got = append(got, *h.Document)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("rerank = %q, want %q", got, c.want)
			}
		})
	}
}

func tenEntries() []vector.Hit {
	var hits []vector.Hit
	for i := range 10 {
		hits = append(hits, hit(fmt.Sprint("doc", i), `{}`, 0.1+float64(i)*0.01))
	}
	return hits
}

func TestEntryTimeComesFromTheCreationTimeOrTheTimestamp(t *testing.T) {
	cases := []struct {
		metadata string
		want     string // RFC 3339 in UTC; empty for no time
	}{
		{`{"source_create_at_max": 1780000000000}`, "2026-05-28T20:26:40Z"},
		{`{"timestamp": 1700000000}`, "2023-11-14T22:13:20Z"},
		{`{"timestamp": "2026-06-17T12:00:00+00:00"}`, "2026-06-17T12:00:00Z"},
		{`{"timestamp": "2026-06-17T15:30:00.123456+03:00"}`, "2026-06-17T12:30:00.123456Z"},
		{`{"timestamp": "2026-06-17T12:00:00Z"}`, "2026-06-17T12:00:00Z"},
		{`{"timestamp": "2026-06-17"}`, "2026-06-17T00:00:00Z"},
		{`{"timestamp": "yesterday"}`, ""},
		{`{"timestamp": "   "}`, ""},
		{`{"no_time": "here"}`, ""},
		{`{"timestamp": ["2026-06-17"]}`, ""},
		{`{"source_create_at_max": 1780000000000, "timestamp": "2020-01-01T00:00:00+00:00"}`, "2026-05-28T20:26:40Z"},
		{`{"source_create_at_max": "2025-01-02T03:04:05+00:00", "timestamp": 1700000000}`, "2025-01-02T03:04:05Z"},
		{`{"source_create_at_max": 0, "timestamp": "2020-01-01T00:00:00+00:00"}`, "2020-01-01T00:00:00Z"},
		{`{"source_create_at_max": "", "timestamp": 1700000000}`, "2023-11-14T22:13:20Z"},
		{`{"source_create_at_max": 0}`, ""},
		{`{"timestamp": 0}`, "1970-01-01T00:00:00Z"},
		{`{"timestamp": false}`, "1970-01-01T00:00:00Z"},
		{`{"source_create_at_max": true}`, "1970-01-01T00:00:01Z"},
		{`{"timestamp": 10000000000}`, "2286-11-20T17:46:40Z"},
		{`{"timestamp": 10000000001}`, "1970-04-26T17:46:40.001Z"},
		{`{"timestamp": 10000000000.0}`, "2286-11-20T17:46:40Z"},
		{`{"timestamp": 10000000000.000002}`, "1970-04-26T17:46:40Z"},
		{`{"timestamp": 1780000000123.4565}`, "2026-05-28T20:26:40.123456Z"},
		{`{"timestamp": 1700000000.0000005}`, "2023-11-14T22:13:20Z"},
		{`{"timestamp": 1700000000.0000015}`, "2023-11-14T22:13:20.000001Z"},
		{`{"timestamp": 1700000000.25}`, "2023-11-14T22:13:20.25Z"},
		{`{"timestamp": -1}`, "1969-12-31T23:59:59Z"},
		{`{"timestamp": -0.0000005}`, "1970-01-01T00:00:00Z"},
		{`{"timestamp": -1.0000005}`, "1969-12-31T23:59:58.999999Z"},
		{`{"timestamp": -62135596800}`, "0001-01-01T00:00:00Z"},
		{`{"timestamp": 253402300799999}`, "9999-12-31T23:59:59.998993Z"},
		{`{"timestamp": -62135596801}`, ""},
		{`{"timestamp": 253402300800000}`, ""},
		{`{"timestamp": 1e20}`, ""},
		{`{"timestamp": 99999999999999999999999}`, ""},
		{`{"timestamp": 1e400}`, ""},
	}
	for _, c := range cases {
		t.Run(c.metadata, func(t *testing.T) {
			got, ok := entryTime(json.RawMessage(c.metadata))
			if s := got.UTC().Format(time.RFC3339Nano); ok != (c.want != "") || ok && s != c.want {
				t.Errorf("entryTime = %s, %t; want %q", s, ok, c.want)
			}
		})
	}
}

func TestRecencyBoostFallsWithAgeToHalfAtThirtyDays(t *testing.T) {
	cases := []struct {
		made string
		want float64
	}{
		{"2026-06-17T12:00:00+00:00", 0.05},
		{"2026-06-17T11:59:59.999999+00:00", 0.04999999999998072},
		{"2026-06-16T12:00:00+00:00", 0.04838709677419355},
		{"2026-05-18T12:00:00+00:00", 0.025},
		{"2025-06-17T12:00:00+00:00", 0.00379746835443038},
		{"2026-08-16T12:00:00+00:00", 0.05},
		{"0001-01-01T00:00:00+00:00", 2.0275380214067463e-06},
	}
	for _, c := range cases {
		t.Run(c.made, func(t *testing.T) {
			made, ok := entryTime(json.RawMessage(`{"timestamp": "` + c.made + `"}`))
			if !ok {
				t.Fatal("no entry time")
			}
			if got := boost(now, made); got != c.want {
				t.Errorf("boost = %v, want %v", got, c.want)
			}
		})
	}
}
