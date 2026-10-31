package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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
	return adapters{
		platforms: map[string]platformAdapter{
			"chatty": {open: fails[chat.Platform]("chatty"), tools: toolsNamed("chatty_search")},
			"quiet":  {open: fails[chat.Platform]("quiet")},
		},
		models:    map[string]func(*config.Settings) (model.Provider, error){"compatible": fails[model.Provider]("compatible")},
		searchers: map[string]func(*config.Settings) (web.Searcher, error){"meta": fails[web.Searcher]("meta")},
		fetchers:  map[string]func(*config.Settings) (web.Fetcher, error){"direct": fails[web.Fetcher]("direct")},
		tools:     []func(*config.Settings) []tool.Tool{toolsNamed("tracker_list", "tracker_create"), toolsNamed("notes")},
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
