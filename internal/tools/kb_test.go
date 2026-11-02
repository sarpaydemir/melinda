package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/kb"
	"github.com/sarpaydemir/melinda/internal/port/tool"
	"github.com/sarpaydemir/melinda/internal/store/vector"
	"github.com/sarpaydemir/melinda/internal/testkit/fakeclock"
	"github.com/sarpaydemir/melinda/internal/tools"
)

// knowledge answers each search with the next of errs while any are left, then with hits, and records each search
// as "query/limit".
type knowledge struct {
	hits     []vector.Hit
	errs     []error
	searches []string
}

func (k *knowledge) Search(_ context.Context, query string, limit int) ([]vector.Hit, error) {
	k.searches = append(k.searches, fmt.Sprintf("%s/%d", query, limit))
	if len(k.errs) > 0 {
		err := k.errs[0]
		k.errs = k.errs[1:]
		return nil, err
	}
	return k.hits, nil
}

// entry is a hit whose document is doc.
func entry(doc string) vector.Hit { return vector.Hit{Document: &doc} }

// kbSet offers the tools of a set whose knowledge searches go to k and whose retries wait on clk.
func kbSet(t *testing.T, k *knowledge, clk clock.Clock) tools.Set {
	t.Helper()
	set, err := tools.Offer(tool.Call{}, nil, tools.Builtin{Knowledge: k, Clock: clk, Log: slog.New(slog.DiscardHandler)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// searchKB calls search_public_kb with args on kbSet(k, nil).
func searchKB(t *testing.T, k *knowledge, args string) string {
	t.Helper()
	return kbSet(t, k, nil).Execute(context.Background(), "search_public_kb", json.RawMessage(args))
}

func TestSearchPublicKBNumbersTheEntriesNearestFirst(t *testing.T) {
	k := &knowledge{hits: []vector.Hit{
		entry("Deploys run on Tuesdays."), entry("Line one\nline two"), entry(""), {}, entry("last"),
	}}
	got := searchKB(t, k, `{"query": " release notes "}`)
	if want := "[1] Deploys run on Tuesdays.\n\n[2] Line one\nline two\n\n[3] \n\n[4] None\n\n[5] last"; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if want := []string{"release notes/4"}; !slices.Equal(k.searches, want) {
		t.Errorf("searches %q, want %q", k.searches, want)
	}
}

func TestSearchPublicKBReadsTheQueryOrItsAlias(t *testing.T) {
	for _, c := range []struct {
		args, want string
		searches   []string
	}{
		{`{"query": "", "q": " news "}`, "[1] entry", []string{"news/4"}},
		{`{"query": "  "}`, "Error: Missing public KB query", nil},
		{`{}`, "Error: Missing public KB query", nil},
		{`{"query": 5}`, "Error: Could not execute search_public_kb: 'int' object has no attribute 'strip'", nil},
	} {
		k := &knowledge{hits: []vector.Hit{entry("entry")}}
		if got := searchKB(t, k, c.args); got != c.want || !slices.Equal(k.searches, c.searches) {
			t.Errorf("%s: got %q after searches %q, want %q after %q", c.args, got, k.searches, c.want, c.searches)
		}
	}
}

func TestSearchPublicKBReadsTheLimitWithinOneToFive(t *testing.T) {
	for args, want := range map[string]string{
		`{"query": "x"}`:                "x/4",
		`{"query": "x", "limit": "3"}`:  "x/3",
		`{"query": "x", "limit": 0}`:    "x/4",
		`{"query": "x", "limit": 9}`:    "x/5",
		`{"query": "x", "limit": -1}`:   "x/1",
		`{"query": "x", "limit": true}`: "x/1",
		`{"query": "x", "limit": 2.7}`:  "x/2",
	} {
		k := &knowledge{}
		searchKB(t, k, args)
		if !slices.Equal(k.searches, []string{want}) {
			t.Errorf("%s: searches %q, want %q", args, k.searches, want)
		}
	}
}

func TestSearchPublicKBSaysWhyItHasNoEntries(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"no embedding model", kb.ErrNoEmbeddingModel, "Error: Public KB search requires embedding_model"},
		{"no query embedding", kb.ErrNotEmbedded, "Error: Could not embed public KB query"},
		{"no entries", nil, "No public KB results found."},
	} {
		k := &knowledge{}
		if c.err != nil {
			k.errs = []error{c.err}
		}
		if got := searchKB(t, k, `{"query": "x"}`); got != c.want || len(k.searches) != 1 {
			t.Errorf("%s: got %q after %d searches, want %q after one", c.name, got, len(k.searches), c.want)
		}
	}
}

func TestSearchPublicKBRetriesAFailedSearch(t *testing.T) {
	refused := errors.New("connection refused")
	both := []time.Duration{500 * time.Millisecond, time.Second}
	for _, c := range []struct {
		name  string
		errs  []error
		waits []time.Duration
		want  string
	}{
		{"recovers", []error{refused, refused}, both, "[1] entry"},
		{"fails every time", []error{refused, refused, refused}, both,
			"Error: Could not execute search_public_kb: connection refused"},
		{"then cannot embed the query", []error{refused, kb.ErrNotEmbedded}, both[:1],
			"Error: Could not embed public KB query"},
	} {
		k := &knowledge{hits: []vector.Hit{entry("entry")}, errs: c.errs}
		clk := fakeclock.New(time.Unix(0, 0))
		done := make(chan string, 1)
		set := kbSet(t, k, clk)
		go func() {
			done <- set.Execute(context.Background(), "search_public_kb", json.RawMessage(`{"query": "x"}`))
		}()
		for _, wait := range c.waits {
			clk.AwaitTimers(t, 1)
			clk.Advance(wait)
		}
		if got := <-done; got != c.want || len(k.searches) != len(c.waits)+1 {
			t.Errorf("%s: got %q after %d searches, want %q after %d", c.name, got, len(k.searches), c.want, len(c.waits)+1)
		}
	}
}
