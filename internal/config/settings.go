// Package config loads the environment and the settings document and serves immutable settings snapshots.
package config

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"reflect"
	"slices"
	"strings"

	"github.com/sarpaydemir/dost/internal/textfmt"
)

const (
	defaultToolIterations = 15
	defaultToolCalls      = 20
)

// Settings is one immutable snapshot of the settings document: the core keys, typed, and the whole document for
// adapters to decode their own keys from. Callers must not modify it.
type Settings struct {
	Model                string      `json:"model"`
	FactModel            string      `json:"fact_model"`
	EmbeddingModel       string      `json:"embedding_model"`
	VisionModel          string      `json:"vision_model"`
	ImageGenerationModel string      `json:"image_generation_model"`
	ToolFallbackModel    string      `json:"tool_fallback_model"`
	Temperature          json.Number `json:"temperature"`
	ToolCallTemperature  json.Number `json:"tool_call_temperature"`
	MaxTokens            int         `json:"max_tokens"`
	PublicKBMaxTokens    int         `json:"public_kb_max_tokens"`
	ToolFinalMaxTokens   int         `json:"tool_final_max_tokens"`
	MaxToolIterations    int         `json:"max_tool_iterations"`
	MaxToolCalls         int         `json:"max_tool_calls"`
	Persona              string      `json:"persona"`
	// AdminIDs is admin_ids, which the document may give as a list or a comma-separated string.
	AdminIDs             []string `json:"-"`
	NameExcludedChannels Channels `json:"name_excluded_channels"`
	// EnabledTools is nil when every offered tool is enabled.
	EnabledTools []string `json:"enabled_tools"`

	ChatPlatform  string `json:"chat_platform"`
	ModelProvider string `json:"model_provider"`
	WebSearch     string `json:"web_search"`
	WebFetch      string `json:"web_fetch"`

	DebounceSeconds float64 `json:"debounce_seconds"`
	BufferPrefix    string  `json:"buffer_prefix"`
	LastSeenKey     string  `json:"last_seen_key"`
	BotIdentityKey  string  `json:"bot_identity_key"`
	MemoryKeyPrefix string  `json:"memory_key_prefix"`
	KBBatchKey      string  `json:"kb_batch_key"`
	FactsCollection string  `json:"facts_collection"`
	KBCollection    string  `json:"kb_collection"`
	AssistantLabel  string  `json:"assistant_label"`

	// FileMaxCount and FileMaxBytes bound the attachments of one message: how many are read and the size of each.
	FileMaxCount int   `json:"file_max_count"`
	FileMaxBytes int64 `json:"file_max_bytes"`

	// Prompts replaces the texts of prompt catalog entries, by ID.
	Prompts map[string]string `json:"prompts"`

	agentID string
	secrets []SecretKey
	doc     map[string]json.RawMessage
}

// Load builds a snapshot from a settings document whose secrets are resolved. The snapshot keeps doc, which the
// caller must not modify afterwards, and secrets, the keys Redacted masks. validate checks the selected adapters'
// settings; the error lists every core problem and validate's error.
func Load(doc map[string]json.RawMessage, agentID string, secrets []SecretKey, validate func(*Settings) error) (*Settings, error) {
	s, problems := build(doc, agentID, secrets)
	if err := check(s, problems, validate); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload builds the snapshot that follows s from a new document, as Load does, but keeps the secret keys,
// namespacing, and adapter selection values of s. It returns the keys whose new values it ignored.
func (s *Settings) Reload(doc map[string]json.RawMessage, validate func(*Settings) error) (*Settings, []string, error) {
	next, problems := build(doc, s.agentID, s.secrets)
	var ignored []string
	kept := s.fixed()
	for i, f := range next.fixed() {
		if *f.value != *kept[i].value {
			ignored = append(ignored, f.key)
			*f.value = *kept[i].value
		}
	}
	if err := check(next, problems, validate); err != nil {
		return nil, nil, err
	}
	return next, ignored, nil
}

// Decode sets the fields of the struct v points to from the document keys named by their json tags. Absent and
// null keys leave a field as it is. A value of the wrong JSON type is an error naming the key.
func (s *Settings) Decode(v any) error {
	return errors.Join(s.decode(v)...)
}

// Redacted returns the document as JSON with the values of the secret keys replaced by "***".
func (s *Settings) Redacted() []byte {
	out := maps.Clone(s.doc)
	for _, secret := range s.secrets {
		if v, ok := out[secret.Key]; ok && string(v) != "null" && string(v) != `""` {
			out[secret.Key] = json.RawMessage(`"***"`)
		}
	}
	b, _ := json.Marshal(out) // a map of valid raw values always marshals
	return b
}

// ToolIterations is max_tool_iterations, or 15 when it is 0.
func (s *Settings) ToolIterations() int { return cmp.Or(s.MaxToolIterations, defaultToolIterations) }

// ToolCalls is max_tool_calls, or 20 when it is 0.
func (s *Settings) ToolCalls() int { return cmp.Or(s.MaxToolCalls, defaultToolCalls) }

// PublicKBTokens is public_kb_max_tokens, or max_tokens when it is 0.
func (s *Settings) PublicKBTokens() int { return cmp.Or(s.PublicKBMaxTokens, s.MaxTokens) }

// ToolFinalTokens is tool_final_max_tokens, or max_tokens when it is 0.
func (s *Settings) ToolFinalTokens() int { return cmp.Or(s.ToolFinalMaxTokens, s.MaxTokens) }

// RoleModel returns m, the value of a <role>_model key, or model when m is empty.
func (s *Settings) RoleModel(m string) string { return cmp.Or(m, s.Model) }

// IsAdmin reports whether userID is listed exactly in admin_ids.
func (s *Settings) IsAdmin(userID string) bool { return slices.Contains(s.AdminIDs, userID) }

// Channels is the name_excluded_channels value: a list of channel IDs, or a string the IDs are substrings of.
type Channels struct {
	IDs  []string
	Text string
}

// Has reports whether id is in the list, or a substring of a non-empty string.
func (c Channels) Has(id string) bool {
	if c.Text != "" {
		return strings.Contains(c.Text, id)
	}
	return slices.Contains(c.IDs, id)
}

// UnmarshalJSON accepts a list of strings or a string.
func (c *Channels) UnmarshalJSON(b []byte) error {
	*c = Channels{}
	if json.Unmarshal(b, &c.Text) != nil && json.Unmarshal(b, &c.IDs) != nil {
		return errors.New("expected list of strings or string")
	}
	return nil
}

func build(doc map[string]json.RawMessage, agentID string, secrets []SecretKey) (*Settings, []error) {
	s := &Settings{
		Temperature:         "0.7",
		ToolCallTemperature: "0.1",
		MaxTokens:           4096,
		PublicKBMaxTokens:   6144,
		ToolFinalMaxTokens:  8192,
		MaxToolIterations:   defaultToolIterations,
		MaxToolCalls:        defaultToolCalls,
		ChatPlatform:        "mattermost",
		ModelProvider:       "openai",
		WebSearch:           "searxng",
		WebFetch:            "httpfetch",
		DebounceSeconds:     3,
		BufferPrefix:        agentID + ":buffer:",
		LastSeenKey:         agentID + ":last_seen",
		BotIdentityKey:      agentID + ":bot_identity",
		MemoryKeyPrefix:     agentID,
		KBBatchKey:          agentID + ":public_kb_batch",
		FactsCollection:     agentID + "_memory",
		KBCollection:        agentID + "_public_kb",
		AssistantLabel:      agentID,
		FileMaxCount:        5,
		FileMaxBytes:        20 << 20,
		agentID:             agentID,
		secrets:             secrets,
		doc:                 doc,
	}
	var problems []error
	if raw := doc["model"]; raw == nil || string(raw) == "null" || string(raw) == `""` {
		problems = append(problems, errors.New("model is required"))
	}
	problems = append(problems, s.decode(s)...)
	if s.DebounceSeconds < 0 {
		problems = append(problems, errors.New("debounce_seconds: must not be negative"))
	}
	if s.FileMaxCount < 1 {
		problems = append(problems, errors.New("file_max_count: must be positive"))
	}
	if s.FileMaxBytes < 1 {
		problems = append(problems, errors.New("file_max_bytes: must be positive"))
	}
	// A null entry would otherwise decode as empty text.
	var prompts map[string]*string
	if json.Unmarshal(doc["prompts"], &prompts) == nil && slices.Contains(slices.Collect(maps.Values(prompts)), nil) {
		problems = append(problems, errors.New("prompts: expected object of strings"))
	}
	if raw, ok := doc["admin_ids"]; ok {
		ids, err := adminIDs(raw)
		if err != nil {
			problems = append(problems, fmt.Errorf("admin_ids: %w", err))
		}
		s.AdminIDs = ids
	}
	return s, problems
}

// check reports the core problems and validate's error together.
func check(s *Settings, problems []error, validate func(*Settings) error) error {
	for i, p := range problems {
		problems[i] = fmt.Errorf("core: %w", p)
	}
	if err := errors.Join(append(problems, validate(s))...); err != nil {
		return fmt.Errorf("invalid settings:\n%w", err)
	}
	return nil
}

type fixedKey struct {
	key   string
	value *string
}

// fixed are the keys whose startup values hold for the life of the process.
func (s *Settings) fixed() []fixedKey {
	return []fixedKey{
		{"chat_platform", &s.ChatPlatform},
		{"model_provider", &s.ModelProvider},
		{"web_search", &s.WebSearch},
		{"web_fetch", &s.WebFetch},
		{"buffer_prefix", &s.BufferPrefix},
		{"last_seen_key", &s.LastSeenKey},
		{"bot_identity_key", &s.BotIdentityKey},
		{"memory_key_prefix", &s.MemoryKeyPrefix},
		{"kb_batch_key", &s.KBBatchKey},
		{"facts_collection", &s.FactsCollection},
		{"kb_collection", &s.KBCollection},
	}
}

var numberType = reflect.TypeFor[json.Number]()

func (s *Settings) decode(v any) []error {
	rv := reflect.ValueOf(v).Elem()
	var errs []error
	for i := range rv.NumField() {
		f := rv.Type().Field(i)
		key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		raw := s.doc[key]
		if !f.IsExported() || key == "" || key == "-" || raw == nil || string(raw) == "null" {
			continue
		}
		var err error
		if f.Type == numberType {
			err = setNumber(rv.Field(i), raw)
		} else {
			err = json.Unmarshal(raw, rv.Field(i).Addr().Interface())
		}
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &typeErr):
			errs = append(errs, fmt.Errorf("%s: expected %s", key, jsonType(f.Type)))
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
	}
	return errs
}

// setNumber stores a finite JSON number as requests serialize it: integers as written, other numbers in shortest
// round-trip form with ".0" on integral values.
func setNumber(field reflect.Value, raw json.RawMessage) error {
	v, _ := textfmt.Decode(raw, false)
	switch n := v.(type) {
	case int64, *big.Int:
	case float64:
		if math.IsInf(n, 0) {
			return &json.UnmarshalTypeError{}
		}
	default:
		return &json.UnmarshalTypeError{}
	}
	field.SetString(textfmt.Repr(v))
	return nil
}

func jsonType(t reflect.Type) string {
	switch k := t.Kind(); {
	case t == numberType, k == reflect.Float32, k == reflect.Float64:
		return "number"
	case k == reflect.String:
		return "string"
	case k == reflect.Bool:
		return "boolean"
	case k >= reflect.Int && k <= reflect.Uint64:
		return "integer"
	case k == reflect.Slice:
		return "list of " + jsonType(t.Elem()) + "s"
	case k == reflect.Map:
		return "object of " + jsonType(t.Elem()) + "s"
	}
	return "object"
}

// adminIDs takes the strings of a list's elements, or the comma-separated parts of a string, stripped and
// without empties. Any other value gives none.
func adminIDs(raw json.RawMessage) ([]string, error) {
	v, err := textfmt.Decode(raw, false)
	if err != nil {
		return nil, err
	}
	var parts []string
	switch v := v.(type) {
	case []any:
		for _, x := range v {
			parts = append(parts, textfmt.Str(x))
		}
	case string:
		parts = strings.Split(v, ",")
	}
	var ids []string
	for _, p := range parts {
		if p = textfmt.Strip(p); p != "" {
			ids = append(ids, p)
		}
	}
	return ids, nil
}
