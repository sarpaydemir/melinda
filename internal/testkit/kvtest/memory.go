// Package kvtest provides an in-memory key-value store for unit tests and the contract every store must pass.
package kvtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/sarpaydemir/melinda/internal/store/kv"
)

// Memory is an in-memory Store; the zero value is empty and ready to use. Update and UpdateAll hold the store's
// lock while their functions run, so those must not call the store.
type Memory struct {
	mu     sync.Mutex
	values map[string]json.RawMessage
}

// Get returns the value under key; ok is false when there is none or it is JSON null.
func (m *Memory) Get(ctx context.Context, key string) (json.RawMessage, bool, error) {
	if err := m.lock(ctx); err != nil {
		return nil, false, err
	}
	defer m.mu.Unlock()
	v, ok := m.values[key]
	if !ok || isNull(v) {
		return nil, false, nil
	}
	return slices.Clone(v), true, nil
}

// Set stores value under key.
func (m *Memory) Set(ctx context.Context, key string, value json.RawMessage) error {
	if err := m.lock(ctx); err != nil {
		return err
	}
	defer m.mu.Unlock()
	return m.put(map[string]json.RawMessage{key: value})
}

// Update applies one change.
func (m *Memory) Update(ctx context.Context, key string, seed json.RawMessage, fn func(json.RawMessage) (json.RawMessage, error)) error {
	return m.UpdateAll(ctx, kv.Change{Key: key, Seed: seed, Fn: fn})
}

// UpdateAll applies the changes in order as one write; each Fn receives its Seed ({} when nil) when the key is
// missing or holds JSON null. An error from any Fn is returned and nothing is written.
func (m *Memory) UpdateAll(ctx context.Context, changes ...kv.Change) error {
	if err := m.lock(ctx); err != nil {
		return err
	}
	defer m.mu.Unlock()
	next := map[string]json.RawMessage{}
	for _, c := range changes {
		cur, ok := next[c.Key]
		if !ok {
			cur, ok = m.values[c.Key]
		}
		if !ok || isNull(cur) {
			cur = c.Seed
			if cur == nil {
				cur = json.RawMessage(`{}`)
			}
		}
		v, err := c.Fn(slices.Clone(cur))
		if err != nil {
			return err
		}
		next[c.Key] = v
	}
	return m.put(next)
}

// DeleteIf removes key if pred holds for its value and returns the removed value.
func (m *Memory) DeleteIf(ctx context.Context, key string, pred func(json.RawMessage) bool) (json.RawMessage, bool, error) {
	if err := m.lock(ctx); err != nil {
		return nil, false, err
	}
	defer m.mu.Unlock()
	v, ok := m.values[key]
	if !ok || !pred(slices.Clone(v)) {
		return nil, false, nil
	}
	delete(m.values, key)
	return v, true, nil
}

// List returns the entries whose keys start with prefix.
func (m *Memory) List(ctx context.Context, prefix string) (map[string]json.RawMessage, error) {
	if err := m.lock(ctx); err != nil {
		return nil, err
	}
	defer m.mu.Unlock()
	out := map[string]json.RawMessage{}
	for k, v := range m.values {
		if strings.HasPrefix(k, prefix) {
			out[k] = slices.Clone(v)
		}
	}
	return out, nil
}

// lock acquires the store's lock unless ctx is already done.
func (m *Memory) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	return nil
}

// put stores a copy of every value, or none when one is not valid JSON; the caller holds the lock.
func (m *Memory) put(values map[string]json.RawMessage) error {
	for key, v := range values {
		if !json.Valid(v) {
			return fmt.Errorf("value for %q is not valid JSON", key)
		}
	}
	if m.values == nil {
		m.values = map[string]json.RawMessage{}
	}
	for key, v := range values {
		m.values[key] = slices.Clone(v)
	}
	return nil
}

func isNull(v json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(v), []byte("null")) }
