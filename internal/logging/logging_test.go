package logging_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/sarpaydemir/melinda/internal/logging"
)

func secretSet(values ...string) *logging.Secrets {
	s := new(logging.Secrets)
	s.Add(values...)
	return s
}

func jsonLogger(secrets ...string) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return logging.New(&buf, slog.LevelDebug, true, secretSet(secrets...)), &buf
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	return rec
}

func TestTextAndJSONFormats(t *testing.T) {
	var text, js bytes.Buffer
	logging.New(&text, slog.LevelInfo, false, nil).Info("started", logging.KeyComponent, "pipeline")
	logging.New(&js, slog.LevelInfo, true, nil).Info("started", logging.KeyComponent, "pipeline")

	if !strings.Contains(text.String(), `msg=started component=pipeline`) {
		t.Errorf("text output = %q", text.String())
	}
	rec := decode(t, &js)
	if rec["msg"] != "started" || rec["component"] != "pipeline" || rec["level"] != "INFO" {
		t.Errorf("JSON output = %v", rec)
	}
}

func TestLevelFiltersRecords(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, slog.LevelWarn, false, nil)
	log.Debug("debug")
	log.Info("info")
	log.Warn("warn")
	log.Error("error")
	out := buf.String()
	for _, msg := range []string{"msg=debug", "msg=info"} {
		if strings.Contains(out, msg) {
			t.Errorf("WARN logger emitted %s: %q", msg, out)
		}
	}
	for _, msg := range []string{"msg=warn", "msg=error"} {
		if !strings.Contains(out, msg) {
			t.Errorf("WARN logger dropped %s: %q", msg, out)
		}
	}
}

func TestSensitiveAttributeNamesAreMasked(t *testing.T) {
	for _, name := range []string{
		"token", "mattermost_token", "key", "api_key", "plane_api_key", "password", "DB_PASSWORD",
		"secret", "key_secret", "Authorization",
	} {
		log, buf := jsonLogger()
		log.Info("m", name, "s3cr3t")
		if got := decode(t, buf)[name]; got != "[REDACTED]" {
			t.Errorf("attribute %q = %v, want [REDACTED]", name, got)
		}
	}
}

func TestMaskingCoversGroupsAndLoggerAttributes(t *testing.T) {
	log, buf := jsonLogger()
	log.With("api_key", "s3cr3t").WithGroup("settings").Info("m",
		"key", "s3cr3t",
		slog.Group("plane", "plane_api_key", "s3cr3t", "workspace", "ws"),
	)
	out := buf.String()
	if strings.Contains(out, "s3cr3t") {
		t.Errorf("secret leaked: %s", out)
	}
	if !strings.Contains(out, `"workspace":"ws"`) {
		t.Errorf("non-secret attribute in group was masked: %s", out)
	}

	log, buf = jsonLogger()
	log.Info("m", slog.Group("secret", "first", "s3cr3t", "count", 2))
	if strings.Contains(buf.String(), "s3cr3t") {
		t.Errorf("value under a sensitive group name leaked: %s", buf.String())
	}
}

func TestIdentifierNamesThatMentionKeysAreNotMasked(t *testing.T) {
	for _, name := range []string{
		logging.KeyConversation, logging.KeyPostID, "last_seen_key", "buffer_key", "cache_key", "keyword",
		"max_tokens",
	} {
		log, buf := jsonLogger()
		log.Info("m", name, "chatbot:dispatch:buffer:c1")
		if got := decode(t, buf)[name]; got != "chatbot:dispatch:buffer:c1" {
			t.Errorf("attribute %q = %v, want it unmasked", name, got)
		}
	}
}

func TestNonTextValuesAndEmptyStringsAreKept(t *testing.T) {
	log, buf := jsonLogger()
	log.Info("m", "prompt_tokens", 120, "key_found", true, "token", "")
	rec := decode(t, buf)
	if rec["prompt_tokens"] != float64(120) || rec["key_found"] != true || rec["token"] != "" {
		t.Errorf("record = %v", rec)
	}
}

func TestSecretValuesAreMaskedEverywhere(t *testing.T) {
	log, buf := jsonLogger("hunter2", "", "admin:hunter2-long")
	log.Info("login as admin:hunter2-long failed with hunter2",
		"url", "postgres://u:hunter2@db/x",
		"err", errors.New("dial: password hunter2 rejected"),
		"detail", struct{ Pass string }{"hunter2"},
	)
	out := buf.String()
	if strings.Contains(out, "hunter2") {
		t.Fatalf("secret leaked: %s", out)
	}
	rec := decode(t, buf)
	if rec["msg"] != "login as [REDACTED] failed with [REDACTED]" {
		t.Errorf("msg = %q", rec["msg"])
	}
	if rec["url"] != "postgres://u:[REDACTED]@db/x" {
		t.Errorf("url = %q", rec["url"])
	}
	if rec["err"] != "dial: password [REDACTED] rejected" {
		t.Errorf("err = %q", rec["err"])
	}
}

type hiddenStringer struct {
	TokenValue string `json:"token_value"`
}

func (hiddenStringer) String() string { return "hidden" }

type textMarshaler struct{ secret string }

func (textMarshaler) String() string                 { return "marshaler" }
func (m textMarshaler) MarshalText() ([]byte, error) { return []byte(m.secret), nil }

func TestSecretValuesInRenderedFormsAreMasked(t *testing.T) {
	for _, tc := range []struct {
		name  string
		json  bool
		value any
	}{
		{"bytes in text", false, []byte("body hunter2 here")},
		{"bytes in JSON", true, []byte("body hunter2 here")},
		{"struct whose String hides a JSON field", true, hiddenStringer{TokenValue: "hunter2"}},
		{"text marshaler in text", false, textMarshaler{"hunter2"}},
		{"text marshaler in JSON", true, textMarshaler{"hunter2"}},
	} {
		var buf bytes.Buffer
		logging.New(&buf, slog.LevelInfo, tc.json, secretSet("hunter2")).Info("m", "v", tc.value)
		if strings.Contains(buf.String(), "hunter2") {
			t.Errorf("%s: secret leaked: %s", tc.name, buf.String())
		}
	}
}

func TestErrorsWithoutSecretsKeepTheirValue(t *testing.T) {
	log, buf := jsonLogger("hunter2")
	log.Info("m", "err", errors.New("boom"), "n", 3)
	rec := decode(t, buf)
	if rec["err"] != "boom" || rec["n"] != float64(3) {
		t.Errorf("record = %v", rec)
	}
}

func TestSecretsAddedAfterTheLoggerIsBuiltAreMasked(t *testing.T) {
	secrets := secretSet("db-pass")
	var buf bytes.Buffer
	log := logging.New(&buf, slog.LevelInfo, true, secrets)
	secrets.Add("sk-old", "")
	secrets.Add("sk-old", "sk-new")
	log.Info("rotated sk-old to sk-new", "err", errors.New("db-pass rejected"))
	rec := decode(t, &buf)
	if rec["msg"] != "rotated [REDACTED] to [REDACTED]" || rec["err"] != "[REDACTED] rejected" {
		t.Errorf("record = %v", rec)
	}
}

func TestSecretsShorterThanSixCharactersAreNotMasked(t *testing.T) {
	log, buf := jsonLogger("x", "none", "12345", "123456")
	log.Info("none of x 12345 123456")
	if got := decode(t, buf)["msg"]; got != "none of x 12345 [REDACTED]" {
		t.Errorf("msg = %q", got)
	}
}

func TestSecretsAreSafeToAddWhileLogging(t *testing.T) {
	secrets := new(logging.Secrets)
	log := logging.New(io.Discard, slog.LevelInfo, false, secrets)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			for j := range 50 {
				secrets.Add(fmt.Sprintf("secret%d-%d", i, j))
				log.Info("m", "v", fmt.Sprintf("secret%d-%d", i, j))
			}
		})
	}
	wg.Wait()
}

func TestClip(t *testing.T) {
	for _, tc := range []struct {
		in    string
		limit int
		want  string
	}{
		{"", 10, ""},
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"abcdefghijkl", 10, "abcde... [truncated 2 chars; showing head/tail] ...hijkl"},
		{"abcdefghijkl", 5, "ab... [truncated 8 chars; showing head/tail] ...kl"},
		{"abc", 1, "a... [truncated 1 chars; showing head/tail] ...c"},
		{"abc", 0, "a... [truncated 1 chars; showing head/tail] ...c"},
		{"çğıöşüÇĞİÖŞÜ", 10, "çğıöş... [truncated 2 chars; showing head/tail] ...ĞİÖŞÜ"},
		{"çğıöşü", 6, "çğıöşü"},
		{"a", -1, "a... [truncated -1 chars; showing head/tail] ...a"},
	} {
		if got := logging.Clip(tc.in, tc.limit); got != tc.want {
			t.Errorf("Clip(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
		}
	}
}
