package brand_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sarpaydemir/dost/internal/automation"
	"github.com/sarpaydemir/dost/internal/automation/brand"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/kb"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/port/web"
	"github.com/sarpaydemir/dost/internal/prompt"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/fakeclock"
	"github.com/sarpaydemir/dost/internal/testkit/kvtest"
)

// catalog is the automation's catalog with every text replaced by its short ID, followed by its placeholders in
// parentheses, so expectations show what each text was given.
func catalog(t *testing.T) *prompt.Catalog {
	t.Helper()
	overrides := map[string]string{}
	for _, e := range brand.Prompts() {
		text := strings.TrimPrefix(e.ID, "automation.brand.")
		if e.Kind == prompt.KindFormat {
			var ps []string
			for _, p := range e.Placeholders {
				ps = append(ps, "{"+p+"}")
			}
			text += "(" + strings.Join(ps, "|") + ")"
		}
		overrides[e.ID] = text
	}
	cat, err := prompt.Load(brand.Prompts(), "")
	if err != nil {
		t.Fatal(err)
	}
	if cat, err = cat.With(overrides); err != nil {
		t.Fatal(err)
	}
	return cat
}

// chatFake records posts; once it holds takeOverAt posts, when set, it refuses every post as taken over.
type chatFake struct {
	posts      []chat.Outgoing
	takeOverAt int
}

func (c *chatFake) Send(ctx context.Context, m chat.Outgoing) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.takeOverAt > 0 && len(c.posts) >= c.takeOverAt {
		return "", chat.ErrTakenOver
	}
	c.posts = append(c.posts, m)
	return "post", nil
}

type modelFake struct {
	answers  []string
	requests []model.Request
}

func (m *modelFake) Generate(_ context.Context, r model.Request) string {
	m.requests = append(m.requests, r)
	if len(m.answers) == 0 {
		return "{}"
	}
	a := m.answers[0]
	m.answers = m.answers[1:]
	return a
}

// prompts returns the user message of each request, after checking that it was the request's only message.
func (m *modelFake) prompts(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, r := range m.requests {
		if r.Model != "chat-model" || len(r.Messages) != 1 || r.Messages[0].Role != model.RoleUser || r.Temperature != "" || r.MaxTokens != 0 {
			t.Fatalf("request %+v is not one user message to the chat model with the default parameters", r)
		}
		out = append(out, r.Messages[0].Content)
	}
	return out
}

type searchFake struct {
	results map[string][]web.Result
	queries []string
}

func (s *searchFake) Search(_ context.Context, query string, limit int) ([]web.Result, error) {
	s.queries = append(s.queries, query+"/"+string(rune('0'+limit)))
	if r, ok := s.results[query]; ok {
		return r, nil
	}
	return nil, errors.New("engine down")
}

type fetchFake struct {
	page string
	urls []string
}

func (f *fetchFake) Fetch(_ context.Context, url string) (string, error) {
	f.urls = append(f.urls, url)
	return f.page, nil
}

// historyFake records each item added as "<key> <item>" and fails the adds listed in fail, counted from 0.
type historyFake struct {
	mu    sync.Mutex
	added []string
	fail  map[int]bool
	n     int
}

func (h *historyFake) Add(_ context.Context, key string, item json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n++
	if h.fail[h.n-1] {
		return errors.New("database down")
	}
	h.added = append(h.added, key+" "+string(item))
	return nil
}

type fixture struct {
	chat    *chatFake
	model   *modelFake
	search  *searchFake
	fetch   *fetchFake
	history *historyFake
	batch   *kvtest.Memory
	run     automation.Run
}

func newFixture(t *testing.T, clk clock.Clock, m chat.Message, brandName string) *fixture {
	f := &fixture{chat: &chatFake{}, model: &modelFake{}, search: &searchFake{results: map[string][]web.Result{}},
		fetch: &fetchFake{}, history: &historyFake{}, batch: &kvtest.Memory{}}
	discard := slog.New(slog.DiscardHandler)
	f.run = automation.Run{
		Message: m, Settings: &config.Settings{Model: "chat-model", KBBatchKey: "agent:public_kb_batch"},
		Args: map[string]string{"brand_name": brandName}, Chat: f.chat, Catalog: catalog(t), Model: f.model,
		Search: f.search, Fetch: f.fetch, History: f.history,
		Knowledge: kb.Batch{Store: f.batch, Learn: kb.NewTrigger(), Log: discard}, Clock: clk, Log: discard,
	}
	return f
}

func (f *fixture) texts() []string {
	var out []string
	for _, p := range f.chat.posts {
		out = append(out, p.Text)
	}
	return out
}

func (f *fixture) queued(t *testing.T) string {
	raw, _, err := f.batch.Get(t.Context(), "agent:public_kb_batch")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

var thread = chat.Message{ID: "p2", ConversationID: "c1", AuthorID: "u1", ThreadID: "r1", Kind: chat.Public}

func str(s string) *string { return &s }

func diff(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n%s\nwant\n%s", what, strings.Join(got, "\n---\n"), strings.Join(want, "\n---\n"))
	}
}

func TestAnalysisPostsEachStepAddsItToTheHistoriesAndQueuesItForLearning(t *testing.T) {
	f := newFixture(t, fakeclock.New(time.Unix(1761900000, 250_000_000)), thread, "Acme")
	f.search.results["Acme official website"] = []web.Result{{URL: str("https://acme.example/")}}
	f.search.results["Acme instagram"] = []web.Result{{URL: str("https://x.example/acme")},
		{URL: str("https://www.instagram.com/acme.tr/reels/")}}
	f.fetch.page = "Title: Acme\nURL: https://acme.example/\n\nWe sell rockets."
	f.model.answers = []string{
		`{"summary": "Rocket maker", "findings": ["f1"], "products": ["rocket", "anvil"]}`,
		"```json\n{\"summary\": \"Bold posts\", \"themes\": [\"space\"]}\n```",
		`{"categories": ["Toys", "Tools"], "target": "Coyotes", "price": 5}`,
		`{"slogans": [{"text": "Go Acme", "why": "short"}, {"text": "Boom"}]}`,
		`{"videos": [{"title": "Launch", "idea": "A rocket", "duration": "30s"}, {"title": "Fall"}]}`,
	}
	reply, err := brand.Analyze(t.Context(), f.run)
	if err != nil {
		t.Fatal(err)
	}

	if want := "summary(Acme|Toys, Tools|Coyotes|5|• Go Acme - short\n• Boom - |• Launch (30s) - A rocket\n• Fall (?) - )"; reply != want {
		t.Errorf("reply = %q, want %q", reply, want)
	}
	messages := []string{
		"web_message(https://acme.example/|Rocket maker)",
		"instagram_message(@acme.tr|Bold posts)",
		"analysis_message(Toys, Tools|Coyotes|5)",
		"• **Go Acme** - short\n• **Boom** - ",
		"• **Launch** (30s): A rocket\n• **Fall** (): ",
	}
	titles := []string{"title_web", "title_instagram", "title_analysis", "title_slogans", "title_videos"}
	posts := []string{"start(Acme)"}
	var added []string
	var queued []map[string]any
	for i, msg := range messages {
		step := string(rune('1' + i))
		posts = append(posts, "step("+step+"|"+titles[i]+"|"+msg+")")
		item, _ := json.Marshal(map[string]any{"user": "step_memory(Acme|" + step + ")", "assistant": "**" + titles[i] + ":**\n" + msg,
			"ts": "2025-10-31T08:40:00.250000+00:00", "scope": "public", "context_id": "r1", "step": i + 1})
		added = append(added, "thread:r1 "+string(item), "u1 "+string(item))
		queued = append(queued, map[string]any{"post_id": "brand:Acme:" + step + ":1761900000",
			"message": "Acme - " + titles[i] + "\n" + msg, "channel_id": "brand_analysis", "create_at": 1761900000.2499998})
	}
	diff(t, "posts", f.texts(), append(posts, "done(Acme)"))
	for _, p := range f.chat.posts {
		if p.ConversationID != "c1" || p.ThreadID != "r1" {
			t.Errorf("post %+v not in thread r1 of c1", p)
		}
	}
	diff(t, "prompts", f.model.prompts(t), []string{
		"prefixweb(Acme|https://acme.example/|Title: Acme\nURL: https://acme.example/\n\nWe sell rockets.)suffix",
		"prefixinstagram(Acme|@acme.tr)suffix",
		"prefixanalysis(Acme|Rocket maker|['rocket', 'anvil']|Bold posts|['space'])suffix",
		"prefixslogans(Acme|['Toys', 'Tools']|Coyotes|Rocket maker)suffix",
		"prefixvideos(Acme|['Go Acme', 'Boom']|Coyotes)suffix",
	})
	diff(t, "searches", f.search.queries, []string{"Acme official website/3", "Acme instagram/3"})
	diff(t, "fetches", f.fetch.urls, []string{"https://acme.example/"})
	if len(f.history.added) != len(added) {
		t.Fatalf("history items:\n%s", strings.Join(f.history.added, "\n"))
	}
	for i, got := range f.history.added {
		key, item, _ := strings.Cut(got, " ")
		wantKey, wantItem, _ := strings.Cut(added[i], " ")
		if eq, err := testkit.JSONEqual([]byte(item), []byte(wantItem)); key != wantKey || err != nil || !eq {
			t.Errorf("history item %d = %s, want %s", i, got, added[i])
		}
	}
	want, _ := json.Marshal(map[string]any{"messages": queued})
	if eq, err := testkit.JSONEqual([]byte(f.queued(t)), want); err != nil || !eq {
		t.Errorf("batch = %s\nwant    %s", f.queued(t), want)
	}
}

func TestDirectMessageWithoutResultsOrJSONObjectsUsesTheDefaultsAndQueuesNothing(t *testing.T) {
	dm := chat.Message{ID: "p1", ConversationID: "d1", AuthorID: "u1", Kind: chat.Direct}
	f := newFixture(t, clock.Real(), dm, "Acme")
	f.model.answers = []string{"not json", `{"categories": "AB"}`, "[]"}
	reply, err := brand.Analyze(t.Context(), f.run)
	if err != nil {
		t.Fatal(err)
	}
	diff(t, "posts", f.texts(), []string{
		"start(Acme)",
		"step(1|title_web|default_no_website)",
		"step(2|title_instagram|default_no_instagram_step)",
		"step(3|title_analysis|analysis_message(default_category|default_target|default_price))",
		"step(4|title_slogans|• **default_slogan_text(Acme)** - default_slogan_why)",
		"step(5|title_videos|• **default_video_title(Acme)** (default_video_duration): default_video_idea)",
		"done(Acme)",
	})
	if want := "summary(Acme|default_category|default_target|default_price|• default_slogan_text(Acme) - default_slogan_why|" +
		"• default_video_title(Acme) (default_video_duration) - default_video_idea)"; reply != want {
		t.Errorf("reply = %q, want %q", reply, want)
	}
	for _, p := range f.chat.posts {
		if p.ConversationID != "d1" || p.ThreadID != "" {
			t.Errorf("post %+v not at the root of d1", p)
		}
	}
	diff(t, "prompts", f.model.prompts(t), []string{
		"prefixanalysis(Acme|default_no_website|[]|default_no_instagram|[])suffix",
		"prefixslogans(Acme|['default_category']|default_target|default_no_website)suffix",
		"prefixvideos(Acme|['default_slogan_text(Acme)']|default_target)suffix",
	})
	var item struct {
		ContextID string `json:"context_id"`
	}
	for _, a := range f.history.added {
		key, raw, _ := strings.Cut(a, " ")
		_ = json.Unmarshal([]byte(raw), &item)
		if key != "u1" || item.ContextID != "" || !strings.Contains(raw, `"scope":"public"`) {
			t.Errorf("history item %s, want a public item under u1 with an empty context", a)
		}
	}
	if len(f.history.added) != 5 || f.queued(t) != "" {
		t.Errorf("%d history items and batch %q, want 5 and none", len(f.history.added), f.queued(t))
	}
}

func TestFencedAnswersAndNonStringValues(t *testing.T) {
	f := newFixture(t, clock.Real(), thread, "Acme")
	f.search.results["Acme official website"] = []web.Result{{URL: str("acme.example")}}
	f.search.results["Acme instagram"] = []web.Result{{URL: str("https://www.instagram.com/acme")}}
	f.fetch.page = strings.Repeat("é", 6000)
	f.model.answers = []string{
		"  pre ```{\"summary\": \"jsonic json\"}``` post",
		"```json\n{\"summary\": \"one fence\"}",
		`{"categories": "AB", "target": [1, "x"], "price": {"a": true}}`,
		`{"slogans": [{"text": null, "why": 1.0}]}`,
		`{"videos": [{"duration": 15}]}`,
	}
	reply, err := brand.Analyze(t.Context(), f.run)
	if err != nil {
		t.Fatal(err)
	}
	diff(t, "posts", f.texts()[1:6], []string{
		"step(1|title_web|web_message(acme.example|ic json))",
		"step(2|title_instagram|instagram_message(@acme|one fence))",
		"step(3|title_analysis|analysis_message(A, B|[1, 'x']|{'a': True}))",
		"step(4|title_slogans|• **None** - 1.0)",
		"step(5|title_videos|• **** (15): )",
	})
	diff(t, "prompts", f.model.prompts(t)[2:], []string{
		"prefixanalysis(Acme|ic json|[]|one fence|[])suffix",
		"prefixslogans(Acme|AB|[1, 'x']|ic json)suffix",
		"prefixvideos(Acme|[None]|[1, 'x'])suffix",
	})
	if want := "prefixweb(Acme|acme.example|" + strings.Repeat("é", 5000) + ")suffix"; f.model.prompts(t)[0] != want {
		t.Errorf("web prompt does not hold the first 5000 characters of the page")
	}
	if want := "summary(Acme|A, B|[1, 'x']|{'a': True}|• None - 1.0|•  (15) - )"; reply != want {
		t.Errorf("reply = %q, want %q", reply, want)
	}
}

func TestEmptyBrandPostsTheEmptyMessageAndReturnsItsReply(t *testing.T) {
	f := newFixture(t, clock.Real(), thread, "")
	reply, err := brand.Analyze(t.Context(), f.run)
	if err != nil || reply != "empty_reply" {
		t.Errorf("Analyze = %q, %v, want the empty reply", reply, err)
	}
	diff(t, "posts", f.texts(), []string{"empty"})
	if len(f.model.requests)+len(f.search.queries)+len(f.history.added) != 0 {
		t.Error("an empty brand was analysed")
	}
}

func TestRunCancelledBeforeTheStartPostWritesNothing(t *testing.T) {
	f := newFixture(t, clock.Real(), thread, "Acme")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := brand.Analyze(ctx, f.run); !errors.Is(err, context.Canceled) {
		t.Errorf("Analyze = %v, want cancelled", err)
	}
	if len(f.chat.posts)+len(f.search.queries) != 0 {
		t.Errorf("cancelled run posted %d messages and searched %d times", len(f.chat.posts), len(f.search.queries))
	}
}

func TestFailingPhaseRunsTwiceMoreTenSecondsApartWithoutRepostingThenPostsTheError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, clock.Real(), thread, "Acme")
		f.search.results["Acme official website"] = []web.Result{}
		f.search.results["Acme instagram"] = []web.Result{{URL: str("https://a.example")}, {URL: nil}}
		start := time.Now()
		reply, err := brand.Analyze(t.Context(), f.run)
		const failure = "argument of type 'NoneType' is not iterable"
		if err != nil || reply != "error_reply("+failure+")" {
			t.Errorf("Analyze = %q, %v", reply, err)
		}
		if waited := time.Since(start); waited != 20*time.Second {
			t.Errorf("waited %v, want 20s", waited)
		}
		diff(t, "posts", f.texts(), []string{"start(Acme)", "step(1|title_web|default_no_website)", "error(" + failure + ")"})
		diff(t, "searches", f.search.queries, []string{"Acme official website/3", "Acme instagram/3", "Acme instagram/3", "Acme instagram/3"})
	})
}

func TestStepThatFailsAfterItsPostIsNotRunAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, clock.Real(), thread, "Acme")
		f.history.fail = map[int]bool{2: true} // step 2's thread item
		start := time.Now()
		if _, err := brand.Analyze(t.Context(), f.run); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != 10*time.Second {
			t.Errorf("waited %v, want 10s", waited)
		}
		if got := f.texts(); len(got) != 7 || !strings.HasPrefix(got[2], "step(2|") || !strings.HasPrefix(got[3], "step(3|") {
			t.Errorf("posts:\n%s", strings.Join(got, "\n"))
		}
		if len(f.history.added) != 8 || len(f.search.queries) != 2 {
			t.Errorf("%d history items and %d searches, want 8 and 2", len(f.history.added), len(f.search.queries))
		}
	})
}

func TestAnswersOfTheWrongShapeFailTheAnalysis(t *testing.T) {
	for _, tc := range []struct{ answer, failure string }{
		{`{"categories": ["a", 1]}`, "sequence item 1: expected str instance, int found"},
		{`{"categories": null}`, "can only join an iterable"},
		{`[1]`, "'list' object has no attribute 'get'"},
		{`{"slogans": null}`, "'NoneType' object is not iterable"},
		{`{"slogans": "ab"}`, "'str' object has no attribute 'get'"},
	} {
		synctest.Test(t, func(t *testing.T) {
			f := newFixture(t, clock.Real(), thread, "Acme")
			f.model.answers = []string{tc.answer, tc.answer, tc.answer, tc.answer, tc.answer, tc.answer}
			reply, err := brand.Analyze(t.Context(), f.run)
			if err != nil || reply != "error_reply("+tc.failure+")" {
				t.Errorf("answer %s: Analyze = %q, %v, want the failure %q", tc.answer, reply, err, tc.failure)
			}
			if got := f.texts(); got[len(got)-1] != "error("+tc.failure+")" {
				t.Errorf("answer %s: last post %q", tc.answer, got[len(got)-1])
			}
		})
	}
}

func TestTakenOverRunStopsWithoutRetryingRememberingOrPostingTheError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, clock.Real(), thread, "Acme")
		f.chat.takeOverAt = 2 // step 2's post is refused
		start := time.Now()
		if _, err := brand.Analyze(t.Context(), f.run); !errors.Is(err, chat.ErrTakenOver) {
			t.Fatalf("Analyze = %v, want taken over", err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("waited %v before stopping", waited)
		}
		diff(t, "posts", f.texts(), []string{"start(Acme)", "step(1|title_web|default_no_website)"})
		var batch struct{ Messages []any }
		_ = json.Unmarshal([]byte(f.queued(t)), &batch)
		if len(f.history.added) != 2 || len(batch.Messages) != 1 || len(f.search.queries) != 2 {
			t.Errorf("%d history items, %d queued, %d searches; want only step 1's 2 items and 1 queued, 2 searches",
				len(f.history.added), len(batch.Messages), len(f.search.queries))
		}
	})
}
