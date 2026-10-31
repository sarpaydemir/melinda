//go:build integration

package app

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/logging"
	"github.com/sarpaydemir/dost/internal/store/pg"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/fakeclock"
	"github.com/sarpaydemir/dost/internal/testkit/golden"
	"github.com/sarpaydemir/dost/internal/testkit/mattermostfake"
	"github.com/sarpaydemir/dost/internal/testkit/openaifake"
	"github.com/sarpaydemir/dost/internal/testkit/planefake"
	"github.com/sarpaydemir/dost/internal/testkit/searxngfake"
	"github.com/sarpaydemir/dost/internal/testkit/testdb"
	"github.com/sarpaydemir/dost/internal/testkit/webfake"
)

// Conversation goldens: each scenario runs every component against the chat and model fakes, a fake clock, and a
// fresh database, and records every model request body, every reply post body, the stored key-value rows after
// each step (generations normalized), and at the end the chat_sessions rows (new turns' ts normalized) and the
// vector rows. With DOST_COMPAT_PROFILE set, each scenario also runs with the profile's prompts and settings and
// must produce the profile's goldens/<area>/<scenario>.json exactly. Scenarios that reach the web, Plane, or file
// uploads also record those requests, with the page server's address written as webHost.

// turnTS is a saved turn's time: microseconds are left out when they are zero.
var turnTS = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d{6})?\+00:00$`)

var (
	start  = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	baseMS = start.UnixMilli()
)

type scenario struct {
	name     string
	settings string // extra settings document members
	bot      string // the agent's username; "agent" when empty
	replies  []testkit.Response
	unsorted bool             // runs answer concurrently: requests and posts are compared as sorted lists
	seed     func(h *harness) // writes rows before the agent starts
	// setup, when set, adjusts the settings document and the adapters before the agent starts, and returns extra
	// environment entries.
	setup func(h *harness, doc map[string]json.RawMessage, table *adapters) []string
	play  func(h *harness)
}

func TestConversationGoldens(t *testing.T) { assertGoldens(t, "conversation", scenarios) }

func TestConversationCompatGoldens(t *testing.T) { assertCompatGoldens(t, "conversation", scenarios) }

func assertGoldens(t *testing.T, area string, scs []scenario) {
	for _, sc := range scs {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			golden.AssertJSON(t, "golden/"+area+"/"+sc.name, play(t, sc, nil, ""))
		})
	}
}

func assertCompatGoldens(t *testing.T, area string, scs []scenario) {
	dir := os.Getenv("DOST_COMPAT_PROFILE")
	if dir == "" {
		t.Skip("DOST_COMPAT_PROFILE is not set")
	}
	goldens := filepath.Join(dir, "goldens", area)
	var settings map[string]json.RawMessage
	if b, err := os.ReadFile(filepath.Join(goldens, "settings.json")); err != nil || json.Unmarshal(b, &settings) != nil {
		t.Skipf("the profile has no %s settings (%v)", area, err)
	}
	for _, sc := range scs {
		want, err := os.ReadFile(filepath.Join(goldens, sc.name+".json"))
		if err != nil {
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			got := play(t, sc, settings, filepath.Join(dir, "prompts"))
			if ok, err := testkit.JSONEqual(got, want); err != nil || !ok {
				t.Errorf("differs from the profile golden (%v):\n got: %s\nwant: %s", err, got, want)
			}
		})
	}
}

var scenarios = []scenario{
	{name: "dm", replies: texts("Hi Ada!"), play: func(h *harness) {
		h.post(post("p1", "u1", "dm1", "", "hello", 0))
		h.flush(3*time.Second, 1)
	}},
	{name: "dm_tools", settings: `"enabled_tools":["send_progress_update"]`, replies: []testkit.Response{
		openaifake.ToolCalls(openaifake.ToolCall{ID: "call_1", Name: "send_progress_update", Arguments: `{"message": "Looking"}`}),
		openaifake.Text("Hi Ada!"),
	}, play: func(h *harness) {
		h.post(post("p1", "u1", "dm1", "", "hello", 0))
		h.flush(3*time.Second, 1)
	}},
	{name: "dm_burst", replies: texts("All three, got it."), play: func(h *harness) {
		h.post(post("p1", "u1", "dm1", "", "one", 0))
		h.clock.Advance(time.Second)
		h.post(post("p2", "u1", "dm1", "", "two", 1000))
		h.clock.Advance(time.Second)
		h.post(post("p3", "u1", "dm1", "", "three", 2000))
		h.flush(3*time.Second, 1)
	}},
	{name: "dm_pause", replies: texts("First answer.", "Second answer."), play: func(h *harness) {
		h.post(post("p1", "u1", "dm1", "", "first", 0))
		h.flush(3*time.Second, 1)
		h.clock.Advance(7 * time.Second)
		h.post(post("p2", "u1", "dm1", "", "second", 10000))
		h.flush(3*time.Second, 2)
	}},
	{name: "channel_mention", replies: texts("Not much."), play: func(h *harness) {
		h.post(post("p1", "u1", "ch1", "", "@agent what is up?", 0))
		h.flush(3*time.Second, 1)
	}},
	{name: "thread_name", replies: texts("Sure."), play: func(h *harness) {
		h.chat.AddPost(post("r1", "u2", "ch1", "", "kickoff", -60000))
		h.post(post("p1", "u1", "ch1", "r1", "Agent can you help?", 0))
		h.flush(3*time.Second, 1)
	}},
	{name: "thread_two_users", replies: texts("Hello Ada.", "Hello Grace."), play: func(h *harness) {
		h.chat.AddPost(post("r1", "u2", "ch1", "", "kickoff", -60000))
		h.post(post("p1", "u1", "ch1", "r1", "@agent first", 0))
		h.clock.Advance(time.Second)
		h.post(post("p2", "u2", "ch1", "r1", "@agent second", 1000))
		h.flush(2*time.Second, 1)
		h.flush(time.Second, 2)
	}},
	{name: "not_answered", play: func(h *harness) {
		h.post(post("p1", "bot", "dm1", "", "my own post", 0))
		system := post("p2", "u1", "ch1", "", "@agent joined", 1000)
		system.Type = "system_join_channel"
		h.post(system)
		deleted := post("p3", "u1", "dm1", "", "deleted", 2000)
		deleted.DeleteAt = baseMS + 2500
		h.post(deleted)
		h.post(post("p4", "u1", "ch1", "", "hello everyone", 3000))
		h.clock.Advance(3 * time.Second)
		h.snapshot("flushed")
	}},
	{name: "excluded_channel", settings: `"name_excluded_channels":["` + id("ch1") + `"]`, replies: texts("Hi all."), play: func(h *harness) {
		h.post(post("p1", "u1", "ch1", "", "hello everyone", 0))
		h.flush(3*time.Second, 1)
	}},
	{name: "dotted_capital_i", bot: "İlke", replies: texts("Answered."), play: func(h *harness) {
		h.post(post("p1", "u1", "ch1", "", "@ilke lowercase", 0))
		h.post(post("p2", "u1", "ch1", "", "@İlke capital", 1000))
		h.flush(3*time.Second, 1)
	}},
	{name: "model_failure", replies: []testkit.Response{
		openaifake.HTTPError(500, "upstream"), openaifake.HTTPError(500, "upstream"), openaifake.HTTPError(500, "upstream"),
	}, play: func(h *harness) {
		h.post(post("p1", "u1", "dm1", "", "hello", 0))
		h.clock.Advance(3 * time.Second)
		h.advanceArmed(time.Second)
		h.advanceArmed(2 * time.Second)
		h.awaitRuns(1)
		h.snapshot("flushed")
	}},
	{name: "truncated_reply", replies: []testkit.Response{
		openaifake.Text("A long answer that was cut", openaifake.Finish("length")),
	}, play: func(h *harness) {
		h.post(post("p1", "u1", "dm1", "", "write a lot", 0))
		h.flush(3*time.Second, 1)
	}},
	{name: "recovery", unsorted: true, play: func(h *harness) {
		h.model.Chat().DefaultFunc(echo)
		h.post(post("p1", "u1", "dm1", "", "live", 0))
		h.flush(3*time.Second, 1)
		h.clock.Advance(7 * time.Second)
		connections := h.chat.Connections()
		h.chat.Drop()
		edited := post("p0", "u1", "dm1", "", "older (edited)", -30000)
		edited.UpdateAt = baseMS + 10000
		h.chat.AddPost(edited)
		h.chat.AddPost(post("p2", "u1", "dm1", "", "missed", 11000))
		h.chat.AddPost(post("p3", "u2", "ch1", "", "@agent missed mention", 12000))
		both := h.chat.AddPost(post("p4", "u1", "dm1", "", "pushed and listed", 13000))
		h.advanceArmed(time.Second)
		h.chat.AwaitWebsocket(h.t, func() bool { return h.chat.Connections() > connections })
		h.awaitLastSeen(both.CreateAt)
		h.chat.Push(h.chat.Event(both))
		h.await("live duplicate skipped", func() bool {
			return strings.Contains(h.logs.String(), `msg="message already buffered; skipped" component=dispatcher post_id=`+id("p4"))
		})
		h.snapshot("recovered")
		h.flush(3*time.Second, 3)
	}},
	{name: "restart", replies: texts("Back again."), seed: func(h *harness) {
		h.exec(`INSERT INTO key_value_store (type, key, value, created_at, updated_at) VALUES ('none', $1, $2, now(), now())`,
			h.setting("buffer_prefix", "dost:buffer:")+"dm:"+id("u1"), restartBuffer)
		h.snapshot("start")
	}, play: func(h *harness) {
		h.flush(3*time.Second, 1)
	}},
	{name: "tools_search_fetch", settings: `"enabled_tools":null`, play: func(h *harness) {
		h.search.Search().Reply(searxngfake.Results(
			searxngfake.Result{Title: "Go 1.26 is released", URL: h.web.URL() + "/go126", Content: "The Go team announces Go 1.26."},
			searxngfake.Result{Title: "Release notes", URL: "https://go.dev/doc/go1.26", Content: "Notes for {Go} 1.26"}))
		h.web.On("GET", "/go126").Default(webfake.HTML(`<html><head><title>Go 1.26 Released</title>` +
			`<style>p { color: red }</style></head><body><nav>Menu</nav><h1>Go 1.26</h1>` +
			`<p>Go 1.26   brings  faster builds.</p><p>  Ünicode &amp; more  </p><script>track()</script>` +
			`<footer>Footer</footer></body></html>`))
		h.model.Chat().Reply(
			openaifake.ToolCalls(openaifake.ToolCall{ID: "call_1", Name: "search_web", Arguments: `{"query": "go 1.26 release"}`}),
			openaifake.ToolCalls(openaifake.ToolCall{ID: "call_2", Name: "visit_page", Arguments: `{"url": "` + h.web.URL() + `/go126"}`}),
			openaifake.Text("Go 1.26 brings faster builds."))
		h.post(post("p1", "u1", "dm1", "", "what is new in go 1.26?", 0))
		h.flush(3*time.Second, 1)
	}},
	{name: "tools_plane", settings: `"enabled_tools":null`, play: func(h *harness) {
		h.plane.Route("POST", "/projects/{p}/work-items/").Reply(testkit.JSON(201,
			map[string]any{"id": planeItem, "sequence_id": 42, "name": "Fix login"}))
		for _, list := range []string{"labels", "states", "project-members"} {
			h.plane.Route("GET", "/projects/{p}/"+list+"/").Default(testkit.JSON(200, map[string]any{"results": []any{}}))
		}
		h.plane.Route("GET", "/projects/{p}/cycles/").Default(testkit.JSON(200,
			map[string]any{"results": []any{map[string]any{"id": planeCycle, "name": "Ekim 2026"}}}))
		h.plane.Route("POST", "/projects/{p}/cycles/{c}/cycle-issues/").Default(testkit.JSON(201, map[string]any{}))
		h.plane.Route("POST", "/projects/{p}/work-items/{w}/comments/").Reply(
			testkit.JSON(201, map[string]any{"id": "comment-1"}), testkit.JSON(201, map[string]any{"id": "comment-2"}))
		h.model.Chat().Reply(
			openaifake.ToolCalls(openaifake.ToolCall{ID: "call_1", Name: "plane_create_issue", Arguments: `{"project_id": "` +
				planeProject + `", "name": "Fix login", "description": "Users cannot sign in.\nSeen on <mobile> & web."}`}),
			openaifake.ToolCalls(openaifake.ToolCall{ID: "call_2", Name: "plane_create_comment", Arguments: `{"project_id": "` +
				planeProject + `", "work_item_id": "` + planeItem + `", "body": "Assigned to the web team."}`}),
			openaifake.Text("Created FIX-42 and added the comment."))
		h.post(post("p1", "u1", "dm1", "", "open a ticket for the login bug and note it is assigned", 0))
		h.flush(3*time.Second, 1)
	}},
	{name: "tools_image", settings: `"enabled_tools":null,"image_generation_model":"image-model"`, replies: []testkit.Response{
		openaifake.ToolCalls(openaifake.ToolCall{ID: "call_1", Name: "generate_image", Arguments: `{"prompt": "a red bicycle"}`}),
		openaifake.Text("", openaifake.Images("data:image/png;base64,"+onePixelPNG)),
		openaifake.Text("Here is your bicycle."),
	}, play: func(h *harness) {
		h.post(post("p1", "u1", "dm1", "", "draw a red bicycle", 0))
		h.flush(3*time.Second, 1)
	}},
	{name: "tools_budget", settings: `"enabled_tools":null,"max_tool_calls":2`, replies: []testkit.Response{
		openaifake.ToolCalls(
			openaifake.ToolCall{ID: "call_1", Name: "search_web", Arguments: `{"query": "one"}`},
			openaifake.ToolCall{ID: "call_2", Name: "search_web", Arguments: `{"query": "two"}`},
			openaifake.ToolCall{ID: "call_3", Name: "search_web", Arguments: `{"query": "three"}`}),
		openaifake.Text("Here is what I found."),
	}, play: func(h *harness) {
		h.search.Search().Default(searxngfake.Results(searxngfake.Result{Title: "A", URL: "https://a.example", Content: "a"}))
		h.post(post("p1", "u1", "dm1", "", "search one, two and three", 0))
		h.flush(3*time.Second, 1)
	}},
	{name: "tools_fallback", settings: `"enabled_tools":null,"tool_fallback_model":"fallback-model"`, replies: []testkit.Response{
		openaifake.HTTPError(500, "upstream"), openaifake.HTTPError(500, "upstream"), openaifake.HTTPError(500, "upstream"),
		openaifake.ToolCalls(openaifake.ToolCall{ID: "call_1", Name: "search_web", Arguments: `{"query": "weather"}`}),
		openaifake.Text("Sunny."),
	}, play: func(h *harness) {
		h.search.Search().Default(searxngfake.Results(searxngfake.Result{Title: "Forecast", URL: "https://w.example", Content: "Sunny"}))
		h.post(post("p1", "u1", "dm1", "", "weather?", 0))
		h.clock.Advance(3 * time.Second)
		h.advanceArmed(time.Second)
		h.advanceArmed(2 * time.Second)
		h.awaitRuns(1)
		h.snapshot("flushed")
	}},
	{name: "brand_analysis", settings: `"automations":[{"name":"marka","pattern":"/marka\\s+(.+)",` +
		`"handler":"brand_analysis","args":{"brand_name":1}}]`, play: func(h *harness) {
		h.search.Search().Reply(
			searxngfake.Results(searxngfake.Result{Title: "Kahve", URL: h.web.URL() + "/kahve", Content: "Coffee"}),
			searxngfake.Results(searxngfake.Result{Title: "Kahve (@kahve)", URL: "https://www.instagram.com/kahve/", Content: "Photos"}))
		h.web.On("GET", "/kahve").Default(webfake.HTML(`<html><head><title>Kahve</title></head><body>` +
			`<p>Coffee and chocolate since 1999.</p></body></html>`))
		h.model.Chat().Reply(
			openaifake.Text("```json\n{\"summary\": \"A coffee chain.\", \"findings\": [\"old\"], \"products\": [\"Coffee\", \"Chocolate\"]}\n```"),
			openaifake.Text(`{"summary": "Product photos.", "themes": ["coffee", "{season}"]}`),
			openaifake.Text(`{"categories": ["Drinks", "Desserts"], "target": "Young adults", "price": "Mid-range"}`),
			openaifake.Text(`{"slogans": [{"text": "Brew joy", "why": "Short"}, {"text": "Sip {more}", "why": "Playful"}]}`),
			openaifake.Text(`{"videos": [{"title": "Morning", "idea": "A morning ritual", "duration": "30s"}]}`))
		h.post(post("p1", "u1", "ch1", "", "@agent /marka Kahve", 0))
		h.flush(3500*time.Millisecond, 1) // a clock between whole seconds
	}},
}

const (
	planeProject = "11111111-1111-4111-8111-111111111111"
	planeItem    = "22222222-2222-4222-8222-222222222222"
	planeCycle   = "33333333-3333-4333-8333-333333333333"
	onePixelPNG  = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
)

// restartBuffer is the buffer a previous run left for p1 before stopping, as the dispatcher stores it.
var restartBuffer = strings.NewReplacer("p1", id("p1"), "u1", id("u1"), "dm1", id("dm1")).Replace(
	`{"generation":"0f0e0d0c-0b0a-4908-8706-050403020100","flush_after":"2026-10-31T12:00:03+00:00",` +
		`"payload":{"event":"posted","data":{"channel_type":"D","post":"{\"id\": \"p1\", \"create_at\": 1793448000000, ` +
		`\"update_at\": 1793448000000, \"delete_at\": 0, \"user_id\": \"u1\", \"channel_id\": \"dm1\", \"root_id\": \"\", ` +
		`\"message\": \"before restart\", \"type\": \"\", \"post_id\": \"p1\", \"channel_type\": \"D\", \"is_dm\": true, ` +
		`\"is_thread\": false, \"scope\": \"private\", \"reply_root_id\": \"\", \"context_id\": \"p1\"}"},` +
		`"broadcast":{"channel_id":"dm1"},"seq":1},` +
		`"messages":[{"post_id":"p1","user_id":"u1","message":"before restart","file_ids":[]}]}`)

func texts(replies ...string) []testkit.Response {
	var out []testkit.Response
	for _, r := range replies {
		out = append(out, openaifake.Text(r))
	}
	return out
}

// echo answers with the last message of the request, so concurrent runs get replies that do not depend on order.
func echo(r testkit.Request) testkit.Response {
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(r.Body, &body)
	return openaifake.Text("Answer to " + body.Messages[len(body.Messages)-1].Content)
}

func post(short, user, channel, root, message string, offsetMS int64) mattermostfake.Post {
	return mattermostfake.Post{ID: id(short), UserID: id(user), ChannelID: id(channel), RootID: id(root),
		Message: message, CreateAt: baseMS + offsetMS}
}

// id pads a short name to the 26 characters of a platform ID.
func id(short string) string {
	if short == "" {
		return ""
	}
	return short + strings.Repeat("0", 26-len(short))
}

type harness struct {
	t      *testing.T
	clock  *fakeclock.Clock
	chat   *mattermostfake.Fake
	model  *openaifake.Fake
	search *searxngfake.Fake
	web    *testkit.Server
	plane  *planefake.Fake
	db     *pg.DB
	doc    map[string]json.RawMessage // the settings document
	logs   *logWatch
	steps  []any
}

// play runs sc and returns its golden document. settings are extra settings members and prompts a prompt profile
// directory, both optional.
func play(t *testing.T, sc scenario, settings map[string]json.RawMessage, prompts string) []byte {
	dsn := testdb.New(t)
	h := &harness{t: t, clock: fakeclock.New(start), logs: &logWatch{match: "\x00", seen: make(chan struct{})}}
	h.chat = mattermostfake.New(t, "chat-token", mattermostfake.User{ID: id("bot"), Username: cmp.Or(sc.bot, "agent")})
	h.chat.AddUser(mattermostfake.User{ID: id("u1"), Username: "ada", FirstName: "Ada", LastName: "Lovelace", Position: "Engineer"},
		mattermostfake.User{ID: id("u2"), Username: "grace", FirstName: "Grace"})
	h.chat.AddTeam(mattermostfake.Team{ID: id("t1"), Name: "team", DisplayName: "Team"})
	h.chat.AddChannel(
		mattermostfake.Channel{ID: id("dm1"), Type: "D", Name: id("bot") + "__" + id("u1"), Members: []string{id("bot"), id("u1")}},
		mattermostfake.Channel{ID: id("ch1"), TeamID: id("t1"), Type: "O", Name: "town", DisplayName: "Town",
			Members: []string{id("bot"), id("u1"), id("u2")}})
	h.model = openaifake.New(t)
	h.model.Chat().Reply(sc.replies...)
	h.model.Embeddings().DefaultFunc(embed)
	h.search, h.web, h.plane = searxngfake.New(t), testkit.NewServer(t), planefake.New(t, "ws", "plane-key")

	doc := map[string]json.RawMessage{
		"model": raw(`"chat-model"`), "api_base": raw(`"` + h.model.URL() + `"`), "key": raw(`"model-key"`),
		"mattermost_endpoint": raw(`"` + h.chat.URL() + `"`), "mattermost_token": raw(`"chat-token"`),
		"mattermost_bot_id": raw(`"` + id("bot") + `"`), "enabled_tools": raw(`[]`),
		"plane_endpoint": raw(`"` + h.plane.URL() + `"`), "plane_api_key": raw(`"plane-key"`),
		"plane_workspace_slug": raw(`"ws"`), "web_fetch_allow_private": raw(`true`),
	}
	if sc.settings != "" {
		var extra map[string]json.RawMessage
		if err := json.Unmarshal([]byte("{"+sc.settings+"}"), &extra); err != nil {
			t.Fatal(err)
		}
		maps.Copy(doc, extra)
	}
	maps.Copy(doc, settings)
	h.doc = doc
	secrets := new(logging.Secrets)
	log := logging.New(h.logs, slog.LevelDebug, false, secrets)
	table := builtin(http.DefaultTransport, h.clock, "dost/test", h.search.URL(), log)
	var environ []string
	if sc.setup != nil {
		environ = sc.setup(h, doc, &table)
	}
	settingsJSON, _ := json.Marshal(doc)
	environ = append(environ, "DOST_DATABASE_URL="+dsn, "DOST_HTTP_ADDR=", "DOST_TIMEZONE=+03:00",
		"DOST_SETTINGS_JSON="+string(settingsJSON))
	if prompts != "" {
		environ = append(environ, "DOST_PROMPTS_DIR="+prompts)
	}
	env := environment(t, environ...)

	var err error
	if h.db, err = pg.Open(t.Context(), dsn, clock.Real(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	defer h.db.Close()
	if sc.seed != nil {
		sc.seed(h)
	}

	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx, env, "test", table, h.clock, secrets, log) }()
	h.chat.AwaitWebsocket(t, func() bool { return h.chat.Connections() > 0 })
	h.await("startup recovery", func() bool { return strings.Contains(h.logs.String(), `msg="recovery finished"`) })

	sc.play(h)
	stop()
	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil; logs:\n%s", err, h.logs)
	}
	if n := h.count(`SELECT count(*) FROM dost_inflight_runs`); n != 0 {
		t.Errorf("in-flight records left = %d, want 0", n)
	}

	var requests, posts []string
	for _, r := range h.model.Requests() {
		requests = append(requests, string(r.Body))
	}
	var uploads, searches, fetches, plane []string
	for _, r := range h.chat.Requests() {
		switch r.Call() {
		case "POST /api/v4/posts":
			posts = append(posts, string(r.Body))
		case "POST /api/v4/files":
			uploads = append(uploads, upload(t, r))
		}
	}
	if sc.unsorted {
		slices.Sort(requests)
		slices.Sort(posts)
	}
	for _, r := range h.search.Requests() {
		searches = append(searches, r.Call())
	}
	for _, r := range h.web.Requests() {
		fetches = append(fetches, r.Call()+" | User-Agent: "+r.Header.Get("User-Agent")+" | Accept: "+
			r.Header.Get("Accept")+" | Accept-Language: "+r.Header.Get("Accept-Language"))
	}
	for _, r := range h.plane.Requests() {
		plane = append(plane, strings.TrimSpace(r.Call()+" "+string(r.Body)))
	}
	slices.Sort(plane) // project metadata is read concurrently
	out := map[string]any{
		"steps": h.steps, "model_requests": orEmpty(requests), "posts": orEmpty(posts), "chat_sessions": h.sessions(),
	}
	if v := h.vectors(); len(v) > 0 {
		out["vectors"] = v
	}
	for key, list := range map[string][]string{"uploads": uploads, "searches": searches, "page_fetches": fetches,
		"plane_requests": plane} {
		if list != nil {
			out[key] = list
		}
	}
	return bytes.ReplaceAll(canonical(t, out), []byte(h.web.URL()), []byte(webHost))
}

// webHost stands for the page server's address in goldens.
const webHost = "http://web.test"

// upload describes a file upload: its channel and each file's name, type, and size.
func upload(t *testing.T, r testkit.Request) string {
	t.Helper()
	m, err := r.Multipart()
	if err != nil {
		t.Fatal(err)
	}
	out := "channel_id=" + strings.Join(m.Values["channel_id"], ",")
	for _, f := range m.Files {
		out += " " + f.Field + "=" + f.Filename + " " + f.ContentType + " " + strconv.Itoa(len(f.Data))
	}
	return out
}

// post delivers p live and waits until the dispatcher has read it.
func (h *harness) post(p mattermostfake.Post) {
	h.t.Helper()
	p = h.chat.Posted(p)
	h.awaitLastSeen(p.CreateAt)
	h.snapshot(strings.TrimRight(p.ID, "0"))
}

// flush advances the clock by d and waits until runs have finished in all.
func (h *harness) flush(d time.Duration, runs int) {
	h.t.Helper()
	h.clock.Advance(d)
	h.awaitRuns(runs)
	h.snapshot("flushed")
}

// advanceArmed waits until the earliest timer is due in d, as a retry or reconnect wait is when armed, and advances
// the clock by d.
func (h *harness) advanceArmed(d time.Duration) {
	h.t.Helper()
	h.await("a timer due in "+d.String(), func() bool { next, ok := h.clock.Next(); return ok && next == d })
	h.clock.Advance(d)
}

func (h *harness) awaitRuns(n int) {
	h.t.Helper()
	h.await("finished runs", func() bool { return strings.Count(h.logs.String(), `msg="run finished"`) >= n })
}

func (h *harness) awaitLastSeen(createAt int64) {
	h.t.Helper()
	want := json.Number(strconv.FormatInt(createAt, 10))
	h.await("last-seen marker", func() bool {
		for _, v := range h.store() {
			if m, ok := v.(map[string]any); ok && len(m) == 1 && m["create_at"] == want {
				return true
			}
		}
		return false
	})
}

func (h *harness) await(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(testkit.AwaitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; logs:\n%s", what, h.logs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) snapshot(after string) {
	h.steps = append(h.steps, map[string]any{"after": after, "store": h.store()})
}

// store returns the key-value rows, with buffer generations replaced by a placeholder.
func (h *harness) store() map[string]any {
	out := map[string]any{}
	for key, v := range h.rows(`SELECT key, value::text FROM key_value_store`) {
		if m, ok := v.(map[string]any); ok && m["generation"] != nil {
			m["generation"] = "<generation>"
		}
		out[key] = v
	}
	return out
}

// sessions returns the chat_sessions rows, each ts in the saved turns' format replaced by a placeholder.
func (h *harness) sessions() map[string]any {
	out := h.rows(`SELECT key, value::text FROM chat_sessions`)
	for _, v := range out {
		items, _ := v.([]any)
		for _, item := range items {
			turn := item.(map[string]any)
			if ts, _ := turn["ts"].(string); turnTS.MatchString(ts) {
				turn["ts"] = "<ts>"
			}
		}
	}
	return out
}

func (h *harness) rows(query string) map[string]any {
	h.t.Helper()
	rows, err := h.db.Query(h.t.Context(), query)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]any{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			h.t.Fatal(err)
		}
		out[key] = decode(h.t, []byte(value))
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *harness) count(query string) int {
	h.t.Helper()
	rows, err := h.db.Query(h.t.Context(), query)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		if err := rows.Scan(&n); err != nil {
			h.t.Fatal(err)
		}
	}
	return n
}

// setting returns a string setting of the document, or def when it has none.
func (h *harness) setting(key, def string) string {
	var v string
	if json.Unmarshal(h.doc[key], &v) != nil || v == "" {
		return def
	}
	return v
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(h.t.Context(), sql, args...); err != nil {
		h.t.Fatal(err)
	}
}

func decode(t *testing.T, b []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return v
}

// canonical encodes v with sorted keys and without HTML escaping.
func canonical(t *testing.T, v any) []byte {
	t.Helper()
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(b.Bytes())
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }
