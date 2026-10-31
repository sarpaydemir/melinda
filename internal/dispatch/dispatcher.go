package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/logging"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/routing"
	"github.com/sarpaydemir/dost/internal/store/kv"
	"github.com/sarpaydemir/dost/internal/store/pg"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

const (
	// intakeSize is how many events wait for the dispatcher before producers block.
	intakeSize = 256
	// recentSize is how many buffered message IDs are remembered, so a message delivered twice is buffered once.
	recentSize = 10_000
	// maxBackoff caps the wait between attempts to reach the store.
	maxBackoff = 30 * time.Second
)

// errMalformed marks a stored buffer that is not an object.
var errMalformed = errors.New("stored buffer is malformed")

type settingsSource interface {
	Current(ctx context.Context) *config.Settings
}

type store interface {
	UpdateAll(ctx context.Context, changes ...kv.Change) error
}

type notifier interface {
	// Notify blocks while the flusher's queue is full and returns ctx.Err() if ctx ends first.
	Notify(ctx context.Context, n Notice) error
}

// buffer is the stored form of a conversation's buffered messages.
type buffer struct {
	Generation string `json:"generation"`
	FlushAfter string `json:"flush_after"`
	// Payload is the first buffered message's Payload.
	Payload  json.RawMessage   `json:"payload"`
	Messages []json.RawMessage `json:"messages"`
}

// entry is one buffered message.
type entry struct {
	PostID  string   `json:"post_id"`
	UserID  string   `json:"user_id"`
	Message string   `json:"message"`
	FileIDs []string `json:"file_ids"`
}

// Dispatcher buffers the messages the agent answers per conversation and advances the last-seen marker past every
// message it reads. It processes events one at a time in arrival order.
type Dispatcher struct {
	work     context.Context
	settings settingsSource
	open     func(*config.Settings) (chat.Platform, error)
	identity *routing.Identity
	store    store
	flusher  notifier
	clk      clock.Clock
	log      *slog.Logger

	intake chan json.RawMessage
	recent recentIDs
}

// NewDispatcher returns a dispatcher that opens the chat platform from each event's settings snapshot. Its store
// writes and notices use work, which outlives Run's context so that queued events are processed on shutdown.
func NewDispatcher(work context.Context, settings settingsSource, open func(*config.Settings) (chat.Platform, error),
	identity *routing.Identity, store store, flusher notifier, clk clock.Clock, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		work: work, settings: settings, open: open, identity: identity, store: store, flusher: flusher, clk: clk,
		log: log, intake: make(chan json.RawMessage, intakeSize),
	}
}

// Name identifies the component.
func (d *Dispatcher) Name() string { return "dispatcher" }

// Enqueue queues an inbound event. It blocks while the queue is full and returns ctx.Err() if ctx ends first.
func (d *Dispatcher) Enqueue(ctx context.Context, event json.RawMessage) error {
	select {
	case d.intake <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run processes events until ctx ends, then processes the events already queued and returns.
func (d *Dispatcher) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		select {
		case e := <-d.intake:
			d.handle(e)
		case <-ctx.Done():
		}
	}
	for len(d.intake) > 0 {
		d.handle(<-d.intake)
	}
	return nil
}

func (d *Dispatcher) handle(event json.RawMessage) {
	s := d.settings.Current(d.work)
	p, err := d.open(s)
	if err != nil {
		d.log.Error("cannot open the chat platform; event skipped", "error", err)
		return
	}
	m, ok := p.Decode(event)
	if !ok {
		d.log.Debug("event is not a message")
		return
	}
	if d.recent.has(m.ID) {
		d.log.Debug("message already buffered; skipped", "post_id", m.ID)
		return
	}
	agent := p.Identity()
	if routing.ShouldEnrichIdentity(m, agent.ID, s.NameExcludedChannels) {
		agent = d.identity.Resolve(d.work, p)
	}

	var changes []kv.Change
	if !m.Created.IsZero() {
		changes = append(changes, advance(s.LastSeenKey, m.Created.UnixMilli()))
	}
	var n *Notice
	if routing.ShouldRespond(m, agent, s.NameExcludedChannels) {
		n = &Notice{Key: routing.ConversationKey(s.BufferPrefix, m)}
		window := time.Duration(s.DebounceSeconds * float64(time.Second))
		changes = append(changes, d.appendTo(n, m, window))
	} else {
		d.log.Debug("message not for the agent", "post_id", m.ID, "conversation_id", m.ConversationID,
			"direct", m.Kind == chat.Direct, "has_username", agent.Username != "")
	}
	if len(changes) == 0 {
		return
	}
	if err := d.write(changes); err != nil {
		d.log.Error("message not stored; skipped", "post_id", m.ID, "error", err)
		return
	}
	if n == nil {
		return
	}
	d.recent.add(m.ID)
	d.log.Info("message buffered", "post_id", m.ID, logging.KeyConversation, n.Key, "generation", n.Generation)
	if err := d.flusher.Notify(d.work, *n); err != nil {
		d.log.Warn("flusher not notified; the buffer is flushed at the next start", logging.KeyConversation, n.Key, "error", err)
	}
}

// write commits changes, retrying with backoff while the store is unreachable. It gives up on an error retrying
// cannot fix, or when the work context ends.
func (d *Dispatcher) write(changes []kv.Change) error {
	for wait := time.Second; ; wait = min(2*wait, maxBackoff) {
		err := d.store.UpdateAll(d.work, changes...)
		if err == nil || errors.Is(err, errMalformed) || pg.Rejected(err) {
			return err
		}
		d.log.Warn("store unavailable; retrying", "retry_in", wait, "error", err)
		if clock.Sleep(d.work, d.clk, wait) != nil {
			return err
		}
	}
}

// marker is the stored last-seen marker: the creation time, in Unix milliseconds, of the newest message read.
type marker struct {
	CreateAt *float64 `json:"create_at"`
}

// advance moves the last-seen marker under key forward to createAt; it never moves back.
func advance(key string, createAt int64) kv.Change {
	return kv.Change{Key: key, Fn: func(cur json.RawMessage) (json.RawMessage, error) {
		var v marker
		if json.Unmarshal(cur, &v) == nil && v.CreateAt != nil && float64(createAt) <= *v.CreateAt {
			return cur, nil
		}
		return fmt.Appendf(nil, `{"create_at": %d}`, createAt), nil
	}}
}

// appendTo adds m to the buffer under n.Key, due window after the append, and records the new generation and due
// time in n. The buffer keeps its first payload. A buffer that already holds m, as after a retry whose earlier
// commit went unconfirmed, only gets the new generation and due time.
func (d *Dispatcher) appendTo(n *Notice, m chat.Message, window time.Duration) kv.Change {
	e := entry{PostID: m.ID, UserID: m.AuthorID, Message: m.Text, FileIDs: m.AttachmentIDs}
	if e.FileIDs == nil {
		e.FileIDs = []string{}
	}
	return kv.Change{Key: n.Key, Fn: func(cur json.RawMessage) (json.RawMessage, error) {
		var stored struct {
			Payload  json.RawMessage   `json:"payload"`
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(cur, &stored); err != nil {
			return nil, fmt.Errorf("%w: %w", errMalformed, err)
		}
		n.Generation, n.FlushAfter = textfmt.NewUUID(), d.clk.Now().Add(window)
		b := buffer{Generation: n.Generation, FlushAfter: textfmt.FormatTime(n.FlushAfter), Payload: stored.Payload}
		if b.Payload == nil {
			b.Payload = m.Payload
		}
		b.Messages = stored.Messages
		if !slices.ContainsFunc(stored.Messages, func(raw json.RawMessage) bool {
			var prior struct {
				PostID string `json:"post_id"`
			}
			return json.Unmarshal(raw, &prior) == nil && prior.PostID == m.ID
		}) {
			raw, _ := json.Marshal(e) // strings only
			b.Messages = append(b.Messages, raw)
		}
		return json.Marshal(b)
	}}
}

// recentIDs remembers the last recentSize IDs added, forgetting the oldest first.
type recentIDs struct {
	seen  map[string]bool
	order []string
	next  int
}

func (r *recentIDs) has(id string) bool { return r.seen[id] }

func (r *recentIDs) add(id string) {
	if r.seen == nil {
		r.seen = make(map[string]bool, recentSize)
	}
	if len(r.order) < recentSize {
		r.order = append(r.order, id)
	} else {
		delete(r.seen, r.order[r.next])
		r.order[r.next] = id
		r.next = (r.next + 1) % recentSize
	}
	r.seen[id] = true
}
