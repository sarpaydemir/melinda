package app

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/port/web"
)

// adapters is the constructor table of the built-in adapters, keyed by the names the settings select them by.
// Adding an adapter is one entry. Constructors build a value from one settings snapshot and do no I/O; an error
// means the snapshot's settings for that adapter are invalid.
type adapters struct {
	platforms map[string]platformAdapter
	models    map[string]func(*config.Settings) (model.Provider, error)
	searchers map[string]func(*config.Settings) (web.Searcher, error)
	fetchers  map[string]func(*config.Settings) (web.Fetcher, error)
	// tools are offered in this order after the platform's tools. Only the first source (Plane) contributes by
	// default; every source added after it returns no tools until its own settings are present, so a deployment's
	// tool set never changes on upgrade.
	tools []func(*config.Settings) []tool.Tool
}

// platformAdapter is a chat platform with the tools it contributes, which are offered only on that platform.
type platformAdapter struct {
	open  func(*config.Settings) (chat.Platform, error)
	tools func(*config.Settings) []tool.Tool
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
	tools []func(*config.Settings) []tool.Tool
}

// choose resolves the selected names, reporting every unknown name at once.
func (a adapters) choose(n names) (selected, error) {
	platform, errPlatform := pick(a.platforms, "chat_platform", n.platform)
	s := selected{platform: platform.open}
	var errModel, errSearch, errFetch error
	s.model, errModel = pick(a.models, "model_provider", n.model)
	s.search, errSearch = pick(a.searchers, "web_search", n.search)
	s.fetch, errFetch = pick(a.fetchers, "web_fetch", n.fetch)
	if platform.tools != nil {
		s.tools = append(s.tools, platform.tools)
	}
	s.tools = append(s.tools, a.tools...)
	return s, errors.Join(errPlatform, errModel, errSearch, errFetch)
}

// pick returns the entry registered under name, the value of the settings key.
func pick[T any](table map[string]T, key, name string) (T, error) {
	v, ok := table[name]
	if !ok {
		return v, fmt.Errorf("%s: unknown adapter %q (available: %s)", key, name, strings.Join(slices.Sorted(maps.Keys(table)), ", "))
	}
	return v, nil
}
