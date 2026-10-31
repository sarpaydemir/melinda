package testkit_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/testkit"
)

// failures captures the failures a helper reports instead of failing the enclosing test.
type failures struct {
	testing.TB
	mu   sync.Mutex
	msgs []string
}

func (f *failures) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, format)
}

func (f *failures) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs)
}

func do(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestRecordsRequestsInOrder(t *testing.T) {
	s := testkit.NewServer(t)
	s.On("POST", "/a").Default(testkit.Status(204))
	s.On("GET", "/b/{id}").Default(testkit.Status(204))

	req, _ := http.NewRequest("POST", s.URL()+"/a?x=1&y=%C4%B0", strings.NewReader(`{"k":"v"}`))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	do(t, "GET", s.URL()+"/b/4%2F2", "")

	s.AssertCalls(t, "POST /a?x=1&y=%C4%B0", "GET /b/4%2F2")
	got := s.Requests()
	if a := got[0].Header.Get("Authorization"); a != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", a)
	}
	if string(got[0].Body) != `{"k":"v"}` {
		t.Errorf("body = %s", got[0].Body)
	}
	if q := got[0].Query().Get("y"); q != "İ" {
		t.Errorf("query y = %q, want İ", q)
	}
	if id := got[1].Params["id"]; id != "4%2F2" {
		t.Errorf("param id = %q, want 4%%2F2 (escaped path)", id)
	}
}

func TestQueueThenDefault(t *testing.T) {
	s := testkit.NewServer(t)
	s.On("GET", "/x").Reply(testkit.Raw(200, "text/plain", []byte("one")), testkit.Status(503)).
		Default(testkit.Raw(200, "text/plain", []byte("default")))

	var got []string
	for range 4 {
		code, body := do(t, "GET", s.URL()+"/x", "")
		got = append(got, http.StatusText(code)+":"+body)
	}
	want := []string{"OK:one", "Service Unavailable:", "OK:default", "OK:default"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("responses = %v, want %v", got, want)
	}
}

func TestMostSpecificRouteAnswersAndFallsThroughWhenExhausted(t *testing.T) {
	s := testkit.NewServer(t)
	s.On("GET", "/users/{id}").DefaultFunc(func(r testkit.Request) testkit.Response {
		return testkit.Raw(200, "text/plain", []byte("user "+r.Params["id"]))
	})
	s.On("GET", "/users/me").Default(testkit.Raw(200, "text/plain", []byte("me")))
	s.On("GET", "/users/{uid}").Reply(testkit.Status(500))

	steps := []struct{ path, want string }{
		{"/users/me", "me"},
		{"/users/7", ""}, // scripted overlay first
		{"/users/7", "user 7"},
	}
	for _, st := range steps {
		if _, body := do(t, "GET", s.URL()+st.path, ""); body != st.want {
			t.Errorf("GET %s = %q, want %q", st.path, body, st.want)
		}
	}
}

func TestRequestOrderMatchesQueuedReplyOrder(t *testing.T) {
	const rounds, clients = 100, 8
	for range rounds {
		s := testkit.NewServer(t)
		route := s.On("POST", "/x")
		for i := range clients {
			route.Reply(testkit.Raw(200, "text/plain", []byte(strconv.Itoa(i))))
		}
		sentFor := make([]string, clients) // reply index -> body of the request that got it
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := range clients {
			wg.Go(func() {
				body := "request " + strconv.Itoa(i)
				resp, err := http.Post(s.URL()+"/x", "text/plain", strings.NewReader(body))
				if err != nil {
					t.Error(err)
					return
				}
				defer resp.Body.Close()
				reply, _ := io.ReadAll(resp.Body)
				k, _ := strconv.Atoi(string(reply))
				mu.Lock()
				sentFor[k] = body
				mu.Unlock()
			})
		}
		wg.Wait()
		for k, r := range s.Requests() {
			if string(r.Body) != sentFor[k] {
				t.Fatalf("Requests()[%d] = %q, but reply %d went to %q", k, r.Body, k, sentFor[k])
			}
		}
	}
}

func TestUnscriptedRequestFailsTheTest(t *testing.T) {
	f := &failures{TB: t}
	s := testkit.NewServer(f)
	s.On("GET", "/once").Reply(testkit.Status(200))

	do(t, "GET", s.URL()+"/once", "")
	if code, _ := do(t, "GET", s.URL()+"/once", ""); code != http.StatusNotImplemented {
		t.Errorf("exhausted route: status %d, want 501", code)
	}
	if code, _ := do(t, "POST", s.URL()+"/nowhere", "payload"); code != http.StatusNotImplemented {
		t.Errorf("unknown route: status %d, want 501", code)
	}
	if n := f.count(); n != 2 {
		t.Errorf("failures = %d, want 2", n)
	}
	s.AssertCalls(t, "GET /once", "GET /once", "POST /nowhere")
}

func TestGuardRunsBeforeRouting(t *testing.T) {
	s := testkit.NewServer(t)
	s.Guard(func(r testkit.Request) (testkit.Response, bool) {
		return testkit.Status(401), r.Header.Get("X-Key") != "k"
	})
	s.On("GET", "/x").Default(testkit.Status(204))

	if code, _ := do(t, "GET", s.URL()+"/x", ""); code != 401 {
		t.Errorf("without key: status %d, want 401", code)
	}
	req, _ := http.NewRequest("GET", s.URL()+"/x", nil)
	req.Header.Set("X-Key", "k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("with key: status %d, want 204", resp.StatusCode)
	}
	s.AssertCalls(t, "GET /x", "GET /x")
}

func TestFaults(t *testing.T) {
	s := testkit.NewServer(t)
	s.On("GET", "/slow").Reply(testkit.Status(200).WithDelay(time.Hour))
	s.On("GET", "/reset").Reply(testkit.ConnReset())
	s.On("GET", "/malformed").Reply(testkit.JSON(200, map[string]string{"key": "value"}).Malformed())

	client := &http.Client{Timeout: 50 * time.Millisecond}
	if _, err := client.Get(s.URL() + "/slow"); err == nil {
		t.Error("delayed response: want a client timeout")
	}
	if _, err := http.Get(s.URL() + "/reset"); err == nil {
		t.Error("reset: want a connection error")
	}
	_, body := do(t, "GET", s.URL()+"/malformed", "")
	if json.Valid([]byte(body)) {
		t.Errorf("malformed body %q is valid JSON", body)
	}
}

func TestAwaitWaitsForConcurrentRequests(t *testing.T) {
	s := testkit.NewServer(t)
	s.On("GET", "/x").Default(testkit.Status(204))
	go func() {
		for range 3 {
			resp, err := http.Get(s.URL() + "/x")
			if err == nil {
				resp.Body.Close()
			}
		}
	}()
	if got := s.Await(t, 3); len(got) != 3 {
		t.Errorf("requests = %d, want 3", len(got))
	}
}

func TestMultipart(t *testing.T) {
	s := testkit.NewServer(t)
	s.On("POST", "/upload").Default(testkit.Status(201))

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("channel_id", "c1")
	part, _ := w.CreateFormFile("files", "çizim.png")
	_, _ = part.Write([]byte{0x89, 'P', 'N', 'G'})
	_ = w.Close()
	resp, err := http.Post(s.URL()+"/upload", w.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	m, err := s.Requests()[0].Multipart()
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Values["channel_id"]; len(got) != 1 || got[0] != "c1" {
		t.Errorf("channel_id = %v, want [c1]", got)
	}
	if len(m.Files) != 1 || m.Files[0].Field != "files" || m.Files[0].Filename != "çizim.png" || string(m.Files[0].Data) != "\x89PNG" {
		t.Errorf("files = %+v", m.Files)
	}
	if _, err := (testkit.Request{Header: http.Header{"Content-Type": {"application/json"}}}).Multipart(); err == nil {
		t.Error("JSON body parsed as multipart")
	}
}

func TestJSONEqual(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{`{"a":1,"b":[1,2]}`, ` { "b" : [1, 2], "a" : 1 } `, true},
		{`{"a":1}`, `{"a":1.0}`, false},
		{`{"t":0.7}`, `{"t":7e-1}`, false},
		{`{"s":"İı"}`, `{"s":"İı"}`, true},
		{`[1,2]`, `[2,1]`, false},
		{`{"a":null}`, `{}`, false},
		{`{"n":9007199254740993}`, `{"n":9007199254740992}`, false},
	}
	for _, tt := range tests {
		got, err := testkit.JSONEqual([]byte(tt.a), []byte(tt.b))
		if err != nil || got != tt.want {
			t.Errorf("JSONEqual(%s, %s) = %v, %v; want %v", tt.a, tt.b, got, err, tt.want)
		}
	}
	for _, bad := range []string{``, `{`, `{} {}`, `nul`} {
		if _, err := testkit.JSONEqual([]byte(bad), []byte(`{}`)); err == nil {
			t.Errorf("JSONEqual(%q) accepted invalid JSON", bad)
		}
	}
}
