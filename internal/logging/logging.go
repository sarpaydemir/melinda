// Package logging builds the structured logger and redacts secrets from log records.
package logging

import (
	"cmp"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// Attribute keys shared by every component.
const (
	KeyComponent    = "component"
	KeyPostID       = "post_id"
	KeyConversation = "conversation_key"
)

const redacted = "[REDACTED]"

// sensitiveNames are attribute and group names, compared case-insensitively, whose text values are masked.
var sensitiveNames = []string{
	"authorization", "password", "secret", "token", "key", "api_key", "db_password",
	"mattermost_token", "plane_api_key",
	"key_secret", "mattermost_token_secret", "plane_api_secret",
}

// New returns a logger that writes text or JSON records at level or above to w. Text attributes with a
// sensitive name are masked, and every occurrence of a value in secrets is masked in the message and in attribute
// values; once secrets holds a value, non-scalar attribute values are logged as text so that what is checked is
// what is written.
func New(w io.Writer, level slog.Level, json bool, secrets *Secrets) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: redactor(secrets)}
	if json {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// Secrets is a set of secret values that can grow while loggers built with it are in use. The zero value is an
// empty set; it is safe for concurrent use.
type Secrets struct {
	mu     sync.Mutex
	values []string
	mask   atomic.Pointer[strings.Replacer]
}

// minSecret is the length below which a value is not masked: placeholders such as "none" would garble every
// record they occur in.
const minSecret = 6

// Add adds values to the set, ignoring those shorter than six characters.
func (s *Secrets) Add(values ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.values)
	for _, v := range values {
		if utf8.RuneCountInString(v) >= minSecret && !slices.Contains(s.values, v) {
			s.values = append(s.values, v)
		}
	}
	if len(s.values) == n {
		return
	}
	// Longest first, so a secret that contains another is masked whole.
	slices.SortFunc(s.values, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	pairs := make([]string, 0, 2*len(s.values))
	for _, v := range s.values {
		pairs = append(pairs, v, redacted)
	}
	s.mask.Store(strings.NewReplacer(pairs...))
}

// replacer masks the values in the set; it is nil while the set is empty.
func (s *Secrets) replacer() *strings.Replacer {
	if s == nil {
		return nil
	}
	return s.mask.Load()
}

func redactor(secrets *Secrets) func([]string, slog.Attr) slog.Attr {
	return func(groups []string, a slog.Attr) slog.Attr {
		kind := a.Value.Kind()
		if kind != slog.KindString && kind != slog.KindAny {
			return a
		}
		if sensitive(a.Key) || slices.ContainsFunc(groups, sensitive) {
			if kind == slog.KindAny || a.Value.String() != "" {
				a.Value = slog.StringValue(redacted)
			}
			return a
		}
		mask := secrets.replacer()
		if mask == nil {
			return a
		}
		text := a.Value.String()
		if kind == slog.KindAny {
			if b, ok := a.Value.Any().([]byte); ok {
				text = string(b)
			} else {
				text = fmt.Sprintf("%+v", a.Value.Any())
			}
		}
		a.Value = slog.StringValue(mask.Replace(text))
		return a
	}
}

func sensitive(name string) bool {
	return slices.Contains(sensitiveNames, strings.ToLower(name))
}

// Clip returns s when it has at most limit characters, and otherwise its first and last limit/2 characters
// around a note of how many were omitted.
func Clip(s string, limit int) string {
	n := utf8.RuneCountInString(s)
	if n <= limit {
		return s
	}
	r := []rune(s)
	half := max(1, limit/2)
	k := min(half, n)
	return fmt.Sprintf("%s... [truncated %d chars; showing head/tail] ...%s", string(r[:k]), n-2*half, string(r[n-k:]))
}
