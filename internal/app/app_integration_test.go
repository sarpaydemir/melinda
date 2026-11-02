//go:build integration

package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sarpaydemir/melinda/internal/adapters/mattermost"
	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/logging"
	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/store/pg"
	"github.com/sarpaydemir/melinda/internal/testkit/mattermostfake"
	"github.com/sarpaydemir/melinda/internal/testkit/testdb"
)

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
	chatServer := mattermostfake.New(t, "chat-token", mattermostfake.User{ID: "agent-id", Username: "agent"})
	table := workingTable()
	table.platforms = map[string]platformAdapter{"mattermost": {
		open: func(s *config.Settings) (chat.Platform, error) {
			return mattermost.Open(s, http.DefaultTransport, clock.Real(), slog.New(slog.DiscardHandler))
		},
	}}
	addr := freeAddr(t)
	env := environment(t, "DOST_DATABASE_URL="+testdb.New(t), "DOST_HTTP_ADDR="+addr,
		`DOST_SETTINGS_JSON={"model":"m","model_provider":"compatible","web_search":"meta","web_fetch":"direct",`+
			`"mattermost_endpoint":"`+chatServer.URL()+`","mattermost_token":"chat-token","mattermost_bot_id":"agent-id"}`)
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
