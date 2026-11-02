package kvtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/sarpaydemir/melinda/internal/store/kv"
	"github.com/sarpaydemir/melinda/internal/testkit"
)

// Store is the key-value store behavior Contract checks.
type Store interface {
	Get(ctx context.Context, key string) (json.RawMessage, bool, error)
	Set(ctx context.Context, key string, value json.RawMessage) error
	Update(ctx context.Context, key string, seed json.RawMessage, fn func(json.RawMessage) (json.RawMessage, error)) error
	UpdateAll(ctx context.Context, changes ...kv.Change) error
	DeleteIf(ctx context.Context, key string, pred func(json.RawMessage) bool) (json.RawMessage, bool, error)
	List(ctx context.Context, prefix string) (map[string]json.RawMessage, error)
}

// Contract checks the behavior every Store must have. open returns an empty store for one subtest.
func Contract(t *testing.T, open func(t *testing.T) Store) {
	cases := []struct {
		name string
		run  func(t *testing.T, s Store)
	}{
		{"a missing key is not found", func(t *testing.T, s Store) {
			assertValue(t, s, "k", "")
		}},
		{"a value round-trips with every field and number literal intact", func(t *testing.T, s Store) {
			const v = `{"generation": "6f1c", "flush_after": "2026-10-31T09:30:00.123456+00:00",
				"create_at": 1780000000000, "ratio": 2.0, "seconds": 1780000000.25, "big": 123456789012345678901234567890,
				"flag": false, "none": null, "messages": [{"post_id": "p1", "file_ids": [], "extra": {"nested": ["ü", 1]}}]}`
			in := json.RawMessage(v)
			if err := s.Set(t.Context(), "k", in); err != nil {
				t.Fatal(err)
			}
			clear(in)
			got := assertValue(t, s, "k", v)
			clear(got)
			assertValue(t, s, "k", v)
		}},
		{"set replaces the whole value", func(t *testing.T, s Store) {
			mustSet(t, s, "k", `{"x": 1, "old": true}`)
			mustSet(t, s, "k", `{"x": 2, "y": 3}`)
			assertValue(t, s, "k", `{"x": 2, "y": 3}`)
		}},
		{"a stored JSON null is not found", func(t *testing.T, s Store) {
			mustSet(t, s, "k", "null")
			assertValue(t, s, "k", "")
		}},
		{"delete if removes the value only when the predicate holds", func(t *testing.T, s Store) {
			called := false
			v, ok, err := s.DeleteIf(t.Context(), "missing", func(json.RawMessage) bool { called = true; return true })
			if err != nil || ok || v != nil || called {
				t.Errorf("DeleteIf on a missing key = %s, %v, %v, predicate called %v; want nothing deleted, no call",
					v, ok, err, called)
			}
			const stored = `{"generation": "g1", "messages": []}`
			mustSet(t, s, "k", stored)
			for _, c := range []struct {
				generation string
				want       bool
			}{{"g2", false}, {"g1", true}} {
				var seen json.RawMessage
				v, ok, err := s.DeleteIf(t.Context(), "k", func(cur json.RawMessage) bool {
					seen = cur
					var b struct{ Generation string }
					return json.Unmarshal(cur, &b) == nil && b.Generation == c.generation
				})
				if err != nil || ok != c.want {
					t.Errorf("DeleteIf(generation %s) = %v, %v; want %v", c.generation, ok, err, c.want)
				}
				if !equalJSON(seen, stored) {
					t.Errorf("predicate saw %s, want %s", seen, stored)
				}
				if c.want && !equalJSON(v, stored) {
					t.Errorf("DeleteIf returned %s, want the deleted value %s", v, stored)
				}
				if !c.want {
					assertValue(t, s, "k", stored)
				}
			}
			assertValue(t, s, "k", "")
		}},
		{"update seeds a missing key", func(t *testing.T, s Store) {
			for _, c := range []struct{ key, seed, want string }{{"nil", "", `{}`}, {"given", `{"messages": []}`, `{"messages": []}`}} {
				var seed json.RawMessage
				if c.seed != "" {
					seed = json.RawMessage(c.seed)
				}
				var seen json.RawMessage
				mustUpdate(t, s, c.key, seed, func(cur json.RawMessage) (json.RawMessage, error) {
					seen = cur
					return json.RawMessage(`{"collected": 1}`), nil
				})
				if !equalJSON(seen, c.want) {
					t.Errorf("seed %q: fn saw %s, want %s", c.seed, seen, c.want)
				}
				assertValue(t, s, c.key, `{"collected": 1}`)
			}
		}},
		{"update passes the stored value and stores the result", func(t *testing.T, s Store) {
			mustSet(t, s, "k", `{"counter": 0}`)
			mustUpdate(t, s, "k", nil, increment)
			mustUpdate(t, s, "k", nil, increment)
			assertValue(t, s, "k", `{"counter": 2}`)
		}},
		{"update passes the seed when the stored value is JSON null", func(t *testing.T, s Store) {
			mustSet(t, s, "k", "null")
			var seen json.RawMessage
			mustUpdate(t, s, "k", json.RawMessage(`{"messages": []}`), func(cur json.RawMessage) (json.RawMessage, error) {
				seen = cur
				return cur, nil
			})
			if !equalJSON(seen, `{"messages": []}`) {
				t.Errorf("fn saw %s, want the seed", seen)
			}
			assertValue(t, s, "k", `{"messages": []}`)
		}},
		{"an update that fails writes nothing", func(t *testing.T, s Store) {
			mustSet(t, s, "k", `{"counter": 1}`)
			failure := errors.New("updater failed")
			fail := func(json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{"counter": 9}`), failure }
			for _, key := range []string{"k", "new"} {
				if err := s.Update(t.Context(), key, nil, fail); !errors.Is(err, failure) {
					t.Errorf("Update(%s) = %v, want the updater's error", key, err)
				}
			}
			assertValue(t, s, "k", `{"counter": 1}`)
			assertValue(t, s, "new", "")
		}},
		{"update all applies each change to its own key", func(t *testing.T, s Store) {
			mustSet(t, s, "a", `{"counter": 1}`)
			if err := s.UpdateAll(t.Context(),
				kv.Change{Key: "a", Fn: increment},
				kv.Change{Key: "b", Seed: json.RawMessage(`{"counter": 5}`), Fn: increment},
			); err != nil {
				t.Fatalf("UpdateAll: %v", err)
			}
			assertValue(t, s, "a", `{"counter": 2}`)
			assertValue(t, s, "b", `{"counter": 6}`)
		}},
		{"an update all with one failing change writes nothing", func(t *testing.T, s Store) {
			mustSet(t, s, "a", `{"counter": 1}`)
			failure := errors.New("updater failed")
			fail := func(json.RawMessage) (json.RawMessage, error) { return nil, failure }
			for _, changes := range [][]kv.Change{
				{{Key: "a", Fn: increment}, {Key: "new", Fn: fail}},
				{{Key: "new", Fn: increment}, {Key: "a", Fn: fail}},
			} {
				if err := s.UpdateAll(t.Context(), changes...); !errors.Is(err, failure) {
					t.Errorf("UpdateAll = %v, want the updater's error", err)
				}
			}
			assertValue(t, s, "a", `{"counter": 1}`)
			assertValue(t, s, "new", "")
		}},
		{"a value that is not JSON is rejected", func(t *testing.T, s Store) {
			mustSet(t, s, "k", `{"counter": 1}`)
			if err := s.Set(t.Context(), "k", json.RawMessage(`{"counter":`)); err == nil {
				t.Error("Set accepted invalid JSON")
			}
			if err := s.Update(t.Context(), "k", nil, func(json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"counter":`), nil
			}); err == nil {
				t.Error("Update stored invalid JSON")
			}
			assertValue(t, s, "k", `{"counter": 1}`)
		}},
		{"concurrent updates of one key all apply", func(t *testing.T, s Store) {
			mustSet(t, s, "k", `{"counter": 0}`)
			incrementConcurrently(t, s, "k", 50)
		}},
		{"concurrent first updates of a new key all apply", func(t *testing.T, s Store) {
			incrementConcurrently(t, s, "k", 50)
		}},
		{"list matches keys that literally start with the prefix", func(t *testing.T, s Store) {
			for _, k := range []string{`a_1`, `a_2`, `ab`, `a%1`, `aX`, `a\1`, `b_1`} {
				mustSet(t, s, k, fmt.Sprintf(`{"key": %q}`, k))
			}
			for prefix, want := range map[string][]string{
				`a_`: {`a_1`, `a_2`},
				`a%`: {`a%1`},
				`a\`: {`a\1`},
				`a`:  {`a%1`, `aX`, `a\1`, `a_1`, `a_2`, `ab`},
				`c`:  nil,
			} {
				got, err := s.List(t.Context(), prefix)
				if err != nil {
					t.Fatalf("List(%q): %v", prefix, err)
				}
				if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, want) {
					t.Errorf("List(%q) keys = %q, want %q", prefix, keys, want)
				}
				for k, v := range got {
					if want := fmt.Sprintf(`{"key": %q}`, k); !equalJSON(v, want) {
						t.Errorf("List(%q)[%q] = %s, want %s", prefix, k, v, want)
					}
				}
			}
		}},
		{"every method fails on a cancelled context", func(t *testing.T, s Store) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			v := json.RawMessage(`{}`)
			calls := map[string]func() error{
				"Get":    func() error { _, _, err := s.Get(ctx, "k"); return err },
				"Set":    func() error { return s.Set(ctx, "k", v) },
				"Update": func() error { return s.Update(ctx, "k", nil, increment) },
				"UpdateAll": func() error {
					return s.UpdateAll(ctx, kv.Change{Key: "k", Fn: increment})
				},
				"DeleteIf": func() error {
					_, _, err := s.DeleteIf(ctx, "k", func(json.RawMessage) bool { return true })
					return err
				},
				"List": func() error { _, err := s.List(ctx, ""); return err },
			}
			for name, call := range calls {
				if err := call(); err == nil {
					t.Errorf("%s returned no error", name)
				}
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.run(t, open(t))
		})
	}
}

func increment(cur json.RawMessage) (json.RawMessage, error) {
	var v struct{ Counter int }
	if err := json.Unmarshal(cur, &v); err != nil {
		return nil, err
	}
	return json.RawMessage(fmt.Sprintf(`{"counter": %d}`, v.Counter+1)), nil
}

func incrementConcurrently(t *testing.T, s Store, key string, n int) {
	t.Helper()
	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			errs[i] = s.Update(t.Context(), key, nil, increment)
		})
	}
	close(start)
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Errorf("concurrent Update: %v", err)
	}
	assertValue(t, s, key, fmt.Sprintf(`{"counter": %d}`, n))
}

func mustSet(t *testing.T, s Store, key, value string) {
	t.Helper()
	if err := s.Set(t.Context(), key, json.RawMessage(value)); err != nil {
		t.Fatalf("Set(%s, %s): %v", key, value, err)
	}
}

func mustUpdate(t *testing.T, s Store, key string, seed json.RawMessage, fn func(json.RawMessage) (json.RawMessage, error)) {
	t.Helper()
	if err := s.Update(t.Context(), key, seed, fn); err != nil {
		t.Fatalf("Update(%s): %v", key, err)
	}
}

// assertValue checks that key holds a value JSON-equal to want, or is not found when want is empty, and returns
// what Get returned.
func assertValue(t *testing.T, s Store, key, want string) json.RawMessage {
	t.Helper()
	got, ok, err := s.Get(t.Context(), key)
	switch {
	case err != nil:
		t.Fatalf("Get(%s): %v", key, err)
	case want == "" && ok:
		t.Errorf("Get(%s) = %s, want not found", key, got)
	case want != "" && (!ok || !equalJSON(got, want)):
		t.Errorf("Get(%s) = %s, %v; want %s", key, got, ok, want)
	}
	return got
}

// equalJSON reports whether a and b are valid JSON encoding the same value.
func equalJSON(a json.RawMessage, b string) bool {
	ok, err := testkit.JSONEqual(a, []byte(b))
	return err == nil && ok
}
