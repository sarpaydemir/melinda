//go:build soak

package app_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sarpaydemir/melinda/internal/app"
	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/logging"
	"github.com/sarpaydemir/melinda/internal/store/pg"
	"github.com/sarpaydemir/melinda/internal/testkit"
	"github.com/sarpaydemir/melinda/internal/testkit/mattermostfake"
	"github.com/sarpaydemir/melinda/internal/testkit/openaifake"
	"github.com/sarpaydemir/melinda/internal/testkit/searxngfake"
	"github.com/sarpaydemir/melinda/internal/testkit/testdb"
	"github.com/sarpaydemir/melinda/internal/testkit/webfake"
)

const (
	turnEvery   = time.Second      // one addressed turn, and half as much chatter
	dropEvery   = 45 * time.Second // longer than a session must last for the reconnect delay to reset
	reloadEvery = 20 * time.Second
	// overdue is how long past its flush deadline a buffer may stay stored at a sample.
	overdue = 30 * time.Second
	// answerWithin is how long after it is posted a message without a reply counts as lost.
	answerWithin = 2 * time.Minute
	// growth is how much more the resources may use at the end than at their peak in the first half, and slack the
	// absolute margin for small counts such as database connections, where one more is not a trend.
	growth = 0.10
	slack  = 2

	dmConversations     = 40
	threadConversations = 12
	threadTurns         = 5 // turns in a thread before its user starts a new one
	bufferPrefix        = "dost:buffer:"
	bot                 = "agent"
)

var token = regexp.MustCompile(`\[m(\d+)\]`)

// TestSoak runs the agent for DOST_SOAK_DURATION (default 3m), writing its log, and heap and goroutine profiles at the
// baseline and the end, to the DOST_SOAK_OUT directory when set, against the chat, model, search and web fakes with
// realistic latency and a fresh database: about 60 addressed turns a minute over 52 conversations (direct messages,
// channel threads, debounced bursts, attachments, web tool calls) plus 120 unaddressed chatter posts a minute, a
// websocket drop every 45s with a message posted while disconnected, and a settings reload every 20s. Halfway and at
// the end, traffic stops until every message is answered or, unanswered for 2 minutes, counted lost. The resources at the end must stay within 10% of their
// peak in the first half, and RSS within 10% of its settled value halfway; every addressed message must be answered by exactly one reply; and no debounce buffer or
// journaled run may be left, nor any buffer stay long past its flush deadline. The dispatcher remembers the last
// 10,000 post IDs, which this traffic fills in about 50 minutes: before then its memory grows by design, so a run of
// 2h or more is the one that tells a leak from that.
func TestSoak(t *testing.T) {
	duration := 3 * time.Minute
	if v := os.Getenv("DOST_SOAK_DURATION"); v != "" {
		var err error
		if duration, err = time.ParseDuration(v); err != nil {
			t.Fatalf("DOST_SOAK_DURATION: %v", err)
		}
	}
	s := newSoak(t)
	out := os.Getenv("DOST_SOAK_OUT")
	if out == "" {
		out = t.TempDir()
	} else {
		runtime.MemProfileRate = 4 << 10 // the heap is a few MiB; the default rate samples too few allocations
	}
	logPath := filepath.Join(out, "dost.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	env, err := config.LoadEnv([]string{"DOST_DATABASE_URL=" + s.dsn, "DOST_HTTP_ADDR=", "DOST_SETTINGS_FILE=" + s.settingsPath,
		"DOST_SETTINGS_TTL=1s", "DOST_SEARXNG_URL=" + s.search.URL()})
	if err != nil {
		t.Fatal(err)
	}
	secrets := new(logging.Secrets)
	secrets.Add(env.Secrets()...)
	log := logging.New(logFile, slog.LevelInfo, false, secrets)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, env, "soak", secrets, log) }()
	s.chat.AwaitWebsocket(t, func() bool { return s.chat.Connections() > 0 })
	if s.db, err = pgx.Connect(t.Context(), s.dsn); err != nil {
		t.Fatal(err)
	}
	defer s.db.Close(context.Background())

	s.start = time.Now()
	every := max(duration/24, 5*time.Second)
	s.traffic(s.start.Add(duration/2), every)
	s.settle()
	base := s.sample()
	profile(t, "baseline")
	s.samples = append(s.samples, base)
	warm := len(s.samples)
	s.traffic(s.start.Add(duration), every)
	s.settle()
	final := s.sample()
	profile(t, "final")

	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(env.ShutdownTimeout + 5*time.Second):
		t.Fatalf("Run did not return within the %v shutdown timeout", env.ShutdownTimeout)
	}
	if n := s.pending(); n != 0 {
		t.Errorf("after shutdown %d buffers and journaled runs are left, want 0", n)
	}
	s.report(warm, final, logPath)
}

type conversation struct {
	user, channel string
	thread        bool
	root          string // the thread's root post; empty before its first turn
	turns         int
	file          string // an attachment in the conversation's channel
	waiting       int    // sent messages not answered yet
}

type sample struct {
	at                    time.Duration
	heap, rss             uint64
	goroutines, fds, conn int
}

type soak struct {
	t            *testing.T
	dsn          string
	db           *pgx.Conn
	chat         *mattermostfake.Fake
	model        *openaifake.Fake
	search       *searxngfake.Fake
	web          *testkit.Server
	settingsPath string
	settings     map[string]any
	start        time.Time
	samples      []sample
	drops        int
	reloads      int
	chatter      int
	lounge       []string // users who chat in the lounge

	mu      sync.Mutex
	changed testkit.Notifier
	convs   []*conversation
	// The state kept per sent token is two bytes, so that it does not show as growth in a long run.
	owner      []uint8                     // the index in convs of the conversation of each token, from token 1
	replies    []uint8                     // replies quoting each token, from token 1, at most 255
	awaiting   map[int]mattermostfake.Post // the posts of the tokens no reply has quoted yet
	duplicated []string                    // the tokens quoted by more than one reply, with their channel
	lost       []string                    // the posts unanswered for answerWithin
	unexpected []string                    // replies quoting no sent token
	overdue    int                         // samples that found a buffer past its deadline
	calls      map[string]int              // model requests by kind
	tokens     int
	ids        int
	lastMS     int64
}

func newSoak(t *testing.T) *soak {
	s := &soak{t: t, dsn: testdb.New(t), awaiting: map[int]mattermostfake.Post{}, calls: map[string]int{}}
	s.chat = mattermostfake.New(t, "chat-token", mattermostfake.User{ID: id("bot", 0), Username: bot})
	s.chat.On("POST", "/api/v4/posts").DefaultFunc(s.reply)
	s.model = openaifake.New(t)
	s.model.Chat().DefaultFunc(s.answer)
	vector := make([]float32, pg.EmbeddingDims)
	vector[0] = 1
	s.model.Embeddings().DefaultFunc(func(testkit.Request) testkit.Response {
		return latency(openaifake.Embedding(vector), 20, 80)
	})
	s.search = searxngfake.New(t)
	s.search.Search().DefaultFunc(func(testkit.Request) testkit.Response {
		return latency(searxngfake.Results(searxngfake.Result{Title: "Release notes", URL: "https://example.com/notes",
			Content: "Version 2 ships on Friday."}), 100, 300)
	})
	s.web = webfake.New(t, map[string]testkit.Response{"/status": webfake.HTML("<h1>Status</h1><p>All systems normal.</p>")})

	s.chat.AddTeam(mattermostfake.Team{ID: id("t", 1), Name: "team", DisplayName: "Team"})
	for i := 1; i <= dmConversations+threadConversations; i++ {
		u := id("u", i)
		s.chat.AddUser(mattermostfake.User{ID: u, Username: fmt.Sprintf("user%d", i), FirstName: fmt.Sprintf("User%d", i)})
		c := &conversation{user: u}
		if i <= dmConversations {
			c.channel = id("d", i)
			s.chat.AddChannel(mattermostfake.Channel{ID: c.channel, Type: "D", Name: id("bot", 0) + "__" + u, Members: []string{id("bot", 0), u}})
			if i <= 5 {
				s.lounge = append(s.lounge, u)
			}
		} else {
			c.thread = true
			c.channel = id("c", 1+i%3)
		}
		c.file = id("f", i)
		s.chat.AddFile(c.file, u, c.channel, "notes.txt", "text/plain", []byte("Standup moves to 10:00 on Mondays.\n"))
		s.convs = append(s.convs, c)
	}
	for n := 1; n <= 3; n++ {
		members := []string{id("bot", 0)}
		for _, c := range s.convs {
			if c.channel == id("c", n) {
				members = append(members, c.user)
			}
		}
		s.chat.AddChannel(mattermostfake.Channel{ID: id("c", n), TeamID: id("t", 1), Type: "O", Name: fmt.Sprintf("project%d", n),
			DisplayName: fmt.Sprintf("Project %d", n), Members: members})
	}
	s.chat.AddChannel(mattermostfake.Channel{ID: id("lounge", 0), TeamID: id("t", 1), Type: "O", Name: "lounge",
		DisplayName: "Lounge", Members: append([]string{id("bot", 0)}, s.lounge...)})

	s.settingsPath = filepath.Join(t.TempDir(), "settings.json")
	s.settings = map[string]any{
		"model": "chat-model", "fact_model": "fact-model", "embedding_model": "embed-model", "vision_model": "vision-model",
		"api_base": s.model.URL(), "key": "model-key", "mattermost_endpoint": s.chat.URL(), "mattermost_token": "chat-token",
		"mattermost_bot_id": id("bot", 0), "enabled_tools": []string{"search_web", "visit_page"}, "web_fetch_allow_private": true,
	}
	s.reload()
	return s
}

// traffic sends turns, chatter, drops and reloads until the deadline, sampling the resources every interval.
func (s *soak) traffic(until time.Time, every time.Duration) {
	end := time.NewTimer(time.Until(until))
	defer end.Stop()
	turns, drops, reloads, samples := time.NewTicker(turnEvery), time.NewTicker(dropEvery), time.NewTicker(reloadEvery), time.NewTicker(every)
	defer turns.Stop()
	defer drops.Stop()
	defer reloads.Stop()
	defer samples.Stop()
	for {
		select {
		case <-end.C:
			return
		case <-turns.C:
			if c := s.idle(false); c != nil {
				s.turn(c)
			}
			for range 2 {
				s.chatter++
				s.chat.Posted(s.post(s.lounge[rand.IntN(len(s.lounge))], id("lounge", 0), "", "lunch anyone?", nil))
			}
		case <-drops.C:
			s.drop()
		case <-reloads.C:
			s.reload()
		case <-samples.C:
			s.samples = append(s.samples, s.sample())
			s.expire()
		}
	}
}

// turn sends the next turn of c: one message, or a burst that the debounce window joins.
func (s *soak) turn(c *conversation) {
	if !c.thread {
		texts := []string{"how is the release going?"}
		switch n := rand.IntN(100); {
		case n < 10:
			s.send(c, true, "please read this file")
			return
		case n < 25:
			texts = []string{"search the web for the release notes"}
		case n < 35:
			texts = []string{"visit the status page"}
		case n < 55:
			texts = []string{"quick question", "about the release", "is it on track?"}[:2+rand.IntN(2)]
		}
		s.send(c, false, texts...)
		return
	}
	if c.turns%threadTurns == 0 {
		c.root = ""
	}
	c.turns++
	if c.root != "" && rand.IntN(5) == 0 {
		s.send(c, false, "@agent one more thing", "@agent what about Friday?")
		return
	}
	s.send(c, false, "@agent can you summarize this?")
}

// send posts texts in c, each with a new token; the first starts c's thread when it has none.
func (s *soak) send(c *conversation, attach bool, texts ...string) {
	var files []string
	if attach {
		files = []string{c.file}
	}
	for _, text := range texts {
		p := s.post(c.user, c.channel, c.root, s.tokenFor(c, text), files)
		if c.thread && c.root == "" {
			c.root = p.ID
		}
		s.chat.Posted(p)
	}
}

// drop closes the websocket, posts a message while the agent is disconnected, and waits for it to reconnect. The
// message is also pushed live when the agent reconnected before it was stored, as the platform would then send it.
func (s *soak) drop() {
	s.drops++
	before := s.chat.Connections()
	s.chat.Drop()
	if c := s.idle(true); c != nil {
		p := s.chat.AddPost(s.post(c.user, c.channel, "", s.tokenFor(c, "are you there?"), nil))
		if s.chat.Connections() != before {
			s.chat.Push(s.chat.Event(p))
		}
	}
	s.chat.AwaitWebsocket(s.t, func() bool { return s.chat.Connections() > before })
}

// reload switches the tool-call temperature in the settings file.
func (s *soak) reload() {
	s.reloads++
	s.settings["tool_call_temperature"] = []float64{0.3, 0.2}[s.reloads%2]
	b, err := json.Marshal(s.settings)
	if err != nil {
		s.t.Fatal(err)
	}
	tmp := s.settingsPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		s.t.Fatal(err)
	}
	if err := os.Rename(tmp, s.settingsPath); err != nil {
		s.t.Fatal(err)
	}
}

// idle returns a random conversation with no unanswered message, only a direct one when dm is set, or nil.
func (s *soak) idle(dm bool) *conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	var free []*conversation
	for _, c := range s.convs {
		if c.waiting == 0 && !(dm && c.thread) {
			free = append(free, c)
		}
	}
	if len(free) == 0 {
		return nil
	}
	return free[rand.IntN(len(free))]
}

// tokenFor appends a new token for c to text.
func (s *soak) tokenFor(c *conversation, text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens++
	s.owner = append(s.owner, uint8(slices.Index(s.convs, c)))
	s.replies = append(s.replies, 0)
	c.waiting++
	return fmt.Sprintf("%s [m%d]", text, s.tokens)
}

func (s *soak) post(user, channel, root, text string, files []string) mattermostfake.Post {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids++
	s.lastMS = max(s.lastMS+1, time.Now().UnixMilli())
	p := mattermostfake.Post{ID: id("p", s.ids), UserID: user, ChannelID: channel, RootID: root, Message: text,
		CreateAt: s.lastMS, FileIDs: files}
	if m := token.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[1])
		s.awaiting[n] = p
	}
	return p
}

// expire counts as lost the messages unanswered for answerWithin, freeing their conversations for more traffic.
func (s *soak) expire() {
	before := time.Now().Add(-answerWithin).UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range slices.Sorted(maps.Keys(s.awaiting)) {
		if p := s.awaiting[n]; p.CreateAt < before {
			s.lost = append(s.lost, fmt.Sprintf("m%d (post %s in %s at %d)", n, p.ID, p.ChannelID, p.CreateAt))
			delete(s.awaiting, n)
			s.convs[s.owner[n-1]].waiting--
		}
	}
}

// reply records the agent's post and answers as the platform does.
func (s *soak) reply(r testkit.Request) testkit.Response {
	var in mattermostfake.Post
	if err := json.Unmarshal(r.Body, &in); err != nil {
		return testkit.Status(400)
	}
	s.mu.Lock()
	quoted := token.FindAllStringSubmatch(in.Message, -1)
	if len(quoted) == 0 {
		s.unexpected = append(s.unexpected, in.Message)
	}
	for _, m := range quoted {
		n, _ := strconv.Atoi(m[1])
		if n < 1 || n > s.tokens {
			s.unexpected = append(s.unexpected, in.Message)
			continue
		}
		c := s.convs[s.owner[n-1]]
		switch s.replies[n-1] = min(s.replies[n-1], 254) + 1; s.replies[n-1] {
		case 1:
			if _, ok := s.awaiting[n]; ok {
				c.waiting--
				delete(s.awaiting, n)
			}
		case 2:
			s.duplicated = append(s.duplicated, fmt.Sprintf("m%d in %s", n, c.channel))
		}
	}
	s.ids++
	in.ID, in.UserID, in.CreateAt = id("r", s.ids), id("bot", 0), time.Now().UnixMilli()
	s.mu.Unlock()
	s.changed.Notify()
	return testkit.JSON(201, in)
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// text is a message's content, or the text parts of a multi-part content.
func (m message) text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct{ Text string }
	_ = json.Unmarshal(m.Content, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

var sourceID = regexp.MustCompile(`source_id: (\S+)`)

// answer plays the chat, fact and vision models. The chat model calls a web tool when the user asks for one and
// otherwise quotes the tokens of the latest user message.
func (s *soak) answer(r testkit.Request) testkit.Response {
	var body struct {
		Model       string          `json:"model"`
		Temperature json.Number     `json:"temperature"`
		Messages    []message       `json:"messages"`
		Tools       json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil || len(body.Messages) == 0 {
		return openaifake.HTTPError(400, "bad request")
	}
	first, last := body.Messages[0].text(), body.Messages[len(body.Messages)-1]
	kind, resp := s.respond(body.Model, first, last, len(body.Tools) > 0, body.Messages)
	if body.Model == "chat-model" {
		kind += " at temperature " + body.Temperature.String()
	}
	s.mu.Lock()
	s.calls[kind]++
	s.mu.Unlock()
	return resp
}

func (s *soak) respond(model, first string, last message, tools bool, messages []message) (string, testkit.Response) {
	switch model {
	case "vision-model":
		return "file description", latency(openaifake.Text("A note: standup moves to 10:00 on Mondays."), 300, 800)
	case "fact-model":
		switch {
		case strings.Contains(first, `{"query"`):
			return "search query", latency(openaifake.Text(`{"query": "release schedule"}`), 100, 400)
		case sourceID.MatchString(last.text()):
			return "knowledge learning", latency(openaifake.Text(fmt.Sprintf(`[{"summary": "Standups are at 10 on Mondays", `+
				`"topic": "meetings", "tags": ["standup"], "source_ids": [%q], "memory_type": "process", "confidence": 0.9, `+
				`"store": true, "rationale": "schedule"}]`, sourceID.FindStringSubmatch(last.text())[1])), 300, 1000)
		}
		return "fact extraction", latency(openaifake.Text(`[{"fact": "Prefers short answers"}]`), 100, 400)
	}
	if last.Role == "user" && tools {
		switch text := last.text(); {
		case strings.Contains(text, "search the web"):
			return "search_web call", latency(openaifake.ToolCalls(openaifake.ToolCall{ID: "call_1", Name: "search_web",
				Arguments: map[string]string{"query": "release notes"}}), 300, 1500)
		case strings.Contains(text, "visit the status page"):
			return "visit_page call", latency(openaifake.ToolCalls(openaifake.ToolCall{ID: "call_1", Name: "visit_page",
				Arguments: map[string]string{"url": s.web.URL() + "/status"}}), 300, 1500)
		}
	}
	for _, m := range slices.Backward(messages) {
		if m.Role == "user" {
			return "reply", latency(openaifake.Text("Noted: "+strings.Join(token.FindAllString(m.text(), -1), " ")), 300, 1500)
		}
	}
	return "no user message", openaifake.HTTPError(400, "no user message")
}

func latency(r testkit.Response, minMS, maxMS int) testkit.Response {
	return r.WithDelay(time.Duration(minMS+rand.IntN(maxMS-minMS)) * time.Millisecond)
}

// settle waits until every sent message is answered or lost and no buffer or journaled run is left.
func (s *soak) settle() {
	deadline := time.NewTimer(answerWithin + time.Minute)
	defer deadline.Stop()
	recheck := time.NewTicker(100 * time.Millisecond)
	defer recheck.Stop()
	for {
		changed := s.changed.C()
		s.expire()
		if s.unanswered() == 0 && s.pending() == 0 {
			return
		}
		select {
		case <-changed:
		case <-recheck.C:
		case <-deadline.C:
			s.t.Fatalf("%d buffers and journaled runs left after every message was answered or lost", s.pending())
		}
	}
}

func (s *soak) unanswered() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.convs {
		n += c.waiting
	}
	return n
}

// pending counts the stored debounce buffers and journaled runs.
func (s *soak) pending() int {
	return s.count(`SELECT (SELECT count(*) FROM key_value_store WHERE key LIKE '` + bufferPrefix + `%') +
		(SELECT count(*) FROM dost_inflight_runs)`)
}

func (s *soak) count(query string) int {
	var n int
	if err := s.db.QueryRow(s.t.Context(), query).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// sample measures the process after dropping what the fakes recorded and a full collection, and counts the agent's
// database connections and the buffers past their flush deadline.
func (s *soak) sample() sample {
	s.forget()
	debug.FreeOSMemory()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		s.t.Fatal(err)
	}
	if s.count(fmt.Sprintf(`SELECT count(*) FROM key_value_store WHERE key LIKE '%s%%' AND
		(value->>'flush_after')::timestamptz < now() - interval '%d seconds'`, bufferPrefix, int(overdue.Seconds()))) > 0 {
		s.mu.Lock()
		s.overdue++
		s.mu.Unlock()
	}
	return sample{at: time.Since(s.start).Round(time.Second), heap: m.HeapAlloc, rss: rss(s.t), goroutines: runtime.NumGoroutine(),
		fds: len(fds), conn: s.count(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()`)}
}

// forget drops what the fakes recorded, so that their memory does not grow with the run.
func (s *soak) forget() {
	for _, f := range []*testkit.Server{s.chat.Server, s.model.Server, s.search.Server, s.web} {
		f.Forget()
	}
	s.chat.Prune(time.Now().Add(-time.Minute).UnixMilli())
}

func (s *soak) report(warm int, final sample, logPath string) {
	t := s.t
	t.Logf("%-9s %8s %10s %10s %10s %6s %6s", "", "at", "heap KiB", "RSS KiB", "goroutines", "fds", "db")
	row := func(name string, x sample) {
		t.Logf("%-9s %8s %10d %10d %10d %6d %6d", name, x.at, x.heap>>10, x.rss>>10, x.goroutines, x.fds, x.conn)
	}
	peak := s.samples[0]
	for i, x := range s.samples {
		name := "traffic"
		if i == warm-1 {
			name = "baseline"
		}
		row(name, x)
		if i < warm {
			peak = sample{heap: max(peak.heap, x.heap), rss: max(peak.rss, x.rss), goroutines: max(peak.goroutines, x.goroutines),
				fds: max(peak.fds, x.fds), conn: max(peak.conn, x.conn)}
		}
	}
	peak.at = s.samples[warm-1].at
	row("final", final)
	row("peak", peak)
	s.mu.Lock()
	t.Logf("addressed messages %d, chatter %d, reconnects %d after %d drops, settings reloads %d",
		s.tokens, s.chatter, s.chat.Connections()-1, s.drops, s.reloads)
	temps := 0
	for _, k := range slices.Sorted(maps.Keys(s.calls)) {
		t.Logf("model requests, %s: %d", k, s.calls[k])
		if strings.HasPrefix(k, "search_web call at temperature") {
			temps++
		}
	}
	if len(s.lost) > 0 {
		t.Errorf("%d messages unanswered after %v: %v", len(s.lost), answerWithin, s.lost)
	}
	if len(s.duplicated) > 0 || len(s.unexpected) > 0 {
		t.Errorf("answered more than once: %v; replies quoting no sent message: %q", s.duplicated, s.unexpected[:min(len(s.unexpected), 10)])
	}
	if s.overdue > 0 {
		t.Errorf("%d samples found a debounce buffer more than %v past its flush deadline", s.overdue, overdue)
	}
	if temps < 2 {
		t.Errorf("tool calls were requested at %d temperatures, want both reloaded values", temps)
	}
	s.mu.Unlock()
	// RSS under traffic runs well above its settled value, so it is compared with the settled sample halfway.
	for _, r := range []struct {
		name, from  string
		base, final float64
	}{
		{"heap", "first-half peak", float64(peak.heap), float64(final.heap)},
		{"RSS", "halfway", float64(s.samples[warm-1].rss), float64(final.rss)},
		{"goroutines", "first-half peak", float64(peak.goroutines), float64(final.goroutines)},
		{"open files", "first-half peak", float64(peak.fds), float64(final.fds)},
		{"database connections", "first-half peak", float64(peak.conn), float64(final.conn)},
	} {
		if r.final > max(r.base*(1+growth), r.base+slack) {
			t.Errorf("%s grew from %.0f (%s) to %.0f, more than %.0f%%", r.name, r.base, r.from, r.final, growth*100)
		}
	}
	logErrors(t, logPath)
}

// logErrors reports the agent's error and warning log lines, counted by message.
func logErrors(t *testing.T, path string) {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	msg := regexp.MustCompile(`level=(ERROR|WARN) msg=("[^"]*"|\S+)`)
	counts := map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if m := msg.FindStringSubmatch(sc.Text()); m != nil {
			counts[m[1]+" "+m[2]]++
		}
	}
	for _, k := range slices.Sorted(maps.Keys(counts)) {
		t.Logf("log %s: %d", k, counts[k])
	}
}

// profile writes heap and goroutine profiles named after point to DOST_SOAK_OUT, when it is set.
func profile(t *testing.T, point string) {
	dir := os.Getenv("DOST_SOAK_OUT")
	if dir == "" {
		return
	}
	for _, name := range []string{"heap", "goroutine"} {
		f, err := os.Create(filepath.Join(dir, name+"-"+point+".pb.gz"))
		if err == nil {
			err = errors.Join(pprof.Lookup(name).WriteTo(f, 0), f.Close())
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// rss is the process's resident set size in bytes.
func rss(t *testing.T) uint64 {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(status)) {
		if kb, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			var n uint64
			if _, err := fmt.Sscan(kb, &n); err != nil {
				t.Fatal(err)
			}
			return n << 10
		}
	}
	t.Fatal("no VmRSS in /proc/self/status")
	return 0
}

// id pads prefix and n to the 26 characters of a platform ID.
func id(prefix string, n int) string {
	return fmt.Sprintf("%s%0*d", prefix, 26-len(prefix), n)
}
