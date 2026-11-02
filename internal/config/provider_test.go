package config_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/logging"
	"github.com/sarpaydemir/melinda/internal/testkit/fakeclock"
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
	p, err := config.NewProvider(env, keys, nil, validate, mask, h.clock, h.log)
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
	file := settingsFile(t, `{"model": "from-file"}`)
	for _, tc := range []struct {
		environ []string
		want    string
	}{
		{[]string{"MELINDA_SETTINGS_FILE=" + file, `MELINDA_SETTINGS_JSON={"model": "inline"}`}, "from-file"},
		{[]string{"MELINDA_SETTINGS_FILE=", `MELINDA_SETTINGS_JSON={"model": "inline"}`}, "inline"},
	} {
		p, _ := mustOpen(t, load(t, tc.environ...))
		if got := p.Current(context.Background()).Model; got != tc.want {
			t.Errorf("%q: model = %q, want %q", tc.environ, got, tc.want)
		}
	}
}

func TestStartingWithoutASettingsSourceFails(t *testing.T) {
	got := openErr(t, load(t, "MELINDA_SETTINGS_FILE=", "MELINDA_SETTINGS_JSON="))
	if want := "no settings source: set MELINDA_SETTINGS_FILE or MELINDA_SETTINGS_JSON"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

// sources returns an environment per kind of settings source serving doc, keyed by the name its errors use.
func sources(t *testing.T, doc string) map[string]config.Env {
	return map[string]config.Env{
		"MELINDA_SETTINGS_FILE": load(t, "MELINDA_SETTINGS_FILE="+settingsFile(t, doc)),
		"MELINDA_SETTINGS_JSON": load(t, "MELINDA_SETTINGS_JSON="+doc),
	}
}

func TestSettingsSourceNamesTheFirstConfiguredSource(t *testing.T) {
	for source, env := range sources(t, `{"model": "m"}`) {
		if got := env.SettingsSource(); got != source {
			t.Errorf("SettingsSource() = %q, want %q", got, source)
		}
	}
	if got := load(t).SettingsSource(); got != "" {
		t.Errorf("SettingsSource() without a source = %q, want empty", got)
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
	for _, doc := range []string{`{"model": "m",}`, `[{"model": "m"}]`, `"[1]"`, `null`, `"null"`, `42`, `"model"`, `"\"{}\""`} {
		for source, env := range sources(t, doc) {
			if got := openErr(t, env); !strings.HasPrefix(got, source+": ") {
				t.Errorf("%s holding %s: error %q does not name the source", source, doc, got)
			}
		}
	}
	if got := openErr(t, load(t, "MELINDA_SETTINGS_FILE=/nonexistent/settings.json")); !strings.HasPrefix(got, "MELINDA_SETTINGS_FILE: ") {
		t.Errorf("unreadable file: error %q does not name the source", got)
	}
}

func TestSecretReferencesFillOnlyEmptyKeys(t *testing.T) {
	t.Setenv("MELINDA_TEST_MODEL_KEY", "sk-ref")
	t.Setenv("MELINDA_TEST_CHAT_TOKEN", "tok-ref")
	t.Setenv("MELINDA_TEST_PLANE_KEY", "pk-ref")
	for _, tc := range []struct {
		doc  string
		want secretValues
	}{
		{
			doc:  `{"key_secret": "env:MELINDA_TEST_MODEL_KEY", "mattermost_token_secret": "env:MELINDA_TEST_CHAT_TOKEN"}`,
			want: secretValues{Key: "sk-ref", MattermostToken: "tok-ref"},
		},
		{
			doc:  `{"key": "", "key_secret": "env:MELINDA_TEST_MODEL_KEY", "mattermost_token": null, "mattermost_token_secret": "env:MELINDA_TEST_CHAT_TOKEN"}`,
			want: secretValues{Key: "sk-ref", MattermostToken: "tok-ref"},
		},
		{
			doc:  `{"key": "sk-inline", "key_secret": "env:MELINDA_TEST_MODEL_KEY", "mattermost_token": "tok-inline", "mattermost_token_secret": "env:MELINDA_TEST_CHAT_TOKEN"}`,
			want: secretValues{Key: "sk-inline", MattermostToken: "tok-inline"},
		},
		{
			doc: `{"key_secret": "", "mattermost_token_secret": null, "plane_api_secret": null}`,
		},
		{
			doc:  `{"plane_api_key": "pk-inline", "plane_api_secret": "env:MELINDA_TEST_PLANE_KEY"}`,
			want: secretValues{PlaneAPIKey: "pk-ref"},
		},
		{
			doc:  `{"plane_api_key": "pk-inline", "plane_api_secret": ""}`,
			want: secretValues{PlaneAPIKey: "pk-inline"},
		},
	} {
		doc := `{"model": "m", ` + tc.doc[1:]
		p, _ := mustOpen(t, load(t, "MELINDA_SETTINGS_JSON="+doc))
		if got := secretsOf(t, p.Current(context.Background())); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.doc, got, tc.want)
		}
	}
}

func TestAdapterDeclaredSecretKeysResolveAndAreMasked(t *testing.T) {
	t.Setenv("MELINDA_TEST_CHAT_TOKEN", "chat-token-value")
	doc := `{"model": "m", "chat_token_secret": "env:MELINDA_TEST_CHAT_TOKEN", "key": "sk-inline-value"}`
	keys := append([]config.SecretKey{{Key: "chat_token"}}, secretKeys...)
	p, h, err := openWith(t, load(t, "MELINDA_SETTINGS_JSON="+doc), keys, noAdapters)
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
	doc := `{"model": "m", "client_secret": "inline-value", "webhook_secret": "env:UNSET_FOR_MELINDA_TEST", "client": ""}`
	p, _ := mustOpen(t, load(t, "MELINDA_SETTINGS_JSON="+doc))
	var got struct {
		ClientSecret  string `json:"client_secret"`
		WebhookSecret string `json:"webhook_secret"`
		Client        string `json:"client"`
	}
	if err := p.Current(context.Background()).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ClientSecret != "inline-value" || got.WebhookSecret != "env:UNSET_FOR_MELINDA_TEST" || got.Client != "" {
		t.Errorf("got %+v", got)
	}
}

func TestEnvironmentAndFileReferences(t *testing.T) {
	t.Setenv("MELINDA_TEST_MODEL_KEY", " sk-env ")
	token := settingsFile(t, " tok-file \r\n\n")
	plane := settingsFile(t, "pk-file")
	doc := fmt.Sprintf(`{"model": "m", "key_secret": "env:MELINDA_TEST_MODEL_KEY", "mattermost_token_secret": "file:%s", "plane_api_secret": "file:%s"}`, token, plane)
	p, _ := mustOpen(t, load(t, "MELINDA_SETTINGS_FILE="+settingsFile(t, doc)))
	want := secretValues{Key: " sk-env ", MattermostToken: " tok-file ", PlaneAPIKey: "pk-file"}
	if got := secretsOf(t, p.Current(context.Background())); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestUnresolvableReferencesNameTheKey(t *testing.T) {
	t.Setenv("MELINDA_TEST_EMPTY", "")
	for _, tc := range []struct {
		ref  string
		want []string
	}{
		{ref: `"key_secret": "env:MELINDA_TEST_UNSET"`, want: []string{"key_secret", "MELINDA_TEST_UNSET"}},
		{ref: `"key_secret": "env:MELINDA_TEST_EMPTY"`, want: []string{"key_secret", "MELINDA_TEST_EMPTY"}},
		{ref: `"mattermost_token_secret": "file:/nonexistent/token"`, want: []string{"mattermost_token_secret", "/nonexistent/token"}},
	} {
		doc := `{"model": "m", ` + tc.ref + `}`
		got := openErr(t, load(t, "MELINDA_SETTINGS_JSON="+doc))
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: error %q does not mention %s", tc.ref, got, w)
			}
		}
	}
}

func TestReferencesOtherThanEnvAndFileAreRejectedWithoutEchoingThem(t *testing.T) {
	for _, ref := range []string{`"sk-pasted-here"`, `"secret/plane-key"`, `7`} {
		doc := `{"model": "m", "plane_api_secret": ` + ref + `}`
		got := openErr(t, load(t, "MELINDA_SETTINGS_JSON="+doc))
		if want := "plane_api_secret: not an env:NAME or file:/path reference"; got != want {
			t.Errorf("%s: error %q, want %q", ref, got, want)
		}
	}
}

func TestReloadsAfterTheTTLWithSecretsResolvedAgain(t *testing.T) {
	ctx := context.Background()
	key := settingsFile(t, "sk-one")
	path := settingsFile(t, `{"model": "m1", "key_secret": "file:`+key+`"}`)
	p, h := mustOpen(t, load(t, "MELINDA_SETTINGS_FILE="+path, "MELINDA_SETTINGS_TTL=10s"))
	first := p.Current(ctx)
	writeFile(t, path, `{"model": "m2", "key_secret": "file:`+key+`"}`)
	writeFile(t, key, "sk-two")

	h.clock.Advance(10*time.Second - time.Nanosecond)
	if got := p.Current(ctx); got != first {
		t.Fatalf("read before the TTL: model %q, want the startup snapshot", got.Model)
	}
	h.clock.Advance(time.Nanosecond)
	second := p.Current(ctx)
	if second.Model != "m2" || secretsOf(t, second).Key != "sk-two" {
		t.Errorf("after the TTL: model %q, key %q", second.Model, secretsOf(t, second).Key)
	}
	writeFile(t, path, `{"model": "m3"}`)
	if got := p.Current(ctx); got != second {
		t.Errorf("read right after a reload: model %q, want the reloaded snapshot", got.Model)
	}
	h.log.Info("sk-two")
	if strings.Contains(h.logs.String(), "sk-two") {
		t.Errorf("reloaded secret logged: %s", h.logs.String())
	}
}

func TestZeroTTLReloadsOnEveryRead(t *testing.T) {
	path := settingsFile(t, `{"model": "m1"}`)
	p, _ := mustOpen(t, load(t, "MELINDA_SETTINGS_FILE="+path, "MELINDA_SETTINGS_TTL=0"))
	writeFile(t, path, `{"model": "m2"}`)
	if got := p.Current(context.Background()).Model; got != "m2" {
		t.Errorf("model = %q, want m2", got)
	}
}

func TestOneReloadAtATimeWhileOtherReadersKeepThePreviousSnapshot(t *testing.T) {
	ctx := context.Background()
	path := settingsFile(t, `{"model": "m1"}`)
	var reloads atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	validate := func(s *config.Settings) error {
		if s.Model == "m2" && reloads.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	p, h, err := open(t, load(t, "MELINDA_SETTINGS_FILE="+path), validate)
	if err != nil {
		t.Fatal(err)
	}
	previous := p.Current(ctx)

	writeFile(t, path, `{"model": "m2"}`)
	h.clock.Advance(5 * time.Second)
	reloaded := make(chan *config.Settings, 1)
	go func() { reloaded <- p.Current(ctx) }()
	<-entered

	if got := p.Current(ctx); got != previous {
		t.Errorf("read during the reload: model %q, want the previous snapshot", got.Model)
	}
	close(release)
	if got := <-reloaded; got.Model != "m2" {
		t.Errorf("reading reader: model %q, want m2", got.Model)
	}
	if n := reloads.Load(); n != 1 {
		t.Errorf("%d reloads, want one", n)
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
	p, h, err := open(t, load(t, "MELINDA_SETTINGS_FILE="+path), validate)
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

func TestBadEditsKeepTheLastGoodSettingsAndLogEachFailureOnce(t *testing.T) {
	ctx := context.Background()
	path := settingsFile(t, `{"model": "m1"}`)
	p, h := mustOpen(t, load(t, "MELINDA_SETTINGS_FILE="+path))
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
	env := load(t, "MELINDA_SETTINGS_FILE="+path)
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
	p, h := mustOpen(t, load(t, "MELINDA_SETTINGS_FILE="+path))
	writeFile(t, path, `{"model": "m2", "chat_platform": "other", "kb_collection": "kb2"}`)
	if got := h.tick(p); got.Model != "m2" || got.ChatPlatform != "mattermost" || got.KBCollection != "melinda_public_kb" {
		t.Errorf("got model %q, chat_platform %q, kb_collection %q", got.Model, got.ChatPlatform, got.KBCollection)
	}
	h.tick(p)
	writeFile(t, path, `{"model": "m3", "chat_platform": "other", "kb_collection": "kb3"}`)
	h.tick(p)
	if a, b := h.count("setting=chat_platform"), h.count("setting=kb_collection"); a != 1 || b != 1 || h.count("level=WARN") != 2 {
		t.Errorf("want one warning per fixed key:\n%s", h.logs)
	}
}
