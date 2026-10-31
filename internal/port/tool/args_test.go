package tool_test

import (
	"encoding/json"
	"testing"

	"github.com/sarpaydemir/dost/internal/port/tool"
)

func arguments(t *testing.T, args string) tool.Args {
	t.Helper()
	a, err := tool.Call{Args: json.RawMessage(args)}.Arguments()
	if err != nil {
		t.Fatalf("Arguments(%s): %v", args, err)
	}
	return a
}

func TestArgumentsThatAreNotAnObjectCannotBeRead(t *testing.T) {
	for args, want := range map[string]string{
		"null":  "'NoneType' object has no attribute 'get'",
		"[]":    "'list' object has no attribute 'get'",
		"[1]":   "'list' object has no attribute 'get'",
		`"s"`:   "'str' object has no attribute 'get'",
		`""`:    "'str' object has no attribute 'get'",
		"0":     "'int' object has no attribute 'get'",
		"1":     "'int' object has no attribute 'get'",
		"1.5":   "'float' object has no attribute 'get'",
		"NaN":   "'float' object has no attribute 'get'",
		"true":  "'bool' object has no attribute 'get'",
		"false": "'bool' object has no attribute 'get'",
	} {
		if _, err := (tool.Call{Args: json.RawMessage(args)}).Arguments(); err == nil || err.Error() != want {
			t.Errorf("Arguments(%s) = %v, want %q", args, err, want)
		}
	}
}

func TestArgumentsThatAreNotJSONAreEmpty(t *testing.T) {
	for _, args := range []string{"", "{", "query=x", "{'query': 'x'}"} {
		if got, err := arguments(t, args).String("query"); got != "" || err != nil {
			t.Errorf("query of %q = %q, %v; want empty", args, got, err)
		}
	}
}

func TestStringIsTheFirstTruthyValueStripped(t *testing.T) {
	for _, c := range []struct{ args, want, err string }{
		{args: `{"query": " weather\n", "q": "news"}`, want: "weather"},
		{args: `{"query": 0, "q": " x "}`, want: "x"},
		{args: `{"query": false, "q": "y"}`, want: "y"},
		{args: `{"query": "  ", "q": "x"}`, want: ""},
		{args: `{"query": null}`, want: ""},
		{args: `{}`, want: ""},
		{args: `{"query": 5}`, err: "'int' object has no attribute 'strip'"},
		{args: `{"query": 1.5}`, err: "'float' object has no attribute 'strip'"},
		{args: `{"query": true}`, err: "'bool' object has no attribute 'strip'"},
		{args: `{"query": [1]}`, err: "'list' object has no attribute 'strip'"},
		{args: `{"query": {"a": 1}}`, err: "'dict' object has no attribute 'strip'"},
		{args: `{"query": "", "q": [1]}`, err: "'list' object has no attribute 'strip'"},
	} {
		got, err := arguments(t, c.args).String("query", "q")
		if got != c.want || (err == nil) != (c.err == "") || (err != nil && err.Error() != c.err) {
			t.Errorf("String of %s = %q, %v; want %q, %q", c.args, got, err, c.want, c.err)
		}
	}
}

func TestLookupTellsAMissingKeyFromANullOne(t *testing.T) {
	args := arguments(t, `{"ids": null, "names": []}`)
	for key, want := range map[string]bool{"ids": true, "names": true, "labels": false} {
		if v, ok := args.Lookup(key); ok != want || (key == "names") != (v != nil) {
			t.Errorf("Lookup(%q) = %v, %t; want present %t", key, v, ok, want)
		}
	}
}

func TestIntConvertsTheValueOrFallsBack(t *testing.T) {
	for args, want := range map[string]int{
		`{"limit": "7"}`:   7,
		`{"limit": 2.9}`:   2,
		`{"limit": 0}`:     4,
		`{"limit": "all"}`: 4,
		`{"last_n": 9}`:    4,
	} {
		if got := arguments(t, args).Int("limit", 4); got != want {
			t.Errorf("Int of %s = %d, want %d", args, got, want)
		}
	}
}
