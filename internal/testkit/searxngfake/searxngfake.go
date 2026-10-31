// Package searxngfake fakes the SearXNG search API.
package searxngfake

import (
	"net/http"
	"testing"

	"github.com/sarpaydemir/dost/internal/testkit"
)

// Fake serves GET /search; answers are scripted. Like a SearXNG instance with only the JSON format enabled, it
// answers 403 when the request does not ask for format=json.
type Fake struct{ *testkit.Server }

// New starts a fake.
func New(t testing.TB) *Fake {
	f := &Fake{testkit.NewServer(t)}
	f.Guard(func(r testkit.Request) (testkit.Response, bool) {
		return testkit.Status(http.StatusForbidden), r.Query().Get("format") != "json"
	})
	return f
}

// Search is the search route.
func (f *Fake) Search() *testkit.Route { return f.On("GET", "/search") }

// Result is one search result.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

// Results answers a search with results.
func Results(results ...Result) testkit.Response {
	return testkit.JSON(200, map[string]any{"number_of_results": len(results), "results": append([]Result{}, results...)})
}
