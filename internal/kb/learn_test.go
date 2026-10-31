package kb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/prompt"
	"github.com/sarpaydemir/dost/internal/store/vector"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/fakeclock"
	"github.com/sarpaydemir/dost/internal/testkit/kvtest"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

// learnNow is 1800000000000 ms after the epoch; items claimed before 1799996400000 are stale.
var learnNow = time.UnixMilli(1_800_000_000_000)

type fixedSettings struct{ s *config.Settings }

func (f fixedSettings) Current(context.Context) *config.Settings { return f.s }

func catalog(t *testing.T) *prompt.Catalog {
	t.Helper()
	c, err := prompt.Load(prompt.Builtin(), "")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func learnSettings() *config.Settings {
	return &config.Settings{Model: "main", FactModel: "fact", EmbeddingModel: "embed", KBBatchKey: batchKey,
		KBCollection: "agent_public_kb", PublicKBMaxTokens: 6144, MaxTokens: 4096}
}

func newLearner(t *testing.T, s *config.Settings, p *provider, st *store, kv *kvtest.Memory, log *slog.Logger) *Learner {
	return &Learner{
		Settings: fixedSettings{s},
		Model:    func(*config.Settings) (model.Provider, error) { return p, nil },
		Batch:    kv, Store: st, Prompts: catalog(t), Learn: NewTrigger(), Work: context.Background(),
		Clock: fakeclock.New(learnNow), Log: log,
	}
}

// fullBatch is 25 pending items p0 to p24 of one thread, then the pending item late.
func fullBatch() string {
	items := make([]string, 0, 26)
	for i := range 25 {
		items = append(items, fmt.Sprintf(`{"post_id": "p%d", "channel_id": "c1", "context_id": "t1", "root_id": "t1", `+
			`"message": "m%d", "create_at": %d, "status": "pending"}`, i, i, 1000+i))
	}
	items = append(items, `{"post_id": "late", "channel_id": "c1", "message": "x", "status": "pending"}`)
	return `{"messages": [` + strings.Join(items, ", ") + `]}`
}

// revertedBatch is fullBatch after a failed run: late first, then p0 to p24 pending again.
func revertedBatch() string {
	items := []string{`{"post_id": "late", "channel_id": "c1", "message": "x", "status": "pending"}`}
	for i := range 25 {
		items = append(items, fmt.Sprintf(`{"post_id": "p%d", "channel_id": "c1", "context_id": "t1", "root_id": "t1", `+
			`"message": "m%d", "create_at": %d, "status": "pending"}`, i, i, 1000+i))
	}
	return `{"messages": [` + strings.Join(items, ", ") + `]}`
}

const purgedBatch = `{"messages": [{"post_id": "late", "channel_id": "c1", "message": "x", "status": "pending"}]}`

// accepted is a model item that passes every acceptance rule for a run on fullBatch.
const accepted = `{"store": true, "confidence": 0.8, "memory_type": "process", "summary": " S1 ", "source_ids": ["p1"], ` +
	`"rationale": "r", "topic": "t", "tags": ["x"]}`

func TestLearnWithoutAnEmbeddingModelLeavesTheBatchUntouched(t *testing.T) {
	s := learnSettings()
	s.EmbeddingModel = ""
	kv := &kvtest.Memory{}
	put(t, kv, `{"messages": [{"post_id": "a", "status": "weird"}]}`)
	p := &provider{answer: "NULL"}

	if err := newLearner(t, s, p, &store{}, kv, discard).learn(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertBatch(t, kv, `{"messages": [{"post_id": "a", "status": "weird"}]}`)
	if len(p.requests) != 0 {
		t.Errorf("%d model requests, want none", len(p.requests))
	}
}

func TestLearnClaimRecoversStaleItemsAndWaitsForAFullBatch(t *testing.T) {
	cases := []struct{ name, batch, want string }{
		{"statuses", `{"messages": [
			{"post_id": "p1", "status": "processing", "processing_started_at": 1799996399999},
			{"post_id": "p2", "status": "processing", "processing_started_at": 1799996400000},
			{"post_id": "p3", "status": "processing"},
			{"post_id": "p4"},
			{"post_id": "p5", "status": null},
			{"post_id": "p6", "status": "PENDING"},
			{"post_id": "p7", "status": "done"},
			{"post_id": "p8", "status": "processing", "processing_started_at": 0},
			{"post_id": "p9", "status": "processing", "processing_started_at": true},
			{"post_id": "p10", "status": "processing", "processing_started_at": "1"},
			{"post_id": "p11", "status": "processing", "processing_started_at": 1.5}]}`,
			`{"messages": [{"post_id": "p1", "status": "pending"}, {"post_id": "p4", "status": "pending"},
			{"post_id": "p5", "status": "pending"}, {"post_id": "p6", "status": "pending"},
			{"post_id": "p7", "status": "pending"}, {"post_id": "p9", "status": "pending"},
			{"post_id": "p11", "status": "pending"},
			{"post_id": "p2", "status": "processing", "processing_started_at": 1799996400000},
			{"post_id": "p3", "status": "processing"},
			{"post_id": "p8", "status": "processing", "processing_started_at": 0},
			{"post_id": "p10", "status": "processing", "processing_started_at": "1"}]}`},
		{"no batch", "", `{"messages": []}`},
		{"null messages", `{"messages": null, "other": 1}`, `{"messages": []}`},
		{"no messages", `{"other": 1}`, `{"messages": []}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kv := &kvtest.Memory{}
			if c.batch != "" {
				put(t, kv, c.batch)
			}
			p := &provider{answer: "NULL"}
			if err := newLearner(t, learnSettings(), p, &store{}, kv, discard).learn(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertBatch(t, kv, c.want)
			if len(p.requests) != 0 {
				t.Errorf("%d model requests, want none", len(p.requests))
			}
		})
	}
}

func TestLearnClaimTakesTheFirst25PendingItems(t *testing.T) {
	items := make([]string, 0, 28)
	for i := range 27 {
		items = append(items, fmt.Sprintf(`{"post_id": "q%d"}`, i))
	}
	items = slices.Insert(items, 3, `{"post_id": "busy", "status": "processing", "processing_started_at": 1800000000000}`)
	kv := &kvtest.Memory{}
	put(t, kv, `{"messages": [`+strings.Join(items, ", ")+`]}`)
	var claimed string
	p := &provider{answer: "oops", onChat: func() { claimed = stored(t, kv) }}

	if err := newLearner(t, learnSettings(), p, &store{}, kv, discard).learn(context.Background()); err == nil {
		t.Error("learn = nil, want the invalid answer's error")
	}

	taken := make([]string, 25)
	reverted := make([]string, 25)
	for i := range taken {
		taken[i] = fmt.Sprintf(`{"post_id": "q%d", "status": "processing", "processing_started_at": 1800000000000}`, i)
		reverted[i] = fmt.Sprintf(`{"post_id": "q%d", "status": "pending"}`, i)
	}
	rest := `{"post_id": "q25", "status": "pending"}, {"post_id": "q26", "status": "pending"}, `
	busy := `, {"post_id": "busy", "status": "processing", "processing_started_at": 1800000000000}`
	want := `{"messages": [` + rest + strings.Join(taken, ", ") + busy + `]}`
	if eq, err := testkit.JSONEqual([]byte(claimed), []byte(want)); err != nil || !eq {
		t.Errorf("claimed batch = %s\nwant            %s (err %v)", claimed, want, err)
	}
	assertBatch(t, kv, `{"messages": [`+rest+strings.Join(reverted, ", ")+busy+`]}`)
}

func TestLearnRevertAndPurgeTouchOnlyTheTakenItems(t *testing.T) {
	// While the model answers, q1 is marked done, q2 gets another start time, and q0 and new are appended.
	items := []string{`{"post_id": "late", "status": "pending"}`}
	for i := range 25 {
		switch i {
		case 1:
			items = append(items, `{"post_id": "q1", "status": "done", "processing_started_at": 1800000000000}`)
		case 2:
			items = append(items, `{"post_id": "q2", "status": "processing", "processing_started_at": 5}`)
		default:
			items = append(items, fmt.Sprintf(`{"post_id": "q%d", "status": "processing", "processing_started_at": 1800000000000}`, i))
		}
	}
	items = append(items, `{"post_id": "", "status": "processing"}`, `{"post_id": "q0", "status": "pending", "note": "dup"}`,
		`{"post_id": "new"}`)
	during := `{"messages": [` + strings.Join(items, ", ") + `]}`

	reverted := []string{`{"post_id": "late", "status": "pending"}`, `{"post_id": "q0", "status": "pending"}`,
		`{"post_id": "q1", "status": "done", "processing_started_at": 1800000000000}`}
	for i := 2; i < 25; i++ {
		reverted = append(reverted, fmt.Sprintf(`{"post_id": "q%d", "status": "pending"}`, i))
	}
	reverted = append(reverted, items[26:]...)

	for _, c := range []struct{ answer, want string }{
		{"oops", `{"messages": [` + strings.Join(reverted, ", ") + `]}`},
		{"NULL", `{"messages": [{"post_id": "late", "status": "pending"}, {"post_id": "", "status": "processing"}, {"post_id": "new"}]}`},
	} {
		t.Run(c.answer, func(t *testing.T) {
			queued := []string{`{"post_id": "", "status": "processing"}`}
			for i := range 25 {
				queued = append(queued, fmt.Sprintf(`{"post_id": "q%d"}`, i))
			}
			queued = append(queued, `{"post_id": "late"}`)
			kv := &kvtest.Memory{}
			put(t, kv, `{"messages": [`+strings.Join(queued, ", ")+`]}`)
			p := &provider{answer: c.answer, onChat: func() { put(t, kv, during) }}

			_ = newLearner(t, learnSettings(), p, &store{}, kv, discard).learn(context.Background())
			assertBatch(t, kv, c.want)
		})
	}
}

func TestLearnAsksTheFactModelAboutTheTakenItems(t *testing.T) {
	cases := []struct {
		name      string
		factModel string
		kbTokens  int
		model     string
		maxTokens int
	}{
		{"fact model", "fact", 6144, "fact", 6144},
		{"fallbacks", "", 0, "main", 4096},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := learnSettings()
			s.FactModel, s.PublicKBMaxTokens = c.factModel, c.kbTokens
			kv := &kvtest.Memory{}
			put(t, kv, fullBatch())
			p := &provider{answer: " null "}
			if err := newLearner(t, s, p, &store{}, kv, discard).learn(context.Background()); err != nil {
				t.Fatal(err)
			}

			entries := make([]string, 25)
			for i := range entries {
				entries[i] = fmt.Sprintf("source_id: p%d\nm%d", i, i)
			}
			want := []model.Request{{Model: c.model, MaxTokens: c.maxTokens, Messages: []model.Message{
				{Role: model.RoleSystem, Content: catalog(t).Text("kb.extract")},
				{Role: model.RoleUser, Content: "Channel: c1\nThread: t1\nRoot: t1\n" + strings.Join(entries, "\n")},
			}}}
			if !reflect.DeepEqual(p.requests, want) {
				t.Errorf("model requests = %+v\nwant %+v", p.requests, want)
			}
			assertBatch(t, kv, purgedBatch)
		})
	}
}

func TestSourceTextGroupsItemsByChannelAndThreadInFirstSeenOrder(t *testing.T) {
	var items []queuedItem
	if err := json.Unmarshal([]byte(`[
		{"post_id": "p1", "channel_id": "c1", "context_id": "t1", "root_id": "r1", "message": "one"},
		{"post_id": "p2", "channel_id": "c2", "context_id": "", "root_id": "r2", "message": "two"},
		{"post_id": "p3", "channel_id": "c1", "context_id": "t1", "root_id": "rX", "message": null},
		{"post_id": "", "channel_id": null, "message": "four"},
		{"post_id": "p5", "channel_id": "c2", "root_id": "r2", "message": 5},
		{"post_id": 7, "channel_id": 9, "context_id": null, "message": "seven"}]`), &items); err != nil {
		t.Fatal(err)
	}
	want := "Channel: c1\nThread: t1\nRoot: r1\nsource_id: p1\none\nsource_id: p3\n" +
		"\n\n====================\n\n" +
		"Channel: c2\nThread: r2\nRoot: r2\nsource_id: p2\ntwo\nsource_id: p5\n5" +
		"\n\n====================\n\n" +
		"Channel: unknown\nThread: unknown\nRoot: \nsource_id: unknown\nfour" +
		"\n\n====================\n\n" +
		"Channel: 9\nThread: 7\nRoot: \nsource_id: 7\nseven"
	if got := sourceText(catalog(t), items); got != want {
		t.Errorf("sourceText = %q\nwant %q", got, want)
	}
}

func TestParseAnswerReadsTheModelItems(t *testing.T) {
	a1 := textfmt.Object{{Key: "a", Value: int64(1)}}
	cases := []struct {
		raw  string
		want []any
		err  bool
	}{
		{raw: ""},
		{raw: "  null "},
		{raw: "Null"},
		{raw: "[]"},
		{raw: "```json\n[]\n```"},
		{raw: "```\nNULL\n```"},
		{raw: "null"},
		{raw: " [ ] ", want: []any{}},
		{raw: "```json {\"a\":1} ```", want: []any{a1}},
		{raw: `{"a":1}`, want: []any{a1}},
		{raw: `{"a":0,"a":1}`, want: []any{a1}},
		{raw: `[1,"x"]`, want: []any{int64(1), "x"}},
		{raw: "[1] ```", want: []any{int64(1)}},
		{raw: "```JSON\n[]```", err: true},
		{raw: "```jsonl\n[]\n```", err: true},
		{raw: "```json\n[{\"a\":1}]\n``` trailing", err: true},
		{raw: `"str"`, err: true},
		{raw: "5", err: true},
		{raw: "true", err: true},
		{raw: "NaN", err: true},
		{raw: "nope", err: true},
	}
	for _, c := range cases {
		got, err := parseAnswer(c.raw)
		if (err != nil) != c.err || !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseAnswer(%q) = %#v, %v; want %#v, error %t", c.raw, got, err, c.want, c.err)
		}
	}
}

func TestDecideAppliesTheAcceptanceRulesInOrder(t *testing.T) {
	cases := []struct{ name, override, want string }{
		{"valid", ``, ""},
		{"store false", `"store": false`, "store_false"},
		{"store null", `"store": null`, "store_false"},
		{"store padded yes", `"store": " Yes "`, ""},
		{"store string 1", `"store": "1"`, ""},
		{"store TRUE", `"store": "TRUE"`, ""},
		{"store y", `"store": "y"`, "store_false"},
		{"store string false", `"store": "false"`, "store_false"},
		{"store number 1", `"store": 1`, ""},
		{"store empty list", `"store": []`, "store_false"},
		{"confidence 0.65", `"confidence": 0.65`, ""},
		{"confidence 0.6499", `"confidence": 0.6499`, "confidence_below_threshold"},
		{"confidence null", `"confidence": null`, "confidence_below_threshold"},
		{"confidence nan", `"confidence": "nan"`, ""},
		{"memory type padded", `"memory_type": " Decision "`, ""},
		{"memory type unknown", `"memory_type": "user_fact"`, "invalid_memory_type"},
		{"memory type null", `"memory_type": null`, "invalid_memory_type"},
		{"external fact 0.75", `"memory_type": "external_fact", "confidence": 0.75`, ""},
		{"external fact 0.74", `"memory_type": "external_fact", "confidence": 0.74`, "external_fact_confidence_below_threshold"},
		{"confidence before memory type", `"memory_type": "nope", "confidence": 0.5`, "confidence_below_threshold"},
		{"summary blank", `"summary": "  "`, "missing_summary"},
		{"summary null", `"summary": null`, "missing_summary"},
		{"summary 0", `"summary": 0`, "missing_summary"},
		{"summary number", `"summary": 5`, "missing_summary"},
		{"summary list", `"summary": ["x"]`, "missing_summary"},
		{"source ids empty", `"source_ids": []`, "missing_source_ids"},
		{"source ids string", `"source_ids": "p1"`, "missing_source_ids"},
		{"source id unknown", `"source_ids": ["p1", "zz"]`, "invalid_source_ids"},
		{"source ids cleaned", `"source_ids": [" p1 ", "p1", ""]`, ""},
		{"source id number", `"source_ids": [7]`, ""},
		{"rationale blank", `"rationale": " "`, "missing_rationale"},
		{"rationale null", `"rationale": null`, "missing_rationale"},
		{"rationale 0", `"rationale": 0`, "missing_rationale"},
		{"rationale number", `"rationale": 5`, ""},
	}
	taken := map[string]bool{"p1": true, "7": true}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A later member replaces an earlier one with the same key.
			raw := `{"store": true, "confidence": 0.8, "memory_type": "process", "summary": "s", "source_ids": ["p1"], "rationale": "r"`
			if c.override != "" {
				raw += ", " + c.override
			}
			it, err := textfmt.Decode([]byte(raw+"}"), true)
			if err != nil {
				t.Fatal(err)
			}
			if got := decide(it, taken); got != c.want {
				t.Errorf("decide = %q, want %q", got, c.want)
			}
		})
	}
	if got := decide(int64(1), taken); got != "non_dict" {
		t.Errorf("decide(1) = %q, want non_dict", got)
	}
}

func TestMetadataDescribesTheEntryAndItsSources(t *testing.T) {
	var taken []queuedItem
	if err := json.Unmarshal([]byte(`[
		{"post_id": "p1", "channel_id": "c1", "context_id": "t1", "root_id": "r1", "create_at": 1780000002000},
		{"post_id": "p2", "channel_id": "c1", "context_id": "", "root_id": null, "create_at": 1780000001000.9},
		{"post_id": "p3", "channel_id": "c2", "context_id": "t2", "root_id": "r1", "create_at": null},
		{"post_id": "p4", "channel_id": 5, "context_id": "t2", "create_at": true},
		{"post_id": "p5", "channel_id": "c3", "create_at": "1"},
		{"post_id": "p6", "channel_id": "first"},
		{"post_id": "p6", "channel_id": "second"}]`), &taken); err != nil {
		t.Fatal(err)
	}
	const ts = `"type": "public_kb", "timestamp": "2027-01-15T08:00:00+00:00"`
	cases := []struct{ item, want string }{
		{`{"topic": "  T  ", "tags": ["a", " a ", 3, null], "memory_type": " Process ", "confidence": "0.9",
			"source_ids": ["p2", "p1", "p3", "p4", "zz", "p1"], "summary": "s", "rationale": "secret"}`,
			`{` + ts + `, "topic": "T", "tags": ["a", "3"], "memory_type": "process", "confidence": 0.9,
			"source_ids": ["p2", "p1", "p3", "p4"], "source_channel_ids": ["c1", "c2", 5], "source_context_ids": ["t1", "t2"],
			"source_root_ids": ["r1"], "source_create_at_min": 1, "source_create_at_max": 1780000002000}`},
		{`{"source_ids": ["p5"], "topic": null, "tags": "x", "confidence": 1}`,
			`{` + ts + `, "topic": "", "tags": [], "memory_type": "", "confidence": 1.0, "source_ids": ["p5"],
			"source_channel_ids": ["c3"], "source_context_ids": [], "source_root_ids": []}`},
		{`{"source_ids": ["p3"], "confidence": true}`,
			`{` + ts + `, "topic": "", "tags": [], "memory_type": "", "confidence": 1.0, "source_ids": ["p3"],
			"source_channel_ids": ["c2"], "source_context_ids": ["t2"], "source_root_ids": ["r1"]}`},
		{`{"source_ids": ["p6"]}`,
			`{` + ts + `, "topic": "", "tags": [], "memory_type": "", "confidence": 0.0, "source_ids": ["p6"],
			"source_channel_ids": ["second"], "source_context_ids": [], "source_root_ids": []}`},
	}
	for _, c := range cases {
		it, err := textfmt.Decode([]byte(c.item), true)
		if err != nil {
			t.Fatal(err)
		}
		got, err := metadata(it.(textfmt.Object), taken, learnNow)
		if err != nil {
			t.Fatal(err)
		}
		if eq, err := testkit.JSONEqual(got, []byte(c.want)); err != nil || !eq {
			t.Errorf("metadata(%s) = %s\nwant %s (err %v)", c.item, got, c.want, err)
		}
	}
}

func TestLearnWritesNewEntriesAndSkipsNearDuplicates(t *testing.T) {
	second := strings.Replace(accepted, `" S1 "`, `"S2"`, 1)
	meta := json.RawMessage(`{"type": "public_kb", "timestamp": "2027-01-15T08:00:00+00:00", "topic": "t", "tags": ["x"],
		"memory_type": "process", "confidence": 0.8, "source_ids": ["p1"], "source_channel_ids": ["c1"],
		"source_context_ids": ["t1"], "source_root_ids": ["t1"], "source_create_at_min": 1001, "source_create_at_max": 1001}`)
	cases := []struct {
		name    string
		answer  string
		hits    []vector.Hit
		written []string
	}{
		{"no stored entry", accepted, nil, []string{"S1"}},
		{"distance 0.15", "[" + accepted + ", " + second + "]", []vector.Hit{hit("old", `{}`, 0.15)}, []string{"S1", "S2"}},
		{"distance below 0.15", accepted, []vector.Hit{hit("old", `{}`, 0.1499)}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kv := &kvtest.Memory{}
			put(t, kv, fullBatch())
			p := &provider{answer: c.answer, vector: queryVector()}
			st := &store{hits: c.hits}
			if err := newLearner(t, learnSettings(), p, st, kv, discard).learn(context.Background()); err != nil {
				t.Fatal(err)
			}

			var embedded []string
			var searches []search
			var docs []vector.Doc
			for _, w := range []string{"S1", "S2"}[:max(1, len(c.written))] {
				embedded = append(embedded, "embed: "+w)
				searches = append(searches, search{"agent_public_kb", queryVector(), map[string]string{"type": "public_kb"}, 1})
			}
			for _, w := range c.written {
				docs = append(docs, vector.Doc{Document: w, Embedding: queryVector()})
			}
			if !reflect.DeepEqual(p.embedded, embedded) || !reflect.DeepEqual(st.searches, searches) {
				t.Errorf("embedded %q and searched %+v, want %q and %+v", p.embedded, st.searches, embedded, searches)
			}
			var wantAdds []add
			if docs != nil {
				wantAdds = []add{{"agent_public_kb", docs}}
			}
			if len(st.added) != len(wantAdds) {
				t.Fatalf("adds = %+v, want %+v", st.added, wantAdds)
			}
			for i, a := range st.added {
				for j := range a.docs {
					if eq, err := testkit.JSONEqual(a.docs[j].Metadata, meta); err != nil || !eq {
						t.Errorf("metadata = %s, want %s (err %v)", a.docs[j].Metadata, meta, err)
					}
					a.docs[j].Metadata = nil
				}
				if !reflect.DeepEqual(a, wantAdds[i]) {
					t.Errorf("add = %+v, want %+v", a, wantAdds[i])
				}
			}
			assertBatch(t, kv, purgedBatch)
		})
	}
}

func TestLearnFailureReturnsTheTakenItemsToPending(t *testing.T) {
	failure := catalog(t).Text("error.generic")
	cases := []struct {
		name   string
		answer string
		vector bool
		store  store
	}{
		{"failure reply", "", true, store{}},
		{"answer starting with the failure text", failure + " []", true, store{}},
		{"invalid JSON", "oops", true, store{}},
		{"scalar JSON", `"str"`, true, store{}},
		{"no embedding", accepted, false, store{}},
		{"search failure", accepted, true, store{err: errors.New("search down")}},
		{"write failure", accepted, true, store{addErr: errors.New("write down")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kv := &kvtest.Memory{}
			put(t, kv, fullBatch())
			p := &provider{answer: c.answer}
			if c.vector {
				p.vector = queryVector()
			}
			if err := newLearner(t, learnSettings(), p, &c.store, kv, discard).learn(context.Background()); err == nil {
				t.Error("learn = nil, want an error")
			}
			assertBatch(t, kv, revertedBatch())
		})
	}
}

func TestLearnPurgesRejectedItemsAndLogsThem(t *testing.T) {
	kv := &kvtest.Memory{}
	put(t, kv, fullBatch())
	answer := `[1, {"store": false, "summary": null, "rationale": null}, {"store": true, "confidence": 0.1, "summary": "` +
		strings.Repeat("ş", 100) + `", "rationale": "` + strings.Repeat("y", 100) + `"}]`
	p := &provider{answer: answer, vector: queryVector()}
	st := &store{}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := newLearner(t, learnSettings(), p, st, kv, log).learn(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertBatch(t, kv, purgedBatch)
	if len(p.embedded) != 0 || len(st.added) != 0 {
		t.Errorf("embedded %q and added %+v, want neither", p.embedded, st.added)
	}
	for _, want := range []string{
		`level=INFO msg="knowledge item rejected" reason=store_false summary=None rationale=None`,
		`level=INFO msg="knowledge item rejected" reason=confidence_below_threshold summary=` + strings.Repeat("ş", 80) +
			` rationale=` + strings.Repeat("y", 80) + "\n",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs do not contain %s:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "non_dict") {
		t.Errorf("a non-object item is logged at info:\n%s", logs.String())
	}
}

// gatedSettings blocks each Current call until the test lets it return, and counts the calls.
type gatedSettings struct {
	entered chan struct{}
	release chan struct{}
}

func (g gatedSettings) Current(context.Context) *config.Settings {
	g.entered <- struct{}{}
	<-g.release
	return &config.Settings{} // no embedding model: the run ends at once
}

func TestRunCoalescesRequestsAndFinishesTheRunInProgressAtShutdown(t *testing.T) {
	g := gatedSettings{entered: make(chan struct{}), release: make(chan struct{})}
	l := &Learner{Settings: g, Learn: NewTrigger(), Work: context.Background(), Log: discard}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()

	l.Learn.Request()
	<-g.entered
	for range 3 {
		l.Learn.Request()
	}
	g.release <- struct{}{}
	<-g.entered // the one follow-up run
	cancel()
	g.release <- struct{}{}
	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
}
