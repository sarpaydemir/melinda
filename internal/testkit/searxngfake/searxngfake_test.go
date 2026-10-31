package searxngfake_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/searxngfake"
)

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func TestSearch(t *testing.T) {
	f := searxngfake.New(t)
	f.Search().Reply(
		searxngfake.Results(searxngfake.Result{Title: "Go", URL: "https://go.dev", Content: "Dil"}),
		searxngfake.Results(),
	)

	status, body := get(t, f.URL()+"/search?q=golang&format=json&language=tr-TR&safesearch=0")
	want := `{"number_of_results":1,"results":[{"title":"Go","url":"https://go.dev","content":"Dil"}]}`
	if eq, err := testkit.JSONEqual([]byte(want), body); status != 200 || err != nil || !eq {
		t.Errorf("search = %d %s, want 200 %s", status, body, want)
	}
	if status, _ := get(t, f.URL()+"/search?q=golang"); status != http.StatusForbidden {
		t.Errorf("without format=json: status %d, want 403", status)
	}
	_, body = get(t, f.URL()+"/search?q=none&format=json")
	if eq, _ := testkit.JSONEqual([]byte(`{"number_of_results":0,"results":[]}`), body); !eq {
		t.Errorf("empty results = %s", body)
	}
	if q := f.Requests()[0].Query(); q.Get("language") != "tr-TR" || q.Get("safesearch") != "0" {
		t.Errorf("recorded query = %v", q)
	}
}
