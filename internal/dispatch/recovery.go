package dispatch

import (
	"context"
	"encoding/json"
	"log/slog"
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
}

// NewRecovery returns a recovery that replays into d with d's settings, platform, identity, clock, and work
// context, which outlives Run's context so that a run in progress at shutdown can finish.
func NewRecovery(d *Dispatcher, store markerStore, log *slog.Logger) *Recovery {
	return &Recovery{d: d, store: store, log: log, pending: make(chan struct{}, 1)}
}

// Name identifies the component.
func (r *Recovery) Name() string { return "recovery" }

// Trigger asks for a run. It never blocks.
func (r *Recovery) Trigger() {
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
	after, err := r.lastSeen(s.LastSeenKey)
	if err != nil {
		r.log.Error("cannot read the last-seen marker; recovery skipped", "error", err)
		return
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
