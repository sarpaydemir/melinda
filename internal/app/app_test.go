package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/logging"
	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/port/web"
	"github.com/sarpaydemir/melinda/internal/prompt"
)

// accepts is a constructor that accepts any settings.
func accepts[T any](*config.Settings) (T, error) {
	var zero T
	return zero, nil
}

// workingTable registers one adapter per port, each accepting any settings, and a tool source whose one catalog
// entry is notes.saved.
func workingTable() adapters {
	return adapters{
		platforms: map[string]platformAdapter{"chatty": {open: accepts[chat.Platform], secrets: []config.SecretKey{{Key: "chatty_token"}}}},
		models:    map[string]entry[model.Provider]{"compatible": {open: accepts[model.Provider]}},
		searchers: map[string]entry[web.Searcher]{"meta": {open: accepts[web.Searcher]}},
		fetchers:  map[string]entry[web.Fetcher]{"direct": {open: accepts[web.Fetcher]}},
		tools: []toolSource{{prompts: []prompt.Entry{
			{ID: "notes.saved", Kind: prompt.KindFormat, Placeholders: []string{"title"}, Default: "Saved {title}."},
		}}},
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

// profile writes a prompt profile directory holding files, by name, and returns its path.
func profile(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
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
		"MELINDA_AGENT_ID=agent",
		`MELINDA_SETTINGS_JSON={"model":"m",`+selectsWorkingTable+`,"chatty_token":"token-value"}`,
		"MELINDA_DATABASE_URL=postgres://dost:db-password@db/dost",
		"MELINDA_COMPONENTS=pipeline,kb-learner",
		"MELINDA_LOG_LEVL=debug",
		"MELINDA_DRYRUN=true",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := `agent: agent
components: kb-learner, pipeline
settings (MELINDA_SETTINGS_JSON):
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
	for _, name := range []string{"MELINDA_DRYRUN", "MELINDA_LOG_LEVL"} {
		if n := strings.Count(logs, `msg="unknown environment variable" name=`+name); n != 1 {
			t.Errorf("%d warnings for %s, want 1; logs:\n%s", n, name, logs)
		}
	}
}

func TestCheckReportsEveryProblemAtOnce(t *testing.T) {
	prompts := profile(t, map[string]string{"notes.saved.txt": "Saved\n {titel}."})
	_, _, err := checkWith(t, workingTable(),
		"MELINDA_COMPONENTS=gateway",
		"MELINDA_PROMPTS_DIR="+prompts,
		`MELINDA_SETTINGS_JSON={"chat_platform":"pigeon","prompts":{"zzz":"x"}}`,
	)
	want := `MELINDA_COMPONENTS: gateway needs dispatcher
MELINDA_COMPONENTS: dispatcher needs flusher
MELINDA_COMPONENTS: flusher needs pipeline
MELINDA_DB_USER and MELINDA_DB_NAME required when MELINDA_DATABASE_URL is not set
invalid prompts:
` + prompts + `/notes.saved.txt: prompt: line 2, column 2 (offset 7): unknown placeholder "{titel}"; write {{ and }} for literal braces
invalid settings:
core: model is required
chat_platform: unknown adapter "pigeon" (available: chatty)
model_provider: unknown adapter "openai" (available: compatible)
web_search: unknown adapter "searxng" (available: meta)
web_fetch: unknown adapter "httpfetch" (available: direct)
invalid prompts:
prompts["zzz"]: unknown ID`
	if err == nil || err.Error() != want {
		t.Errorf("check = %v, want:\n%s", err, want)
	}
}

func TestCheckReportsInvalidPromptOverrides(t *testing.T) {
	_, _, err := checkWith(t, workingTable(),
		"MELINDA_DATABASE_URL=postgres://dost@db/dost",
		`MELINDA_SETTINGS_JSON={"model":"m",`+selectsWorkingTable+`,"prompts":{"notes.saved":"Saved\n{title","nope":"text"}}`,
	)
	want := `invalid settings:
invalid prompts:
prompts["nope"]: unknown ID
prompts["notes.saved"]: prompt: line 2, column 1 (offset 6): unmatched '{'; write {{ and }} for literal braces`
	if err == nil || err.Error() != want {
		t.Errorf("check = %v, want:\n%s", err, want)
	}
}

func TestExportWritesTheEffectiveCatalog(t *testing.T) {
	table := workingTable()
	table.platforms["quiet"] = platformAdapter{open: accepts[chat.Platform], prompts: []prompt.Entry{
		{ID: "quiet.hello", Kind: prompt.KindRaw, Default: "Hello."},
	}}
	prompts := profile(t, map[string]string{"notes.saved.txt": "Profile {title}."})
	files := []string{"notes.saved.txt", "quiet.hello.txt"}
	var generic string
	for _, e := range prompt.Builtin() {
		files = append(files, e.ID+".txt")
		if e.ID == "error.generic" {
			generic = e.Default
		}
	}
	slices.Sort(files)

	for _, tc := range []struct {
		name     string
		settings string
		want     map[string]string
	}{
		{
			name: "without a settings source",
			want: map[string]string{"notes.saved": "Profile {title}.", "quiet.hello": "Hello.", "error.generic": generic},
		},
		{
			name:     "with settings",
			settings: `MELINDA_SETTINGS_JSON={"model":"m",` + selectsWorkingTable + `,"prompts":{"quiet.hello":"Hi.","error.generic":"Sorry."}}`,
			want:     map[string]string{"notes.saved": "Profile {title}.", "quiet.hello": "Hi.", "error.generic": "Sorry."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "agent")
			secrets, log, _ := testLogger()
			env := environment(t, "MELINDA_PROMPTS_DIR="+prompts, tc.settings)
			if err := exportPrompts(context.Background(), env, out, false, table, clock.Real(), secrets, log); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(out)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, e := range entries {
				got = append(got, e.Name())
			}
			if !slices.Equal(got, files) {
				t.Errorf("exported %v, want %v", got, files)
			}
			for id, text := range tc.want {
				if b, err := os.ReadFile(filepath.Join(out, id+".txt")); err != nil || string(b) != text {
					t.Errorf("%s.txt: %q, %v; want %q", id, b, err, text)
				}
			}
		})
	}
}

// TestCompatProfileRendersItsFixtures loads the private profile in MELINDA_COMPAT_PROFILE with the built-in and
// adapter entries and renders each fixture in its renders/*.json ({"id", "values", "expected"}) byte for byte.
func TestCompatProfileRendersItsFixtures(t *testing.T) {
	dir := os.Getenv("MELINDA_COMPAT_PROFILE")
	if dir == "" {
		t.Skip("MELINDA_COMPAT_PROFILE is not set")
	}
	c, err := prompt.Load(builtin(http.DefaultTransport, clock.Real(), "melinda/test", "", slog.New(slog.DiscardHandler)).prompts(), filepath.Join(dir, "prompts"))
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := filepath.Glob(filepath.Join(dir, "renders", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Skip("the profile has no render fixtures")
	}
	for _, path := range fixtures {
		var f struct {
			ID       string            `json:"id"`
			Values   map[string]string `json:"values"`
			Expected string            `json:"expected"`
		}
		if b, err := os.ReadFile(path); err != nil || json.Unmarshal(b, &f) != nil {
			t.Errorf("%s: not a render fixture (%v)", path, err)
			continue
		}
		if got, err := c.Template(f.ID).Render(f.Values); err != nil || got != f.Expected {
			t.Errorf("%s: %s renders\n%q, %v\nwant\n%q", path, f.ID, got, err, f.Expected)
		}
	}
}
