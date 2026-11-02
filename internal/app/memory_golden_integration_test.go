//go:build integration

package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/store/pg"
	"github.com/sarpaydemir/melinda/internal/store/vector"
	"github.com/sarpaydemir/melinda/internal/testkit"
	"github.com/sarpaydemir/melinda/internal/testkit/openaifake"
)

// Memory goldens: the conversation harness with an embedding model, over rows in the formats production databases
// already hold: turns without message_count, facts with only key, type, and scope, and knowledge rows with
// evidence and verified.

func TestMemoryGoldens(t *testing.T) { assertGoldens(t, "memory", memoryScenarios) }

func TestMemoryCompatGoldens(t *testing.T) { assertCompatGoldens(t, "memory", memoryScenarios) }

const (
	withEmbeddings = `"embedding_model":"embed-model","fact_model":"fact-model"`
	dayMS          = 24 * 60 * 60 * 1000
)

var memoryScenarios = []scenario{
	memoryRecall("memory_recall", withEmbeddings),
	memoryRecall("memory_recall_user_context", withEmbeddings+`,"context_role":"user"`),
	{name: "fact_extraction", settings: withEmbeddings, seed: func(h *harness) {
		var turns []string
		for i := 1; i <= 4; i++ {
			scope := map[bool]string{true: "private", false: "public"}[i == 2]
			turns = append(turns, fmt.Sprintf(`{"scope": %q, "context_id": %q, "user_id": %q, "is_dm": %t, "user": "%s: earlier %d", `+
				`"assistant": "reply %d", "ts": "2026-10-30T09:00:0%d+00:00", "message_count": %d}`, scope, id("c0"), id("u1"), i == 2, id("u1"), i, i, i, i))
		}
		h.history(h.memoryKey("u1"), turns...)
		h.vector(h.setting("facts_collection", "dost_memory"), "Ada prefers tea", []float32{1, 0.1}, `{"key": "%s", "scope": "public", `+
			`"type": "user_fact", "user_id": "%s", "is_dm": false, "context_id": "%s"}`, h.memoryKey("u1"), id("u1"), id("c0"))
	}, replies: texts("NULL", "Noted.", `[{"fact": "Ada moved to Izmir"}, {"fact": "Ada prefers tea"}]`), play: func(h *harness) {
		h.chat.AddPost(post("r1", "u2", "ch1", "", "kickoff", -60000))
		h.post(post("p1", "u1", "ch1", "r1", "@agent I moved to Izmir", 0))
		h.flush(3*time.Second, 1)
	}},
	{name: "kb_learning", settings: withEmbeddings, seed: func(h *harness) {
		items := []string{`{"post_id": "brand:acme:1:1793447000", "message": "acme - Overview\nAcme sells anvils.", ` +
			`"channel_id": "brand_analysis", "create_at": 1793447000.25}`}
		note := func(n int, text string, ageMS int64) string {
			s := fmt.Sprintf("s%d", n)
			return fmt.Sprintf(`{"post_id": %q, "channel_id": %q, "user_id": %q, "create_at": %d, "context_id": %q, "root_id": "", `+
				`"is_thread": false, "message": "%s: %s\nagent: ok", "status": "pending"}`, id(s), id("ch1"), id("u2"), baseMS-ageMS, id(s), id("u2"), text)
		}
		for i := 1; i <= 22; i++ {
			items = append(items, note(i, fmt.Sprintf("note %d", i), int64(30-i)*60000))
		}
		items = append(items, fmt.Sprintf(`{"post_id": "brand:acme:2:1793447500", "message": "acme - Pricing\nAnvils cost a lot.", `+
			`"channel_id": "brand_analysis", "create_at": 1793447500.5, "status": "processing", "processing_started_at": %d}`, baseMS-60000),
			note(23, "standup is at 10", 7*60000))
		h.exec(`INSERT INTO key_value_store (type, key, value, created_at, updated_at) VALUES ('none', $1, $2, now(), now())`,
			h.setting("kb_batch_key", "dost:public_kb_batch"), `{"messages": [`+strings.Join(items, ", ")+`]}`)
		h.deployRule()
	}, replies: texts(`{"query": "deploy approvals"}`, "Two approvals.", learningAnswer), play: func(h *harness) {
		h.post(post("p1", "u1", "ch1", "", "@agent who approves deploys?", 0))
		h.clock.Advance(3 * time.Second)
		h.awaitRuns(1)
		h.await("learning run", func() bool { return strings.Contains(h.logs.String(), `msg="learning finished"`) })
		h.snapshot("flushed")
	}},
	{name: "attachments", settings: withEmbeddings + `,"vision_model":"vision-model"`,
		replies: texts("NULL", "The notes move standup to 10:00.", "A one-page report.", "Got both files."), play: func(h *harness) {
			h.chat.AddFile(id("f1"), id("u1"), id("dm1"), "notes.txt", "text/plain", []byte("Standup moved to 10:00.\n"))
			h.chat.AddFile(id("f2"), id("u1"), id("dm1"), "report.pdf", "application/pdf", []byte("%PDF-1.4\n%fake\n"))
			p := post("p1", "u1", "dm1", "", "please read these", 0)
			p.FileIDs = []string{id("f1"), id("f2")}
			h.post(p)
			h.flush(3*time.Second, 1)
		}},
}

// memoryRecall answers in a direct and a public conversation over stored turns, facts, and knowledge.
func memoryRecall(name, settings string) scenario {
	return scenario{name: name, settings: settings, seed: func(h *harness) {
		h.history(h.memoryKey("u1"),
			`{"user": "acme", "assistant": "Brand overview done.", "ts": "2026-03-01T10:00:00.123456+00:00", "scope": "public", "context_id": "", "step": 1}`,
			`{"ts": "2026-02-01T00:00:00", "user": "old user msg", "scope": "private", "assistant": "old assistant msg"}`)
		facts := h.setting("facts_collection", "dost_memory")
		h.vector(facts, "Ada prefers tea", []float32{1, 0.1}, `{"key": "%s", "type": "user_fact", "scope": "private"}`, h.memoryKey("u1"))
		h.vector(facts, "Ada works on the compiler", []float32{1, 0.5}, `{"key": "%s", "scope": "public", "type": "user_fact", `+
			`"user_id": "%s", "is_dm": false, "context_id": "%s"}`, h.memoryKey("u1"), id("u1"), id("c0"))
		h.vector(facts, "Grace likes coffee", []float32{1, 0.2}, `{"key": "%s", "type": "user_fact", "scope": "public"}`, h.memoryKey("u2"))
		kb := h.setting("kb_collection", "dost_public_kb")
		h.vector(kb, "The release train leaves on Fridays", []float32{0, 1, 0.3}, `{"type": "public_kb", "topic": "releases", `+
			`"tags": ["release"], "timestamp": "2026-06-17T12:00:00+00:00", "source_ids": ["pid_1"], "source_channel_ids": ["cid_1"], `+
			`"source_create_at_min": 1781697600000, "source_create_at_max": 1781697600000, "evidence": "website", "confidence": 0.8, "verified": true}`)
		h.deployRule()
		h.vector(kb, "Unrelated note", []float32{0, 1}, `{"type": "other"}`)
		h.vector(kb, "Office closes at six", []float32{0, 1, 0.5},
			`{"type": "public_kb", "timestamp": "2026-10-01T00:00:00Z", "evidence": "document", "verified": false}`)
	}, replies: texts(`{"query": "release schedule"}`, "Hello again, Ada.", "NULL", "Quite a bit."), play: func(h *harness) {
		h.post(post("p1", "u1", "dm1", "", "hello", 0))
		h.flush(3*time.Second, 1)
		h.clock.Advance(7 * time.Second)
		h.post(post("p2", "u1", "ch1", "", "@agent what do you know?", 10000))
		h.flush(3*time.Second, 2)
	}}
}

// learningAnswer is the fact model's answer to the learning run: a duplicate of a stored entry, two entries to
// store (an integer confidence, and a string confidence and store flag), one below the confidence threshold, and
// one that is not an object.
var learningAnswer = strings.NewReplacer("p1", id("p1"), "s5", id("s5"), "s23", id("s23"), "s2", id("s2")).Replace(
	`[{"summary": "Deploys need two approvals", "topic": "deploys", "tags": ["deploy"], "source_ids": ["p1"], ` +
		`"memory_type": "process", "confidence": 0.9, "store": true, "rationale": "team rule"}, ` +
		`{"summary": " Acme sells anvils ", "topic": " brands ", "tags": ["acme", " acme", "", "anvils"], ` +
		`"source_ids": ["brand:acme:1:1793447000", "s5", "s5"], "memory_type": "brand_strategy", "confidence": 1, "store": true, "rationale": "durable"}, ` +
		`{"summary": "Standups are at 10", "topic": "meetings", "tags": [], "source_ids": ["s23", "p1"], ` +
		`"memory_type": " Process ", "confidence": "0.8", "store": "yes", "rationale": "schedule"}, ` +
		`{"summary": "Maybe", "topic": "x", "tags": [], "source_ids": ["s2"], "memory_type": "process", "confidence": 0.5, "store": true, "rationale": "unsure"}, ` +
		`"not an object"]`)

// embeddings are the leading components of the embedding of each text the scenarios embed; other texts embed to
// axis 5.
var embeddings = map[string][]float32{
	id("u1") + ": hello":                        {1, 0.2},
	id("u1") + ": @agent what do you know?":     {1, 0.2},
	id("u1") + ": @agent I moved to Izmir":      {1, 0.2},
	id("u1") + ": @agent who approves deploys?": {1, 0.2},
	id("u1") + ": please read these":            {1, 0.2},
	"release schedule":                          {0, 1},
	"deploy approvals":                          {0, 1, 0.35},
	"Ada prefers tea":                           {1, 0.1},
	"Ada moved to Izmir":                        {0, 0, 1},
	"Deploys need two approvals":                {0, 1, 0.35},
	"Acme sells anvils":                         {0, 0, 0, 1},
	"Standups are at 10":                        {0, 0, 0, 0, 1},
}

func embed(r testkit.Request) testkit.Response {
	var body struct {
		Input string `json:"input"`
	}
	_ = json.Unmarshal(r.Body, &body)
	lead, ok := embeddings[body.Input]
	if !ok {
		lead = []float32{0, 0, 0, 0, 0, 1}
	}
	return openaifake.Embedding(dims(lead))
}

func dims(lead []float32) []float32 {
	return append(lead[:len(lead):len(lead)], make([]float32, pg.EmbeddingDims-len(lead))...)
}

func (h *harness) memoryKey(user string) string {
	return h.setting("memory_key_prefix", "dost") + ":" + id(user)
}

// history stores items as the history under key.
func (h *harness) history(key string, items ...string) {
	h.exec(`INSERT INTO chat_sessions (key, value, created_at, updated_at) VALUES ($1, $2, now(), now())`,
		key, "["+strings.Join(items, ", ")+"]")
}

// vector stores a document in collection; metadata is a format for args.
func (h *harness) vector(collection, document string, lead []float32, metadata string, args ...any) {
	h.t.Helper()
	doc := vector.Doc{Document: document, Embedding: dims(lead), Metadata: json.RawMessage(fmt.Sprintf(metadata, args...))}
	if err := vector.New(h.db).Add(h.t.Context(), collection, []vector.Doc{doc}); err != nil {
		h.t.Fatal(err)
	}
}

// deployRule stores a knowledge entry learned a day ago.
func (h *harness) deployRule() {
	h.vector(h.setting("kb_collection", "dost_public_kb"), "Deploys need two approvals", []float32{0, 1, 0.35},
		`{"type": "public_kb", "timestamp": "2026-10-30T12:00:00+00:00", "topic": "deploys", "tags": ["deploy"], `+
			`"memory_type": "process", "confidence": 0.9, "source_ids": ["%s"], "source_channel_ids": ["%s"], `+
			`"source_context_ids": ["%s"], "source_root_ids": [], "source_create_at_min": %d, "source_create_at_max": %d}`,
		id("s1"), id("ch1"), id("s1"), baseMS-dayMS, baseMS-dayMS)
}

// vectors returns every vector row with its collection's name, document, and metadata, ordered by both names.
func (h *harness) vectors() []any {
	h.t.Helper()
	rows, err := h.db.Query(h.t.Context(), `SELECT c.name, e.document, e.cmetadata::text FROM langchain_pg_embedding e `+
		`JOIN langchain_pg_collection c ON c.uuid = e.collection_id ORDER BY c.name COLLATE "C", e.document COLLATE "C"`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []any
	for rows.Next() {
		var collection, document, metadata string
		if err := rows.Scan(&collection, &document, &metadata); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, map[string]any{"collection": collection, "document": document, "cmetadata": decode(h.t, []byte(metadata))})
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out
}
