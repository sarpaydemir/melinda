package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/logging"
	"github.com/sarpaydemir/dost/internal/routing"
	"github.com/sarpaydemir/dost/internal/store/journal"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

const (
	// noticeSize is how many notices wait for the flusher before Notify blocks.
	noticeSize = 1024
	// maxWaiting is how many runs of one conversation may wait in the pipeline before its buffer is left to merge.
	maxWaiting = 4
	// busyRetry is how long a buffer left to merge waits before the flusher looks at it again.
	busyRetry = time.Second
	// maxResubmissions is how many times a journaled run is resubmitted at startup before it is dropped.
	maxResubmissions = 2
)

type buffers interface {
	Get(ctx context.Context, key string) (json.RawMessage, bool, error)
	List(ctx context.Context, prefix string) (map[string]json.RawMessage, error)
}

type inflight interface {
	// Take deletes the buffer under key if its generation is generation and journals it, in one transaction.
	Take(ctx context.Context, key, generation string) (json.RawMessage, bool, error)
	Resubmit(ctx context.Context, limit int) (records, expired []journal.Record, err error)
}

type instanceLock interface {
	Acquire(ctx context.Context) error
	// Wait returns nil when ctx ends and an error when the lock is lost.
	Wait(ctx context.Context) error
}

// timer is a conversation's pending flush.
type timer struct {
	generation string
	at         time.Time
	backoff    time.Duration // the wait after the last store error
}

// Flusher hands each conversation's buffer to the pipeline once its debounce window has closed. One goroutine owns
// every timer and take, and only while the process holds the agent's instance lock.
type Flusher struct {
	prefix   string
	buffers  buffers
	runs     inflight
	lock     instanceLock
	pipeline Scheduler
	clk      clock.Clock
	log      *slog.Logger

	notices chan Notice
	due     map[string]timer
}

// NewFlusher returns a flusher for the buffers under prefix. Start acquires lock, and it stays held after Run
// returns: the runs handed over may still be going, so its owner releases it once the pipeline has stopped.
func NewFlusher(prefix string, buffers buffers, runs inflight, lock instanceLock, pipeline Scheduler, clk clock.Clock, log *slog.Logger) *Flusher {
	return &Flusher{
		prefix: prefix, buffers: buffers, runs: runs, lock: lock, pipeline: pipeline, clk: clk, log: log,
		notices: make(chan Notice, noticeSize), due: map[string]timer{},
	}
}

// Name identifies the component.
func (f *Flusher) Name() string { return "flusher" }

// Notify queues n. It blocks while the queue is full and returns ctx.Err() if ctx ends first.
func (f *Flusher) Notify(ctx context.Context, n Notice) error {
	select {
	case f.notices <- n:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Start takes the instance lock, resubmits the journaled runs oldest first, and schedules every stored buffer.
func (f *Flusher) Start(ctx context.Context) error {
	if err := f.lock.Acquire(ctx); err != nil {
		return err
	}
	return f.resume(ctx)
}

func (f *Flusher) resume(ctx context.Context) error {
	records, expired, err := f.runs.Resubmit(ctx, maxResubmissions)
	if err != nil {
		return err
	}
	for _, r := range expired {
		b, _ := parse(r.Buffer)
		f.log.Error("run dropped after repeated restarts", logging.KeyConversation, r.Key, "generation", r.Generation, "post_ids", b.postIDs())
	}
	for _, r := range records {
		b, err := parse(r.Buffer)
		if err != nil {
			f.log.Error("journaled run is malformed; not resubmitted", logging.KeyConversation, r.Key, "generation", r.Generation, "error", err)
			continue
		}
		submit, _, err := f.pipeline.Reserve(ctx)
		if err != nil {
			return err
		}
		submit(b.job(r.Key))
		f.log.Info("run resubmitted", logging.KeyConversation, r.Key, "generation", r.Generation, "post_ids", b.postIDs())
	}
	all, err := f.buffers.List(ctx, f.prefix)
	if err != nil {
		return err
	}
	for key, v := range all {
		b, err := parse(v)
		if err != nil {
			f.log.Error("malformed buffer left in place", logging.KeyConversation, key, "error", err)
			continue
		}
		f.due[key] = timer{generation: b.generation, at: b.flushAfter}
	}
	return nil
}

// Run flushes buffers as they fall due until ctx ends or the instance lock is lost. Pending timers and queued notices
// are dropped; their buffers stay stored for the next start.
func (f *Flusher) Run(ctx context.Context) error {
	ctx, lost := context.WithCancelCause(ctx)
	defer lost(nil)
	held := make(chan struct{})
	go func() {
		defer close(held)
		if err := f.lock.Wait(ctx); err != nil {
			lost(err)
		}
	}()
	f.loop(ctx)
	<-held
	if err := context.Cause(ctx); !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func (f *Flusher) loop(ctx context.Context) {
	t := f.clk.NewTimer(0)
	defer t.Stop()
	for {
		var fire <-chan time.Time
		key, at, ok := f.next()
		if ok {
			t.Reset(at.Sub(f.clk.Now()))
			fire = t.C()
		} else {
			t.Stop()
		}
		select {
		case <-ctx.Done():
			return
		case n := <-f.notices:
			f.due[n.Key] = timer{generation: n.Generation, at: n.FlushAfter}
		case <-fire:
			f.flush(ctx, key)
		}
	}
}

// next returns the key due first and when it is due.
func (f *Flusher) next() (key string, at time.Time, ok bool) {
	for k, t := range f.due {
		if !ok || t.at.Before(at) || t.at.Equal(at) && k < key {
			key, at, ok = k, t.at, true
		}
	}
	return key, at, ok
}

// flush hands the buffer under key to the pipeline if it still has the timer's generation and its window has closed.
func (f *Flusher) flush(ctx context.Context, key string) {
	t := f.due[key]
	delete(f.due, key)
	v, ok, err := f.buffers.Get(ctx, key)
	if err != nil {
		f.retry(ctx, key, t, err)
		return
	}
	if !ok {
		return
	}
	b, err := parse(v)
	switch {
	case err != nil:
		f.log.Error("malformed buffer left in place", logging.KeyConversation, key, "error", err)
		return
	case b.generation != t.generation || len(b.fragments) == 0:
		return
	case b.flushAfter.After(f.clk.Now()):
		f.due[key] = timer{generation: t.generation, at: b.flushAfter}
		return
	case f.pipeline.Waiting(key) >= maxWaiting:
		f.due[key] = timer{generation: t.generation, at: f.clk.Now().Add(busyRetry)}
		return
	}
	submit, cancel, err := f.pipeline.Reserve(ctx)
	if err != nil {
		return
	}
	_, ok, err = f.runs.Take(ctx, key, t.generation)
	if err != nil || !ok {
		cancel()
		if err != nil {
			f.retry(ctx, key, t, err)
		}
		return
	}
	submit(b.job(key))
	f.log.Info("conversation taken", logging.KeyConversation, key, "generation", t.generation, "fragments", len(b.fragments), "post_ids", b.postIDs())
}

// retry re-arms key's timer after a store error, waiting 1 s at first and twice as long each time up to 30 s.
func (f *Flusher) retry(ctx context.Context, key string, t timer, err error) {
	if ctx.Err() != nil {
		return
	}
	t.backoff = min(max(2*t.backoff, time.Second), maxBackoff)
	t.at = f.clk.Now().Add(t.backoff)
	f.due[key] = t
	f.log.Warn("store unavailable; flush retried", logging.KeyConversation, key, "retry_in", t.backoff, "error", err)
}

// parsed is a stored buffer read for a flush.
type parsed struct {
	generation string
	flushAfter time.Time // zero when flush_after is missing or not a timestamp
	payload    json.RawMessage
	fragments  []fragment
}

type fragment struct {
	postID, author, text string
	fileIDs              []string
}

// parse reads a stored buffer. A flush_after without an offset is UTC; null file_ids are none.
func parse(v json.RawMessage) (parsed, error) {
	var b buffer
	if err := json.Unmarshal(v, &b); err != nil {
		return parsed{}, fmt.Errorf("%w: %w", errMalformed, err)
	}
	if b.Generation == "" {
		return parsed{}, fmt.Errorf("%w: no generation", errMalformed)
	}
	p := parsed{generation: b.Generation, payload: b.Payload}
	p.flushAfter, _, _ = textfmt.ParseTime(b.FlushAfter)
	for i, raw := range b.Messages {
		var m struct {
			PostID  string          `json:"post_id"`
			UserID  json.RawMessage `json:"user_id"`
			Message json.RawMessage `json:"message"`
			FileIDs []string        `json:"file_ids"`
		}
		var author, text string
		err := json.Unmarshal(raw, &m)
		if err == nil {
			author, err = str(m.UserID, "user_id")
		}
		if err == nil {
			text, err = str(m.Message, "message")
		}
		if err != nil {
			return parsed{}, fmt.Errorf("%w: message %d: %w", errMalformed, i, err)
		}
		p.fragments = append(p.fragments, fragment{postID: m.PostID, author: author, text: text, fileIDs: m.FileIDs})
	}
	return p, nil
}

// str prints the stored JSON value of field as text; a missing value is an error.
func str(raw json.RawMessage, field string) (string, error) {
	if raw == nil {
		return "", fmt.Errorf("no %s", field)
	}
	v, err := textfmt.Decode(raw, false)
	if err != nil {
		return "", err
	}
	return textfmt.Str(v), nil
}

// job returns the run for p's fragments under key: one "<author>: <text>" line per fragment and the ordered union of
// their attachments.
func (p parsed) job(key string) routing.Job {
	j := routing.Job{Key: key, Generation: p.generation, Payload: p.payload, MessageIDs: p.postIDs()}
	lines := make([]string, len(p.fragments))
	for i, m := range p.fragments {
		lines[i] = m.author + ": " + m.text
		for _, id := range m.fileIDs {
			if !slices.Contains(j.AttachmentIDs, id) {
				j.AttachmentIDs = append(j.AttachmentIDs, id)
			}
		}
	}
	j.Text = strings.Join(lines, "\n")
	return j
}

func (p parsed) postIDs() []string {
	ids := make([]string, len(p.fragments))
	for i, m := range p.fragments {
		ids[i] = m.postID
	}
	return ids
}
