package app

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/sarpaydemir/dost/internal/adapters/httpfetch"
	"github.com/sarpaydemir/dost/internal/adapters/mattermost"
	"github.com/sarpaydemir/dost/internal/adapters/openai"
	"github.com/sarpaydemir/dost/internal/adapters/plane"
	"github.com/sarpaydemir/dost/internal/adapters/searxng"
	"github.com/sarpaydemir/dost/internal/automation"
	"github.com/sarpaydemir/dost/internal/automation/brand"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/port/web"
	"github.com/sarpaydemir/dost/internal/prompt"
	"github.com/sarpaydemir/dost/internal/tools"
)

// adapters is the constructor table of the built-in adapters, keyed by the names the settings select them by.
// Adding an adapter is one entry. Constructors build a value from one settings snapshot and do no I/O; an error
// means the snapshot's settings for that adapter are invalid. Each entry declares the settings keys that hold its
// secrets.
type adapters struct {
	platforms map[string]platformAdapter
	models    map[string]entry[model.Provider]
	searchers map[string]entry[web.Searcher]
	fetchers  map[string]entry[web.Fetcher]
	// tools are offered in this order after the platform's tools. Only the first source (Plane) contributes by
	// default; every source added after it returns no tools until its own settings are present, so a deployment's
	// tool set never changes on upgrade.
	tools []toolSource
	// handlers are the automation handlers, by the IDs the automations setting names them by.
	handlers map[string]handler
}

// builtin is the table of the adapters this binary ships: their HTTP requests go through transport, clk times
// their waits, userAgent identifies them where they send one, searxngURL is the address of the SearXNG instance,
// and they log to log.
func builtin(transport http.RoundTripper, clk clock.Clock, userAgent, searxngURL string, log *slog.Logger) adapters {
	return adapters{
		platforms: map[string]platformAdapter{
			"mattermost": {
				open:    func(s *config.Settings) (chat.Platform, error) { return mattermost.Open(s, transport, clk, log) },
				tools:   func(s *config.Settings, c *prompt.Catalog) []tool.Tool { return mattermost.Tools(s, c, transport, log) },
				prompts: mattermost.Prompts(),
				secrets: []config.SecretKey{{Key: "mattermost_token"}},
			},
		},
		models: map[string]entry[model.Provider]{
			"openai": {
				open:    func(s *config.Settings) (model.Provider, error) { return openai.New(s, transport, clk, userAgent, log) },
				secrets: []config.SecretKey{{Key: "key"}},
			},
		},
		searchers: map[string]entry[web.Searcher]{
			"searxng": {
				open: func(s *config.Settings) (web.Searcher, error) { return searxng.New(s, searxngURL, transport) },
			},
		},
		fetchers: map[string]entry[web.Fetcher]{
			// Page fetches connect through the adapter's own guarded dialer, not transport.
			"httpfetch": {open: httpfetch.New},
		},
		tools: []toolSource{{
			tools:   func(s *config.Settings, _ *prompt.Catalog) []tool.Tool { return plane.Tools(s, transport, clk, log) },
			prompts: plane.Prompts(),
			secrets: []config.SecretKey{{Key: "plane_api_key", Ref: "plane_api_secret", Overwrite: true}},
		}},
		handlers: map[string]handler{
			"brand_analysis": {run: brand.Analyze, prompts: brand.Prompts()},
		},
	}
}

// entry is an adapter's constructor and the settings keys that hold its secrets.
type entry[T any] struct {
	open    func(*config.Settings) (T, error)
	secrets []config.SecretKey
}

// platformAdapter is a chat platform with the tools it contributes, which are offered only on that platform, and
// the catalog entries of those tools' texts.
type platformAdapter struct {
	open    func(*config.Settings) (chat.Platform, error)
	tools   func(*config.Settings, *prompt.Catalog) []tool.Tool
	prompts []prompt.Entry
	secrets []config.SecretKey
}

// handler is an automation handler and the catalog entries of its texts.
type handler struct {
	run     automation.Handler
	prompts []prompt.Entry
}

// toolSource contributes tools on every platform, and the catalog entries of their texts.
type toolSource struct {
	tools   func(*config.Settings, *prompt.Catalog) []tool.Tool
	prompts []prompt.Entry
	secrets []config.SecretKey
}

// prompts are the built-in catalog entries followed by those of every registered adapter, selected or not, since
// the selection is read from the document whose overrides the catalog checks.
func (a adapters) prompts() []prompt.Entry {
	entries := prompt.Builtin()
	for _, name := range slices.Sorted(maps.Keys(a.platforms)) {
		entries = append(entries, a.platforms[name].prompts...)
	}
	for _, src := range a.tools {
		entries = append(entries, src.prompts...)
	}
	for _, id := range slices.Sorted(maps.Keys(a.handlers)) {
		entries = append(entries, a.handlers[id].prompts...)
	}
	return entries
}

// secretKeys are the secret keys of every registered adapter, selected or not, since the selection is read from
// the document whose references they resolve.
func (a adapters) secretKeys() []config.SecretKey {
	var keys []config.SecretKey
	for _, p := range a.platforms {
		keys = append(keys, p.secrets...)
	}
	for _, e := range a.models {
		keys = append(keys, e.secrets...)
	}
	for _, e := range a.searchers {
		keys = append(keys, e.secrets...)
	}
	for _, e := range a.fetchers {
		keys = append(keys, e.secrets...)
	}
	for _, src := range a.tools {
		keys = append(keys, src.secrets...)
	}
	slices.SortFunc(keys, func(x, y config.SecretKey) int { return strings.Compare(x.Key, y.Key) })
	return slices.Compact(keys)
}

// names are the adapter names a settings document selects.
type names struct {
	platform, model, search, fetch string
}

// selected are the constructors a process runs with.
type selected struct {
	platform func(*config.Settings) (chat.Platform, error)
	model    func(*config.Settings) (model.Provider, error)
	search   func(*config.Settings) (web.Searcher, error)
	fetch    func(*config.Settings) (web.Fetcher, error)
	// tools contribute tools in the order they are offered, after the built-in knowledge tools and before the
	// built-in conversation tools.
	tools    []func(*config.Settings, *prompt.Catalog) []tool.Tool
	handlers map[string]automation.Handler
}

// contributed are the tools the selected adapters contribute with s and the catalog c of their texts, in the order
// they are offered.
func (sel selected) contributed(s *config.Settings, c *prompt.Catalog) []tool.Tool {
	var contributed []tool.Tool
	for _, contribute := range sel.tools {
		contributed = append(contributed, contribute(s, c)...)
	}
	return contributed
}

// choose resolves the selected names, reporting every unknown name at once.
func (a adapters) choose(n names) (selected, error) {
	platform, errPlatform := pick(a.platforms, "chat_platform", n.platform)
	model, errModel := pick(a.models, "model_provider", n.model)
	search, errSearch := pick(a.searchers, "web_search", n.search)
	fetch, errFetch := pick(a.fetchers, "web_fetch", n.fetch)
	s := selected{platform: platform.open, model: model.open, search: search.open, fetch: fetch.open,
		handlers: make(map[string]automation.Handler, len(a.handlers))}
	for id, h := range a.handlers {
		s.handlers[id] = h.run
	}
	if platform.tools != nil {
		s.tools = append(s.tools, platform.tools)
	}
	for _, src := range a.tools {
		if src.tools != nil {
			s.tools = append(s.tools, src.tools)
		}
	}
	return s, errors.Join(errPlatform, errModel, errSearch, errFetch)
}

// selection is the adapter names that s selects.
func selection(s *config.Settings) names {
	return names{platform: s.ChatPlatform, model: s.ModelProvider, search: s.WebSearch, fetch: s.WebFetch}
}

// validate builds the adapters that s selects and checks that enabled_tools names only offered tools and every
// automation a registered handler, reporting every unknown name and invalid adapter setting at once.
func (a adapters) validate(s *config.Settings) error {
	sel, err := a.choose(selection(s))
	_, errTools := tools.Offer(tool.Call{}, s.EnabledTools, tools.Builtin{}, sel.contributed(s, nil)) // only their names are read
	errs := []error{err, build(sel.platform, s), build(sel.model, s), build(sel.search, s), build(sel.fetch, s), errTools}
	for i, auto := range s.Automations {
		if _, ok := a.handlers[auto.Handler]; !ok {
			errs = append(errs, fmt.Errorf("automations[%d]: unknown handler %q (available: %s)", i, auto.Handler,
				strings.Join(slices.Sorted(maps.Keys(a.handlers)), ", ")))
		}
	}
	return errors.Join(errs...)
}

// build returns the error of building an adapter with open, which is nil for an unknown name, reported by choose,
// and for a kind with no registered adapters.
func build[T any](open func(*config.Settings) (T, error), s *config.Settings) error {
	if open == nil {
		return nil
	}
	_, err := open(s)
	return err
}

// pick returns the entry registered under name, the value of the settings key. While no adapter of a kind is
// registered, any name is accepted and the entry is empty.
func pick[T any](table map[string]T, key, name string) (T, error) {
	v, ok := table[name]
	if !ok && len(table) > 0 {
		return v, fmt.Errorf("%s: unknown adapter %q (available: %s)", key, name, strings.Join(slices.Sorted(maps.Keys(table)), ", "))
	}
	return v, nil
}
