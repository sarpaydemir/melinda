package config_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/logging"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/fakeclock"
	"github.com/sarpaydemir/dost/internal/testkit/prefectfake"
)

type harness struct {
	clock *fakeclock.Clock
	log   *slog.Logger
	logs  *bytes.Buffer
}

func open(t *testing.T, env config.Env, validate func(*config.Settings) error) (*config.Provider, *harness, error) {
	t.Helper()
	return openWith(t, env, secretKeys, validate)
}

func openWith(t *testing.T, env config.Env, keys []config.SecretKey, validate func(*config.Settings) error) (*config.Provider, *harness, error) {
	t.Helper()
	mask := new(logging.Secrets)
	h := &harness{clock: fakeclock.New(time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)), logs: new(bytes.Buffer)}
	h.log = logging.New(h.logs, slog.LevelDebug, false, mask)
	p, err := config.NewProvider(context.Background(), env, keys, validate, mask, h.clock, h.log)
	return p, h, err
}

func mustOpen(t *testing.T, env config.Env) (*config.Provider, *harness) {
	t.Helper()
	p, h, err := open(t, env, noAdapters)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p, h
}

func openErr(t *testing.T, env config.Env) string {
	t.Helper()
	p, _, err := open(t, env, noAdapters)
	if err == nil {
		t.Fatalf("NewProvider succeeded with %s, want an error", p.Current(context.Background()).Redacted())
	}
	return err.Error()
}

// tick moves past the settings TTL and reads the settings.
func (h *harness) tick(p *config.Provider) *config.Settings {
	h.clock.Advance(5 * time.Second)
	return p.Current(context.Background())
}

func (h *harness) count(s string) int { return strings.Count(h.logs.String(), s) }

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// settingsFile returns the path of a new file holding content.
func settingsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	writeFile(t, path, content)
	return path
}

type secretValues struct {
	Key             string `json:"key"`
	MattermostToken string `json:"mattermost_token"`
	PlaneAPIKey     string `json:"plane_api_key"`
}

func secretsOf(t *testing.T, s *config.Settings) secretValues {
	t.Helper()
	var v secretValues
	if err := s.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestFirstConfiguredSourceWins(t *testing.T) {
	prefect := prefectfake.New(t)
	prefect.SetVariable("chatbot_settings", map[string]any{"model": "from-prefect"})
	file := settingsFile(t, `{"model": "from-file"}`)
	for _, tc := range []struct {
		environ []string
		want    string
	}{
		{[]string{"DOST_SETTINGS_FILE=" + file, `DOST_SETTINGS_JSON={"model": "inline"}`, "DOST_PREFECT_API_URL=" + prefect.API()}, "from-file"},
		{[]string{"DOST_SETTINGS_FILE=", `DOST_SETTINGS_JSON={"model": "inline"}`, "DOST_PREFECT_API_URL=" + prefect.API()}, "inline"},
		{[]string{"DOST_SETTINGS_JSON=", "DOST_PREFECT_API_URL=" + prefect.API()}, "from-prefect"},
	} {
		p, _ := mustOpen(t, load(t, tc.environ...))
		if got := p.Current(context.Background()).Model; got != tc.want {
			t.Errorf("%q: model = %q, want %q", tc.environ, got, tc.want)
		}
	}
	prefect.AssertCalls(t, "GET /api/variables/name/chatbot_settings")
}

func TestStartingWithoutASettingsSourceFails(t *testing.T) {
	got := openErr(t, load(t, "DOST_SETTINGS_FILE=", "DOST_SETTINGS_JSON="))
	for _, name := range []string{"DOST_SETTINGS_FILE", "DOST_SETTINGS_JSON", "DOST_PREFECT_API_URL"} {
		if !strings.Contains(got, name) {
			t.Errorf("error %q does not name %s", got, name)
		}
	}
}

// sources returns an environment per kind of settings source serving doc, keyed by the name its errors use.
func sources(t *testing.T, doc string) map[string]config.Env {
	prefect := prefectfake.New(t)
	prefect.SetVariable("chatbot_settings", json.RawMessage(doc))
	return map[string]config.Env{
		"DOST_SETTINGS_FILE":                  load(t, "DOST_SETTINGS_FILE="+settingsFile(t, doc)),
		"DOST_SETTINGS_JSON":                  load(t, "DOST_SETTINGS_JSON="+doc),
		`Prefect variable "chatbot_settings"`: load(t, "DOST_PREFECT_API_URL="+prefect.API()),
	}
}

func TestEverySourceAcceptsAnObjectOrAStringHoldingOne(t *testing.T) {
	const object = `{"model": "m", "admin_ids": "u1"}`
	quoted, _ := json.Marshal(object)
	for _, doc := range []string{object, string(quoted)} {
		for source, env := range sources(t, doc) {
			p, _ := mustOpen(t, env)
			if s := p.Current(context.Background()); s.Model != "m" || len(s.AdminIDs) != 1 {
				t.Errorf("%s holding %s: got %s", source, doc, s.Redacted())
			}
		}
	}
}

func TestDocumentsThatAreNotObjectsNameTheirSource(t *testing.T) {
	for _, doc := range []string{`[{"model": "m"}]`, `"[1]"`, `null`, `"null"`, `42`, `"model"`, `"\"{}\""`} {
		for source, env := range sources(t, doc) {
			if got := openErr(t, env); !strings.HasPrefix(got, source+": ") {
				t.Errorf("%s holding %s: error %q does not name the source", source, doc, got)
			}
		}
	}
	for _, name := range []string{"DOST_SETTINGS_FILE", "DOST_SETTINGS_JSON"} {
		env := sources(t, `{"model": "m",}`)[name]
		if got := openErr(t, env); !strings.HasPrefix(got, name+": ") {
			t.Errorf("%s with invalid JSON: error %q does not name the source", name, got)
		}
	}
	if got := openErr(t, load(t, "DOST_SETTINGS_FILE=/nonexistent/settings.json")); !strings.HasPrefix(got, "DOST_SETTINGS_FILE: ") {
		t.Errorf("unreadable file: error %q does not name the source", got)
	}
}

func TestPrefectVariableIsReadByItsEscapedName(t *testing.T) {
	prefect := prefectfake.New(t)
	prefect.SetVariable("team settings/v2", map[string]any{"model": "m"})
	mustOpen(t, load(t, "DOST_PREFECT_API_URL="+prefect.API()+"/", "DOST_PREFECT_VARIABLE=team settings/v2"))
	prefect.AssertCalls(t, "GET /api/variables/name/team%20settings%2Fv2")
}

func TestMissingPrefectVariable(t *testing.T) {
	prefect := prefectfake.New(t)
	got := openErr(t, load(t, "DOST_PREFECT_API_URL="+prefect.API()))
	if got != `Prefect variable "chatbot_settings": settings variable not found` {
		t.Errorf("error = %q", got)
	}
}

func TestPrefectFailuresReportTheStatus(t *testing.T) {
	prefect := prefectfake.New(t)
	prefect.On(http.MethodGet, "/api/variables/name/{name}").Reply(testkit.JSON(http.StatusUnauthorized, map[string]string{"detail": "x"}))
	if got := openErr(t, load(t, "DOST_PREFECT_API_URL="+prefect.API())); !strings.Contains(got, "401") {
		t.Errorf("error = %q", got)
	}
}

func TestPrefectRequestsCarryTheConfiguredAuthorization(t *testing.T) {
	for _, tc := range []struct {
		environ []string
		want    string
	}{
		{nil, ""},
		{[]string{"DOST_PREFECT_API_KEY=pnu_1"}, "Bearer pnu_1"},
		{[]string{"DOST_PREFECT_API_AUTH_STRING=admin:pass"}, "Basic YWRtaW46cGFzcw=="},
		{[]string{"DOST_PREFECT_API_KEY=pnu_1", "DOST_PREFECT_API_AUTH_STRING=admin:pass"}, "Basic YWRtaW46cGFzcw=="},
	} {
		prefect := prefectfake.New(t)
		prefect.SetVariable("chatbot_settings", map[string]any{"model": "m", "key_secret": "model-key"})
		prefect.SetSecret("model-key", "sk-1")
		mustOpen(t, load(t, append(tc.environ, "DOST_PREFECT_API_URL="+prefect.API())...))
		reqs := prefect.Requests()
		if len(reqs) != 2 {
			t.Fatalf("%q: %d requests, want the variable and the secret", tc.environ, len(reqs))
		}
		for _, r := range reqs {
			if got := r.Header.Get("Authorization"); got != tc.want {
				t.Errorf("%q: %s has Authorization %q, want %q", tc.environ, r.Call(), got, tc.want)
			}
		}
	}
}

func TestSecretReferencesFillOnlyEmptyKeys(t *testing.T) {
	for _, tc := range []struct {
		doc   string
		want  secretValues
		calls []string
	}{
		{
			doc:   `{"key_secret": "model-key", "mattermost_token_secret": "chat-token"}`,
			want:  secretValues{Key: "sk-ref", MattermostToken: "tok-ref"},
			calls: []string{"model-key", "chat-token"},
		},
		{
			doc:   `{"key": "", "key_secret": "model-key", "mattermost_token": null, "mattermost_token_secret": "chat-token"}`,
			want:  secretValues{Key: "sk-ref", MattermostToken: "tok-ref"},
			calls: []string{"model-key", "chat-token"},
		},
		{
			doc:  `{"key": "sk-inline", "key_secret": "model-key", "mattermost_token": "tok-inline", "mattermost_token_secret": "chat-token"}`,
			want: secretValues{Key: "sk-inline", MattermostToken: "tok-inline"},
		},
		{
			doc: `{"key_secret": "", "mattermost_token_secret": null, "plane_api_secret": null}`,
		},
		{
			doc:   `{"plane_api_key": "pk-inline", "plane_api_secret": "plane-key"}`,
			want:  secretValues{PlaneAPIKey: "pk-ref"},
			calls: []string{"plane-key"},
		},
		{
			doc:  `{"plane_api_key": "pk-inline", "plane_api_secret": ""}`,
			want: secretValues{PlaneAPIKey: "pk-inline"},
		},
	} {
		prefect := prefectfake.New(t)
		prefect.SetSecret("model-key", "sk-ref")
		prefect.SetSecret("chat-token", "tok-ref")
		prefect.SetSecret("plane-key", "pk-ref")
		doc := `{"model": "m", ` + tc.doc[1:]
		p, _ := mustOpen(t, load(t, "DOST_SETTINGS_JSON="+doc, "DOST_PREFECT_API_URL="+prefect.API()))
		if got := secretsOf(t, p.Current(context.Background())); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.doc, got, tc.want)
		}
		var want []string
		for _, name := range tc.calls {
			want = append(want, "GET /api/block_types/slug/secret/block_documents/name/"+name+"?include_secrets=true")
		}
		prefect.AssertCalls(t, want...)
	}
}

func TestAdapterDeclaredSecretKeysResolveAndAreMasked(t *testing.T) {
	t.Setenv("DOST_TEST_CHAT_TOKEN", "chat-token-value")
	doc := `{"model": "m", "chat_token_secret": "env:DOST_TEST_CHAT_TOKEN", "key": "sk-inline-value"}`
	keys := append([]config.SecretKey{{Key: "chat_token"}}, secretKeys...)
	p, h, err := openWith(t, load(t, "DOST_SETTINGS_JSON="+doc), keys, noAdapters)
	if err != nil {
		t.Fatal(err)
	}
	s := p.Current(context.Background())
	var got struct {
		ChatToken string `json:"chat_token"`
	}
	if err := s.Decode(&got); err != nil || got.ChatToken != "chat-token-value" {
		t.Errorf("chat_token = %q, %v", got.ChatToken, err)
	}
	h.log.Info("chat-token-value sk-inline-value")
	if out := h.logs.String() + string(s.Redacted()); strings.Contains(out, "chat-token-value") || strings.Contains(out, "sk-inline-value") {
		t.Errorf("secret shown: %s", out)
	}
}

func TestUnknownKeysEndingInSecretAreLeftAsGiven(t *testing.T) {
	doc := `{"model": "m", "client_secret": "inline-value", "webhook_secret": "env:UNSET_FOR_DOST_TEST", "client": ""}`
	p, _ := mustOpen(t, load(t, "DOST_SETTINGS_JSON="+doc))
	var got struct {
		ClientSecret  string `json:"client_secret"`
		WebhookSecret string `json:"webhook_secret"`
		Client        string `json:"client"`
	}
	if err := p.Current(context.Background()).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ClientSecret != "inline-value" || got.WebhookSecret != "env:UNSET_FOR_DOST_TEST" || got.Client != "" {
		t.Errorf("got %+v", got)
	}
}

func TestEnvironmentAndFileReferences(t *testing.T) {
	t.Setenv("DOST_TEST_MODEL_KEY", " sk-env ")
	token := settingsFile(t, " tok-file \r\n\n")
	plane := settingsFile(t, "pk-file")
	doc := fmt.Sprintf(`{"model": "m", "key_secret": "env:DOST_TEST_MODEL_KEY", "mattermost_token_secret": "file:%s", "plane_api_secret": "file:%s"}`, token, plane)
	p, _ := mustOpen(t, load(t, "DOST_SETTINGS_FILE="+settingsFile(t, doc)))
	want := secretValues{Key: " sk-env ", MattermostToken: " tok-file ", PlaneAPIKey: "pk-file"}
	if got := secretsOf(t, p.Current(context.Background())); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestUnresolvableReferencesNameTheKey(t *testing.T) {
	t.Setenv("DOST_TEST_EMPTY", "")
	prefect := prefectfake.New(t)
	withPrefect := "DOST_PREFECT_API_URL=" + prefect.API()
	for _, tc := range []struct {
		ref, prefect string
		want         []string
	}{
		{ref: `"key_secret": "env:DOST_TEST_UNSET"`, want: []string{"key_secret", "DOST_TEST_UNSET"}},
		{ref: `"key_secret": "env:DOST_TEST_EMPTY"`, want: []string{"key_secret", "DOST_TEST_EMPTY"}},
		{ref: `"mattermost_token_secret": "file:/nonexistent/token"`, want: []string{"mattermost_token_secret", "/nonexistent/token"}},
		{ref: `"plane_api_secret": "plane-key"`, want: []string{"plane_api_secret", "plane-key", "DOST_PREFECT_API_URL"}},
		{ref: `"key_secret": "secret/model-key"`, prefect: withPrefect, want: []string{"key_secret", `"model-key"`, "not found"}},
		{ref: `"key_secret": 7`, want: []string{"key_secret"}},
	} {
		doc := `{"model": "m", ` + tc.ref + `}`
		got := openErr(t, load(t, "DOST_SETTINGS_JSON="+doc, tc.prefect))
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: error %q does not mention %s", tc.ref, got, w)
			}
		}
	}
}

func TestPrefectSecretValuesReadAsTheirText(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
	}{
		{"sk-1", "sk-1"},
		{`"sk-1"`, "sk-1"},
		{`"\"sk-1\""`, `"sk-1"`},
		{" sk-1\n", " sk-1\n"},
		{"007", "007"},
		{12345, "12345"},
		{"12345", "12345"},
		{"1e3", "1000.0"},
		{true, "True"},
		{"null", ""},
		{map[string]any{"user": "u"}, "{'user': 'u'}"},
	} {
		prefect := prefectfake.New(t)
		prefect.SetSecret("model-key", tc.value)
		doc := `{"model": "m", "key_secret": "secret/model-key"}`
		p, _ := mustOpen(t, load(t, "DOST_SETTINGS_JSON="+doc, "DOST_PREFECT_API_URL="+prefect.API()))
		if got := secretsOf(t, p.Current(context.Background())).Key; got != tc.want {
			t.Errorf("secret %#v: key = %q, want %q", tc.value, got, tc.want)
		}
		prefect.AssertCalls(t, "GET /api/block_types/slug/secret/block_documents/name/model-key?include_secrets=true")
	}
}

func TestReferencesInAPrefectDocumentAreSecretBlockNames(t *testing.T) {
	t.Setenv("DOST_TEST_MODEL_KEY", "sk-env")
	prefect := prefectfake.New(t)
	prefect.SetVariable("chatbot_settings", map[string]any{"model": "m", "key_secret": "env:DOST_TEST_MODEL_KEY"})
	got := openErr(t, load(t, "DOST_PREFECT_API_URL="+prefect.API()))
	if !strings.Contains(got, `"env:DOST_TEST_MODEL_KEY" not found`) {
		t.Errorf("error = %q", got)
	}
}

func TestSettingsSecretsAreMaskedInLogs(t *testing.T) {
	t.Setenv("DOST_TEST_MODEL_KEY", "sk-env")
	prefect := prefectfake.New(t)
	prefect.SetSecret("plane-key", "pk-prefect")
	doc := `{"model": "m", "key_secret": "env:DOST_TEST_MODEL_KEY", "mattermost_token": "tok-inline", "plane_api_secret": "plane-key"}`
	_, h := mustOpen(t, load(t, "DOST_SETTINGS_JSON="+doc, "DOST_PREFECT_API_URL="+prefect.API()))
	h.log.Info("sk-env tok-inline pk-prefect")
	if got := h.logs.String(); strings.Contains(got, "sk-env") || strings.Contains(got, "tok-inline") || strings.Contains(got, "pk-prefect") {
		t.Errorf("secret logged: %s", got)
	}
}

func TestReloadsAfterTheTTLWithSecretsResolvedAgain(t *testing.T) {
	ctx := context.Background()
	prefect := prefectfake.New(t)
	prefect.SetVariable("chatbot_settings", map[string]any{"model": "m1", "key_secret": "model-key"})
	prefect.SetSecret("model-key", "sk-one")
	p, h := mustOpen(t, load(t, "DOST_PREFECT_API_URL="+prefect.API(), "DOST_SETTINGS_TTL=10s"))
	first := p.Current(ctx)
	prefect.SetVariable("chatbot_settings", map[string]any{"model": "m2", "key_secret": "model-key"})
	prefect.SetSecret("model-key", "sk-two")

	h.clock.Advance(10*time.Second - time.Nanosecond)
	if got := p.Current(ctx); got != first || len(prefect.Requests()) != 2 {
		t.Fatalf("read before the TTL: model %q after %d requests, want the startup snapshot", got.Model, len(prefect.Requests()))
	}
	h.clock.Advance(time.Nanosecond)
	second := p.Current(ctx)
	if second.Model != "m2" || secretsOf(t, second).Key != "sk-two" {
		t.Errorf("after the TTL: model %q, key %q", second.Model, secretsOf(t, second).Key)
	}
	if got := p.Current(ctx); got != second || len(prefect.Requests()) != 4 {
		t.Errorf("read right after a reload: %d requests, want the reloaded snapshot without more", len(prefect.Requests()))
	}
	h.log.Info("sk-two")
	if strings.Contains(h.logs.String(), "sk-two") {
		t.Errorf("reloaded secret logged: %s", h.logs.String())
	}
}

func TestZeroTTLReloadsOnEveryRead(t *testing.T) {
	path := settingsFile(t, `{"model": "m1"}`)
	p, _ := mustOpen(t, load(t, "DOST_SETTINGS_FILE="+path, "DOST_SETTINGS_TTL=0"))
	writeFile(t, path, `{"model": "m2"}`)
	if got := p.Current(context.Background()).Model; got != "m2" {
		t.Errorf("model = %q, want m2", got)
	}
}

func TestOneReloadAtATimeWhileOtherReadersKeepThePreviousSnapshot(t *testing.T) {
	ctx := context.Background()
	prefect := prefectfake.New(t)
	prefect.SetVariable("chatbot_settings", map[string]any{"model": "m1"})
	p, h := mustOpen(t, load(t, "DOST_PREFECT_API_URL="+prefect.API()))
	previous := p.Current(ctx)

	release := make(chan struct{})
	prefect.On(http.MethodGet, "/api/variables/name/{name}").Reply(testkit.Response{Handler: func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-prefect.Context().Done():
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name": "chatbot_settings", "value": {"model": "m2"}}`))
	}})
	h.clock.Advance(5 * time.Second)
	reloaded := make(chan *config.Settings, 1)
	go func() { reloaded <- p.Current(ctx) }()
	prefect.Await(t, 2)

	if got := p.Current(ctx); got != previous {
		t.Errorf("read during the reload: model %q, want the previous snapshot", got.Model)
	}
	close(release)
	if got := <-reloaded; got.Model != "m2" {
		t.Errorf("reading reader: model %q, want m2", got.Model)
	}
	if n := len(prefect.Requests()); n != 2 {
		t.Errorf("%d requests, want one reload", n)
	}
}

func TestAPanickingReloadDoesNotStopLaterReloads(t *testing.T) {
	path := settingsFile(t, `{"model": "m1"}`)
	validate := func(s *config.Settings) error {
		if s.Model == "boom" {
			panic("adapter constructor bug")
		}
		return nil
	}
	p, h, err := open(t, load(t, "DOST_SETTINGS_FILE="+path), validate)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, `{"model": "boom"}`)
	func() {
		defer func() { _ = recover() }()
		h.tick(p)
	}()
	writeFile(t, path, `{"model": "m2"}`)
	if got := h.tick(p).Model; got != "m2" {
		t.Errorf("after a panicking reload: model %q, want m2", got)
	}
}

func TestAReloadCancelledByTheReaderIsNotAnError(t *testing.T) {
	prefect := prefectfake.New(t)
	prefect.SetVariable("chatbot_settings", map[string]any{"model": "m1"})
	p, h := mustOpen(t, load(t, "DOST_PREFECT_API_URL="+prefect.API()))
	previous := p.Current(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.clock.Advance(5 * time.Second)
	if got := p.Current(ctx); got != previous {
		t.Errorf("cancelled reload: model %q, want the previous snapshot", got.Model)
	}
	if h.count("level=ERROR") != 0 || h.count("level=DEBUG") != 1 {
		t.Errorf("want one debug record and no error:\n%s", h.logs)
	}
}

func TestBadEditsKeepTheLastGoodSettingsAndLogEachFailureOnce(t *testing.T) {
	ctx := context.Background()
	path := settingsFile(t, `{"model": "m1"}`)
	p, h := mustOpen(t, load(t, "DOST_SETTINGS_FILE="+path))
	good := p.Current(ctx)
	for _, bad := range []string{`{"model": `, `{"model": ""}`, `{"model": ""}`, `{"model": 1}`} {
		writeFile(t, path, bad)
		if got := h.tick(p); got != good {
			t.Fatalf("after %s: settings %s, want the last good ones", bad, got.Redacted())
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	h.tick(p)
	h.tick(p)
	if n := h.count("level=ERROR"); n != 4 {
		t.Errorf("%d errors logged, want 4 (one per distinct failure):\n%s", n, h.logs)
	}

	writeFile(t, path, `{"model": "m2"}`)
	if got := h.tick(p).Model; got != "m2" {
		t.Errorf("after fixing the document: model %q, want m2", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	h.tick(p)
	if n := h.count("level=ERROR"); n != 5 {
		t.Errorf("a failure that returns after a good reload: %d errors logged, want 5", n)
	}
}

func TestAdapterValidationGuardsStartupAndReload(t *testing.T) {
	validate := func(s *config.Settings) error {
		var c struct {
			Endpoint string `json:"mattermost_endpoint"`
			Key      string `json:"key"`
		}
		if err := s.Decode(&c); err != nil {
			return err
		}
		if c.Endpoint == "" {
			return errors.New("mattermost: mattermost_endpoint is required")
		}
		if c.Key == "sk-bad" {
			return fmt.Errorf("openai: key %s was rejected", c.Key)
		}
		return nil
	}
	key := settingsFile(t, "sk-1")
	path := settingsFile(t, `{"model": "m"}`)
	env := load(t, "DOST_SETTINGS_FILE="+path)
	if _, _, err := open(t, env, validate); err == nil || !strings.Contains(err.Error(), "mattermost_endpoint is required") {
		t.Fatalf("startup without an adapter setting: error %v", err)
	}

	writeFile(t, path, `{"model": "m", "mattermost_endpoint": "https://chat.example", "key_secret": "file:`+key+`"}`)
	p, h, err := open(t, env, validate)
	if err != nil {
		t.Fatal(err)
	}
	good := p.Current(context.Background())
	writeFile(t, path, `{"model": "m"}`)
	if got := h.tick(p); got != good || !strings.Contains(h.logs.String(), "mattermost_endpoint is required") {
		t.Errorf("reload without an adapter setting kept %v; logs:\n%s", got == good, h.logs)
	}
	writeFile(t, path, `{"model": "m", "mattermost_endpoint": "https://chat.example", "key_secret": "file:`+key+`"}`)
	writeFile(t, key, "sk-bad")
	if got := h.tick(p); got != good || strings.Contains(h.logs.String(), "sk-bad") {
		t.Errorf("reload with a rejected secret kept %v; logs:\n%s", got == good, h.logs)
	}
}

func TestChangingAFixedKeyIsLoggedOnce(t *testing.T) {
	path := settingsFile(t, `{"model": "m1"}`)
	p, h := mustOpen(t, load(t, "DOST_SETTINGS_FILE="+path))
	writeFile(t, path, `{"model": "m2", "chat_platform": "other", "kb_collection": "kb2"}`)
	if got := h.tick(p); got.Model != "m2" || got.ChatPlatform != "mattermost" || got.KBCollection != "dost_public_kb" {
		t.Errorf("got model %q, chat_platform %q, kb_collection %q", got.Model, got.ChatPlatform, got.KBCollection)
	}
	h.tick(p)
	writeFile(t, path, `{"model": "m3", "chat_platform": "other", "kb_collection": "kb3"}`)
	h.tick(p)
	if a, b := h.count("setting=chat_platform"), h.count("setting=kb_collection"); a != 1 || b != 1 || h.count("level=WARN") != 2 {
		t.Errorf("want one warning per fixed key:\n%s", h.logs)
	}
}
