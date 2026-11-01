package dispatch

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

type markerStore interface {
	Get(ctx context.Context, key string) (json.RawMessage, bool, error)
}

// Recovery replays the messages created after the last-seen marker into the dispatcher's intake. Each Trigger asks
// for a run; one runs at a time, and the triggers that arrive during a run start one more after it.
type Recovery struct {
	d     *Dispatcher
	store markerStore
	log   *slog.Logger

	pending chan struct{}

	mu sync.Mutex
	// from is where the next run replays from: the earliest marker recorded at a disconnect or read by a run
	// since the last run that covered them all. Zero means the next run reads the marker.
	from time.Time
	// drops counts disconnects; down reports whether the latest one has not reconnected yet.
	drops int
	down  bool
}

// NewRecovery returns a recovery that replays into d with d's settings, platform, identity, clock, and work
// context, which outlives Run's context so that a run in progress at shutdown can finish.
func NewRecovery(d *Dispatcher, store markerStore, log *slog.Logger) *Recovery {
	return &Recovery{d: d, store: store, log: log, pending: make(chan struct{}, 1)}
}

// Name identifies the component.
func (r *Recovery) Name() string { return "recovery" }

// Disconnected records the marker as it stands before the first connection and whenever the connection drops, so
// that the run after connecting replays from there even after live messages on the new connection move the marker.
// It must return before the next connection delivers messages.
func (r *Recovery) Disconnected() {
	at, err := r.lastSeen(r.d.settings.Current(r.d.work).LastSeenKey)
	r.mu.Lock()
	r.drops++
	r.down = true
	r.mu.Unlock()
	if err != nil {
		r.log.Error("cannot read the last-seen marker at disconnect; the next run reads it then", "error", err)
		return
	}
	r.keep(at)
}

// keep makes the next run replay from at or earlier.
func (r *Recovery) keep(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.from.IsZero() || at.Before(r.from) {
		r.from = at
	}
}

// Trigger asks for a run after a connection is made. It never blocks.
func (r *Recovery) Trigger() {
	r.mu.Lock()
	r.down = false
	r.mu.Unlock()
	select {
	case r.pending <- struct{}{}:
	default:
	}
}

// Run starts a run for each trigger until ctx ends. A run in progress then finishes; a pending one is dropped.
func (r *Recovery) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.pending:
		}
		if ctx.Err() != nil {
			return nil
		}
		r.run()
	}
}

func (r *Recovery) run() {
	work := r.d.work
	s := r.d.settings.Current(work)
	p, err := r.d.open(s)
	if err != nil {
		r.log.Error("cannot open the chat platform; recovery skipped", "error", err)
		return
	}
	// Resolves the identity on every run, filling its cache when missing or expired.
	r.d.identity.Resolve(work, p)
	r.mu.Lock()
	after, drops, connected := r.from, r.drops, !r.down
	r.mu.Unlock()
	if after.IsZero() {
		if after, err = r.lastSeen(s.LastSeenKey); err != nil {
			r.log.Error("cannot read the last-seen marker; recovery skipped", "error", err)
			return
		}
		r.keep(after)
	}
	replayed := 0
	err = p.Replay(work, after, func(ctx context.Context, event json.RawMessage) error {
		if err := r.d.Enqueue(ctx, event); err != nil {
			return err
		}
		replayed++
		return nil
	})
	if err != nil {
		r.log.Error("recovery stopped early", "replayed", replayed, "error", err)
		return
	}
	// The replay covers every recorded disconnect only if it ran connected and no disconnect happened since.
	r.mu.Lock()
	if connected && r.drops == drops {
		r.from = time.Time{}
	}
	r.mu.Unlock()
	r.log.Info("recovery finished", "replayed", replayed)
}

// lastSeen returns the time in the marker under key, or now when there is no marker with a time, so that a first
// start replays nothing.
func (r *Recovery) lastSeen(key string) (time.Time, error) {
	raw, ok, err := r.store.Get(r.d.work, key)
	if err != nil {
		return time.Time{}, err
	}
	var m marker
	if !ok || json.Unmarshal(raw, &m) != nil || m.CreateAt == nil {
		return r.d.clk.Now(), nil
	}
	return time.UnixMilli(int64(*m.CreateAt)), nil
}
