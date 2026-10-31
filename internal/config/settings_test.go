package config_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sarpaydemir/dost/internal/config"
)

func document(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatalf("document %s: %v", s, err)
	}
	return doc
}

func noAdapters(*config.Settings) error { return nil }

// secretKeys are the secret keys the built-in adapters declare.
var secretKeys = []config.SecretKey{
	{Key: "key"}, {Key: "mattermost_token"}, {Key: "plane_api_key", Ref: "plane_api_secret", Overwrite: true},
}

func settings(t *testing.T, s string) *config.Settings {
	t.Helper()
	got, err := config.Load(document(t, s), "agent", secretKeys, noAdapters)
	if err != nil {
		t.Fatalf("Load(%s): %v", s, err)
	}
	return got
}

func settingsErr(t *testing.T, s string) string {
	t.Helper()
	got, err := config.Load(document(t, s), "agent", secretKeys, noAdapters)
	if err == nil {
		t.Fatalf("Load(%s) = %+v, want an error", s, got)
	}
	return err.Error()
}

// core returns the typed core values of s.
func core(s *config.Settings) config.Settings {
	var out config.Settings
	src, dst := reflect.ValueOf(s).Elem(), reflect.ValueOf(&out).Elem()
	for i := range src.NumField() {
		if src.Type().Field(i).IsExported() {
			dst.Field(i).Set(src.Field(i))
		}
	}
	return out
}

func defaults() config.Settings {
	return config.Settings{
		Model:                  "m",
		Temperature:            "0.7",
		ToolCallTemperature:    "0.1",
		MaxTokens:              4096,
		PublicKBMaxTokens:      6144,
		ToolFinalMaxTokens:     8192,
		MaxToolIterations:      15,
		MaxToolCalls:           20,
		ToolSummaryRole:        "user",
		ContextRole:            "user",
		ChatPlatform:           "mattermost",
		ModelProvider:          "openai",
		WebSearch:              "searxng",
		WebFetch:               "httpfetch",
		DebounceSeconds:        3,
		BufferPrefix:           "agent:buffer:",
		LastSeenKey:            "agent:last_seen",
		BotIdentityKey:         "agent:bot_identity",
		MemoryKeyPrefix:        "agent",
		KBBatchKey:             "agent:public_kb_batch",
		FactsCollection:        "agent_memory",
		KBCollection:           "agent_public_kb",
		AssistantLabel:         "agent",
		FileMaxCount:           5,
		FileMaxBytes:           20971520,
		SearchResultLimit:      5,
		MaxProgressUpdates:     2,
		ProgressUpdateMaxChars: 500,

		RestrictOutboundAfterCrossReads: true,
	}
}

const deployed = `{
	"api_base": "https://models.example/v1",
	"model": "chat-large",
	"key": "model-secret-value",
	"key_secret": "model-key",
	"mattermost_endpoint": "https://chat.example",
	"mattermost_token": "chat-token",
	"mattermost_bot_id": "bot1",
	"mattermost_bot_username": null,
	"fact_model": "chat-small",
	"embedding_model": "embed-1",
	"vision_model": null,
	"image_generation_model": "image-1",
	"tool_fallback_model": "chat-backup",
	"name_excluded_channels": ["c1", "c2"],
	"admin_ids": "u1, u2,,",
	"persona": "",
	"temperature": 0.7,
	"tool_call_temperature": 0.1,
	"max_tokens": 4096,
	"public_kb_max_tokens": 6144,
	"tool_final_max_tokens": 8192,
	"max_tool_iterations": 15,
	"max_tool_calls": 20,
	"request_timeout": 60.0,
	"plane_endpoint": "https://plane.example",
	"plane_api_secret": "plane-key",
	"plane_api_key": "plane-secret-value",
	"plane_workspace_slug": "team"
}`

func TestExistingDocumentLoadsUnchanged(t *testing.T) {
	s := settings(t, deployed)

	want := defaults()
	want.Model = "chat-large"
	want.FactModel = "chat-small"
	want.EmbeddingModel = "embed-1"
	want.ImageGenerationModel = "image-1"
	want.ToolFallbackModel = "chat-backup"
	want.NameExcludedChannels = config.Channels{IDs: []string{"c1", "c2"}}
	want.AdminIDs = []string{"u1", "u2"}
	if got := core(s); !reflect.DeepEqual(got, want) {
		t.Errorf("core settings:\n got %+v\nwant %+v", got, want)
	}

	var chat struct {
		Endpoint string `json:"mattermost_endpoint"`
		Token    string `json:"mattermost_token"`
		BotID    string `json:"mattermost_bot_id"`
		Username string `json:"mattermost_bot_username"`
	}
	var models struct {
		APIBase string      `json:"api_base"`
		Timeout json.Number `json:"request_timeout"`
	}
	var plane struct {
		Endpoint string `json:"plane_endpoint"`
		Key      string `json:"plane_api_key"`
		Slug     string `json:"plane_workspace_slug"`
	}
	if err := errors.Join(s.Decode(&chat), s.Decode(&models), s.Decode(&plane)); err != nil {
		t.Fatal(err)
	}
	if chat.Endpoint != "https://chat.example" || chat.Token != "chat-token" || chat.BotID != "bot1" ||
		chat.Username != "" {
		t.Errorf("chat settings: %+v", chat)
	}
	if models.APIBase != "https://models.example/v1" || models.Timeout != "60.0" {
		t.Errorf("model settings: %+v", models)
	}
	if plane.Endpoint != "https://plane.example" || plane.Key != "plane-secret-value" || plane.Slug != "team" {
		t.Errorf("plane settings: %+v", plane)
	}
}

func TestAbsentKeysTakeDefaults(t *testing.T) {
	if got, want := core(settings(t, `{"model": "m"}`)), defaults(); !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
}

func TestNullKeysTakeDefaults(t *testing.T) {
	s := settings(t, `{
		"model": "m", "fact_model": null, "embedding_model": null, "vision_model": null,
		"image_generation_model": null, "tool_fallback_model": null, "temperature": null,
		"tool_call_temperature": null, "max_tokens": null, "public_kb_max_tokens": null,
		"tool_final_max_tokens": null, "max_tool_iterations": null, "max_tool_calls": null, "persona": null,
		"admin_ids": null, "name_excluded_channels": null, "enabled_tools": null, "chat_platform": null,
		"model_provider": null, "web_search": null, "web_fetch": null, "debounce_seconds": null,
		"buffer_prefix": null, "last_seen_key": null, "bot_identity_key": null, "memory_key_prefix": null,
		"kb_batch_key": null, "facts_collection": null, "kb_collection": null, "assistant_label": null,
		"file_max_count": null, "file_max_bytes": null, "search_result_limit": null, "prompts": null,
		"max_progress_updates": null, "progress_update_max_chars": null, "kb_learn_from_private_channels": null,
		"tool_summary_role": null, "context_role": null, "restrict_outbound_after_cross_reads": null
	}`)
	if got, want := core(s), defaults(); !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
}

func TestFalsyValuesAreKept(t *testing.T) {
	s := settings(t, `{
		"model": "m", "fact_model": "", "temperature": 0, "tool_call_temperature": 0.0, "max_tokens": 0,
		"public_kb_max_tokens": 0, "tool_final_max_tokens": 0, "max_tool_iterations": 0, "max_tool_calls": 0,
		"persona": "", "name_excluded_channels": [], "enabled_tools": [], "chat_platform": "",
		"debounce_seconds": 0, "buffer_prefix": "", "assistant_label": "", "prompts": {},
		"restrict_outbound_after_cross_reads": false
	}`)
	want := defaults()
	want.Temperature, want.ToolCallTemperature = "0", "0.0"
	want.MaxTokens, want.PublicKBMaxTokens, want.ToolFinalMaxTokens = 0, 0, 0
	want.MaxToolIterations, want.MaxToolCalls = 0, 0
	want.NameExcludedChannels = config.Channels{IDs: []string{}}
	want.EnabledTools = []string{}
	want.ChatPlatform = ""
	want.DebounceSeconds = 0
	want.BufferPrefix = ""
	want.AssistantLabel = ""
	want.Prompts = map[string]string{}
	want.RestrictOutboundAfterCrossReads = false
	if got := core(s); !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
}

func TestDeploymentSetsNamespacingLiterals(t *testing.T) {
	s := settings(t, `{
		"model": "m", "buffer_prefix": "bot:buf:", "last_seen_key": "bot:seen", "bot_identity_key": "bot:id",
		"memory_key_prefix": "bot", "kb_batch_key": "bot:batch", "facts_collection": "bot_facts",
		"kb_collection": "bot_kb", "assistant_label": "Bot", "debounce_seconds": 2.5
	}`)
	got := []any{s.BufferPrefix, s.LastSeenKey, s.BotIdentityKey, s.MemoryKeyPrefix, s.KBBatchKey,
		s.FactsCollection, s.KBCollection, s.AssistantLabel, s.DebounceSeconds}
	want := []any{"bot:buf:", "bot:seen", "bot:id", "bot", "bot:batch", "bot_facts", "bot_kb", "Bot", 2.5}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestAdapterSelection(t *testing.T) {
	s := settings(t, `{"model": "m", "chat_platform": "telegram", "model_provider": "local",
		"web_search": "engine", "web_fetch": "browser", "enabled_tools": ["search_web", "visit_page"]}`)
	got := []string{s.ChatPlatform, s.ModelProvider, s.WebSearch, s.WebFetch}
	if want := []string{"telegram", "local", "engine", "browser"}; !slices.Equal(got, want) {
		t.Errorf("selection: got %v, want %v", got, want)
	}
	if want := []string{"search_web", "visit_page"}; !slices.Equal(s.EnabledTools, want) {
		t.Errorf("enabled_tools: got %v, want %v", s.EnabledTools, want)
	}
}

func TestToolLimitsFallBackWhenZero(t *testing.T) {
	for _, tc := range []struct {
		doc               string
		iterations, calls int
		kbTokens, final   int
	}{
		{`{"model": "m"}`, 15, 20, 6144, 8192},
		{`{"model": "m", "max_tool_iterations": 40, "max_tool_calls": 60}`, 40, 60, 6144, 8192},
		{`{"model": "m", "max_tool_iterations": 0, "max_tool_calls": 0}`, 15, 20, 6144, 8192},
		{`{"model": "m", "public_kb_max_tokens": 500, "tool_final_max_tokens": 700}`, 15, 20, 500, 700},
		{`{"model": "m", "max_tokens": 100, "public_kb_max_tokens": 0, "tool_final_max_tokens": 0}`, 15, 20, 100, 100},
	} {
		s := settings(t, tc.doc)
		got := []int{s.ToolIterations(), s.ToolCalls(), s.PublicKBTokens(), s.ToolFinalTokens()}
		if want := []int{tc.iterations, tc.calls, tc.kbTokens, tc.final}; !slices.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", tc.doc, got, want)
		}
	}
}

func TestLimitsAreSettings(t *testing.T) {
	s := settings(t, `{"model": "m", "file_max_count": 2, "file_max_bytes": 1000, "search_result_limit": 8,
		"max_progress_updates": 3, "progress_update_max_chars": 200, "tool_summary_role": "system",
		"context_role": "system"}`)
	if s.ToolSummaryRole != "system" || s.ContextRole != "system" {
		t.Errorf("tool_summary_role %q, context_role %q, want system", s.ToolSummaryRole, s.ContextRole)
	}
	if s.FileMaxCount != 2 || s.FileMaxBytes != 1000 || s.SearchResultLimit != 8 {
		t.Errorf("got %d files of %d bytes and %d search results, want 2 of 1000 and 8",
			s.FileMaxCount, s.FileMaxBytes, s.SearchResultLimit)
	}
	if s.MaxProgressUpdates != 3 || s.ProgressUpdateMaxChars != 200 {
		t.Errorf("got %d progress updates of %d code points, want 3 of 200", s.MaxProgressUpdates, s.ProgressUpdateMaxChars)
	}
}

func TestPromptOverridesAreSettings(t *testing.T) {
	s := settings(t, `{"model": "m", "prompts": {"error.generic": "Sorry.\n", "kb.query": "{\"query\"}"}}`)
	if want := map[string]string{"error.generic": "Sorry.\n", "kb.query": `{"query"}`}; !reflect.DeepEqual(s.Prompts, want) {
		t.Errorf("prompts %q, want %q", s.Prompts, want)
	}
}

func TestRoleModelFallsBackToModel(t *testing.T) {
	for doc, want := range map[string]string{
		`{"model": "m"}`:                        "m",
		`{"model": "m", "fact_model": null}`:    "m",
		`{"model": "m", "fact_model": ""}`:      "m",
		`{"model": "m", "fact_model": "small"}`: "small",
	} {
		s := settings(t, doc)
		if got := s.RoleModel(s.FactModel); got != want {
			t.Errorf("%s: got %q, want %q", doc, got, want)
		}
	}
}

func TestNumbersKeepTheirKindInShortestForm(t *testing.T) {
	for literal, want := range map[string]string{
		"1":                                    "1",
		"-0":                                   "0",
		"1.0":                                  "1.0",
		"4096.0":                               "4096.0",
		"0.7":                                  "0.7",
		"0.70":                                 "0.7",
		"-0.0":                                 "-0.0",
		"1e2":                                  "100.0",
		"1E-5":                                 "1e-05",
		"1e16":                                 "1e+16",
		"0.1000000000000000055511151231257827": "0.1",
	} {
		s := settings(t, `{"model": "m", "temperature": `+literal+`, "timeout": `+literal+`}`)
		var c adapterConfig
		if err := s.Decode(&c); err != nil {
			t.Fatal(err)
		}
		for _, n := range []json.Number{s.Temperature, c.Timeout} {
			if got, err := json.Marshal(n); err != nil || string(got) != want {
				t.Errorf("%s: serialized as %s (%v), want %s", literal, got, err, want)
			}
		}
	}
}

func TestAdminIDs(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  []string
	}{
		{`null`, nil},
		{`""`, nil},
		{`[]`, nil},
		{`0`, nil},
		{`false`, nil},
		{`7`, nil},
		{`true`, nil},
		{`{"u1": true}`, nil},
		{`"u1,u2"`, []string{"u1", "u2"}},
		{`" u1 , ,u2 ,,"`, []string{"u1", "u2"}},
		{`" u1　,\u001cu2\t"`, []string{"u1", "u2"}},
		{`["u1", " u2 ", "", "  "]`, []string{"u1", "u2"}},
		{`[1, 1.0, true, null, "x"]`, []string{"1", "1.0", "True", "None", "x"}},
		{`[["a"], {"k": 1}]`, []string{"['a']", "{'k': 1}"}},
	} {
		s := settings(t, `{"model": "m", "admin_ids": `+tc.value+`}`)
		if !slices.Equal(s.AdminIDs, tc.want) {
			t.Errorf("admin_ids %s: got %q, want %q", tc.value, s.AdminIDs, tc.want)
		}
	}
	if s := settings(t, `{"model": "m"}`); s.AdminIDs != nil {
		t.Errorf("absent admin_ids: got %q", s.AdminIDs)
	}
}

func TestIsAdminMatchesConfiguredIDsExactly(t *testing.T) {
	admins := []string{"admin_id", "other"}
	for _, tc := range []struct {
		name     string
		adminIDs []string
		userID   string
		want     bool
	}{
		{"listed user", admins, "admin_id", true},
		{"unlisted user", admins, "user_id", false},
		{"prefix of a listed ID", admins, "admin", false},
		{"different case", admins, "ADMIN_ID", false},
		{"empty list", nil, "user_id", false},
		{"empty user ID", admins, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &config.Settings{AdminIDs: tc.adminIDs}
			if got := s.IsAdmin(tc.userID); got != tc.want {
				t.Errorf("IsAdmin(%q) with admin IDs %q = %v, want %v", tc.userID, tc.adminIDs, got, tc.want)
			}
		})
	}
}

func TestExcludedChannelsMatchLikeTheirForm(t *testing.T) {
	for _, tc := range []struct {
		value, id string
		want      bool
	}{
		{`["c1", "c2"]`, "c1", true},
		{`["c1", "c2"]`, "c", false},
		{`[]`, "c1", false},
		{`"c1,c2"`, "c2", true},
		{`"c1,c2"`, "1,c", true},
		{`"c1,c2"`, "c3", false},
		{`"c1"`, "", true},
		{`""`, "", false},
		{`""`, "c1", false},
	} {
		s := settings(t, `{"model": "m", "name_excluded_channels": `+tc.value+`}`)
		if got := s.NameExcludedChannels.Has(tc.id); got != tc.want {
			t.Errorf("%s has %q: got %v, want %v", tc.value, tc.id, got, tc.want)
		}
	}
}

func TestModelIsRequired(t *testing.T) {
	for _, doc := range []string{`{}`, `{"model": null}`, `{"model": ""}`} {
		if got, want := settingsErr(t, doc), "invalid settings:\ncore: model is required"; got != want {
			t.Errorf("%s: got %q, want %q", doc, got, want)
		}
	}
	if got, want := settingsErr(t, `{"model": 0}`), "invalid settings:\ncore: model: expected string"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if s := settings(t, `{"model": " "}`); s.Model != " " {
		t.Errorf("got %q", s.Model)
	}
}

func TestWrongTypesNameTheKeyAndExpectedType(t *testing.T) {
	for _, tc := range []struct{ entry, want string }{
		{`"fact_model": 1`, "fact_model: expected string"},
		{`"temperature": "0.7"`, "temperature: expected number"},
		{`"tool_call_temperature": true`, "tool_call_temperature: expected number"},
		{`"temperature": 1e400`, "temperature: expected number"},
		{`"max_tokens": 4096.0`, "max_tokens: expected integer"},
		{`"max_tool_iterations": "15"`, "max_tool_iterations: expected integer"},
		{`"persona": []`, "persona: expected string"},
		{`"enabled_tools": "search_web"`, "enabled_tools: expected list of strings"},
		{`"enabled_tools": [1]`, "enabled_tools: expected list of strings"},
		{`"name_excluded_channels": 5`, "name_excluded_channels: expected list of strings or string"},
		{`"name_excluded_channels": [1]`, "name_excluded_channels: expected list of strings or string"},
		{`"chat_platform": {}`, "chat_platform: expected string"},
		{`"debounce_seconds": "3"`, "debounce_seconds: expected number"},
		{`"debounce_seconds": -1`, "debounce_seconds: must not be negative"},
		{`"kb_collection": 1`, "kb_collection: expected string"},
		{`"file_max_count": "5"`, "file_max_count: expected integer"},
		{`"file_max_count": 0`, "file_max_count: must be positive"},
		{`"file_max_bytes": 1.5`, "file_max_bytes: expected integer"},
		{`"file_max_bytes": -1`, "file_max_bytes: must be positive"},
		{`"search_result_limit": 0`, "search_result_limit: must be positive"},
		{`"max_progress_updates": 0`, "max_progress_updates: must be positive"},
		{`"progress_update_max_chars": -1`, "progress_update_max_chars: must be positive"},
		{`"prompts": ["x"]`, "prompts: expected object of strings"},
		{`"prompts": {"error.generic": 1}`, "prompts: expected object of strings"},
		{`"prompts": {"error.generic": null}`, "prompts: expected object of strings"},
		{`"tool_summary_role": "assistant"`, "tool_summary_role: must be user or system"},
		{`"tool_summary_role": ""`, "tool_summary_role: must be user or system"},
		{`"context_role": "assistant"`, "context_role: must be user or system"},
		{`"context_role": ""`, "context_role: must be user or system"},
	} {
		got := settingsErr(t, `{"model": "m", `+tc.entry+`}`)
		if want := "invalid settings:\ncore: " + tc.want; got != want {
			t.Errorf("%s: got %q, want %q", tc.entry, got, want)
		}
	}
}

func TestInvalidAutomationsAreReportedByPosition(t *testing.T) {
	got := settingsErr(t, `{"model": "m", "automations": [
		{"name": "ok", "pattern": "/go\\s+(.+)", "handler": "h", "args": {"what": 1}},
		{"name": "open", "pattern": "/go (", "handler": "h"},
		{"name": "boundary", "pattern": "\\bgo", "handler": "h"},
		{"name": "group", "pattern": "/go\\s+(.+)", "handler": "h", "args": {"what": 1, "where": 2}}
	]}`)
	want := "invalid settings:\n" +
		"core: automations[1]: error parsing regexp: missing closing ): `(?i)/go (`\n" +
		`core: automations[2]: textfmt: pattern "\\bgo": word boundary \b is not supported` + "\n" +
		`core: automations[3]: args["where"]: pattern has no group 2`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := settingsErr(t, `{"model": "m", "automations": {}}`); got != "invalid settings:\ncore: automations: expected list of objects" {
		t.Errorf("got %q", got)
	}
}

func TestLoadReportsEveryProblemByOwner(t *testing.T) {
	var seen *config.Settings
	adapterErr := errors.New("models: api_base is required")
	validate := func(s *config.Settings) error {
		seen = s
		return errors.Join(errors.New("chat: mattermost_token is required"), adapterErr)
	}
	_, err := config.Load(document(t, `{"temperature": "hot", "max_tool_calls": 1.5}`), "agent", nil, validate)
	want := "invalid settings:\n" +
		"core: model is required\n" +
		"core: temperature: expected number\n" +
		"core: max_tool_calls: expected integer\n" +
		"chat: mattermost_token is required\n" +
		"models: api_base is required"
	if err == nil || err.Error() != want || !errors.Is(err, adapterErr) {
		t.Fatalf("got %v, want %q wrapping the validation error", err, want)
	}
	if seen == nil {
		t.Error("the validation function did not run")
	}
}

type adapterConfig struct {
	Endpoint string      `json:"endpoint"`
	Timeout  json.Number `json:"timeout"`
	Retries  int         `json:"retries,omitempty"`
	Private  bool        `json:"allow_private"`
	Tags     []string    `json:"tags"`
	Ignored  string      `json:"-"`
	Untagged string
}

func TestDecodeLeavesAbsentAndNullKeysAlone(t *testing.T) {
	s := settings(t, `{"model": "m", "endpoint": null, "tags": null}`)
	c := adapterConfig{Endpoint: "preset", Timeout: "60.0", Tags: []string{"a"}}
	if err := s.Decode(&c); err != nil {
		t.Fatal(err)
	}
	if want := (adapterConfig{Endpoint: "preset", Timeout: "60.0", Tags: []string{"a"}}); !reflect.DeepEqual(c, want) {
		t.Errorf("got %+v, want %+v", c, want)
	}
}

func TestDecodeReadsTaggedKeysExactly(t *testing.T) {
	s := settings(t, `{"model": "m", "endpoint": "https://x.example", "timeout": 180, "retries": 2,
		"allow_private": false, "tags": [], "Ignored": "x", "Untagged": "x", "Endpoint": "other"}`)
	var c adapterConfig
	if err := s.Decode(&c); err != nil {
		t.Fatal(err)
	}
	want := adapterConfig{Endpoint: "https://x.example", Timeout: "180", Retries: 2, Tags: []string{}}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("got %+v, want %+v", c, want)
	}
}

func TestDecodeReportsEveryWrongType(t *testing.T) {
	s := settings(t, `{"model": "m", "endpoint": 1, "timeout": "60", "retries": 1.5, "allow_private": "yes",
		"tags": "a"}`)
	err := s.Decode(&adapterConfig{})
	want := "endpoint: expected string\ntimeout: expected number\nretries: expected integer\n" +
		"allow_private: expected boolean\ntags: expected list of strings"
	if err == nil || err.Error() != want {
		t.Errorf("got %v, want %q", err, want)
	}
}

func TestReloadKeepsFixedKeys(t *testing.T) {
	startup := settings(t, `{"model": "m", "chat_platform": "mattermost", "buffer_prefix": "p:"}`)
	var validated string
	next, ignored, err := startup.Reload(document(t, `{
		"model": "m2", "chat_platform": "telegram", "web_fetch": "browser", "buffer_prefix": "q:",
		"kb_collection": "other", "debounce_seconds": 5, "assistant_label": "Bot"
	}`), func(s *config.Settings) error { validated = s.ChatPlatform; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"chat_platform", "web_fetch", "buffer_prefix", "kb_collection"}; !slices.Equal(ignored, want) {
		t.Errorf("ignored: got %v, want %v", ignored, want)
	}
	want := defaults()
	want.Model = "m2"
	want.BufferPrefix = "p:"
	want.DebounceSeconds = 5
	want.AssistantLabel = "Bot"
	if got := core(next); !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
	if validated != "mattermost" {
		t.Errorf("validation saw chat_platform %q, want the startup value", validated)
	}
}

func TestReloadWithoutChangesIgnoresNothing(t *testing.T) {
	startup := settings(t, `{"model": "m"}`)
	next, ignored, err := startup.Reload(document(t, `{"model": "m", "persona": "p"}`), noAdapters)
	if err != nil || ignored != nil || next.Persona != "p" {
		t.Errorf("got %v, %v, %v", next, ignored, err)
	}
}

func TestReloadFailsLikeLoad(t *testing.T) {
	startup := settings(t, `{"model": "m"}`)
	if _, _, err := startup.Reload(document(t, `{"model": ""}`), noAdapters); err == nil ||
		!strings.Contains(err.Error(), "model is required") {
		t.Errorf("got %v", err)
	}
}

func TestRedactedMasksTheDeclaredSecretKeys(t *testing.T) {
	keys := append([]config.SecretKey{{Key: "chat_token"}, {Key: "empty_token"}, {Key: "null_token"}}, secretKeys...)
	s, err := config.Load(document(t, `{
		"model": "m", "key": "sk-1", "key_secret": "model-key", "mattermost_token": "tok",
		"plane_api_key": "pk", "plane_api_secret": "plane", "chat_token": "ct", "chat_token_secret": "env:CT",
		"empty_token": "", "empty_token_secret": "x", "null_token": null, "null_token_secret": "y",
		"extra": [1, 1.0], "undeclared": "u", "undeclared_secret": "x"
	}`), "agent", keys, noAdapters)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"chat_token":"***","chat_token_secret":"env:CT","empty_token":"","empty_token_secret":"x",` +
		`"extra":[1,1.0],"key":"***","key_secret":"model-key","mattermost_token":"***","model":"m",` +
		`"null_token":null,"null_token_secret":"y","plane_api_key":"***",` +
		`"plane_api_secret":"plane","undeclared":"u","undeclared_secret":"x"}`
	if got := string(s.Redacted()); got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}
