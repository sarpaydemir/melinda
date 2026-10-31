package tools_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/port/web"
	"github.com/sarpaydemir/dost/internal/tools"
)

// engine is a web search that answers every search with results, or fails with err, and records each search as
// "query/limit".
type engine struct {
	results  []web.Result
	err      error
	searches []string
}

func (e *engine) Search(_ context.Context, query string, limit int) ([]web.Result, error) {
	e.searches = append(e.searches, fmt.Sprintf("%s/%d", query, limit))
	return e.results, e.err
}

// searchWeb calls search_web with args on a set whose searches go to e and logs to logs.
func searchWeb(t *testing.T, e *engine, logs *bytes.Buffer, args string) string {
	t.Helper()
	b := tools.Builtin{Search: e, SearchLimit: 3, Log: slog.New(slog.NewTextHandler(logs, nil))}
	set, err := tools.Offer(tool.Call{}, nil, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	return set.Execute(context.Background(), "search_web", json.RawMessage(args))
}

func TestSearchWebPrintsTheResultsAsAStructuredValue(t *testing.T) {
	text := func(s string) *string { return &s }
	e := &engine{results: []web.Result{
		{Title: text("It's"), URL: nil, Content: text("a\nb")},
		{Title: text(`say "hi"`), URL: text(""), Content: text("")},
		{Title: text(`both ' and "`), URL: text("u"), Content: text("tab\there é \a")},
		{Title: nil, URL: text("https://b.example"), Content: nil},
	}}
	got := searchWeb(t, e, &bytes.Buffer{}, `{"query": " weather\n"}`)
	want := `[{'title': "It's", 'url': None, 'content': 'a\nb'}, {'title': 'say "hi"', 'url': '', 'content': ''}, ` +
		`{'title': 'both \' and "', 'url': 'u', 'content': 'tab\there é \x07'}, ` +
		`{'title': None, 'url': 'https://b.example', 'content': None}]`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if want := []string{"weather/3"}; !slices.Equal(e.searches, want) {
		t.Errorf("searches %q, want %q", e.searches, want)
	}
}

func TestSearchWebReadsTheQueryOrItsAlias(t *testing.T) {
	for _, c := range []struct {
		args, want string
		searches   []string
	}{
		{`{"query": "", "q": "news"}`, "[]", []string{"news/3"}},
		{`{"query": "   "}`, "Error: Missing search query", nil},
		{`{}`, "Error: Missing search query", nil},
		{`{"query": 5}`, "Error: Could not execute search_web: 'int' object has no attribute 'strip'", nil},
	} {
		e := &engine{}
		if got := searchWeb(t, e, &bytes.Buffer{}, c.args); got != c.want || !slices.Equal(e.searches, c.searches) {
			t.Errorf("%s: got %q after searches %q, want %q after %q", c.args, got, e.searches, c.want, c.searches)
		}
	}
}

func TestSearchWebAnswersAFailedSearchAsNoResults(t *testing.T) {
	var logs bytes.Buffer
	e := &engine{err: errors.New("search engine unreachable")}
	if got := searchWeb(t, e, &logs, `{"query": "weather"}`); got != "[]" {
		t.Errorf("got %q, want []", got)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "search engine unreachable") {
		t.Errorf("logs %q, want the failure logged as an error", logs.String())
	}
}
