package dispatch_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/dispatch"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/routing"
	"github.com/sarpaydemir/dost/internal/store/kv"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/fakeclock"
	"github.com/sarpaydemir/dost/internal/testkit/kvtest"
)

const (
	prefix  = "agent:buffer:"
	marker  = "agent:last_seen"
	agentID = "bot"
)

// start has microseconds, as stored timestamps do.
var start = time.Date(2026, 10, 31, 10, 0, 0, 123456000, time.UTC)

var uuid4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// platform decodes events that are JSON-encoded messages.
type platform struct {
	chat.Platform
	configured, reported chat.User
	lookups              atomic.Int32
}

func (p *platform) Decode(event json.RawMessage) (chat.Message, bool) {
	var m chat.Message
	return m, json.Unmarshal(event, &m) == nil && m.ID != ""
}

func (p *platform) Identity() chat.User { return p.configured }

func (p *platform) Self(context.Context) (chat.User, error) {
	p.lookups.Add(1)
	return p.reported, nil
}

// snapshot serves settings that a test may replace.
type snapshot struct {
	current atomic.Pointer[config.Settings]
}

func (s *snapshot) Current(context.Context) *config.Settings { return s.current.Load() }

func (s *snapshot) window(seconds float64) {
	s.current.Store(&config.Settings{DebounceSeconds: seconds, BufferPrefix: prefix, LastSeenKey: marker})
}

// flusher records notices.
type flusher struct {
	mu      sync.Mutex
	notices []dispatch.Notice
	changed testkit.Notifier
}

func (f *flusher) Notify(_ context.Context, n dispatch.Notice) error {
	f.mu.Lock()
	f.notices = append(f.notices, n)
	f.mu.Unlock()
	f.changed.Notify()
	return nil
}

func (f *flusher) received() []dispatch.Notice {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.notices)
}

func (f *flusher) await(t *testing.T, n int) {
	t.Helper()
	f.changed.Await(t, fmt.Sprintf("%d notices", n), func() bool { return len(f.received()) >= n })
}

type store interface {
	Get(ctx context.Context, key string) (json.RawMessage, bool, error)
	Set(ctx context.Context, key string, value json.RawMessage) error
	UpdateAll(ctx context.Context, changes ...kv.Change) error
}

type fixture struct {
	t        *testing.T
	store    store
	settings *snapshot
	platform *platform
	flusher  *flusher
	clk      *fakeclock.Clock
	timers   *timerLog
	logs     *bytes.Buffer
	d        *dispatch.Dispatcher
}

// timerLog records the duration of every timer the dispatcher starts.
type timerLog struct {
	*fakeclock.Clock
	mu    sync.Mutex
	waits []time.Duration
}

func (c *timerLog) NewTimer(d time.Duration) clock.Timer {
	c.mu.Lock()
	c.waits = append(c.waits, d)
	c.mu.Unlock()
	return c.Clock.NewTimer(d)
}

func (c *timerLog) recorded() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.waits)
}

// newFixture returns a dispatcher with a 3 s window for an agent named "helper" in its settings.
func newFixture(t *testing.T) *fixture {
	f := &fixture{
		t: t, settings: &snapshot{}, flusher: &flusher{},
		platform: &platform{configured: chat.User{ID: agentID, Username: "helper"}},
		clk:      fakeclock.New(start), logs: &bytes.Buffer{},
	}
	f.timers = &timerLog{Clock: f.clk}
	f.settings.window(3)
	f.use(&kvtest.Memory{})
	return f
}

// use replaces the dispatcher with one on s.
func (f *fixture) use(s store) {
	log := slog.New(slog.NewTextHandler(f.logs, nil))
	open := func(*config.Settings) (chat.Platform, error) { return f.platform, nil }
	identity := routing.NewIdentity(&kvtest.Memory{}, "agent:bot_identity", f.clk, log)
	f.store = s
	f.d = dispatch.NewDispatcher(f.t.Context(), f.settings, open, identity, s, f.flusher, f.timers, log)
}

// enqueue queues the messages, failing the test if the queue stays full.
func (f *fixture) enqueue(ms ...chat.Message) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), testkit.AwaitTimeout)
	defer cancel()
	for _, m := range ms {
		if err := f.d.Enqueue(ctx, event(m)); err != nil {
			f.t.Fatalf("Enqueue(%s): %v", m.ID, err)
		}
	}
}

// deliver passes the messages through the dispatcher, stopping it after each queueful.
func (f *fixture) deliver(ms ...chat.Message) {
	f.t.Helper()
	for chunk := range slices.Chunk(ms, 256) {
		f.enqueue(chunk...)
		f.stop()
	}
}

// stop runs the dispatcher with its intake already stopped, so it returns once the queue is empty.
func (f *fixture) stop() {
	f.t.Helper()
	ctx, cancel := context.WithCancel(f.t.Context())
	cancel()
	f.wait(f.run(ctx))
}

func (f *fixture) run(ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- f.d.Run(ctx) }()
	return done
}

func (f *fixture) wait(done <-chan error) {
	f.t.Helper()
	timeout := time.NewTimer(testkit.AwaitTimeout)
	defer timeout.Stop()
	select {
	case err := <-done:
		if err != nil {
			f.t.Fatalf("Run = %v", err)
		}
	case <-timeout.C:
		f.t.Fatal("Run did not return after its queue emptied")
	}
}

// value returns the value under key, or "" when there is none.
func (f *fixture) value(key string) string {
	f.t.Helper()
	v, _, err := f.store.Get(f.t.Context(), key)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(v)
}

func (f *fixture) set(key, value string) {
	f.t.Helper()
	if err := f.store.Set(f.t.Context(), key, json.RawMessage(value)); err != nil {
		f.t.Fatal(err)
	}
}

// assertBuffer checks the buffer under key, apart from its generation, against want and returns the generation.
func (f *fixture) assertBuffer(key, want string) string {
	f.t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(f.value(key)), &fields); err != nil {
		f.t.Fatalf("buffer %s: %v", key, err)
	}
	var generation string
	if err := json.Unmarshal(fields["generation"], &generation); err != nil || !uuid4.MatchString(generation) {
		f.t.Errorf("buffer %s: generation %s is not a version 4 UUID", key, fields["generation"])
	}
	delete(fields, "generation")
	rest, _ := json.Marshal(fields)
	assertJSON(f.t, "buffer "+key, string(rest), want)
	return generation
}

func (f *fixture) assertNotices(want ...dispatch.Notice) {
	f.t.Helper()
	if got := f.flusher.received(); !slices.Equal(got, want) {
		f.t.Errorf("notices = %+v, want %+v", got, want)
	}
}

// postIDs returns the IDs of a stored buffer's messages in order.
func postIDs(buffer string) []string {
	var b struct {
		Messages []struct {
			PostID string `json:"post_id"`
		}
	}
	_ = json.Unmarshal([]byte(buffer), &b)
	var ids []string
	for _, m := range b.Messages {
		ids = append(ids, m.PostID)
	}
	return ids
}

func assertJSON(t *testing.T, what, got, want string) {
	t.Helper()
	if eq, err := testkit.JSONEqual([]byte(got), []byte(want)); err != nil || !eq {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

// msg is a message at the root of a public conversation, created at 1000 ms, whose payload names it.
func msg(id, author, text string, change ...func(*chat.Message)) chat.Message {
	m := chat.Message{
		ID: id, ConversationID: "channel-1", AuthorID: author, Text: text, Kind: chat.Public,
		Created: time.UnixMilli(1000), Payload: json.RawMessage(fmt.Sprintf(`{"event": %q}`, id)),
	}
	for _, c := range change {
		c(&m)
	}
	return m
}

func inThread(m *chat.Message) { m.ThreadID = "root" }

func direct(m *chat.Message) { m.Kind = chat.Direct }

func createdAt(ms int64) func(*chat.Message) {
	return func(m *chat.Message) {
		m.Created = time.Time{}
		if ms != 0 {
			m.Created = time.UnixMilli(ms)
		}
	}
}

func event(m chat.Message) json.RawMessage {
	b, _ := json.Marshal(m)
	return b
}

func TestAuthorsInOneThreadGetSeparateBuffers(t *testing.T) {
	f := newFixture(t)
	f.deliver(
		msg("p1", "user-a", "@helper do this", inThread),
		msg("p2", "user-b", "helper add this too", inThread),
	)
	ga := f.assertBuffer(prefix+"thread:root:user-a", `{"flush_after": "2026-10-31T10:00:03.123456+00:00",
		"payload": {"event": "p1"},
		"messages": [{"post_id": "p1", "user_id": "user-a", "message": "@helper do this", "file_ids": []}]}`)
	gb := f.assertBuffer(prefix+"thread:root:user-b", `{"flush_after": "2026-10-31T10:00:03.123456+00:00",
		"payload": {"event": "p2"},
		"messages": [{"post_id": "p2", "user_id": "user-b", "message": "helper add this too", "file_ids": []}]}`)
	due := start.Add(3 * time.Second)
	f.assertNotices(
		dispatch.Notice{Key: prefix + "thread:root:user-a", Generation: ga, FlushAfter: due},
		dispatch.Notice{Key: prefix + "thread:root:user-b", Generation: gb, FlushAfter: due},
	)
}

func TestOneAuthorsFragmentsMergeInArrivalOrderUnderTheFirstPayload(t *testing.T) {
	f := newFixture(t)
	f.deliver(msg("p1", "user-a", "@helper do this", inThread))
	f.clk.Advance(time.Second)
	f.deliver(msg("p2", "user-a", "helper with this file", inThread, func(m *chat.Message) {
		m.AttachmentIDs = []string{"f1", "f2"}
	}))
	key := prefix + "thread:root:user-a"
	generation := f.assertBuffer(key, `{"flush_after": "2026-10-31T10:00:04.123456+00:00",
		"payload": {"event": "p1"},
		"messages": [{"post_id": "p1", "user_id": "user-a", "message": "@helper do this", "file_ids": []},
			{"post_id": "p2", "user_id": "user-a", "message": "helper with this file", "file_ids": ["f1", "f2"]}]}`)
	notices := f.flusher.received()
	if len(notices) != 2 || notices[0].Generation == generation || notices[1] != (dispatch.Notice{
		Key: key, Generation: generation, FlushAfter: start.Add(4 * time.Second),
	}) {
		t.Errorf("notices = %+v; want one per fragment, the second with the buffer's generation and due at %v",
			notices, start.Add(4*time.Second))
	}
}

func TestBufferWrittenByThePreviousDeploymentIsAppendedTo(t *testing.T) {
	f := newFixture(t)
	const payload = `{"data": {"post": "{\"id\": \"p1\", \"user_id\": \"user-a\", \"channel_id\": \"channel-1\", \"root_id\": \"\", \"message\": \"first\", \"create_at\": 1000, \"post_id\": \"p1\", \"channel_type\": \"D\", \"is_dm\": true, \"is_thread\": false, \"scope\": \"private\", \"reply_root_id\": \"\", \"context_id\": \"p1\"}", "channel_type": "D"}}`
	key := prefix + "dm:user-a"
	f.set(key, `{"generation": "3b0c8a52-5d0e-4c55-9f7e-1d2a3b4c5d6e", "flush_after": "2026-10-31T10:00:03.123456+00:00",
		"payload": `+payload+`, "messages": [{"post_id": "p1", "user_id": "user-a", "message": "first", "file_ids": []}]}`)
	f.clk.Advance(time.Second)
	f.deliver(msg("p2", "user-a", "second", direct, func(m *chat.Message) { m.AttachmentIDs = []string{"f1", "f2"} }))
	generation := f.assertBuffer(key, `{"flush_after": "2026-10-31T10:00:04.123456+00:00", "payload": `+payload+`,
		"messages": [{"post_id": "p1", "user_id": "user-a", "message": "first", "file_ids": []},
			{"post_id": "p2", "user_id": "user-a", "message": "second", "file_ids": ["f1", "f2"]}]}`)
	if generation == "3b0c8a52-5d0e-4c55-9f7e-1d2a3b4c5d6e" {
		t.Error("the append kept the stored generation")
	}
}

func TestEventsForSeveralConversationsAreAppliedInArrivalOrder(t *testing.T) {
	f := newFixture(t)
	f.deliver(
		msg("a1", "user-a", "hi", direct),
		msg("b1", "user-b", "@helper look", inThread),
		msg("a2", "user-a", "are you there", direct),
		msg("c1", "user-c", "@helper new topic"),
		msg("b2", "user-b", "helper and this", inThread),
		msg("a3", "user-a", "hello?", direct),
	)
	for key, ids := range map[string][]string{
		"dm:user-a":          {"a1", "a2", "a3"},
		"thread:root:user-b": {"b1", "b2"},
		"thread:c1:user-c":   {"c1"},
	} {
		if got := postIDs(f.value(prefix + key)); !slices.Equal(got, ids) {
			t.Errorf("buffer %s holds %q, want %q", key, got, ids)
		}
	}
	var keys []string
	for _, n := range f.flusher.received() {
		keys = append(keys, strings.TrimPrefix(n.Key, prefix))
	}
	if want := []string{"dm:user-a", "thread:root:user-b", "dm:user-a", "thread:c1:user-c", "thread:root:user-b", "dm:user-a"}; !slices.Equal(keys, want) {
		t.Errorf("notices for %q, want %q", keys, want)
	}
}

func TestLastSeenAdvancesForEveryParsedMessageAndOnlyForward(t *testing.T) {
	f := newFixture(t)
	f.deliver(msg("x0", "user-a", "@helper no creation time", createdAt(0)))
	if v := f.value(marker); v != "" {
		t.Errorf("a message without a creation time set last seen to %s", v)
	}
	for _, c := range []struct {
		m    chat.Message
		want string
	}{
		{msg("x1", "user-a", "not addressed", createdAt(2000)), `{"create_at": 2000}`},
		{msg("x2", "user-a", "older", createdAt(1000)), `{"create_at": 2000}`},
		{msg("x3", agentID, "@helper my own reply", createdAt(3000)), `{"create_at": 3000}`},
		{msg("x4", "user-a", "no creation time", createdAt(0)), `{"create_at": 3000}`},
		{msg("x5", "user-a", "older again", createdAt(2500)), `{"create_at": 3000}`},
	} {
		f.deliver(c.m)
		assertJSON(t, "last seen after "+c.m.ID, f.value(marker), c.want)
	}
	if v := f.value(prefix + "thread:x3:" + agentID); v != "" {
		t.Errorf("the agent's own message was buffered: %s", v)
	}
	if n := f.flusher.received(); len(n) != 1 || n[0].Key != prefix+"thread:x0:user-a" {
		t.Errorf("notices = %+v, want only the one for the addressed message", n)
	}
}

// markerLog records the last-seen value after every committed update.
type markerLog struct {
	store
	values []int64
}

func (s *markerLog) UpdateAll(ctx context.Context, changes ...kv.Change) error {
	if err := s.store.UpdateAll(ctx, changes...); err != nil {
		return err
	}
	v, _, _ := s.Get(ctx, marker)
	var m struct {
		CreateAt int64 `json:"create_at"`
	}
	_ = json.Unmarshal(v, &m)
	s.values = append(s.values, m.CreateAt)
	return nil
}

func TestLastSeenNeverMovesBackwardsWithConcurrentProducers(t *testing.T) {
	f := newFixture(t)
	log := &markerLog{store: f.store}
	f.use(log)
	ctx, cancel := context.WithCancel(t.Context())
	done := f.run(ctx)

	const producers, each = 4, 50
	created := rand.New(rand.NewPCG(1, 2)).Perm(producers * each)
	var wg sync.WaitGroup
	for p := range producers {
		wg.Go(func() {
			author := fmt.Sprintf("user-%d", p)
			for i := range each {
				m := msg(fmt.Sprintf("%s-%d", author, i), author, "hi", direct, createdAt(int64(1000+created[p*each+i])))
				if err := f.d.Enqueue(t.Context(), event(m)); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	f.flusher.await(t, producers*each)
	cancel()
	f.wait(done)

	if !slices.IsSorted(log.values) || log.values[len(log.values)-1] != 1000+producers*each-1 {
		t.Errorf("last seen took the values %v; want them never decreasing and ending at %d", log.values, 1000+producers*each-1)
	}
	for p := range producers {
		var want []string
		for i := range each {
			want = append(want, fmt.Sprintf("user-%d-%d", p, i))
		}
		if got := postIDs(f.value(fmt.Sprintf("%sdm:user-%d", prefix, p))); !slices.Equal(got, want) {
			t.Errorf("producer %d's buffer holds %q, want its messages in the order sent", p, got)
		}
	}
}

func TestFailedAppendLeavesTheMarkerUnchangedAndIntakeContinues(t *testing.T) {
	f := newFixture(t)
	f.set(marker, `{"create_at": 1000}`)
	f.set(prefix+"dm:user-a", `["not a buffer"]`)
	f.deliver(
		msg("p2", "user-a", "hi", direct, createdAt(2000)),
		msg("p3", "user-b", "hi", direct, createdAt(1500)),
	)
	assertJSON(t, "malformed buffer", f.value(prefix+"dm:user-a"), `["not a buffer"]`)
	assertJSON(t, "last seen", f.value(marker), `{"create_at": 1500}`)
	if n := f.flusher.received(); len(n) != 1 || n[0].Key != prefix+"dm:user-b" {
		t.Errorf("notices = %+v, want one for the second message", n)
	}
	if logs := f.logs.String(); !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "post_id=p2") {
		t.Errorf("logs do not report the failed message:\n%s", logs)
	}
}

// flakyStore fails the first failures updates as an unreachable database would, then applies them. With commits,
// the failed updates are applied too, as when the connection drops after the server commits.
type flakyStore struct {
	store
	failures int32
	commits  bool
	attempts atomic.Int32
	changed  testkit.Notifier
}

func (s *flakyStore) UpdateAll(ctx context.Context, changes ...kv.Change) error {
	defer s.changed.Notify()
	if s.attempts.Add(1) > s.failures {
		return s.store.UpdateAll(ctx, changes...)
	}
	if s.commits {
		if err := s.store.UpdateAll(ctx, changes...); err != nil {
			return err
		}
	}
	return errors.New("read tcp 10.0.0.5:5432: connection reset by peer")
}

func TestUnreachableStoreIsRetriedWithBackoffWhileIntakeWaits(t *testing.T) {
	f := newFixture(t)
	flaky := &flakyStore{store: f.store, failures: 7}
	f.use(flaky)
	f.enqueue(msg("p1", "user-a", "hi", direct), msg("p2", "user-b", "hi", direct))
	ctx, cancel := context.WithCancel(t.Context())
	done := f.run(ctx)

	waits := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		30 * time.Second, 30 * time.Second}
	for i, wait := range waits {
		f.clk.AwaitTimers(t, 1)
		f.clk.Advance(wait)
		flaky.changed.Await(t, "next attempt", func() bool { return flaky.attempts.Load() > int32(i+1) })
	}
	f.flusher.await(t, 2)
	cancel()
	f.wait(done)

	if got := f.timers.recorded(); !slices.Equal(got, waits) {
		t.Errorf("waits between attempts = %v, want %v", got, waits)
	}
	due := start.Add(94 * time.Second)
	notices := f.flusher.received()
	if len(notices) != 2 || notices[0].Key != prefix+"dm:user-a" || notices[1].Key != prefix+"dm:user-b" ||
		!notices[0].FlushAfter.Equal(due) || !notices[1].FlushAfter.Equal(due) {
		t.Errorf("notices = %+v; want user-a's then user-b's, both due at %v", notices, due)
	}
	if got := flaky.attempts.Load(); got != 9 {
		t.Errorf("store attempts = %d, want 8 for the first message and 1 for the second", got)
	}
}

func TestRecentlyBufferedMessagesAreSkipped(t *testing.T) {
	f := newFixture(t)
	var all []chat.Message
	for i := range 10_001 {
		all = append(all, msg("t"+strconv.Itoa(i), "user-a", "@helper hi"))
	}
	f.deliver(all...)
	f.deliver(all[1], all[0])
	notices := f.flusher.received()
	if len(notices) != len(all)+1 || notices[len(all)].Key != prefix+"thread:t0:user-a" {
		t.Errorf("%d notices; want one per message, then one more for the message older than the last 10,000 "+
			"buffered and none for the one among them", len(notices))
	}
}

func TestRetryAfterAnUnconfirmedCommitBuffersTheMessageOnce(t *testing.T) {
	f := newFixture(t)
	flaky := &flakyStore{store: f.store, failures: 1, commits: true}
	f.use(flaky)
	f.enqueue(msg("p1", "user-a", "hi", direct))
	ctx, cancel := context.WithCancel(t.Context())
	done := f.run(ctx)
	f.clk.AwaitTimers(t, 1)
	f.clk.Advance(time.Second)
	f.flusher.await(t, 1)
	cancel()
	f.wait(done)

	key := prefix + "dm:user-a"
	generation := f.assertBuffer(key, `{"flush_after": "2026-10-31T10:00:04.123456+00:00", "payload": {"event": "p1"},
		"messages": [{"post_id": "p1", "user_id": "user-a", "message": "hi", "file_ids": []}]}`)
	f.assertNotices(dispatch.Notice{Key: key, Generation: generation, FlushAfter: start.Add(4 * time.Second)})
}

func TestUsernameIsLookedUpOnlyForMessagesThatNeedIt(t *testing.T) {
	f := newFixture(t)
	f.platform.configured = chat.User{ID: agentID}
	f.platform.reported = chat.User{ID: agentID, Username: "helper"}
	f.deliver(msg("d1", "user-a", "hi", direct))
	if n := f.platform.lookups.Load(); n != 0 || f.value(prefix+"dm:user-a") == "" {
		t.Errorf("direct message: %d lookups, buffered %v; want no lookup and buffered", n, f.value(prefix+"dm:user-a") != "")
	}
	f.deliver(msg("p1", "user-a", "@helper status?", inThread))
	if n := f.platform.lookups.Load(); n != 1 || f.value(prefix+"thread:root:user-a") == "" {
		t.Errorf("mention: %d lookups, buffered %v; want one lookup and buffered", n, f.value(prefix+"thread:root:user-a") != "")
	}
}

func TestWindowLengthComesFromTheEventsSettings(t *testing.T) {
	f := newFixture(t)
	f.deliver(msg("t1", "user-a", "@helper one"))
	f.settings.window(10.5)
	f.deliver(msg("t2", "user-a", "@helper two"))
	notices := f.flusher.received()
	if len(notices) != 2 || !notices[0].FlushAfter.Equal(start.Add(3*time.Second)) ||
		!notices[1].FlushAfter.Equal(start.Add(10500*time.Millisecond)) {
		t.Errorf("notices = %+v; want due 3 s and then 10.5 s after the append", notices)
	}
	var b struct {
		FlushAfter string `json:"flush_after"`
	}
	_ = json.Unmarshal([]byte(f.value(prefix+"thread:t2:user-a")), &b)
	if b.FlushAfter != "2026-10-31T10:00:10.623456+00:00" {
		t.Errorf("stored flush_after = %q", b.FlushAfter)
	}
}

func TestIntakeHolds256EventsAndShutdownProcessesThem(t *testing.T) {
	f := newFixture(t)
	for i := range 256 {
		f.enqueue(msg(strconv.Itoa(i), fmt.Sprintf("user-%d", i), "hi", direct))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := f.d.Enqueue(ctx, event(msg("over", "user-x", "hi", direct))); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Enqueue on a full intake = %v, want it to wait until its context ends", err)
	}
	f.stop()
	if n := len(f.flusher.received()); n != 256 {
		t.Errorf("%d messages buffered on shutdown, want all 256 queued", n)
	}
}
