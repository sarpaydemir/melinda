package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/logging"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/port/web"
)

// accepts is a constructor that accepts any settings.
func accepts[T any](*config.Settings) (T, error) {
	var zero T
	return zero, nil
}

// workingTable registers one adapter per port, each accepting any settings.
func workingTable() adapters {
	return adapters{
		platforms: map[string]platformAdapter{"chatty": {open: accepts[chat.Platform], secrets: []config.SecretKey{{Key: "chatty_token"}}}},
		models:    map[string]entry[model.Provider]{"compatible": {open: accepts[model.Provider]}},
		searchers: map[string]entry[web.Searcher]{"meta": {open: accepts[web.Searcher]}},
		fetchers:  map[string]entry[web.Fetcher]{"direct": {open: accepts[web.Fetcher]}},
	}
}

const selectsWorkingTable = `"chat_platform":"chatty","model_provider":"compatible","web_search":"meta","web_fetch":"direct"`

func environment(t *testing.T, environ ...string) config.Env {
	t.Helper()
	env, err := config.LoadEnv(environ)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func testLogger() (*logging.Secrets, *slog.Logger, *bytes.Buffer) {
	logs := new(bytes.Buffer)
	secrets := new(logging.Secrets)
	return secrets, logging.New(logs, slog.LevelInfo, false, secrets), logs
}

func checkWith(t *testing.T, table adapters, environ ...string) (string, string, error) {
	t.Helper()
	secrets, log, logs := testLogger()
	summary, err := check(context.Background(), environment(t, environ...), false, table, clock.Real(), secrets, log)
	return summary, logs.String(), err
}

func TestCheckSummarizesTheConfigurationWithSecretsMasked(t *testing.T) {
	summary, logs, err := checkWith(t, workingTable(),
		"DOST_AGENT_ID=agent",
		`DOST_SETTINGS_JSON={"model":"m",`+selectsWorkingTable+`,"chatty_token":"token-value"}`,
		"DOST_DATABASE_URL=postgres://dost:db-password@db/dost",
		"DOST_COMPONENTS=pipeline,kb-learner",
		"DOST_LOG_LEVL=debug",
		"DOST_DRYRUN=true",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := `agent: agent
components: kb-learner, pipeline
settings (DOST_SETTINGS_JSON):
{
  "chat_platform": "chatty",
  "chatty_token": "***",
  "model": "m",
  "model_provider": "compatible",
  "web_fetch": "direct",
  "web_search": "meta"
}
`
	if summary != want {
		t.Errorf("summary:\n%s\nwant:\n%s", summary, want)
	}
	for _, name := range []string{"DOST_DRYRUN", "DOST_LOG_LEVL"} {
		if n := strings.Count(logs, `msg="unknown environment variable" name=`+name); n != 1 {
			t.Errorf("%d warnings for %s, want 1; logs:\n%s", n, name, logs)
		}
	}
}

func TestCheckReportsEveryProblemAtOnce(t *testing.T) {
	_, _, err := checkWith(t, workingTable(),
		"DOST_COMPONENTS=gateway",
		`DOST_SETTINGS_JSON={"chat_platform":"pigeon"}`,
	)
	want := `DOST_COMPONENTS: gateway needs dispatcher
DOST_COMPONENTS: dispatcher needs flusher
DOST_COMPONENTS: flusher needs pipeline
DOST_DB_USER and DOST_DB_NAME required when DOST_DATABASE_URL is not set
invalid settings:
core: model is required
chat_platform: unknown adapter "pigeon" (available: chatty)
model_provider: unknown adapter "openai" (available: compatible)
web_search: unknown adapter "searxng" (available: meta)
web_fetch: unknown adapter "httpfetch" (available: direct)`
	if err == nil || err.Error() != want {
		t.Errorf("check = %v, want:\n%s", err, want)
	}
}
