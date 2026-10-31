//go:build integration

package dispatch_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/store/kv"
	"github.com/sarpaydemir/dost/internal/store/pg"
	"github.com/sarpaydemir/dost/internal/testkit/testdb"
)

func openStore(t *testing.T) *kv.Store {
	t.Helper()
	db, err := pg.Open(context.Background(), testdb.New(t), clock.Real(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	return kv.New(db, clock.Real())
}

func TestAppendsRacingTakesLoseAndRepeatNoMessage(t *testing.T) {
	f := newFixture(t)
	s := openStore(t)
	f.use(s)
	ctx, cancel := context.WithCancel(t.Context())
	done := f.run(ctx)

	key := prefix + "dm:user-a"
	var taken []string
	take := func() {
		raw, ok, err := s.Get(t.Context(), key)
		var b struct{ Generation string }
		if err != nil || !ok || json.Unmarshal(raw, &b) != nil {
			return
		}
		v, deleted, err := s.DeleteIf(t.Context(), key, func(cur json.RawMessage) bool {
			var c struct{ Generation string }
			return json.Unmarshal(cur, &c) == nil && c.Generation == b.Generation
		})
		if err != nil {
			t.Error(err)
		}
		if deleted {
			taken = append(taken, postIDs(string(v))...)
		}
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			default:
				take()
			}
		}
	}()

	const n = 200
	var want []string
	for i := range n {
		id := fmt.Sprintf("p%03d", i)
		want = append(want, id)
		f.enqueue(msg(id, "user-a", "hi", direct))
	}
	f.flusher.await(t, n)
	close(stop)
	<-stopped
	take()
	cancel()
	f.wait(done)

	if !slices.Equal(taken, want) {
		t.Errorf("taken messages = %q, want each of the %d appended once, in order", taken, n)
	}
}

func TestMessageTheDatabaseRejectsIsSkipped(t *testing.T) {
	f := newFixture(t)
	f.use(openStore(t))
	f.deliver(
		msg("p1", "user-a", "a NUL \x00 character", direct, createdAt(3000)),
		msg("p2", "user-b", "hi", direct, createdAt(2000)),
	)
	if v := f.value(prefix + "dm:user-a"); v != "" {
		t.Errorf("rejected message was buffered: %s", v)
	}
	assertJSON(t, "last seen", f.value(marker), `{"create_at": 2000}`)
	if n := f.flusher.received(); len(n) != 1 || n[0].Key != prefix+"dm:user-b" {
		t.Errorf("notices = %+v, want one for the message after the rejected one", n)
	}
	if logs := f.logs.String(); !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "post_id=p1") {
		t.Errorf("logs do not report the rejected message:\n%s", logs)
	}
}
