//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/adapters/mattermost"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/logging"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/store/history"
	"github.com/sarpaydemir/dost/internal/store/pg"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/mattermostfake"
	"github.com/sarpaydemir/dost/internal/testkit/openaifake"
	"github.com/sarpaydemir/dost/internal/testkit/testdb"
)

func TestPostedDirectMessageIsAnsweredAtTheRootAndItsTurnStored(t *testing.T) {
	dsn := testdb.New(t)
	chatServer := mattermostfake.New(t, "chat-token", mattermostfake.User{ID: "bot-id", Username: "agent"})
	chatServer.AddUser(mattermostfake.User{ID: "u1", Username: "ada", FirstName: "Ada"})
	chatServer.AddChannel(mattermostfake.Channel{ID: "dm1", Type: "D", Name: "bot-id__u1", Members: []string{"bot-id", "u1"}})
	modelServer := openaifake.New(t)
	modelServer.Chat().Reply(openaifake.Text("Hi Ada!"))
	env := environment(t, "DOST_DATABASE_URL="+dsn, "DOST_HTTP_ADDR=",
		`DOST_SETTINGS_JSON={"model":"chat-model","api_base":"`+modelServer.URL()+`","key":"model-key",`+
			`"mattermost_endpoint":"`+chatServer.URL()+`","mattermost_token":"chat-token","mattermost_bot_id":"bot-id",`+
			`"debounce_seconds":0,"memory_key_prefix":"chatbot"}`)
	logs := &logWatch{match: `msg="run finished"`, seen: make(chan struct{})}
	secrets := new(logging.Secrets)
	log := logging.New(logs, slog.LevelDebug, false, secrets)
	table := builtin(http.DefaultTransport, clock.Real(), "dost/test", "http://searxng.invalid", log)

	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx, env, "test", table, clock.Real(), secrets, log) }()
	chatServer.AwaitWebsocket(t, func() bool { return chatServer.Connections() > 0 })
	posted := chatServer.Posted(mattermostfake.Post{UserID: "u1", ChannelID: "dm1", Message: "hello"})
	select {
	case <-logs.seen:
	case err := <-done:
		t.Fatalf("Run returned %v before answering; logs:\n%s", err, logs)
	case <-time.After(testkit.AwaitTimeout):
		t.Fatalf("no run finished; logs:\n%s", logs)
	}
	stop()
	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil", err)
	}

	var replies []string
	for _, r := range chatServer.Requests() {
		if r.Call() == "POST /api/v4/posts" {
			replies = append(replies, string(r.Body))
		}
	}
	if len(replies) != 1 {
		t.Fatalf("replies posted = %q, want one", replies)
	}
	if ok, err := testkit.JSONEqual([]byte(replies[0]), []byte(`{"channel_id":"dm1","message":"Hi Ada!"}`)); !ok || err != nil {
		t.Errorf("reply body = %s, want it at the conversation root", replies[0])
	}
	asked := string(modelServer.Requests()[0].Body)
	if !strings.Contains(asked, `"model":"chat-model"`) || !strings.Contains(asked, `{"role":"user","content":"u1: hello"}`) {
		t.Errorf("model request = %s, want chat-model asked about the combined text", asked)
	}

	db, err := pg.Open(t.Context(), dsn, clock.Real(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	items, err := history.New(db, clock.Real()).Get(t.Context(), "chatbot:u1")
	if err != nil || len(items) != 1 {
		t.Fatalf("history chatbot:u1 = %s, %v; want one turn", items, err)
	}
	var turn map[string]any
	if err := json.Unmarshal(items[0], &turn); err != nil {
		t.Fatal(err)
	}
	if ts, _ := turn["ts"].(string); !tsPattern.MatchString(ts) {
		t.Errorf("turn ts = %q, want an ISO time in UTC with microseconds", ts)
	}
	delete(turn, "ts")
	got, _ := json.Marshal(turn)
	want := `{"scope":"private","context_id":"` + posted.ID + `","user_id":"u1","is_dm":true,"user":"u1: hello","assistant":"Hi Ada!","message_count":1}`
	if ok, err := testkit.JSONEqual(got, []byte(want)); !ok || err != nil {
		t.Errorf("stored turn = %s, want %s", got, want)
	}
	rows, err := db.Query(t.Context(), "SELECT count(*) FROM dost_inflight_runs")
	if err != nil {
		t.Fatal(err)
	}
	var left int
	for rows.Next() {
		if err := rows.Scan(&left); err != nil {
			t.Fatal(err)
		}
	}
	if left != 0 {
		t.Errorf("in-flight records left after the reply = %d, want 0", left)
	}
}

var tsPattern = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}\+00:00$`)

// logWatch collects log lines written concurrently and closes seen when a line contains match.
type logWatch struct {
	match string
	seen  chan struct{}

	mu   sync.Mutex
	logs strings.Builder
}

func (w *logWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !strings.Contains(w.logs.String(), w.match) && strings.Contains(string(p), w.match) {
		close(w.seen)
	}
	return w.logs.Write(p)
}

func (w *logWatch) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.logs.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestRunPreparesTheDatabaseVerifiesTheAgentAndServesReadiness(t *testing.T) {
	chatServer := mattermostfake.New(t, "chat-token", mattermostfake.User{ID: "reported-id", Username: "agent"})
	table := workingTable()
	table.platforms = map[string]platformAdapter{"mattermost": {
		open: func(s *config.Settings) (chat.Platform, error) {
			return mattermost.Open(s, http.DefaultTransport, clock.Real(), slog.New(slog.DiscardHandler))
		},
	}}
	addr := freeAddr(t)
	env := environment(t, "DOST_DATABASE_URL="+testdb.New(t), "DOST_HTTP_ADDR="+addr,
		`DOST_SETTINGS_JSON={"model":"m","model_provider":"compatible","web_search":"meta","web_fetch":"direct",`+
			`"mattermost_endpoint":"`+chatServer.URL()+`","mattermost_token":"chat-token","mattermost_bot_id":"configured-id"}`)
	logs := &logWatch{match: `msg=connected component=gateway`, seen: make(chan struct{})}
	secrets := new(logging.Secrets)
	log := logging.New(logs, slog.LevelInfo, false, secrets)

	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx, env, "1.2.3", table, clock.Real(), secrets, log) }()
	select {
	case <-logs.seen:
	case err := <-done:
		t.Fatalf("Run returned %v before serving; logs:\n%s", err, logs)
	}

	resp, err := http.Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"ready":true,"failing":[]}` {
		t.Errorf("GET /readyz = %d %s", resp.StatusCode, body)
	}
	if calls := chatServer.Calls(); len(calls) == 0 || calls[0] != "GET /api/v4/users/me" {
		t.Errorf("first requests %q, want the account check first", calls)
	}
	for _, want := range []string{
		`msg=starting version=1.2.3 agent=dost settings=DOST_SETTINGS_JSON components="[kb-learner pipeline flusher dispatcher recovery gateway]"`,
		`msg="the platform reports a different ID for the agent's account" configured=configured-id reported=reported-id`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs do not contain %s:\n%s", want, logs)
		}
	}

	stop()
	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
}

func TestCheckWithTheDatabaseVerifiesTheSchemaWithoutChangingIt(t *testing.T) {
	secrets, log, _ := testLogger()
	prepared := testdb.New(t)
	db, err := pg.Open(t.Context(), prepared, clock.Real(), log)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	for _, tc := range []struct {
		dsn, want string
	}{
		{prepared, "\ndatabase: schema verified\n"},
		{testdb.New(t), "database schema does not match: chat_sessions is missing; "},
	} {
		env := environment(t, "DOST_DATABASE_URL="+tc.dsn, `DOST_SETTINGS_JSON={"model":"m",`+selectsWorkingTable+`}`)
		summary, err := check(t.Context(), env, true, workingTable(), clock.Real(), secrets, log)
		if got := fmt.Sprint(summary, err); !strings.Contains(got, tc.want) {
			t.Errorf("check = %q, want it to contain %q", got, tc.want)
		}
	}
}
