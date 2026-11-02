package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"testing"

	"github.com/sarpaydemir/melinda/internal/automation"
	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/port/tool"
	"github.com/sarpaydemir/melinda/internal/port/web"
	"github.com/sarpaydemir/melinda/internal/prompt"
	"github.com/sarpaydemir/melinda/internal/testkit/searxngfake"
)

type namedTool string

func (n namedTool) Name() string { return string(n) }
func (namedTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (namedTool) Call(context.Context, tool.Call) (string, error) { return "", nil }

// sourceTools are the tools of a tool source, named names.
func sourceTools(names ...string) func(*config.Settings, *prompt.Catalog, http.RoundTripper) []tool.Tool {
	named := toolsNamed(names...)
	return func(s *config.Settings, c *prompt.Catalog, _ http.RoundTripper) []tool.Tool { return named(s, c) }
}

func toolsNamed(names ...string) func(*config.Settings, *prompt.Catalog) []tool.Tool {
	return func(*config.Settings, *prompt.Catalog) []tool.Tool {
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
			{tools: sourceTools("tracker_list", "tracker_create"), secrets: []config.SecretKey{{Key: "tracker_key", Ref: "tracker_secret", Overwrite: true}}},
			{tools: sourceTools("notes")},
		},
		handlers: map[string]handler{
			"summarize": {run: func(context.Context, automation.Run) (string, error) { return "", nil }},
			"analyze":   {run: func(context.Context, automation.Run) (string, error) { return "", nil }},
		},
	}
}

func offered(s selected) []string {
	var names []string
	for _, t := range s.contributed(nil, nil) {
		names = append(names, t.Name())
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

func TestChooseAcceptsAnyNameForAKindWithoutAdapters(t *testing.T) {
	table := testTable()
	table.searchers, table.fetchers = nil, nil
	s, err := table.choose(names{platform: "quiet", model: "compatible", search: "searxng", fetch: "anything"})
	if err != nil || s.search != nil || s.fetch != nil {
		t.Errorf("choose = %+v, %v; want no searcher, no fetcher, and no error", s, err)
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

func TestValidateBuildsTheSelectedAdaptersAndReportsEveryProblem(t *testing.T) {
	doc := map[string]json.RawMessage{
		"model": json.RawMessage(`"m"`), "chat_platform": json.RawMessage(`"pigeon"`),
		"model_provider": json.RawMessage(`"compatible"`), "web_search": json.RawMessage(`"meta"`),
		"web_fetch": json.RawMessage(`"direct"`), "enabled_tools": json.RawMessage(`["search_web", "tracker_list", "teleport"]`),
		"automations": json.RawMessage(`[{"pattern": "a", "handler": "analyze"}, {"pattern": "b", "handler": "translate"}]`),
	}
	s, err := config.Load(doc, "agent", nil, func(*config.Settings) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	want := `chat_platform: unknown adapter "pigeon" (available: chatty, quiet)` + "\ncompatible\nmeta\ndirect\n" +
		`enabled_tools: unknown tool "teleport"` + "\n" +
		`automations[1]: unknown handler "translate" (available: analyze, summarize)`
	if err := testTable().validate(s); err == nil || err.Error() != want {
		t.Errorf("validate = %v, want %q", err, want)
	}
}

func TestBuiltinSecretKeysAreTheExistingDocumentKeys(t *testing.T) {
	want := []config.SecretKey{
		{Key: "key"}, {Key: "mattermost_token"}, {Key: "plane_api_key", Ref: "plane_api_secret", Overwrite: true},
	}
	if got := builtin(http.DefaultTransport, clock.Real(), "melinda/test", "", slog.New(slog.DiscardHandler)).secretKeys(); !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestBuiltinModelProviderIsOpenAIByDefault(t *testing.T) {
	doc := map[string]json.RawMessage{
		"model": json.RawMessage(`"m"`), "api_base": json.RawMessage(`"https://models.example/v1"`),
		"key": json.RawMessage(`"k"`),
	}
	s, err := config.Load(doc, "agent", nil, func(*config.Settings) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	e, ok := builtin(http.DefaultTransport, clock.Real(), "melinda/test", "", slog.New(slog.DiscardHandler)).models[s.ModelProvider]
	if !ok {
		t.Fatalf("no model provider registered as %q", s.ModelProvider)
	}
	if _, err := e.open(s); err != nil {
		t.Error(err)
	}
}

func TestBuiltinPageFetcherIsHTTPFetchByDefault(t *testing.T) {
	s, err := config.Load(map[string]json.RawMessage{"model": json.RawMessage(`"m"`)}, "agent", nil,
		func(*config.Settings) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	e, ok := builtin(http.DefaultTransport, clock.Real(), "melinda/test", "", slog.New(slog.DiscardHandler)).fetchers[s.WebFetch]
	if !ok {
		t.Fatalf("no page fetcher registered as %q", s.WebFetch)
	}
	if _, err := e.open(s); err != nil {
		t.Error(err)
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
	p, ok := builtin(http.DefaultTransport, clock.Real(), "melinda/test", "", slog.New(slog.DiscardHandler)).platforms[s.ChatPlatform]
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

func TestBuiltinWebSearchIsSearXNGByDefaultAtTheConfiguredAddress(t *testing.T) {
	fake := searxngfake.New(t)
	fake.Search().Reply(searxngfake.Results(searxngfake.Result{Title: "Go", URL: "https://go.dev"}))
	s, err := config.Load(map[string]json.RawMessage{"model": json.RawMessage(`"m"`)}, "agent", nil,
		func(*config.Settings) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	e, ok := builtin(http.DefaultTransport, clock.Real(), "melinda/test", fake.URL(), slog.New(slog.DiscardHandler)).searchers[s.WebSearch]
	if !ok {
		t.Fatalf("no web search registered as %q", s.WebSearch)
	}
	searcher, err := e.open(s)
	if err != nil {
		t.Fatal(err)
	}
	if results, err := searcher.Search(t.Context(), "golang", 5); err != nil || len(results) != 1 {
		t.Errorf("search = %d results, %v; want the instance's one", len(results), err)
	}
}
