package kb

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sarpaydemir/melinda/internal/testkit/snapshot"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

// TestSnapshotEntryTimesRead checks that every knowledge entry of a database copy that carries a creation time or
// timestamp gets a time for the recency preference.
func TestSnapshotEntryTimesRead(t *testing.T) {
	conn, s := snapshot.Open(t)
	id := snapshot.CollectionID(t, conn, s.KBCollection)
	if id == "" {
		t.Skip("the copy has no knowledge collection")
	}
	rows, _ := conn.Query(context.Background(),
		"SELECT id, cmetadata::text FROM langchain_pg_embedding WHERE collection_id = $1 AND cmetadata IS NOT NULL ORDER BY id", id)
	var n int
	var rowID, meta string
	_, err := pgx.ForEachRow(rows, []any{&rowID, &meta}, func() error {
		n++
		v, _ := textfmt.Decode([]byte(meta), true)
		obj, _ := v.(textfmt.Object)
		if !textfmt.Truthy(obj.Get("source_create_at_max")) && !textfmt.Truthy(obj.Get("timestamp")) {
			return nil
		}
		if _, ok := entryTime(json.RawMessage(meta)); !ok {
			t.Errorf("entry %s: no time read from %.300s", rowID, meta)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d entries", n)
}

// TestSnapshotBatchDecodes checks that the learning queue of a database copy reads as a list of objects, each with
// a post ID and no status other than pending or processing.
func TestSnapshotBatchDecodes(t *testing.T) {
	conn, s := snapshot.Open(t)
	var value string
	err := conn.QueryRow(context.Background(), "SELECT value::text FROM key_value_store WHERE key = $1", s.KBBatchKey).Scan(&value)
	if err == pgx.ErrNoRows {
		t.Skip("the copy has no learning queue")
	}
	if err != nil {
		t.Fatal(err)
	}
	raws, err := queued(json.RawMessage(value))
	if err != nil {
		t.Fatal(err)
	}
	for i, raw := range raws {
		var q queuedItem
		if json.Unmarshal(raw, &q) != nil || q == nil {
			t.Errorf("item %d is not an object: %.300s", i, raw)
			continue
		}
		if q.id() == "" {
			t.Errorf("item %d has no post ID: %.300s", i, raw)
		}
		if status := q.get("status"); status != nil && status != "pending" && status != "processing" {
			t.Errorf("item %d has status %v", i, status)
		}
	}
	t.Logf("%d items", len(raws))
}
