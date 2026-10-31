package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/port/web"
)

type namedTool string

func (n namedTool) Name() string { return string(n) }
func (namedTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (namedTool) Call(context.Context, tool.Call) (string, error) { return "", nil }

func toolsNamed(names ...string) func(*config.Settings) []tool.Tool {
	return func(*config.Settings) []tool.Tool {
		var ts []tool.Tool
		for _, n := range names {
			ts = append(ts, namedTool(n))
		}
		return ts
	}
}

// fails returns a constructor that identifies itself by its error.
func fails[T any](name string) func(*config.Settings) (T, error) {
	return func(*config.Settings) (T, error) {
		var zero T
		return zero, errors.New(name)
	}
}

func testTable() adapters {
	secret := func(key string) []config.SecretKey { return []config.SecretKey{{Key: key}} }
	return adapters{
		platforms: map[string]platformAdapter{
			"chatty": {open: fails[chat.Platform]("chatty"), tools: toolsNamed("chatty_search"), secrets: secret("chatty_token")},
			"quiet":  {open: fails[chat.Platform]("quiet")},
		},
		models:    map[string]entry[model.Provider]{"compatible": {open: fails[model.Provider]("compatible"), secrets: secret("key")}},
		searchers: map[string]entry[web.Searcher]{"meta": {open: fails[web.Searcher]("meta"), secrets: secret("meta_key")}},
		fetchers:  map[string]entry[web.Fetcher]{"direct": {open: fails[web.Fetcher]("direct"), secrets: secret("key")}},
		tools: []toolSource{
			{tools: toolsNamed("tracker_list", "tracker_create"), secrets: []config.SecretKey{{Key: "tracker_key", Ref: "tracker_secret", Overwrite: true}}},
			{tools: toolsNamed("notes")},
		},
	}
}

func offered(s selected) []string {
	var names []string
	for _, contribute := range s.tools {
		for _, t := range contribute(nil) {
			names = append(names, t.Name())
		}
	}
	return names
}

func TestChooseResolvesSelectedAdapters(t *testing.T) {
	s, err := testTable().choose(names{platform: "chatty", model: "compatible", search: "meta", fetch: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	for want, open := range map[string]func() error{
		"chatty":     func() error { _, err := s.platform(nil); return err },
		"compatible": func() error { _, err := s.model(nil); return err },
		"meta":       func() error { _, err := s.search(nil); return err },
		"direct":     func() error { _, err := s.fetch(nil); return err },
	} {
		if err := open(); err == nil || err.Error() != want {
			t.Errorf("constructor for %s: got %v", want, err)
		}
	}
	if got, want := offered(s), []string{"chatty_search", "tracker_list", "tracker_create", "notes"}; !slices.Equal(got, want) {
		t.Errorf("tools: got %v, want %v", got, want)
	}
}

func TestChooseOffersPlatformToolsOnlyOnTheirPlatform(t *testing.T) {
	s, err := testTable().choose(names{platform: "quiet", model: "compatible", search: "meta", fetch: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := offered(s), []string{"tracker_list", "tracker_create", "notes"}; !slices.Equal(got, want) {
		t.Errorf("tools: got %v, want %v", got, want)
	}
}

func TestChooseReportsEveryUnknownName(t *testing.T) {
	_, err := testTable().choose(names{platform: "pigeon", model: "oracle", search: "meta", fetch: "telepathy"})
	want := `chat_platform: unknown adapter "pigeon" (available: chatty, quiet)` + "\n" +
		`model_provider: unknown adapter "oracle" (available: compatible)` + "\n" +
		`web_fetch: unknown adapter "telepathy" (available: direct)`
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

func TestSecretKeysAreTheUnionOfEveryRegisteredAdapter(t *testing.T) {
	want := []config.SecretKey{
		{Key: "chatty_token"}, {Key: "key"}, {Key: "meta_key"}, {Key: "tracker_key", Ref: "tracker_secret", Overwrite: true},
	}
	if got := testTable().secretKeys(); !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestBuiltinSecretKeysAreTheExistingDocumentKeys(t *testing.T) {
	want := []config.SecretKey{
		{Key: "key"}, {Key: "mattermost_token"}, {Key: "plane_api_key", Ref: "plane_api_secret", Overwrite: true},
	}
	if got := builtin(http.DefaultTransport).secretKeys(); !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestChooseReportsAnAdapterWithoutAConstructor(t *testing.T) {
	_, err := builtin(http.DefaultTransport).choose(names{platform: "mattermost", model: "openai", search: "searxng", fetch: "httpfetch"})
	if err == nil || !strings.Contains(err.Error(), `model_provider: adapter "openai" is not available yet`) {
		t.Errorf("got %v", err)
	}
}

func TestBuiltinPlatformIsMattermostByDefault(t *testing.T) {
	doc := map[string]json.RawMessage{
		"model": json.RawMessage(`"m"`), "mattermost_endpoint": json.RawMessage(`"https://chat.example"`),
		"mattermost_token": json.RawMessage(`"t"`), "mattermost_bot_id": json.RawMessage(`"bot"`),
	}
	s, err := config.Load(doc, "agent", nil, func(*config.Settings) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	p, ok := builtin(http.DefaultTransport).platforms[s.ChatPlatform]
	if !ok {
		t.Fatalf("no platform registered as %q", s.ChatPlatform)
	}
	platform, err := p.open(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := platform.Identity(); got != (chat.User{ID: "bot"}) {
		t.Errorf("identity = %+v", got)
	}
}
