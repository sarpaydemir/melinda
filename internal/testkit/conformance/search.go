package conformance

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/port/web"
)

// SearchHarness is a web search adapter wired to a fake search engine.
type SearchHarness struct {
	Searcher web.Searcher
	// Hits makes the engine answer the next search with hits titled titles, in that ranking order.
	Hits func(titles ...string)
	// Fail makes the engine fail every later search.
	Fail func()
	// Stall makes the engine hold every later search until its caller gives up.
	Stall func()
}

// Search checks a web search adapter's results and failures, each against a fresh harness from newHarness.
func Search(t *testing.T, newHarness func(*testing.T) SearchHarness) {
	t.Run("ReturnsAtMostLimitHitsInRankingOrder", func(t *testing.T) {
		h := newHarness(t)
		for _, c := range []struct {
			hits  []string
			limit int
			want  []string
		}{
			{[]string{"a", "b", "c", "d"}, 3, []string{"a", "b", "c"}},
			{[]string{"a", "b"}, 5, []string{"a", "b"}},
		} {
			h.Hits(c.hits...)
			results, err := h.Searcher.Search(t.Context(), "golang", c.limit)
			if err != nil {
				t.Fatal(err)
			}
			var titles []string
			for _, r := range results {
				if r.Title == nil {
					t.Fatalf("result %+v has no title", r)
				}
				titles = append(titles, *r.Title)
			}
			if !slices.Equal(titles, c.want) {
				t.Errorf("%d hits with limit %d: got %q, want %q", len(c.hits), c.limit, titles, c.want)
			}
		}
	})

	t.Run("NoHitsIsAnEmptyResult", func(t *testing.T) {
		h := newHarness(t)
		h.Hits()
		if results, err := h.Searcher.Search(t.Context(), "golang", 5); len(results) != 0 || err != nil {
			t.Errorf("got %+v, %v; want no results and no error", results, err)
		}
	})

	t.Run("FailuresAreErrors", func(t *testing.T) {
		h := newHarness(t)
		h.Fail()
		if _, err := h.Searcher.Search(t.Context(), "golang", 5); err == nil {
			t.Error("no error")
		}
	})

	t.Run("CancellationReturnsPromptly", func(t *testing.T) {
		h := newHarness(t)
		h.Stall()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		start := time.Now()
		if _, err := h.Searcher.Search(ctx, "golang", 5); err == nil {
			t.Error("no error")
		}
		if d := time.Since(start); d > prompt {
			t.Errorf("search returned %v after its context ended", d)
		}
	})
}
