package searxng

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/web"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/conformance"
	"github.com/sarpaydemir/dost/internal/testkit/searxngfake"
)

func settings(t *testing.T, doc string) *config.Settings {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatal(err)
	}
	s, err := config.Load(m, "agent", nil, func(*config.Settings) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// open builds the searcher for the instance at base with transport; fields are more members of the settings
// document.
func open(t *testing.T, base string, transport http.RoundTripper, fields ...string) web.Searcher {
	t.Helper()
	doc := `{"model":"m"`
	for _, f := range fields {
		doc += "," + f
	}
	s, err := New(settings(t, doc+"}"), base, transport)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConformance(t *testing.T) {
	conformance.Search(t, func(t *testing.T) conformance.SearchHarness {
		fake := searxngfake.New(t)
		return conformance.SearchHarness{
			Searcher: open(t, fake.URL(), http.DefaultTransport),
			Hits: func(titles ...string) {
				hits := []searxngfake.Result{}
				for _, title := range titles {
					hits = append(hits, searxngfake.Result{Title: title, URL: "https://" + title + ".example"})
				}
				fake.Search().Reply(searxngfake.Results(hits...))
			},
			Fail:  func() { fake.Search().Default(testkit.Status(http.StatusInternalServerError)) },
			Stall: func() { fake.Search().Default(searxngfake.Results().WithDelay(time.Hour)) },
		}
	})
}

func TestRequestCarriesTheEncodedQueryAndTheConfiguredLanguage(t *testing.T) {
	for _, c := range []struct {
		base, field, query, want string
	}{
		{"", `"search_language":"tr-TR"`, "hello world & more ç ğ ü /?=+#%~*'()!;,:@$",
			"GET /search?q=hello+world+%26+more+%C3%A7+%C4%9F+%C3%BC+%2F%3F%3D%2B%23%25~%2A%27%28%29%21%3B%2C%3A%40%24" +
				"&format=json&language=tr-TR&safesearch=0"},
		{"//", `"search_language":"en-US"`, "news", "GET /search?q=news&format=json&language=en-US&safesearch=0"},
		{"/", `"search_language":""`, "news", "GET /search?q=news&format=json&safesearch=0"},
		{"", `"search_language":null`, "news", "GET /search?q=news&format=json&safesearch=0"},
		{"", `"web_search":"searxng"`, "news", "GET /search?q=news&format=json&safesearch=0"},
	} {
		fake := searxngfake.New(t)
		fake.Search().Reply(searxngfake.Results())
		if _, err := open(t, fake.URL()+c.base, http.DefaultTransport, c.field).Search(t.Context(), c.query, 5); err != nil {
			t.Errorf("%s: %v", c.field, err)
		}
		fake.AssertCalls(t, c.want)
		// Only the headers the transport adds itself.
		want := http.Header{"Accept-Encoding": {"gzip"}, "User-Agent": {"Go-http-client/1.1"}}
		if h := fake.Requests()[0].Header; fmt.Sprint(h) != fmt.Sprint(want) {
			t.Errorf("headers %v, want %v", h, want)
		}
	}
}

func TestResultsKeepNullFieldsAndFillMissingOnes(t *testing.T) {
	fake := searxngfake.New(t)
	fake.Search().Reply(testkit.Raw(http.StatusOK, "application/json", []byte(`{"results": [
		{"title": "It's", "url": null, "content": "a\nb", "engine": "e"},
		{"title": "say \"hi\""},
		{"content": null, "title": null, "url": "https://b.example"},
		{"url": "https://c.example", "content": ""},
		"beyond the limit"
	]}`)))
	got, err := open(t, fake.URL(), http.DefaultTransport).Search(t.Context(), "q", 4)
	if err != nil {
		t.Fatal(err)
	}
	text := func(s string) *string { return &s }
	want := []web.Result{
		{Title: text("It's"), URL: nil, Content: text("a\nb")},
		{Title: text(`say "hi"`), URL: text(""), Content: text("")},
		{Title: nil, URL: text("https://b.example"), Content: nil},
		{Title: text(""), URL: text("https://c.example"), Content: text("")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %s, want %s", show(got), show(want))
	}
}

func TestAnswersWithoutAResultsListAreEmpty(t *testing.T) {
	for _, body := range []string{`{"answers": []}`, `{"results": []}`} {
		fake := searxngfake.New(t)
		fake.Search().Reply(testkit.Raw(http.StatusCreated, "application/json", []byte(body)))
		if got, err := open(t, fake.URL(), http.DefaultTransport).Search(t.Context(), "q", 5); len(got) != 0 || err != nil {
			t.Errorf("%s: got %s, %v; want no results", body, show(got), err)
		}
	}
}

func TestUnusableAnswersAreErrors(t *testing.T) {
	results := `{"results": [{"title": "A"}]}`
	for name, resp := range map[string]testkit.Response{
		"server error":         testkit.Raw(http.StatusInternalServerError, "application/json", []byte(results)),
		"redirect":             {Status: http.StatusFound, Header: http.Header{"Location": {"/search?q=q&format=json"}}},
		"invalid JSON":         testkit.Raw(http.StatusOK, "application/json", []byte(`{"results": [`)),
		"not an object":        testkit.Raw(http.StatusOK, "application/json", []byte(`[1]`)),
		"null answer":          testkit.Raw(http.StatusOK, "application/json", []byte(`null`)),
		"null results":         testkit.Raw(http.StatusOK, "application/json", []byte(`{"results": null}`)),
		"results not a list":   testkit.Raw(http.StatusOK, "application/json", []byte(`{"results": {"a": 1}}`)),
		"null hit":             testkit.Raw(http.StatusOK, "application/json", []byte(`{"results": [null]}`)),
		"hit not an object":    testkit.Raw(http.StatusOK, "application/json", []byte(`{"results": ["A"]}`)),
		"body larger than cap": testkit.Raw(http.StatusOK, "application/json", padded(results, maxBody+1)),
	} {
		fake := searxngfake.New(t)
		fake.Search().Reply(resp)
		if got, err := open(t, fake.URL(), http.DefaultTransport).Search(t.Context(), "q", 5); err == nil {
			t.Errorf("%s: got %s, want an error", name, show(got))
		}
		if n := len(fake.Requests()); n != 1 {
			t.Errorf("%s: %d requests, want 1", name, n)
		}
	}
}

func TestBodiesUpToTheCapAreRead(t *testing.T) {
	fake := searxngfake.New(t)
	fake.Search().Reply(testkit.Raw(http.StatusOK, "application/json", padded(`{"results": [{"title": "A"}]}`, maxBody)))
	if got, err := open(t, fake.URL(), http.DefaultTransport).Search(t.Context(), "q", 5); len(got) != 1 || err != nil {
		t.Errorf("got %s, %v; want one result", show(got), err)
	}
}

func TestSearchersOfLaterSnapshotsReuseConnections(t *testing.T) {
	fake := searxngfake.New(t)
	fake.Search().Default(searxngfake.Results(searxngfake.Result{Title: "A"}))
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	var reused []bool
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = append(reused, info.Reused) },
	})
	for range 3 {
		if _, err := open(t, fake.URL(), transport).Search(ctx, "q", 5); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(reused) != "[false true true]" {
		t.Errorf("connections reused: %v, want [false true true]", reused)
	}
}

func TestRequestsTimeOutAfterTenSeconds(t *testing.T) {
	if got := open(t, "http://search.test", http.DefaultTransport).(*searcher).client.Timeout; got != 10*time.Second {
		t.Errorf("timeout %v, want 10s", got)
	}
}

func TestNewRejectsALanguageThatIsNotText(t *testing.T) {
	_, err := New(settings(t, `{"model":"m","search_language":5}`), "http://search.test", http.DefaultTransport)
	if err == nil || err.Error() != "search_language: expected string" {
		t.Errorf("got %v, want search_language: expected string", err)
	}
}

// padded is body followed by spaces up to n bytes.
func padded(body string, n int) []byte {
	return append([]byte(body), bytes.Repeat([]byte(" "), n-len(body))...)
}

func show(results []web.Result) string {
	text := func(s *string) string {
		if s == nil {
			return "nil"
		}
		return fmt.Sprintf("%q", *s)
	}
	var parts []string
	for _, r := range results {
		parts = append(parts, fmt.Sprintf("{%s %s %s}", text(r.Title), text(r.URL), text(r.Content)))
	}
	return "[" + strings.Join(parts, " ") + "]"
}
