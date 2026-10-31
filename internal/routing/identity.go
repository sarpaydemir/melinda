package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

const (
	// identityTTL is how long a username the platform reported is reused.
	identityTTL = 24 * time.Hour
	// lookupPause is how long a failed lookup is not repeated, so a slow platform cannot stall intake.
	lookupPause = 30 * time.Second
)

type identityStore interface {
	Get(ctx context.Context, key string) (json.RawMessage, bool, error)
	Set(ctx context.Context, key string, value json.RawMessage) error
}

// agent is the part of a chat platform that reports the agent's own account.
type agent interface {
	Identity() chat.User
	Self(ctx context.Context) (chat.User, error)
}

// cachedIdentity is the value stored under the identity key.
type cachedIdentity struct {
	Username  string `json:"username"`
	BotID     string `json:"bot_id"`
	ExpiresAt string `json:"expires_at"`
}

// username returns the cached username if the entry belongs to agent id and expires after now.
func (c cachedIdentity) username(id string, now time.Time) string {
	if c.Username == "" || c.BotID != id {
		return ""
	}
	t, naive, err := textfmt.ParseTime(c.ExpiresAt)
	if err != nil || naive || !t.After(now) {
		return ""
	}
	return c.Username
}

// Identity resolves the agent's username, caching a looked-up one in the store and in process. It is safe for
// concurrent use.
type Identity struct {
	store identityStore
	key   string
	clk   clock.Clock
	log   *slog.Logger

	mu         sync.Mutex
	last       cachedIdentity
	pauseUntil time.Time
}

// NewIdentity returns an Identity that caches under key in store.
func NewIdentity(store identityStore, key string, clk clock.Clock, log *slog.Logger) *Identity {
	return &Identity{store: store, key: key, clk: clk, log: log}
}

// Resolve returns the agent's account with its configured username, else an unexpired cached one, else the one
// the platform reports, which is cached for 24 hours. When none is available, Username is empty.
func (r *Identity) Resolve(ctx context.Context, p agent) chat.User {
	me := p.Identity()
	if me.Username != "" {
		return me
	}
	now := r.clk.Now()
	r.mu.Lock()
	me.Username = r.last.username(me.ID, now)
	paused := now.Before(r.pauseUntil)
	r.mu.Unlock()
	if me.Username == "" {
		me.Username = r.load(ctx, me.ID, now)
	}
	if me.Username == "" && !paused {
		me.Username = r.lookup(ctx, p, me.ID)
	}
	return me
}

// Verify returns an error when the platform reports a different ID for the agent's account than the configured
// one, since the agent would then answer its own posts. A failed lookup is only logged.
func (r *Identity) Verify(ctx context.Context, p agent) error {
	self, err := p.Self(ctx)
	if err != nil {
		r.log.Warn("could not verify the agent's account", "error", err)
		return nil
	}
	if id := p.Identity().ID; self.ID != id {
		return fmt.Errorf("the configured agent ID %q is not the token's account; the platform reports %q", id, self.ID)
	}
	return nil
}

// load returns the username of the stored entry when it is usable, keeping the entry in process.
func (r *Identity) load(ctx context.Context, id string, now time.Time) string {
	raw, ok, err := r.store.Get(ctx, r.key)
	if err != nil {
		r.log.Warn("could not read the cached agent identity", "error", err)
	}
	var c cachedIdentity
	if !ok || json.Unmarshal(raw, &c) != nil || c.username(id, now) == "" {
		return ""
	}
	r.keep(c)
	return c.Username
}

// lookup asks the platform for the agent's username and caches it. After a failure that was not a cancellation,
// lookups pause.
func (r *Identity) lookup(ctx context.Context, p agent, id string) string {
	self, err := p.Self(ctx)
	now := r.clk.Now()
	if err != nil || self.Username == "" {
		r.log.Warn("the platform did not report the agent's username; only direct messages and excluded conversations are answered", "error", err)
		if !errors.Is(ctx.Err(), context.Canceled) {
			r.mu.Lock()
			r.pauseUntil = now.Add(lookupPause)
			r.mu.Unlock()
		}
		return ""
	}
	c := cachedIdentity{Username: self.Username, BotID: id, ExpiresAt: textfmt.FormatTime(now.Add(identityTTL))}
	r.keep(c)
	b, _ := json.Marshal(c) // a struct of strings always marshals
	if err := r.store.Set(ctx, r.key, b); err != nil {
		r.log.Warn("could not cache the agent identity", "error", err)
	}
	return c.Username
}

func (r *Identity) keep(c cachedIdentity) {
	r.mu.Lock()
	r.last = c
	r.mu.Unlock()
}
