package vector

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sarpaydemir/dost/internal/store/pg"
	"github.com/sarpaydemir/dost/internal/testkit/snapshot"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

// TestSnapshotRowsDecode checks every row of the facts and knowledge collections of a database copy: a
// full-length embedding, and metadata that is a JSON object whose search key (key, type) is a string.
func TestSnapshotRowsDecode(t *testing.T) {
	conn, s := snapshot.Open(t)
	for collection, searchKey := range map[string]string{s.FactsCollection: "key", s.KBCollection: "type"} {
		t.Run(collection, func(t *testing.T) {
			id := snapshot.CollectionID(t, conn, collection)
			if id == "" {
				t.Skip("the copy has no such collection")
			}
			rows, _ := conn.Query(context.Background(), `
				SELECT id, coalesce(vector_dims(embedding), 0), cmetadata::text
				FROM langchain_pg_embedding WHERE collection_id = $1 ORDER BY id`, id)
			var n, dims int
			var rowID string
			var meta *string
			_, err := pgx.ForEachRow(rows, []any{&rowID, &dims, &meta}, func() error {
				n++
				if dims != pg.EmbeddingDims {
					t.Errorf("row %s: embedding has %d dimensions", rowID, dims)
				}
				if meta == nil {
					t.Errorf("row %s: no metadata", rowID)
					return nil
				}
				v, err := textfmt.Decode([]byte(*meta), true)
				obj, ok := v.(textfmt.Object)
				if err != nil || !ok {
					t.Errorf("row %s: metadata is not an object: %.200s", rowID, *meta)
					return nil
				}
				if _, ok := obj.Get(searchKey).(string); !ok {
					t.Errorf("row %s: metadata %s is not a string: %.200s", rowID, searchKey, *meta)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%d rows", n)
		})
	}
}

// recorded is one recorded search: the limit rows of collection nearest to the embedding of row query among
// those whose metadata holds filter, as ids nearest first.
type recorded struct {
	Collection string            `json:"collection"`
	Query      string            `json:"query_id"`
	Filter     map[string]string `json:"filter"`
	Limit      int               `json:"limit"`
	IDs        []string          `json:"ids"`
}

// TestSnapshotSearchesReturnTheRecordedIDs runs each search recorded in the file named by DOST_COMPAT_SEARCHES on
// the database copy and expects the recorded ids in the recorded order, where rows at an equal distance may trade
// places.
func TestSnapshotSearchesReturnTheRecordedIDs(t *testing.T) {
	conn, _ := snapshot.Open(t)
	path := os.Getenv(snapshot.EnvSearches)
	if path == "" {
		t.Skipf("%s is not set", snapshot.EnvSearches)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	lines.Buffer(nil, 1<<20)
	var n int
	for lines.Scan() {
		if strings.TrimSpace(lines.Text()) == "" {
			continue
		}
		n++
		var r recorded
		if err := json.Unmarshal(lines.Bytes(), &r); err != nil {
			t.Fatalf("search %d: %v", n, err)
		}
		vec, ids, distances := replaySearch(t, conn, r)
		if !sameRanking(t, conn, vec, ids, distances, r.IDs) {
			t.Errorf("search %d (%s, query %s, %v, limit %d):\n got %v\nwant %v", n, r.Collection, r.Query, r.Filter, r.Limit, ids, r.IDs)
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d searches", n)
}

// search runs r with the statement Store.Search uses.
func replaySearch(t *testing.T, conn *pgx.Conn, r recorded) ([]float32, []string, []float64) {
	t.Helper()
	ctx := context.Background()
	collection := snapshot.CollectionID(t, conn, r.Collection)
	var text string
	if err := conn.QueryRow(ctx, "SELECT embedding::text FROM langchain_pg_embedding WHERE id = $1", r.Query).Scan(&text); err != nil {
		t.Fatalf("query row %s: %v", r.Query, err)
	}
	vec := parseVector(t, text)
	sql, args := searchQuery(collection, vec, r.Filter, r.Limit)
	rows, _ := conn.Query(ctx, sql, args...)
	hits, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Hit])
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(hits))
	distances := make([]float64, len(hits))
	for i, h := range hits {
		ids[i], distances[i] = h.ID, h.Distance
	}
	return vec, ids, distances
}

// sameRanking reports whether want lists ids, except that where they differ the ids are at the same distance.
func sameRanking(t *testing.T, conn *pgx.Conn, vec []float32, ids []string, distances []float64, want []string) bool {
	if len(ids) != len(want) {
		return false
	}
	rows, _ := conn.Query(context.Background(), "SELECT id, embedding <=> $1 FROM langchain_pg_embedding WHERE id = ANY($2)", vectorText(vec), want)
	wantDistance := map[string]float64{}
	var id string
	var d float64
	if _, err := pgx.ForEachRow(rows, []any{&id, &d}, func() error { wantDistance[id] = d; return nil }); err != nil {
		t.Fatal(err)
	}
	for i := range ids {
		if d, ok := wantDistance[want[i]]; ids[i] != want[i] && (!ok || d != distances[i]) {
			return false
		}
	}
	return true
}

func parseVector(t *testing.T, text string) []float32 {
	t.Helper()
	parts := strings.Split(strings.Trim(text, "[]"), ",")
	vec := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(p, 32)
		if err != nil {
			t.Fatalf("embedding %q: %v", p, err)
		}
		vec[i] = float32(f)
	}
	return vec
}
