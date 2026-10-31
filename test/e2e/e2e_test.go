//go:build e2e && linux

// Package e2e runs the dost binary named by DOST_E2E_BINARY against the chat and model fakes and a fresh
// database per test, and checks what only a real process shows: real-time debounce, restarts, reconnects,
// signals, and settings edits.
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/mattermostfake"
	"github.com/sarpaydemir/dost/internal/testkit/openaifake"
	"github.com/sarpaydemir/dost/internal/testkit/testdb"
)

// deadline bounds every wait; the race-enabled binary is slow and a model failure retries for seconds.
const deadline = 30 * time.Second

const shutdownTimeout = 20 * time.Second

// env is a test's world: the fakes, the database, and the settings file the agent processes share.
type env struct {
	t        *testing.T
	chat     *mattermostfake.Fake
	model    *openaifake.Fake
	db       *pgx.Conn
	dsn      string
	settings string // settings file path
	// settingsTTL is DOST_SETTINGS_TTL.
	settingsTTL string
}

// newEnv starts the fakes and creates the database. extra holds settings members merged into the document.
func newEnv(t *testing.T, extra map[string]any) *env {
	t.Helper()
	binary := os.Getenv("DOST_E2E_BINARY")
	if binary == "" {
		t.Skip("DOST_E2E_BINARY is not set")
	}
	e := &env{t: t, dsn: testdb.New(t), settings: filepath.Join(t.TempDir(), "settings.json"), settingsTTL: "1s"}
	e.chat = mattermostfake.New(t, "chat-token", mattermostfake.User{ID: id("bot"), Username: "agent"})
	e.chat.AddUser(mattermostfake.User{ID: id("u1"), Username: "ada", FirstName: "Ada"})
	e.chat.AddTeam(mattermostfake.Team{ID: id("t1"), Name: "team", DisplayName: "Team"})
	e.chat.AddChannel(
		mattermostfake.Channel{ID: id("dm1"), Type: "D", Name: id("bot") + "__" + id("u1"), Members: []string{id("bot"), id("u1")}},
		mattermostfake.Channel{ID: id("ch1"), TeamID: id("t1"), Type: "O", Name: "town", DisplayName: "Town",
			Members: []string{id("bot"), id("u1")}})
	e.model = openaifake.New(t)
	e.model.Chat().DefaultFunc(echo)
	e.writeSettings(extra)

	var err error
	if e.db, err = pgx.Connect(context.Background(), e.dsn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.db.Close(context.Background()) })
	return e
}

// writeSettings replaces the settings file atomically, as an editor saving it would.
func (e *env) writeSettings(extra map[string]any) {
	e.t.Helper()
	doc := map[string]any{
		"model": "chat-model", "api_base": e.model.URL(), "key": "model-key",
		"mattermost_endpoint": e.chat.URL(), "mattermost_token": "chat-token", "mattermost_bot_id": id("bot"),
		"enabled_tools": []string{},
	}
	for k, v := range extra {
		doc[k] = v
	}
	b, err := json.Marshal(doc)
	if err != nil {
		e.t.Fatal(err)
	}
	tmp := e.settings + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Rename(tmp, e.settings); err != nil {
		e.t.Fatal(err)
	}
}

// agent is one dost run process.
type agent struct {
	t    *testing.T
	cmd  *exec.Cmd
	logs *logs
	done chan struct{}
	err  error // the Wait result, set when done closes
}

// start runs dost and waits until its first recovery has finished, which follows connecting.
func (e *env) start() *agent {
	e.t.Helper()
	a := e.launch()
	a.await("startup recovery", func() bool { return strings.Contains(a.logs.String(), `msg="recovery finished"`) })
	return a
}

// launch runs dost without waiting for it to start.
func (e *env) launch() *agent {
	e.t.Helper()
	a := &agent{t: e.t, logs: &logs{}, done: make(chan struct{}), cmd: e.command()}
	a.cmd.Stdout, a.cmd.Stderr = a.logs, a.logs
	if err := a.cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	go func() { a.err = a.cmd.Wait(); close(a.done) }()
	e.t.Cleanup(func() {
		_ = a.cmd.Process.Kill()
		<-a.done
		if strings.Contains(a.logs.String(), "DATA RACE") {
			e.t.Errorf("the agent reported a data race:\n%s", a.logs)
		}
	})
	return a
}

// command returns an unstarted dost run against e.
func (e *env) command() *exec.Cmd {
	cmd := exec.Command(os.Getenv("DOST_E2E_BINARY"), "run")
	cmd.Env = []string{
		"DOST_DATABASE_URL=" + e.dsn, "DOST_SETTINGS_FILE=" + e.settings, "DOST_HTTP_ADDR=",
		"DOST_SETTINGS_TTL=" + e.settingsTTL, "DOST_SHUTDOWN_TIMEOUT=" + shutdownTimeout.String(),
	}
	// The agent dies with the test binary, even when go test kills it at its timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	return cmd
}

// stop sends SIGTERM and fails unless the agent exits 0 within the shutdown timeout.
func (a *agent) stop() {
	a.t.Helper()
	if err := a.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		a.t.Fatal(err)
	}
	select {
	case <-a.done:
	case <-time.After(shutdownTimeout + 5*time.Second):
		a.t.Fatalf("still running %v after SIGTERM; logs:\n%s", shutdownTimeout+5*time.Second, a.logs)
	}
	if a.err != nil {
		a.t.Fatalf("exit after SIGTERM: %v; logs:\n%s", a.err, a.logs)
	}
}

// kill stops the agent with SIGKILL, as a crash or OOM kill would.
func (a *agent) kill() {
	a.t.Helper()
	if err := a.cmd.Process.Kill(); err != nil {
		a.t.Fatal(err)
	}
	<-a.done
}

// await polls cond until it holds and fails the test at the deadline or when the agent exits first.
func (a *agent) await(what string, cond func() bool) {
	a.t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(deadline)
	for !cond() {
		select {
		case <-tick.C:
		case <-a.done:
			if !cond() {
				a.t.Fatalf("the agent exited (%v) while waiting for %s; logs:\n%s", a.err, what, a.logs)
			}
			return
		case <-timeout:
			a.t.Fatalf("timed out after %v waiting for %s; logs:\n%s", deadline, what, a.logs)
		}
	}
}

// awaitReplies waits until the agent has posted n replies and returns them.
func (a *agent) awaitReplies(e *env, n int) []reply {
	a.t.Helper()
	a.await("replies", func() bool { return len(e.replies()) >= n })
	return e.replies()
}

type reply struct {
	ChannelID string `json:"channel_id"`
	RootID    string `json:"root_id"`
	Message   string `json:"message"`
}

// replies returns the posts the agent created, in order.
func (e *env) replies() []reply {
	e.t.Helper()
	var out []reply
	for _, r := range e.chat.Requests() {
		if r.Call() != "POST /api/v4/posts" {
			continue
		}
		var p reply
		if err := json.Unmarshal(r.Body, &p); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// modelRequest is the part of a chat completion request the scenarios check.
type modelRequest struct {
	Model    string
	Messages []string // contents
}

func (e *env) modelRequests() []modelRequest {
	e.t.Helper()
	var out []modelRequest
	for _, r := range e.model.Requests() {
		if r.Call() != "POST /chat/completions" {
			continue
		}
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(r.Body, &body); err != nil {
			e.t.Fatal(err)
		}
		m := modelRequest{Model: body.Model}
		for _, msg := range body.Messages {
			m.Messages = append(m.Messages, msg.Content)
		}
		out = append(out, m)
	}
	return out
}

// last returns the content of the request's last message, the user's message.
func (m modelRequest) last() string { return m.Messages[len(m.Messages)-1] }

func (e *env) count(query string) int {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	if err := e.db.QueryRow(ctx, query).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) buffers() int {
	return e.count(`SELECT count(*) FROM key_value_store WHERE key LIKE 'dost:buffer:%'`)
}

func (e *env) inflight() int { return e.count(`SELECT count(*) FROM dost_inflight_runs`) }

// assertSettled fails unless no buffered message or unfinished run is left in the database.
func (e *env) assertSettled() {
	e.t.Helper()
	if n := e.buffers(); n != 0 {
		e.t.Errorf("buffers left = %d, want 0", n)
	}
	if n := e.inflight(); n != 0 {
		e.t.Errorf("in-flight runs left = %d, want 0", n)
	}
}

// post makes u1 post message in channel, under root when it is set, and pushes it live.
func (e *env) post(channel, root, message string) mattermostfake.Post {
	return e.chat.Posted(mattermostfake.Post{UserID: id("u1"), ChannelID: id(channel), RootID: id(root), Message: message})
}

// echo answers with the user's message, so each reply names the message it answers.
func echo(r testkit.Request) testkit.Response {
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(r.Body, &body)
	return openaifake.Text("Answer to " + body.Messages[len(body.Messages)-1].Content)
}

// id pads a short name to the 26 characters of a platform ID.
func id(short string) string {
	if short == "" {
		return ""
	}
	return short + strings.Repeat("0", 26-len(short))
}

// line is how the model sees one buffered message.
func line(user, message string) string { return id(user) + ": " + message }

// logs collects the agent's output.
type logs struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
