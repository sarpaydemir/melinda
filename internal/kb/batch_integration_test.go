//go:build integration

package kb

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/store/kv"
	"github.com/sarpaydemir/dost/internal/store/pg"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/testdb"
)

func TestQueuedItemsRoundTripThroughPostgresWithTheirNumberKinds(t *testing.T) {
	ctx := context.Background()
	db, err := pg.Open(ctx, testdb.New(t), clock.Real(), discard)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	store := kv.New(db, clock.Real())
	b := Batch{Store: store, Learn: NewTrigger(), Log: discard}

	queued := `{"post_id": "a", "create_at": 1780000000.0, "big": 12345678901234567890, "f": 1.5,
		"processing_started_at": 1780000000999, "status": "processing", "unknown": {"n": [1, 2.0]}},
		{"post_id": "b", "message": "m"}`
	if err := store.Set(ctx, batchKey, json.RawMessage(`{"messages": [`+queued+`]}`)); err != nil {
		t.Fatal(err)
	}
	if err := b.Append(ctx, batchSettings(false), answered(), "hi there"); err != nil {
		t.Fatal(err)
	}
	automation := []string{
		`{"post_id": "auto:1:1780000000", "message": "step 1", "channel_id": "automation", "create_at": 1780000000.0}`,
		`{"post_id": "auto:2:1780000000", "message": "step 2", "channel_id": "automation", "create_at": 1780000000.25}`,
	}
	for _, item := range automation {
		if err := b.AppendRaw(ctx, batchSettings(false), answered(), json.RawMessage(item)); err != nil {
			t.Fatal(err)
		}
	}

	got, _, err := store.Get(ctx, batchKey)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"messages": [` + queued + `, {"post_id": "p1", "channel_id": "c1", "user_id": "u1",
		"create_at": 1780000000123, "context_id": "r1", "root_id": "r1", "is_thread": true,
		"message": "u1: hello\nagent: hi there", "status": "pending"}, ` + automation[0] + `, ` + automation[1] + `]}`
	if eq, err := testkit.JSONEqual(got, []byte(want)); err != nil || !eq {
		t.Errorf("batch = %s\nwant    %s (err %v)", got, want, err)
	}
}
