package routing_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/routing"
	"github.com/sarpaydemir/dost/internal/testkit/fakeclock"
	"github.com/sarpaydemir/dost/internal/testkit/kvtest"
)

const (
	identityKey = "agent:bot_identity"
	agentID     = "agent-id"
)

var start = time.Date(2026, 10, 31, 10, 0, 0, 123456000, time.UTC)

// fakeAgent reports a configured account and answers Self with self and err.
type fakeAgent struct {
	configured chat.User
	self       chat.User
	err        error
	calls      atomic.Int32
}

func (a *fakeAgent) Identity() chat.User { return a.configured }

func (a *fakeAgent) Self(ctx context.Context) (chat.User, error) {
	a.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return chat.User{}, err
	}
	return a.self, a.err
}

// reportsSelf is an agent without a configured username whose platform reports "helper".
func reportsSelf() *fakeAgent {
	return &fakeAgent{configured: chat.User{ID: agentID}, self: chat.User{ID: agentID, Username: "helper"}}
}

// countingStore counts reads and fails every call while fail is set.
type countingStore struct {
	kvtest.Memory
	gets atomic.Int32
	fail error
}

func (s *countingStore) Get(ctx context.Context, key string) (json.RawMessage, bool, error) {
	s.gets.Add(1)
	if s.fail != nil {
		return nil, false, s.fail
	}
	return s.Memory.Get(ctx, key)
}

func (s *countingStore) Set(ctx context.Context, key string, value json.RawMessage) error {
	if s.fail != nil {
		return s.fail
	}
	return s.Memory.Set(ctx, key, value)
}

type fixture struct {
	store *countingStore
	clk   *fakeclock.Clock
	logs  *bytes.Buffer
	id    *routing.Identity
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{store: &countingStore{}, clk: fakeclock.New(start), logs: &bytes.Buffer{}}
	f.id = routing.NewIdentity(f.store, identityKey, f.clk, slog.New(slog.NewTextHandler(f.logs, nil)))
	return f
}

func (f *fixture) cache(t *testing.T, value string) {
	t.Helper()
	if err := f.store.Set(t.Context(), identityKey, json.RawMessage(value)); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) stored(t *testing.T) string {
	t.Helper()
	v, ok, err := f.store.Memory.Get(t.Context(), identityKey)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return ""
	}
	return string(v)
}

func entry(username, botID, expiresAt string) string {
	b, _ := json.Marshal(map[string]any{"username": username, "bot_id": botID, "expires_at": expiresAt})
	return string(b)
}

func assertUser(t *testing.T, got chat.User, username string) {
	t.Helper()
	if want := (chat.User{ID: agentID, Username: username}); got != want {
		t.Errorf("Resolve = %+v, want %+v", got, want)
	}
}

func assertCalls(t *testing.T, a *fakeAgent, want int32) {
	t.Helper()
	if got := a.calls.Load(); got != want {
		t.Errorf("platform looked up %d times, want %d", got, want)
	}
}

func TestConfiguredUsernameWinsWithoutStoreOrPlatform(t *testing.T) {
	f := newFixture(t)
	f.cache(t, entry("cached", agentID, "2026-11-01T00:00:00+00:00"))
	a := reportsSelf()
	a.configured.Username = "configured"

	got := f.id.Resolve(t.Context(), a)

	if want := (chat.User{ID: agentID, Username: "configured"}); got != want {
		t.Errorf("Resolve = %+v, want %+v", got, want)
	}
	assertCalls(t, a, 0)
	if n := f.store.gets.Load(); n != 0 {
		t.Errorf("store read %d times, want 0", n)
	}
}

func TestValidCacheEntryIsUsedWithoutLookup(t *testing.T) {
	for _, expires := range []string{
		"2026-10-31T10:00:00.123457+00:00",
		"2026-10-31T13:00:01+03:00",
		"2026-10-31T10:00:01Z",
	} {
		t.Run(expires, func(t *testing.T) {
			f := newFixture(t)
			f.cache(t, entry("cached", agentID, expires))
			a := reportsSelf()

			assertUser(t, f.id.Resolve(t.Context(), a), "cached")
			assertCalls(t, a, 0)
		})
	}
}

func TestUnusableCacheEntryFallsBackToPlatform(t *testing.T) {
	cases := map[string]string{
		"expires now":            entry("cached", agentID, "2026-10-31T10:00:00.123456+00:00"),
		"expired":                entry("cached", agentID, "2026-10-30T10:00:00+00:00"),
		"other agent":            entry("cached", "other-id", "2026-11-01T00:00:00+00:00"),
		"null agent":             `{"username":"cached","bot_id":null,"expires_at":"2026-11-01T00:00:00+00:00"}`,
		"naive timestamp":        entry("cached", agentID, "2026-11-01T00:00:00"),
		"date-only timestamp":    entry("cached", agentID, "2026-11-02"),
		"unparseable timestamp":  entry("cached", agentID, "tomorrow"),
		"empty timestamp":        entry("cached", agentID, ""),
		"no timestamp":           `{"username":"cached","bot_id":"agent-id"}`,
		"numeric timestamp":      `{"username":"cached","bot_id":"agent-id","expires_at":1793000000}`,
		"empty username":         entry("", agentID, "2026-11-01T00:00:00+00:00"),
		"no username":            `{"bot_id":"agent-id","expires_at":"2026-11-01T00:00:00+00:00"}`,
		"numeric username":       `{"username":7,"bot_id":"agent-id","expires_at":"2026-11-01T00:00:00+00:00"}`,
		"not an object":          `["cached","agent-id","2026-11-01T00:00:00+00:00"]`,
		"string value":           `"cached"`,
		"JSON null":              `null`,
		"empty object":           `{}`,
		"numeric agent":          `{"username":"cached","bot_id":7,"expires_at":"2026-11-01T00:00:00+00:00"}`,
		"timestamp out of range": entry("cached", agentID, "2026-13-01T00:00:00+00:00"),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.cache(t, value)
			a := reportsSelf()

			assertUser(t, f.id.Resolve(t.Context(), a), "helper")
			assertCalls(t, a, 1)
		})
	}
}

func TestLookupWritesCacheEntry(t *testing.T) {
	f := newFixture(t)
	a := reportsSelf()
	a.self.ID = "reported-id"

	assertUser(t, f.id.Resolve(t.Context(), a), "helper")

	want := `{"username":"helper","bot_id":"agent-id","expires_at":"2026-11-01T10:00:00.123456+00:00"}`
	if got := f.stored(t); got != want {
		t.Errorf("cache entry = %s, want %s", got, want)
	}
}

func TestLookupReplacesUnusableCacheEntry(t *testing.T) {
	f := newFixture(t)
	f.cache(t, entry("old", "other-id", "2026-12-01T00:00:00+00:00"))
	f.clk.Advance(time.Second - 123456*time.Microsecond)

	assertUser(t, f.id.Resolve(t.Context(), reportsSelf()), "helper")

	if want := `{"username":"helper","bot_id":"agent-id","expires_at":"2026-11-01T10:00:01+00:00"}`; f.stored(t) != want {
		t.Errorf("cache entry = %s, want %s", f.stored(t), want)
	}
}

func TestResolvedIdentityIsKeptInProcessUntilItExpires(t *testing.T) {
	f := newFixture(t)
	a := reportsSelf()
	f.id.Resolve(t.Context(), a)
	reads := f.store.gets.Load()
	f.cache(t, entry("changed", agentID, "2026-12-01T00:00:00+00:00"))

	f.clk.Advance(24*time.Hour - time.Microsecond)
	assertUser(t, f.id.Resolve(t.Context(), a), "helper")
	if n := f.store.gets.Load(); n != reads {
		t.Errorf("store read %d more times before expiry, want 0", n-reads)
	}

	f.clk.Advance(time.Microsecond)
	assertUser(t, f.id.Resolve(t.Context(), a), "changed")
	assertCalls(t, a, 1)
}

func TestCacheHitIsKeptInProcessUntilItExpires(t *testing.T) {
	f := newFixture(t)
	f.cache(t, entry("cached", agentID, "2026-10-31T11:00:00+00:00"))
	a := reportsSelf()
	f.id.Resolve(t.Context(), a)
	f.cache(t, `{}`)

	f.clk.Advance(59 * time.Minute)
	assertUser(t, f.id.Resolve(t.Context(), a), "cached")
	assertCalls(t, a, 0)

	f.clk.Advance(time.Minute)
	assertUser(t, f.id.Resolve(t.Context(), a), "helper")
	assertCalls(t, a, 1)
}

func TestInProcessIdentityOfAnotherAgentIsNotUsed(t *testing.T) {
	f := newFixture(t)
	f.id.Resolve(t.Context(), reportsSelf())

	other := &fakeAgent{configured: chat.User{ID: "other-id"}, self: chat.User{ID: "other-id", Username: "other"}}
	if got, want := f.id.Resolve(t.Context(), other), (chat.User{ID: "other-id", Username: "other"}); got != want {
		t.Errorf("Resolve = %+v, want %+v", got, want)
	}
	assertCalls(t, other, 1)
}

func TestFailedLookupLeavesUsernameEmptyAndWarns(t *testing.T) {
	cases := map[string]*fakeAgent{
		"error":       {configured: chat.User{ID: agentID}, err: errors.New("connection refused")},
		"no username": {configured: chat.User{ID: agentID}, self: chat.User{ID: agentID}},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)

			assertUser(t, f.id.Resolve(t.Context(), a), "")

			if !strings.Contains(f.logs.String(), "level=WARN") {
				t.Errorf("no warning logged; logs:\n%s", f.logs)
			}
			if got := f.stored(t); got != "" {
				t.Errorf("cache written after a failed lookup: %s", got)
			}
		})
	}
}

func TestFailedLookupIsNotRetriedFor30Seconds(t *testing.T) {
	f := newFixture(t)
	a := &fakeAgent{configured: chat.User{ID: agentID}, err: errors.New("timeout")}
	f.id.Resolve(t.Context(), a)

	f.clk.Advance(30*time.Second - time.Nanosecond)
	assertUser(t, f.id.Resolve(t.Context(), a), "")
	assertCalls(t, a, 1)

	a.err, a.self = nil, chat.User{ID: agentID, Username: "helper"}
	f.clk.Advance(time.Nanosecond)
	assertUser(t, f.id.Resolve(t.Context(), a), "helper")
	assertCalls(t, a, 2)
}

func TestCacheIsReadWhileLookupsArePaused(t *testing.T) {
	f := newFixture(t)
	a := &fakeAgent{configured: chat.User{ID: agentID}, err: errors.New("timeout")}
	f.id.Resolve(t.Context(), a)
	f.cache(t, entry("cached", agentID, "2026-11-01T00:00:00+00:00"))

	assertUser(t, f.id.Resolve(t.Context(), a), "cached")
	assertCalls(t, a, 1)
}

func TestCancelledLookupIsRetriedImmediately(t *testing.T) {
	f := newFixture(t)
	a := reportsSelf()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	assertUser(t, f.id.Resolve(ctx, a), "")
	assertUser(t, f.id.Resolve(t.Context(), a), "helper")
	assertCalls(t, a, 2)
}

func TestLookupPastCallerDeadlinePausesLookups(t *testing.T) {
	f := newFixture(t)
	a := reportsSelf()
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	assertUser(t, f.id.Resolve(ctx, a), "")
	assertUser(t, f.id.Resolve(t.Context(), a), "")
	assertCalls(t, a, 1)
}

func TestStoreFailureFallsBackToPlatform(t *testing.T) {
	f := newFixture(t)
	f.store.fail = errors.New("database unreachable")
	a := reportsSelf()

	assertUser(t, f.id.Resolve(t.Context(), a), "helper")
	assertCalls(t, a, 1)
	if !strings.Contains(f.logs.String(), "database unreachable") {
		t.Errorf("store failure not logged; logs:\n%s", f.logs)
	}

	assertUser(t, f.id.Resolve(t.Context(), a), "helper")
	assertCalls(t, a, 1)
}

func TestConcurrentResolvesAgree(t *testing.T) {
	f := newFixture(t)
	a := reportsSelf()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { assertUser(t, f.id.Resolve(t.Context(), a), "helper") })
	}
	wg.Wait()
}

func TestVerifyWarnsWhenPlatformReportsAnotherAgentID(t *testing.T) {
	cases := []struct {
		name string
		self chat.User
		err  error
		warn bool
	}{
		{"same ID", chat.User{ID: agentID, Username: "helper"}, nil, false},
		{"different ID", chat.User{ID: "reported-id", Username: "helper"}, nil, true},
		{"lookup failed", chat.User{}, errors.New("connection refused"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			a := &fakeAgent{configured: chat.User{ID: agentID}, self: c.self, err: c.err}

			f.id.Verify(t.Context(), a)

			if got := strings.Contains(f.logs.String(), "level=WARN"); got != c.warn {
				t.Errorf("warned = %v, want %v; logs:\n%s", got, c.warn, f.logs)
			}
			if c.warn && c.err == nil && !strings.Contains(f.logs.String(), "reported-id") {
				t.Errorf("warning does not name the reported ID; logs:\n%s", f.logs)
			}
			if got := f.stored(t); got != "" {
				t.Errorf("cache written by Verify: %s", got)
			}
		})
	}
}
