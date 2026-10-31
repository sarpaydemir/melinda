//go:build integration

package vector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"math/rand/v2"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/store/pg"
	"github.com/sarpaydemir/dost/internal/testkit/testdb"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func openDB(t *testing.T, dsn string) *pg.DB {
	t.Helper()
	db, err := pg.Open(context.Background(), dsn, clock.Real(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func dial(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// open returns a store on a new database and a direct connection to it.
func open(t *testing.T) (*Store, *pgx.Conn) {
	t.Helper()
	dsn := testdb.New(t)
	return New(openDB(t, dsn)), dial(t, dsn)
}

func execSQL(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func count(t *testing.T, conn *pgx.Conn, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func collectionID(t *testing.T, conn *pgx.Conn, name string) string {
	t.Helper()
	var id string
	if err := conn.QueryRow(context.Background(), "SELECT uuid::text FROM langchain_pg_collection WHERE name = $1", name).
		Scan(&id); err != nil {
		t.Fatalf("collection %s: %v", name, err)
	}
	return id
}

// fill returns an embedding whose every component is x.
func fill(x float32) []float32 { return slices.Repeat([]float32{x}, pg.EmbeddingDims) }

func randomVector(r *rand.Rand) []float32 {
	v := make([]float32, pg.EmbeddingDims)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

func meta(s string) json.RawMessage { return json.RawMessage(s) }

func add(t *testing.T, s *Store, collection string, docs ...Doc) {
	t.Helper()
	if err := s.Add(context.Background(), collection, docs); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

func search(t *testing.T, s *Store, collection string, vec []float32, filter map[string]string, limit int) []Hit {
	t.Helper()
	hits, err := s.Search(context.Background(), collection, vec, filter, limit)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return hits
}

func documents(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = *h.Document
	}
	return out
}

func hitIDs(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

func TestCollectionIsCreatedOnFirstUseAndItsIDIsKept(t *testing.T) {
	s, conn := open(t)
	if hits := search(t, s, "facts", fill(1), nil, 4); len(hits) != 0 {
		t.Fatalf("new collection has hits %v", documents(hits))
	}
	var id string
	var metaIsNull bool
	if err := conn.QueryRow(t.Context(), "SELECT uuid::text, cmetadata IS NULL FROM langchain_pg_collection WHERE name = 'facts'").
		Scan(&id, &metaIsNull); err != nil {
		t.Fatalf("collection row: %v", err)
	}
	if !uuidV4.MatchString(id) || !metaIsNull {
		t.Errorf("collection row: uuid %q, cmetadata NULL %v; want a v4 uuid and NULL", id, metaIsNull)
	}

	execSQL(t, conn, "UPDATE langchain_pg_collection SET name = 'renamed'")
	add(t, s, "facts", Doc{Document: "a", Embedding: fill(1)})
	if n := count(t, conn, "SELECT count(*) FROM langchain_pg_collection"); n != 1 {
		t.Errorf("%d collection rows after the row was renamed; want the resolved uuid kept and no new row", n)
	}
	if n := count(t, conn, "SELECT count(*) FROM langchain_pg_embedding WHERE collection_id = $1", id); n != 1 {
		t.Errorf("%d rows under the resolved uuid; want 1", n)
	}
}

func TestConcurrentFirstUseCreatesOneCollection(t *testing.T) {
	s, conn := open(t)
	const writers = 8
	start := make(chan struct{})
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			<-start
			errs[i] = s.Add(context.Background(), "facts", []Doc{{Document: strconv.Itoa(i), Embedding: fill(1)}})
		})
	}
	close(start)
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if n := count(t, conn, "SELECT count(*) FROM langchain_pg_collection WHERE name = 'facts'"); n != 1 {
		t.Errorf("%d collection rows; want 1", n)
	}
	if n := count(t, conn, "SELECT count(*) FROM langchain_pg_embedding WHERE collection_id = $1", collectionID(t, conn, "facts")); n != writers {
		t.Errorf("%d rows in the collection; want %d", n, writers)
	}
}

func TestAddWritesRowsInTheExistingShape(t *testing.T) {
	s, conn := open(t)
	r := rand.New(rand.NewPCG(1, 2))
	plain, tagged := randomVector(r), randomVector(r)
	add(t, s, "facts",
		Doc{Document: "plain", Embedding: plain},
		Doc{Document: "tagged", Embedding: tagged, Metadata: meta(`{"type":"user_fact"}`)},
	)

	rows, _ := conn.Query(t.Context(), `
		SELECT e.id, e.collection_id = c.uuid, e.document, e.cmetadata::text, e.embedding::text
		FROM langchain_pg_embedding e, langchain_pg_collection c
		WHERE c.name = 'facts' ORDER BY e.document`)
	type row struct {
		ID           string
		InCollection bool
		Document     string
		Metadata     string
		Embedding    string
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		doc, meta string
		vec       []float32
	}{
		{"plain", `{}`, plain},
		{"tagged", `{"type": "user_fact"}`, tagged},
	}
	if len(got) != len(want) {
		t.Fatalf("%d rows; want %d", len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if !uuidV4.MatchString(g.ID) || !g.InCollection || g.Document != w.doc || g.Metadata != w.meta {
			t.Errorf("row %d: id %q, in collection %v, document %q, cmetadata %s; want a v4 uuid, true, %q, %s",
				i, g.ID, g.InCollection, g.Document, g.Metadata, w.doc, w.meta)
		}
		if stored := parseVector(t, g.Embedding); !slices.Equal(stored, w.vec) {
			t.Errorf("row %d: stored embedding differs from the one given", i)
		}
	}
	if got[0].ID == got[1].ID {
		t.Errorf("both rows have id %s", got[0].ID)
	}
}

func parseVector(t *testing.T, text string) []float32 {
	t.Helper()
	var v []float32
	for f := range strings.SplitSeq(strings.Trim(text, "[]"), ",") {
		x, err := strconv.ParseFloat(f, 32)
		if err != nil {
			t.Fatal(err)
		}
		v = append(v, float32(x))
	}
	return v
}

func TestAddWritesAllRowsOrNone(t *testing.T) {
	s, conn := open(t)
	err := s.Add(t.Context(), "facts", []Doc{
		{Document: "valid", Embedding: fill(1)},
		{Document: "invalid metadata", Embedding: fill(1), Metadata: meta(`{`)},
	})
	if err == nil {
		t.Fatal("Add succeeded with invalid metadata")
	}
	if n := count(t, conn, "SELECT count(*) FROM langchain_pg_embedding"); n != 0 {
		t.Errorf("%d rows written; want none", n)
	}
}

func TestMetadataReadsBackAsExistingRowsStoreIt(t *testing.T) {
	s, _ := open(t)
	add(t, s, "kb", Doc{
		Document:  "rich",
		Embedding: fill(0.5),
		Metadata:  meta(`{"confidence":1.0,"n":5,"is_dm":false,"s":"x","tags":["a"],"nested":{"k":null},"f":0.8}`),
	})
	hits := search(t, s, "kb", fill(0.5), nil, 1)
	want := `{"f": 0.8, "n": 5, "s": "x", "tags": ["a"], "is_dm": false, "nested": {"k": null}, "confidence": 1.0}`
	if len(hits) != 1 || string(hits[0].Metadata) != want {
		t.Fatalf("hits %+v; want one with metadata %s", hits, want)
	}
}

func TestSearchFiltersByMetadataWithinTheCollection(t *testing.T) {
	s, conn := open(t)
	add(t, s, "kb",
		Doc{Document: "a", Embedding: fill(1), Metadata: meta(`{"type":"public_kb","topic":"topic_a"}`)},
		Doc{Document: "b", Embedding: fill(1), Metadata: meta(`{"type":"public_kb","topic":"topic_b"}`)},
		Doc{Document: "private fact", Embedding: fill(1), Metadata: meta(`{"key":"chatbot:u1","scope":"private","type":"user_fact"}`)},
		Doc{Document: "public fact", Embedding: fill(1), Metadata: meta(`{"key":"chatbot:u1","scope":"public","type":"user_fact"}`)},
		Doc{Document: "typed", Embedding: fill(1), Metadata: meta(`{"n":5,"b":true}`)},
	)
	add(t, s, "other", Doc{Document: "elsewhere", Embedding: fill(1), Metadata: meta(`{"type":"public_kb","topic":"topic_a"}`)})
	execSQL(t, conn, `
		INSERT INTO langchain_pg_embedding (id, collection_id, embedding, document, cmetadata)
		SELECT 'no-metadata', uuid, $1, 'no metadata', NULL FROM langchain_pg_collection WHERE name = 'kb'`,
		vectorText(fill(1)))

	for _, tc := range []struct {
		filter map[string]string
		want   []string
	}{
		{nil, []string{"a", "b", "no metadata", "private fact", "public fact", "typed"}},
		{map[string]string{"topic": "topic_a"}, []string{"a"}},
		{map[string]string{"type": "public_kb"}, []string{"a", "b"}},
		{map[string]string{"key": "chatbot:u1"}, []string{"private fact", "public fact"}},
		{map[string]string{"key": "chatbot:u1", "scope": "public"}, []string{"public fact"}},
		{map[string]string{"topic": "topic_c"}, []string{}},
		{map[string]string{"n": "5"}, []string{}},
		{map[string]string{"b": "true"}, []string{}},
	} {
		got := documents(search(t, s, "kb", fill(1), tc.filter, 10))
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("filter %v: documents %q; want %q", tc.filter, got, tc.want)
		}
	}
	for _, h := range search(t, s, "kb", fill(1), nil, 10) {
		if *h.Document == "no metadata" && h.Metadata != nil {
			t.Errorf("NULL metadata read as %s; want nil", h.Metadata)
		}
	}
}

func TestSearchReturnsRowsWithoutADocument(t *testing.T) {
	s, conn := open(t)
	add(t, s, "kb", Doc{Document: "", Embedding: fill(1)})
	execSQL(t, conn, `
		INSERT INTO langchain_pg_embedding (id, collection_id, embedding, document, cmetadata)
		SELECT 'no-document', uuid, $1, NULL, '{}' FROM langchain_pg_collection WHERE name = 'kb'`,
		vectorText(fill(1)))

	hits := search(t, s, "kb", fill(1), nil, 10)
	if len(hits) != 2 {
		t.Fatalf("%d hits; want 2", len(hits))
	}
	for _, h := range hits {
		switch {
		case h.ID == "no-document" && h.Document != nil:
			t.Errorf("NULL document read as %q; want nil", *h.Document)
		case h.ID != "no-document" && (h.Document == nil || *h.Document != ""):
			t.Errorf("empty document read as %v; want a pointer to an empty string", h.Document)
		}
	}
}

func TestSearchRanksByCosineDistanceWithZeroVectorsLast(t *testing.T) {
	s, _ := open(t)
	lastAxis := fill(0)
	lastAxis[pg.EmbeddingDims-1] = 1
	mostlyHalf := fill(0.5)
	mostlyHalf[0] = 1
	add(t, s, "kb",
		Doc{Document: "zero", Embedding: fill(0)},
		Doc{Document: "last axis", Embedding: lastAxis},
		Doc{Document: "opposite", Embedding: fill(-0.5)},
		Doc{Document: "half", Embedding: fill(0.5)},
		Doc{Document: "mostly half", Embedding: mostlyHalf},
	)

	hits := search(t, s, "kb", fill(0.5), nil, 10)
	if got, want := documents(hits), []string{"half", "mostly half", "last axis", "opposite", "zero"}; !slices.Equal(got, want) {
		t.Fatalf("documents %q; want %q", got, want)
	}
	for _, h := range hits[:4] {
		if !(h.Distance >= 0) {
			t.Errorf("%s: distance %v; want a non-negative number", *h.Document, h.Distance)
		}
	}
	if d := hits[0].Distance; d != 0 {
		t.Errorf("identical vector: distance %v; want 0", d)
	}
	if d := hits[2].Distance; math.Abs(d-0.9744844818460086) > 1e-12 {
		t.Errorf("last axis: distance %v; want 0.9744844818460086", d)
	}
	if d := hits[4].Distance; !math.IsNaN(d) {
		t.Errorf("zero vector: distance %v; want NaN", d)
	}

	if got, want := documents(search(t, s, "kb", fill(0.5), nil, 3)), []string{"half", "mostly half", "last axis"}; !slices.Equal(got, want) {
		t.Errorf("limit 3: documents %q; want %q", got, want)
	}
	for _, h := range search(t, s, "kb", fill(0), nil, 10) {
		if !math.IsNaN(h.Distance) {
			t.Errorf("zero query vector, %s: distance %v; want NaN", *h.Document, h.Distance)
		}
	}
}

func TestVectorOfAnotherLengthIsRejectedBeforeAnyQuery(t *testing.T) {
	s, conn := open(t)
	ctx := t.Context()
	short := make([]float32, 3)
	for name, err := range map[string]error{
		"Add": s.Add(ctx, "facts", []Doc{{Document: "a", Embedding: fill(1)}, {Document: "b", Embedding: short}}),
		"Search": func() error {
			_, err := s.Search(ctx, "facts", short, nil, 1)
			return err
		}(),
	} {
		var dimErr *DimensionError
		if !errors.As(err, &dimErr) || dimErr.Len != 3 {
			t.Errorf("%s: error %v; want a DimensionError for length 3", name, err)
		}
	}
	if n := count(t, conn, "SELECT count(*) FROM langchain_pg_collection"); n != 0 {
		t.Errorf("%d collection rows; want none", n)
	}
}

func TestDeleteRemovesMatchingRowsOfTheCollection(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	add(t, s, "kb",
		Doc{Document: "keep me", Embedding: fill(1), Metadata: meta(`{"type":"public_kb","status":"keep"}`)},
		Doc{Document: "delete me", Embedding: fill(1), Metadata: meta(`{"type":"public_kb","status":"trash"}`)},
	)
	add(t, s, "other", Doc{Document: "elsewhere", Embedding: fill(1), Metadata: meta(`{"status":"trash"}`)})
	remaining := func(collection string) []string {
		return documents(search(t, s, collection, fill(1), nil, 10))
	}

	if n, err := s.Delete(ctx, "kb", map[string]string{"status": "trash"}); err != nil || n != 1 {
		t.Fatalf("Delete: %d, %v; want 1", n, err)
	}
	if got := remaining("kb"); !slices.Equal(got, []string{"keep me"}) {
		t.Errorf("after Delete: %q; want [keep me]", got)
	}

	if n, err := s.Delete(ctx, "kb", nil); err == nil || n != 0 {
		t.Errorf("Delete without a filter: %d, %v; want an error", n, err)
	}
	if got := remaining("kb"); !slices.Equal(got, []string{"keep me"}) {
		t.Errorf("after Delete without a filter: %q; want [keep me]", got)
	}

	if n, err := s.DeleteAll(ctx, "kb"); err != nil || n != 1 {
		t.Fatalf("DeleteAll: %d, %v; want 1", n, err)
	}
	if got := remaining("kb"); len(got) != 0 {
		t.Errorf("after DeleteAll: %q; want none", got)
	}
	if got := remaining("other"); !slices.Equal(got, []string{"elsewhere"}) {
		t.Errorf("other collection: %q; want [elsewhere]", got)
	}
}

func TestJSONMetadataColumnWithoutIndexes(t *testing.T) {
	dsn := testdb.New(t)
	conn := dial(t, dsn)
	execSQL(t, conn, `
		CREATE EXTENSION vector;
		CREATE TABLE langchain_pg_collection (
			uuid UUID NOT NULL, name VARCHAR NOT NULL, cmetadata JSONB, PRIMARY KEY (uuid), UNIQUE (name));
		CREATE TABLE langchain_pg_embedding (
			id VARCHAR NOT NULL, collection_id UUID, embedding VECTOR(1536), document VARCHAR, cmetadata JSON,
			PRIMARY KEY (id), FOREIGN KEY (collection_id) REFERENCES langchain_pg_collection (uuid) ON DELETE CASCADE);`)
	s := New(openDB(t, dsn))
	kb := `{"type":"public_kb","topic":"a","confidence":1.0}`
	add(t, s, "kb",
		Doc{Document: "knowledge", Embedding: fill(1), Metadata: meta(kb)},
		Doc{Document: "fact", Embedding: fill(1), Metadata: meta(`{"type":"user_fact"}`)},
	)

	hits := search(t, s, "kb", fill(1), map[string]string{"type": "public_kb"}, 10)
	if len(hits) != 1 || *hits[0].Document != "knowledge" || string(hits[0].Metadata) != kb {
		t.Errorf("hits %+v; want knowledge with metadata %s", hits, kb)
	}
	if n, err := s.Delete(t.Context(), "kb", map[string]string{"type": "user_fact"}); err != nil || n != 1 {
		t.Errorf("Delete: %d, %v; want 1", n, err)
	}
}

// lookup is one filtered search the engine issues.
type lookup struct {
	collection string
	filter     map[string]string
	limit      int
}

var lookups = []lookup{
	{"facts", map[string]string{"key": "chatbot:u1"}, 10},
	{"facts", map[string]string{"key": "chatbot:u2", "scope": "public"}, 10},
	{"facts", map[string]string{"key": "chatbot:u3", "scope": "private"}, 1},
	{"kb", map[string]string{"type": "public_kb"}, 20},
	{"kb", map[string]string{"type": "public_kb"}, 1},
}

// seed adds facts of users with skewed activity, and knowledge entries among other entries.
func seed(t *testing.T, s *Store, r *rand.Rand) {
	t.Helper()
	var facts, kb []Doc
	for _, u := range []struct {
		id    string
		facts int
	}{{"u1", 120}, {"u2", 60}, {"u3", 20}, {"u4", 5}} {
		for i := range u.facts {
			scope := []string{"private", "public", "public"}[i%3]
			facts = append(facts, Doc{
				Document:  fmt.Sprintf("%s fact %d", u.id, i),
				Embedding: randomVector(r),
				Metadata:  meta(fmt.Sprintf(`{"key":"chatbot:%s","scope":%q,"type":"user_fact","user_id":%q}`, u.id, scope, u.id)),
			})
		}
	}
	for i := range 180 {
		typ := "public_kb"
		if i%6 == 0 {
			typ = "other"
		}
		kb = append(kb, Doc{Document: fmt.Sprintf("entry %d", i), Embedding: randomVector(r), Metadata: meta(fmt.Sprintf(`{"type":%q}`, typ))})
	}
	add(t, s, "facts", facts...)
	add(t, s, "kb", kb...)
}

// textEqualityQuery is the lookup as text equality on each filter key, ordered directly by distance.
func textEqualityQuery(collectionID string, vec []float32, l lookup) (string, []any) {
	sql := "SELECT id FROM langchain_pg_embedding WHERE collection_id = $1"
	args := []any{collectionID, vectorText(vec), l.limit}
	for _, k := range slices.Sorted(maps.Keys(l.filter)) {
		args = append(args, k, l.filter[k])
		sql += fmt.Sprintf(" AND (cmetadata ->> $%d) = $%d", len(args)-1, len(args))
	}
	return sql + " ORDER BY embedding <=> $2 LIMIT $3", args
}

func queryIDs(t *testing.T, conn *pgx.Conn, sql string, args ...any) []string {
	t.Helper()
	rows, _ := conn.Query(context.Background(), sql, args...)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return ids
}

// usesVectorIndex reports whether the plan of sql orders rows by ix_vector_hnsw.
func usesVectorIndex(t *testing.T, conn *pgx.Conn, sql string, args ...any) bool {
	t.Helper()
	var plan json.RawMessage
	if err := conn.QueryRow(context.Background(), "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %s: %v", sql, err)
	}
	return strings.Contains(string(plan), "ix_vector_hnsw")
}

func TestSearchMatchesTheTextEqualityRankingWhereItScansSequentially(t *testing.T) {
	s, conn := open(t)
	r := rand.New(rand.NewPCG(30, 1))
	seed(t, s, r)
	execSQL(t, conn, "ANALYZE langchain_pg_embedding")

	for _, l := range lookups {
		for range 3 {
			q := randomVector(r)
			sql, args := textEqualityQuery(collectionID(t, conn, l.collection), q, l)
			if usesVectorIndex(t, conn, sql, args...) {
				t.Fatalf("%v: the text-equality query uses the vector index on this dataset", l)
			}
			want := queryIDs(t, conn, sql, args...)
			if got := hitIDs(search(t, s, l.collection, q, l.filter, l.limit)); !slices.Equal(got, want) {
				t.Errorf("%v: ids %q; want %q", l, got, want)
			}
		}
	}
}

func TestSearchRanksExactlyWhereTheVectorIndexWouldOrderTheScan(t *testing.T) {
	dsn := testdb.New(t)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.RawQuery += "&options=" + strings.ReplaceAll(url.QueryEscape("-c enable_seqscan=off -c enable_bitmapscan=off"), "+", "%20")
	indexPreferred := u.String()
	s, conn, exact := New(openDB(t, indexPreferred)), dial(t, indexPreferred), dial(t, dsn)
	execSQL(t, exact, "SET enable_indexscan = off")

	r := rand.New(rand.NewPCG(30, 2))
	docs := make([]Doc, 1000)
	for i := range docs {
		topic := "common"
		if i%50 == 0 {
			topic = "rare"
		}
		docs[i] = Doc{Document: strconv.Itoa(i), Embedding: randomVector(r), Metadata: meta(fmt.Sprintf(`{"type":"public_kb","topic":%q}`, topic))}
	}
	add(t, s, "kb", docs...)
	execSQL(t, conn, "ANALYZE langchain_pg_embedding")
	id := collectionID(t, conn, "kb")

	l := lookup{"kb", map[string]string{"topic": "rare"}, 10}
	vec := randomVector(r)
	sql, args := searchQuery(id, vec, l.filter, l.limit)
	unfenced := strings.Replace(sql, "OFFSET 0", "", 1)
	if unfenced == sql {
		t.Fatal("the search has no OFFSET 0")
	}
	if !usesVectorIndex(t, conn, unfenced, args...) {
		t.Fatal("without OFFSET 0 the search does not use the vector index here; the test cannot tell")
	}
	if usesVectorIndex(t, conn, sql, args...) {
		t.Error("the search uses the vector index")
	}
	exactSQL, exactArgs := textEqualityQuery(id, vec, l)
	want := queryIDs(t, exact, exactSQL, exactArgs...)
	if len(want) != l.limit {
		t.Fatalf("exact ranking has %d rows; want %d", len(want), l.limit)
	}
	if got := hitIDs(search(t, s, l.collection, vec, l.filter, l.limit)); !slices.Equal(got, want) {
		t.Errorf("ids %q; want %q", got, want)
	}
}
