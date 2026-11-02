package config_test

import (
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/config"
)

func load(t *testing.T, environ ...string) config.Env {
	t.Helper()
	env, err := config.LoadEnv(environ)
	if err != nil {
		t.Fatalf("LoadEnv(%q): %v", environ, err)
	}
	return env
}

func loadErr(t *testing.T, environ ...string) string {
	t.Helper()
	_, err := config.LoadEnv(environ)
	if err == nil {
		t.Fatalf("LoadEnv(%q) succeeded, want an error", environ)
	}
	return err.Error()
}

func TestLoadEnvDefaults(t *testing.T) {
	env := load(t, "PATH=/usr/bin", "HOME=/root")
	want := config.Env{
		AgentID:            "dost",
		SettingsTTL:        5 * time.Second,
		DBHost:             "localhost",
		DBPort:             5432,
		Timezone:           time.UTC,
		LogLevel:           slog.LevelInfo,
		SearXNGURL:         "http://searxng:8080",
		HTTPAddr:           ":8080",
		ShutdownTimeout:    30 * time.Second,
		PipelineWorkers:    8,
		PipelineRunTimeout: 30 * time.Minute,
	}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("LoadEnv:\n got %+v\nwant %+v", env, want)
	}
}

func TestLoadEnvReadsEveryVariable(t *testing.T) {
	env := load(t,
		"MELINDA_AGENT_ID=sales_bot2",
		"MELINDA_SETTINGS_FILE=/etc/dost/settings.json",
		`MELINDA_SETTINGS_JSON={"model":"m"}`,
		"MELINDA_SETTINGS_TTL=1m",
		"MELINDA_PROMPTS_DIR=/prompts",
		"MELINDA_DATABASE_URL=postgres://u:p@db/x",
		"MELINDA_DB_HOST=db",
		"MELINDA_DB_PORT=6543",
		"MELINDA_DB_USER=app",
		"MELINDA_DB_PASSWORD=dbpw",
		"MELINDA_DB_NAME=data",
		"MELINDA_TIMEZONE=Europe/Istanbul",
		"MELINDA_LOG_LEVEL=debug",
		"MELINDA_LOG_FORMAT=json",
		"MELINDA_SEARXNG_URL=https://search.example.com",
		"MELINDA_HTTP_ADDR=127.0.0.1:9090",
		"MELINDA_COMPONENTS=gateway, dispatcher,,flusher ",
		"MELINDA_SHUTDOWN_TIMEOUT=45",
		"MELINDA_PIPELINE_WORKERS=2",
		"MELINDA_PIPELINE_RUN_TIMEOUT=10m",
		"MELINDA_DRY_RUN=true",
		"MELINDA_DRY_RUN_LOG=/var/log/dost/shadow.jsonl",
	)
	want := config.Env{
		AgentID:            "sales_bot2",
		SettingsFile:       "/etc/dost/settings.json",
		SettingsJSON:       `{"model":"m"}`,
		SettingsTTL:        time.Minute,
		PromptsDir:         "/prompts",
		DatabaseURL:        "postgres://u:p@db/x",
		DBHost:             "db",
		DBPort:             6543,
		DBUser:             "app",
		DBPassword:         "dbpw",
		DBName:             "data",
		LogLevel:           slog.LevelDebug,
		LogJSON:            true,
		SearXNGURL:         "https://search.example.com",
		HTTPAddr:           "127.0.0.1:9090",
		Components:         []string{"gateway", "dispatcher", "flusher"},
		ShutdownTimeout:    45 * time.Second,
		PipelineWorkers:    2,
		PipelineRunTimeout: 10 * time.Minute,
		DryRun:             true,
		DryRunLog:          "/var/log/dost/shadow.jsonl",
	}
	if env.Timezone == nil || env.Timezone.String() != "Europe/Istanbul" {
		t.Errorf("Timezone = %v, want Europe/Istanbul", env.Timezone)
	}
	env.Timezone = nil
	if !reflect.DeepEqual(env, want) {
		t.Errorf("LoadEnv:\n got %+v\nwant %+v", env, want)
	}
}

func TestDurations(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"30s", 30 * time.Second},
		{"1h30m", 90 * time.Minute},
		{"250ms", 250 * time.Millisecond},
		{"45", 45 * time.Second},
		{"1.5", 1500 * time.Millisecond},
		{"0", 0},
	} {
		env := load(t, "MELINDA_SETTINGS_TTL="+tc.value)
		if env.SettingsTTL != tc.want {
			t.Errorf("MELINDA_SETTINGS_TTL=%q: got %v, want %v", tc.value, env.SettingsTTL, tc.want)
		}
	}
	for _, bad := range []string{"", "soon", "5 s", "-5s", "-5", "1e3", "inf", "NaN", "99999999999999999999"} {
		if msg := loadErr(t, "MELINDA_SETTINGS_TTL="+bad); !strings.Contains(msg, "MELINDA_SETTINGS_TTL") {
			t.Errorf("MELINDA_SETTINGS_TTL=%q: error %q does not name the variable", bad, msg)
		}
	}
}

func TestTimeoutsMustBePositive(t *testing.T) {
	for _, name := range []string{"MELINDA_SHUTDOWN_TIMEOUT", "MELINDA_PIPELINE_RUN_TIMEOUT"} {
		if msg := loadErr(t, name+"=0"); !strings.Contains(msg, name) {
			t.Errorf("%s=0: error %q does not name the variable", name, msg)
		}
	}
}

func TestBooleans(t *testing.T) {
	for value, want := range map[string]bool{"true": true, "1": true, "false": false, "0": false} {
		if got := load(t, "MELINDA_DRY_RUN="+value).DryRun; got != want {
			t.Errorf("MELINDA_DRY_RUN=%q: got %v, want %v", value, got, want)
		}
	}
	for _, bad := range []string{"", "yes", "on", "2", "TRUE", "True"} {
		if msg := loadErr(t, "MELINDA_DRY_RUN="+bad); !strings.Contains(msg, "MELINDA_DRY_RUN") {
			t.Errorf("MELINDA_DRY_RUN=%q: error %q does not name the variable", bad, msg)
		}
	}
}

func TestLogLevels(t *testing.T) {
	for value, want := range map[string]slog.Level{
		"DEBUG": slog.LevelDebug, "debug": slog.LevelDebug, "Info": slog.LevelInfo,
		"WARNING": slog.LevelWarn, "warn": slog.LevelWarn, "ERROR": slog.LevelError,
	} {
		if got := load(t, "MELINDA_LOG_LEVEL="+value).LogLevel; got != want {
			t.Errorf("MELINDA_LOG_LEVEL=%q: got %v, want %v", value, got, want)
		}
	}
	for _, bad := range []string{"", "verbose", "TRACE", " INFO"} {
		if msg := loadErr(t, "MELINDA_LOG_LEVEL="+bad); !strings.Contains(msg, "MELINDA_LOG_LEVEL") {
			t.Errorf("MELINDA_LOG_LEVEL=%q: error %q does not name the variable", bad, msg)
		}
	}
}

func TestLogFormat(t *testing.T) {
	if load(t, "MELINDA_LOG_FORMAT=text").LogJSON {
		t.Error("text format selected JSON")
	}
	if !load(t, "MELINDA_LOG_FORMAT=json").LogJSON {
		t.Error("json format selected text")
	}
	if msg := loadErr(t, "MELINDA_LOG_FORMAT=logfmt"); !strings.Contains(msg, "MELINDA_LOG_FORMAT") {
		t.Errorf("error %q does not name the variable", msg)
	}
}

func TestAgentID(t *testing.T) {
	for _, ok := range []string{"a", "dost", "team_bot_2", "a" + strings.Repeat("b", 31)} {
		if got := load(t, "MELINDA_AGENT_ID="+ok).AgentID; got != ok {
			t.Errorf("MELINDA_AGENT_ID=%q: got %q", ok, got)
		}
	}
	for _, bad := range []string{"", "Dost", "2bot", "_bot", "bot-1", "a" + strings.Repeat("b", 32)} {
		if msg := loadErr(t, "MELINDA_AGENT_ID="+bad); !strings.Contains(msg, "MELINDA_AGENT_ID") {
			t.Errorf("MELINDA_AGENT_ID=%q: error %q does not name the variable", bad, msg)
		}
	}
}

func TestTimezoneIANA(t *testing.T) {
	loc := load(t, "MELINDA_TIMEZONE=America/New_York").Timezone
	winter := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC).In(loc)
	summer := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC).In(loc)
	if _, off := winter.Zone(); off != -5*3600 {
		t.Errorf("winter offset = %d, want -5h", off)
	}
	if _, off := summer.Zone(); off != -4*3600 {
		t.Errorf("summer offset = %d, want -4h (DST)", off)
	}
}

func TestTimezoneFixedOffset(t *testing.T) {
	for value, want := range map[string]int{
		"+03:00": 3 * 3600, "-05:30": -(5*3600 + 30*60), "+00:00": 0, "+14:00": 14 * 3600, "-12:00": -12 * 3600,
	} {
		loc := load(t, "MELINDA_TIMEZONE="+value).Timezone
		for _, month := range []time.Month{time.January, time.July} {
			name, off := time.Date(2026, month, 1, 0, 0, 0, 0, time.UTC).In(loc).Zone()
			if off != want {
				t.Errorf("MELINDA_TIMEZONE=%q in %v: offset %d, want %d", value, month, off, want)
			}
			if name != value {
				t.Errorf("MELINDA_TIMEZONE=%q: zone name %q, want the offset", value, name)
			}
		}
	}
	for _, bad := range []string{"", "+3:00", "+0300", "03:00", "+15:00", "+03:60", "-12:30", "Mars/Olympus", "UTC+3", "Local"} {
		if msg := loadErr(t, "MELINDA_TIMEZONE="+bad); !strings.Contains(msg, "MELINDA_TIMEZONE") {
			t.Errorf("MELINDA_TIMEZONE=%q: error %q does not name the variable", bad, msg)
		}
	}
}

func TestIntegers(t *testing.T) {
	for _, bad := range []string{"", "0", "-1", "eight", "8.5"} {
		if msg := loadErr(t, "MELINDA_PIPELINE_WORKERS="+bad); !strings.Contains(msg, "MELINDA_PIPELINE_WORKERS") {
			t.Errorf("MELINDA_PIPELINE_WORKERS=%q: error %q does not name the variable", bad, msg)
		}
	}
	for _, bad := range []string{"", "0", "65536", "pg"} {
		if msg := loadErr(t, "MELINDA_DB_PORT="+bad); !strings.Contains(msg, "MELINDA_DB_PORT") {
			t.Errorf("MELINDA_DB_PORT=%q: error %q does not name the variable", bad, msg)
		}
	}
}

func TestURLs(t *testing.T) {
	for _, bad := range []string{"searxng:8080", "ftp://host", "http://", "/relative"} {
		if msg := loadErr(t, "MELINDA_SEARXNG_URL="+bad); !strings.Contains(msg, "MELINDA_SEARXNG_URL") {
			t.Errorf("MELINDA_SEARXNG_URL=%q: error %q does not name the variable", bad, msg)
		}
	}
	msg := loadErr(t, "MELINDA_SEARXNG_URL=search.example.com/user:hunter2")
	if strings.Contains(msg, "hunter2") {
		t.Errorf("URL error %q echoes the value", msg)
	}
}

func TestHTTPAddr(t *testing.T) {
	if got := load(t, "MELINDA_HTTP_ADDR=").HTTPAddr; got != "" {
		t.Errorf("empty MELINDA_HTTP_ADDR = %q, want empty (disabled)", got)
	}
	if got := load(t, "MELINDA_HTTP_ADDR=:9000").HTTPAddr; got != ":9000" {
		t.Errorf("HTTPAddr = %q, want :9000", got)
	}
	if msg := loadErr(t, "MELINDA_HTTP_ADDR=8080"); !strings.Contains(msg, "MELINDA_HTTP_ADDR") {
		t.Errorf("error %q does not name the variable", msg)
	}
}

func TestEmptyComponentsMeansAll(t *testing.T) {
	for _, value := range []string{"", " , "} {
		if got := load(t, "MELINDA_COMPONENTS="+value).Components; got != nil {
			t.Errorf("MELINDA_COMPONENTS=%q: got %q, want nil", value, got)
		}
	}
}

func TestEveryErrorIsReportedTogether(t *testing.T) {
	msg := loadErr(t,
		"MELINDA_LOG_LEVEL=loud",
		"MELINDA_DRY_RUN=maybe",
		"MELINDA_TIMEZONE=Nowhere/Land",
		"MELINDA_PIPELINE_WORKERS=0",
	)
	if strings.Count(msg, "\n") != 0 {
		t.Errorf("error spans several lines: %q", msg)
	}
	last := -1
	for _, name := range []string{"MELINDA_TIMEZONE", "MELINDA_LOG_LEVEL", "MELINDA_PIPELINE_WORKERS", "MELINDA_DRY_RUN"} {
		i := strings.Index(msg, name)
		if i < 0 {
			t.Errorf("error %q does not mention %s", msg, name)
			continue
		}
		if i < last {
			t.Errorf("error %q does not list %s in declaration order", msg, name)
		}
		last = i
	}
}

func TestUnknownVariablesAreListed(t *testing.T) {
	env := load(t, "MELINDA_LOG_LEVL=DEBUG", "MELINDA_AGENT_ID=dost", "MELINDA_DRYRUN=1", "OTHER_VAR=1", "MELINDA=1")
	if want := []string{"MELINDA_DRYRUN", "MELINDA_LOG_LEVL"}; !slices.Equal(env.Unknown, want) {
		t.Errorf("Unknown = %q, want %q", env.Unknown, want)
	}
}

func TestFirstOccurrenceWins(t *testing.T) {
	if got := load(t, "MELINDA_AGENT_ID=first", "MELINDA_AGENT_ID=second").AgentID; got != "first" {
		t.Errorf("AgentID = %q, want first", got)
	}
}

func TestSecrets(t *testing.T) {
	env := load(t,
		"MELINDA_DB_PASSWORD=dbpw",
		"MELINDA_DATABASE_URL=postgres://app:url%40pw@db:5432/data",
	)
	got := env.Secrets()
	for _, want := range []string{"dbpw", "url@pw"} {
		if !slices.Contains(got, want) {
			t.Errorf("Secrets() = %q, missing %q", got, want)
		}
	}
	if slices.Contains(got, "") {
		t.Errorf("Secrets() = %q, contains an empty value", got)
	}
	if got := load(t, "MELINDA_DATABASE_URL=postgres://db/data").Secrets(); len(got) != 0 {
		t.Errorf("Secrets() without secrets = %q, want none", got)
	}
}
