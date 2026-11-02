package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/llm"
	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/store/pg"
	"github.com/sarpaydemir/melinda/internal/store/vector"
)

const factsPrompt = `Extract facts. Output JSON array: [{"fact": "..."}] or NULL.`

var discard = slog.New(slog.DiscardHandler)

// provider answers every chat request with one text and embeds every text as one vector, except the texts in
// unembeddable; it records the requests and the embedded texts.
type provider struct {
	answer       string
	unembeddable []string
	requests     []model.Request
	embedded     []string
}

func (p *provider) Chat(_ context.Context, r model.Request) (model.Response, error) {
	p.requests = append(p.requests, r)
	return model.Response{Message: model.Message{Role: model.RoleAssistant, Content: p.answer}}, nil
}

func (p *provider) Embed(_ context.Context, m, text string) ([]float32, error) {
	p.embedded = append(p.embedded, m+": "+text)
	if slices.Contains(p.unembeddable, text) {
		return nil, errors.New("embedding unavailable")
	}
	return unit(), nil
}

type search struct {
	collection string
	filter     map[string]string
	limit      int
}

type add struct {
	collection string
	docs       []vector.Doc
}

// store answers successive searches with successive hit lists, then with none; it records the searches and adds.
type store struct {
	hits      [][]vector.Hit
	searchErr error
	addErr    error
	searches  []search
	adds      []add
}

func (s *store) Search(_ context.Context, collection string, _ []float32, filter map[string]string, limit int) ([]vector.Hit, error) {
	s.searches = append(s.searches, search{collection, filter, limit})
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	if len(s.hits) == 0 {
		return nil, nil
	}
	hits := s.hits[0]
	s.hits = s.hits[1:]
	return hits, nil
}

func (s *store) Add(_ context.Context, collection string, docs []vector.Doc) error {
	s.adds = append(s.adds, add{collection, docs})
	return s.addErr
}

// stored returns the documents of every add, in order.
func (s *store) stored() []string {
	var docs []string
	for _, a := range s.adds {
		for _, d := range a.docs {
			docs = append(docs, d.Document)
		}
	}
	return docs
}

// turns serves the same items for every scoped read and records the reads.
type turns struct {
	items []json.RawMessage
	err   error
	reads []string
}

func (h *turns) Scoped(_ context.Context, key, scope string, n int) ([]json.RawMessage, error) {
	h.reads = append(h.reads, fmt.Sprint(key, " ", scope, " ", n))
	return h.items, h.err
}

func unit() []float32 {
	v := make([]float32, pg.EmbeddingDims)
	v[0] = 1
	return v
}

func settings() *config.Settings {
	return &config.Settings{Model: "main", FactModel: "fact", EmbeddingModel: "embed", MemoryKeyPrefix: "agent",
		FactsCollection: "agent_memory"}
}

func facts(s *config.Settings, p *provider, st *store, h *turns, log *slog.Logger) Facts {
	return Facts{
		Settings: s,
		Model:    llm.Client{Provider: p, Failure: "Something went wrong.", Log: log},
		Embedder: p,
		Store:    st,
		History:  h,
		Prompt:   factsPrompt,
		Log:      log,
	}
}

func hit(doc string, distance float64) vector.Hit {
	return vector.Hit{ID: doc, Document: &doc, Distance: distance}
}

func recentTurns() *turns {
	return &turns{items: []json.RawMessage{
		json.RawMessage(`{"user": "u1: I moved to Izmir", "assistant": "Noted.", "scope": "public"}`),
		json.RawMessage(`{"user": "u1: I work on billing", "assistant": "Got it.", "scope": "public"}`),
	}}
}

var (
	channel = chat.Message{ID: "p1", AuthorID: "u1", Kind: chat.Public, Text: "u1: I work on billing"}
	direct  = chat.Message{ID: "p1", AuthorID: "u1", Kind: chat.Direct, Text: "u1: I moved to Izmir"}
)

func TestRecallShowsTheAuthorsNearestFactsForTheMessageText(t *testing.T) {
	cases := []struct {
		name   string
		kind   chat.Kind
		filter map[string]string
	}{
		{"direct message: private and public facts", chat.Direct, map[string]string{"key": "agent:u1"}},
		{"channel: public facts", chat.Public, map[string]string{"key": "agent:u1", "scope": "public"}},
		{"private channel: public facts", chat.Private, map[string]string{"key": "agent:u1", "scope": "public"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &provider{}
			st := &store{hits: [][]vector.Hit{{hit("Works on billing.", 0.12), hit("Prefers short answers.", 0.31)}}}
			m := chat.Message{ID: "p1", AuthorID: "u1", Kind: c.kind, Text: "u1: who handles invoices?\nu1: and refunds?"}

			got := facts(settings(), p, st, &turns{}, discard).Recall(context.Background(), m)

			if want := "Works on billing.\nPrefers short answers."; got != want {
				t.Errorf("Recall = %q, want %q", got, want)
			}
			if want := []string{"embed: u1: who handles invoices?\nu1: and refunds?"}; !reflect.DeepEqual(p.embedded, want) {
				t.Errorf("embedded %q, want %q", p.embedded, want)
			}
			if want := []search{{"agent_memory", c.filter, 10}}; !reflect.DeepEqual(st.searches, want) {
				t.Errorf("searches = %+v, want %+v", st.searches, want)
			}
		})
	}
}

func TestRecallSkipsFactsWithoutADocument(t *testing.T) {
	st := &store{hits: [][]vector.Hit{{hit("first", 0.1), {ID: "no document", Distance: 0.2}, hit("", 0.3), hit("last", 0.4)}}}

	got := facts(settings(), &provider{}, st, &turns{}, discard).Recall(context.Background(), direct)

	if want := "first\n\nlast"; got != want {
		t.Errorf("Recall = %q, want %q", got, want)
	}
}

func TestRecallNeedsAnEmbeddingModelAndAMessageEmbedding(t *testing.T) {
	noModel := settings()
	noModel.EmbeddingModel = ""
	cases := []struct {
		name     string
		settings *config.Settings
		p        *provider
		embedded int
	}{
		{"no embedding model", noModel, &provider{}, 0},
		{"no message embedding", settings(), &provider{unembeddable: []string{direct.Text}}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &store{hits: [][]vector.Hit{{hit("Works on billing.", 0.1)}}}
			if got := facts(c.settings, c.p, st, &turns{}, discard).Recall(context.Background(), direct); got != "" {
				t.Errorf("Recall = %q, want nothing", got)
			}
			if len(c.p.embedded) != c.embedded || len(st.searches) != 0 {
				t.Errorf("%d embeddings and %d searches, want %d and none", len(c.p.embedded), len(st.searches), c.embedded)
			}
		})
	}
}

func TestRecallLogsAFailedSearchAndShowsNothing(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	st := &store{searchErr: errors.New("connection refused")}

	if got := facts(settings(), &provider{}, st, &turns{}, log).Recall(context.Background(), direct); got != "" {
		t.Errorf("Recall = %q, want nothing", got)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "connection refused") {
		t.Errorf("log lacks the search error:\n%s", logs.String())
	}
}

func TestExtractRunsAfterEveryFifthTurnWithASuccessfulReply(t *testing.T) {
	noModel := settings()
	noModel.EmbeddingModel = ""
	cases := []struct {
		name     string
		settings *config.Settings
		reply    string
		count    int
		turns    *turns
		reads    int
		requests int
	}{
		{"fifth turn", settings(), "Noted.", 5, recentTurns(), 1, 1},
		{"fourth turn", settings(), "Noted.", 4, recentTurns(), 0, 0},
		{"sixth turn", settings(), "Noted.", 6, recentTurns(), 0, 0},
		{"tenth turn", settings(), "Noted.", 10, recentTurns(), 1, 1},
		{"no embedding model", noModel, "Noted.", 5, recentTurns(), 0, 0},
		{"failure reply", settings(), "Something went wrong. [ERR_EMPTY_RESPONSE]", 5, recentTurns(), 0, 0},
		{"reply quoting a failure marker", settings(), "It said [ERR_ here", 10, recentTurns(), 0, 0},
		{"no turns in the message's scope", settings(), "Noted.", 5, &turns{}, 1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &provider{answer: "NULL"}
			facts(c.settings, p, &store{}, c.turns, discard).Extract(context.Background(), channel, c.reply, c.count)
			if len(c.turns.reads) != c.reads || len(p.requests) != c.requests {
				t.Errorf("%d history reads and %d model requests, want %d and %d", len(c.turns.reads), len(p.requests),
					c.reads, c.requests)
			}
		})
	}
}

func TestExtractAsksTheFactModelAboutTheAuthorsRecentTurnsOfTheMessageScope(t *testing.T) {
	cases := []struct {
		name      string
		m         chat.Message
		factModel string
		read      string
		model     string
	}{
		{"channel", channel, "fact", "u1 public 5", "fact"},
		{"direct message", direct, "fact", "u1 private 5", "fact"},
		{"no fact model", channel, "", "u1 public 5", "main"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := settings()
			s.FactModel = c.factModel
			p := &provider{answer: "NULL"}
			h := recentTurns()

			facts(s, p, &store{}, h, discard).Extract(context.Background(), c.m, "Got it.", 5)

			if want := []string{c.read}; !reflect.DeepEqual(h.reads, want) {
				t.Errorf("history reads = %q, want %q", h.reads, want)
			}
			want := []model.Request{{
				Model: c.model,
				Messages: []model.Message{
					{Role: model.RoleSystem, Content: factsPrompt},
					{Role: model.RoleUser, Content: "User: u1: I moved to Izmir\nAssistant: Noted.\nUser: u1: I work on billing\nAssistant: Got it."},
				},
			}}
			if !reflect.DeepEqual(p.requests, want) {
				t.Errorf("model requests = %+v, want %+v", p.requests, want)
			}
		})
	}
}

func TestExtractReadsTheFactsFromTheModelAnswer(t *testing.T) {
	cases := []struct {
		answer string
		facts  []string
		warn   bool
	}{
		{`[]`, nil, false},
		{`{}`, nil, false},
		{`0`, nil, false},
		{`""`, nil, false},
		{`false`, nil, false},
		{`{"fact": "Works on billing."}`, []string{"Works on billing."}, false},
		{`{"topic": "billing"}`, nil, false},
		{`[{"fact": "a"}, {"fact": ""}, {"fact": null}, {"fact": 5}, {"fact": true}, {"fact": ["l"]}, {"other": 1}]`, []string{"a"}, false},
		{`[" spaced ", "", 0, 1.5, true, false, null, [1, "b"], {"fact": "  "}, "x"]`,
			[]string{" spaced ", "0", "1.5", "True", "False", "None", "[1, 'b']", "  ", "x"}, false},
		{`"abc"`, nil, true},
		{`5`, nil, true},
		{`true`, nil, true},
		{`1.5`, nil, true},
	}
	for _, c := range cases {
		t.Run(c.answer, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			p := &provider{answer: c.answer}
			st := &store{}

			facts(settings(), p, st, recentTurns(), log).Extract(context.Background(), channel, "Noted.", 5)

			var embedded []string
			for _, f := range c.facts {
				embedded = append(embedded, "embed: "+f)
			}
			if !reflect.DeepEqual(p.embedded, embedded) || !reflect.DeepEqual(st.stored(), c.facts) {
				t.Errorf("embedded %q and stored %q, want %q stored", p.embedded, st.stored(), c.facts)
			}
			if warned := strings.Contains(logs.String(), "level=WARN"); warned != c.warn {
				t.Errorf("warned = %t, want %t:\n%s", warned, c.warn, logs.String())
			}
		})
	}
}

func TestExtractSkipsAFactNearAStoredFactOfTheSameScope(t *testing.T) {
	cases := []struct {
		name   string
		hits   []vector.Hit
		stored bool
	}{
		{"just under the threshold", []vector.Hit{hit("Works on billing.", 0.1499)}, false},
		{"at the threshold", []vector.Hit{hit("Works on billing.", 0.15)}, true},
		{"zero vector", []vector.Hit{hit("Works on billing.", math.NaN())}, true},
		{"no stored fact", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			st := &store{hits: [][]vector.Hit{c.hits}}

			facts(settings(), &provider{answer: `[{"fact": "Works on the billing team."}]`}, st, recentTurns(), log).
				Extract(context.Background(), channel, "Noted.", 5)

			want := []search{{"agent_memory", map[string]string{"key": "agent:u1", "scope": "public"}, 1}}
			if !reflect.DeepEqual(st.searches, want) {
				t.Errorf("searches = %+v, want %+v", st.searches, want)
			}
			if got := len(st.adds) == 1; got != c.stored {
				t.Errorf("stored = %t, want %t", got, c.stored)
			}
			if skipped := strings.Contains(logs.String(), "level=DEBUG msg=\"duplicate fact skipped\""); skipped == c.stored {
				t.Errorf("logged a skip = %t, want %t:\n%s", skipped, !c.stored, logs.String())
			}
		})
	}
}

func TestExtractSkipsAFactThatCannotBeEmbedded(t *testing.T) {
	p := &provider{answer: `[{"fact": "a"}, {"fact": "b"}]`, unembeddable: []string{"a"}}
	st := &store{}

	facts(settings(), p, st, recentTurns(), discard).Extract(context.Background(), channel, "Noted.", 5)

	if want := []string{"embed: a", "embed: b"}; !reflect.DeepEqual(p.embedded, want) {
		t.Errorf("embedded %q, want %q", p.embedded, want)
	}
	if len(st.searches) != 1 || !reflect.DeepEqual(st.stored(), []string{"b"}) {
		t.Errorf("%d searches and stored %q, want one search and b stored", len(st.searches), st.stored())
	}
}

func TestExtractStoresEachFactWithTheMessageMetadata(t *testing.T) {
	cases := []struct {
		name     string
		m        chat.Message
		metadata string
	}{
		{"direct message", chat.Message{ID: "p1", AuthorID: "u1", Kind: chat.Direct},
			`{"key":"agent:u1","scope":"private","type":"user_fact","user_id":"u1","is_dm":true,"context_id":"p1"}`},
		{"thread in a direct message", chat.Message{ID: "p2", ThreadID: "root1", AuthorID: "u1", Kind: chat.Direct},
			`{"key":"agent:u1","scope":"private","type":"user_fact","user_id":"u1","is_dm":true,"context_id":"root1","thread_id":"root1"}`},
		{"channel post", chat.Message{ID: "p1", AuthorID: "u1", Kind: chat.Public},
			`{"key":"agent:u1","scope":"public","type":"user_fact","user_id":"u1","is_dm":false,"context_id":"p1"}`},
		{"channel thread reply", chat.Message{ID: "p2", ThreadID: "root1", AuthorID: "u1", Kind: chat.Public},
			`{"key":"agent:u1","scope":"public","type":"user_fact","user_id":"u1","is_dm":false,"context_id":"root1","thread_id":"root1"}`},
		{"private channel thread reply", chat.Message{ID: "p2", ThreadID: "root1", AuthorID: "u1", Kind: chat.Private},
			`{"key":"agent:u1","scope":"public","type":"user_fact","user_id":"u1","is_dm":false,"context_id":"root1","thread_id":"root1"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &store{}
			facts(settings(), &provider{answer: `[{"fact": "a"}, {"fact": "b"}]`}, st, recentTurns(), discard).
				Extract(context.Background(), c.m, "Noted.", 5)

			var want []add
			for _, f := range []string{"a", "b"} {
				want = append(want, add{"agent_memory", []vector.Doc{{Document: f, Embedding: unit(), Metadata: json.RawMessage(c.metadata)}}})
			}
			if !reflect.DeepEqual(st.adds, want) {
				t.Errorf("adds = %+v\nwant %+v", st.adds, want)
			}
		})
	}
}

func TestExtractLogsAFailureAndStoresNoMoreFacts(t *testing.T) {
	down := errors.New("connection refused")
	cases := []struct {
		name     string
		turns    *turns
		store    *store
		embedded []string
		adds     int
	}{
		{"history read", &turns{err: down}, &store{}, nil, 0},
		{"duplicate search", recentTurns(), &store{searchErr: down}, []string{"embed: a"}, 0},
		{"insert", recentTurns(), &store{addErr: down}, []string{"embed: a"}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			p := &provider{answer: `[{"fact": "a"}, {"fact": "b"}]`}

			facts(settings(), p, c.store, c.turns, log).Extract(context.Background(), channel, "Noted.", 5)

			if !reflect.DeepEqual(p.embedded, c.embedded) || len(c.store.adds) != c.adds {
				t.Errorf("embedded %q and added %d times, want %q and %d", p.embedded, len(c.store.adds), c.embedded, c.adds)
			}
			if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "connection refused") {
				t.Errorf("log lacks the failure:\n%s", logs.String())
			}
		})
	}
}
