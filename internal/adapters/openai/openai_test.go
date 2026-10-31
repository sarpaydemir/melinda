package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/conformance"
	"github.com/sarpaydemir/dost/internal/testkit/golden"
	"github.com/sarpaydemir/dost/internal/testkit/openaifake"
)

const key = "sk-test-0123456789"

// instantClock fires every timer at once and records how long each was set to wait.
type instantClock struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (c *instantClock) Now() time.Time { return time.Time{} }

func (c *instantClock) NewTimer(d time.Duration) clock.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, d)
	t := make(firedTimer, 1)
	t <- time.Time{}
	return t
}

func (c *instantClock) Waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.waits)
}

type firedTimer chan time.Time

func (t firedTimer) C() <-chan time.Time    { return t }
func (firedTimer) Stop() bool               { return false }
func (firedTimer) Reset(time.Duration) bool { return false }

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

// open builds the adapter for the API at base; fields are more members of the settings document.
func open(t *testing.T, base string, fields ...string) (model.Provider, *instantClock) {
	t.Helper()
	doc := fmt.Sprintf(`{"model":"m","api_base":%q,"key":%q`, base, key)
	for _, f := range fields {
		doc += "," + f
	}
	clk := &instantClock{}
	p, err := New(settings(t, doc+"}"), http.DefaultTransport, clk, "dost/1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	return p, clk
}

var ask = model.Request{Model: "m", Messages: []model.Message{{Role: model.RoleUser, Content: "Hello"}}}

func TestModelConformance(t *testing.T) {
	conformance.Model(t, func(t *testing.T) conformance.ModelHarness {
		fake := openaifake.New(t)
		p, _ := open(t, fake.URL())
		return conformance.ModelHarness{
			Provider: p,
			Text:     func(text string) { fake.Chat().Reply(openaifake.Text(text)) },
			ToolCall: func(id, name, arguments string) {
				fake.Chat().Reply(openaifake.ToolCalls(openaifake.ToolCall{ID: id, Name: name, Arguments: arguments}))
			},
			Fail: func() { fake.Chat().Default(openaifake.HTTPError(500, "unavailable")) },
			Hang: func() { fake.Chat().Default(openaifake.Text("late").WithDelay(time.Hour)) },
		}
	})
}

func TestNewValidatesSettings(t *testing.T) {
	for _, tc := range []struct {
		doc, want string
	}{
		{`{"model":"m","key":"k"}`, "api_base: required"},
		{`{"model":"m","api_base":"","key":"k"}`, "api_base: required"},
		{`{"model":"m","api_base":"http://h"}`, "key: required"},
		{`{"model":"m","api_base":"http://h","key":null}`, "key: required"},
		{`{"model":"m","api_base":"http://h","key":"k","request_timeout":"60"}`, "request_timeout: expected number"},
		{`{"model":"m","api_base":"http://h","key":"k","request_timeout":-1}`, "request_timeout: must not be negative"},
		{`{"model":"m","api_base":"http://h","key":"k","request_timeout":1e10}`, "request_timeout: too large"},
	} {
		_, err := New(settings(t, tc.doc), http.DefaultTransport, &instantClock{}, "dost/1.2.3")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.doc, err, tc.want)
		}
	}
}

func TestRequestTimeoutComesFromSettings(t *testing.T) {
	for field, want := range map[string]time.Duration{
		``:                        time.Minute,
		`"request_timeout":null`:  time.Minute,
		`"request_timeout":0`:     time.Minute,
		`"request_timeout":0.0`:   time.Minute,
		`"request_timeout":180.0`: 3 * time.Minute,
		`"request_timeout":2`:     2 * time.Second,
		`"request_timeout":0.25`:  250 * time.Millisecond,
	} {
		fields := []string{}
		if field != "" {
			fields = append(fields, field)
		}
		p, _ := open(t, "http://models.test", fields...)
		if got := p.(*provider).timeout; got != want {
			t.Errorf("%q: timeout %v, want %v", field, got, want)
		}
	}
}

func TestChatURL(t *testing.T) {
	for _, tc := range []struct{ base, path string }{
		{"", "/chat/completions"},
		{"/", "/chat/completions"},
		{"/v1", "/v1/chat/completions"},
		{"/v1/", "/v1/chat/completions"},
		{"/v1//", "/v1/chat/completions"},
		{"/v1/chat/completions", "/v1/chat/completions"},
		{"/v1/chat/completions/", "/v1/chat/completions"},
		{"/api/chat/completions/v1", "/api/chat/completions/v1/chat/completions"},
	} {
		fake := openaifake.New(t)
		fake.On("POST", tc.path).Reply(openaifake.Text("ok"))
		p, _ := open(t, fake.URL()+tc.base)
		if _, err := p.Chat(context.Background(), ask); err != nil {
			t.Errorf("%q: %v", tc.base, err)
		}
		fake.AssertCalls(t, "POST "+tc.path)
	}
}

func TestChatSendsOnlyTheContractHeaders(t *testing.T) {
	fake := openaifake.New(t)
	fake.Chat().Reply(openaifake.Text("ok"))
	p, _ := open(t, fake.URL())
	if _, err := p.Chat(context.Background(), ask); err != nil {
		t.Fatal(err)
	}
	req := fake.Requests()[0]
	want := http.Header{
		"Accept-Encoding": {"gzip"}, // added by the transport, which then decompresses the answer
		"Authorization":   {"Bearer " + key},
		"Content-Length":  {strconv.Itoa(len(req.Body))},
		"Content-Type":    {"application/json"},
		"User-Agent":      {"dost/1.2.3"},
	}
	if fmt.Sprint(req.Header) != fmt.Sprint(want) {
		t.Errorf("headers:\n got %v\nwant %v", req.Header, want)
	}
}

// sent runs r against a fake answering "ok" and returns the request body.
func sent(t *testing.T, r model.Request, fields ...string) []byte {
	t.Helper()
	fake := openaifake.New(t)
	fake.Chat().Reply(openaifake.Text("ok"))
	p, _ := open(t, fake.URL(), fields...)
	if _, err := p.Chat(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return fake.Requests()[0].Body
}

func TestChatBodyWithoutTools(t *testing.T) {
	body := sent(t, model.Request{
		Model: "chat-model",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "You answer briefly.\nUse the user's language."},
			{Role: model.RoleUser, Content: "Şu <b>kod</b> & \"alıntı\" ne işe yarar?\tC:\\yol\x01"},
			{Role: model.RoleAssistant, Content: "Bir örnek: a < b && c > d"},
			{Role: model.RoleUser, Content: "Satır\u2028ayırıcı, paragraf\u2029ayırıcı, düz metin \\u2028 ve emoji 🙂"},
		},
		Temperature: "0.7",
		MaxTokens:   4096,
	})
	golden.Assert(t, "chat_without_tools", append(body, '\n'))
}

func TestChatBodyWithParts(t *testing.T) {
	body := sent(t, model.Request{
		Model: "vision-model",
		Messages: []model.Message{{Role: model.RoleUser, Content: "Bu dosyalarda ne var?", Parts: []model.Part{
			{Filename: "grafik.png", MediaType: "image/png", Data: []byte("\x89PNG\r\n\x1a\n")},
			{Filename: "rapor.pdf", MediaType: "application/pdf", Data: []byte("%PDF-1.7\n")},
			{Data: []byte{0xff, 0xfe, 0xfd}},
		}}},
		Temperature: "0.7",
		MaxTokens:   4096,
	})
	golden.Assert(t, "chat_with_parts", append(body, '\n'))
}

// toolTurn is a provider's answer requesting two calls, pretty-printed and with fields that are not sent back.
const toolTurn = `{
  "id": "gen-1",
  "choices": [
    {
      "index": 0,
      "finish_reason": "tool_calls",
      "message": {
        "role": "assistant",
        "content": null,
        "reasoning": "The user wants the weather.",
        "tool_calls": [
          {
            "id": "call_1",
            "type": "function",
            "function": {"name": "search_web", "arguments": "{\"query\": \"İstanbul hava durumu\"}"}
          },
          {
            "index": 1,
            "id": "call_2",
            "type": "function",
            "function": {"name": "visit_page", "arguments": {"url": "https://example.com/?a=1&b=2", "limit": 1.5}}
          }
        ],
        "refusal": null
      }
    }
  ]
}`

func TestChatBodyWithToolsSendsToolCallsBack(t *testing.T) {
	fake := openaifake.New(t)
	fake.Chat().Reply(testkit.Raw(200, "application/json", []byte(toolTurn)), openaifake.Text("Hava güneşli."))
	p, _ := open(t, fake.URL())
	tools := []model.ToolSpec{
		{Name: "search_web", Description: "Search the web.", Parameters: json.RawMessage(`{
			"type": "object", "properties": {"query": {"type": "string"}}, "required": ["query"]}`)},
		{Name: "visit_page", Description: "Read a page's <body> text.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`)},
	}
	messages := []model.Message{
		{Role: model.RoleSystem, Content: "You are an assistant."},
		{Role: model.RoleSystem, Content: "Call tools only through the API."},
		{Role: model.RoleUser, Content: "İstanbul'da hava nasıl?"},
	}
	turn, err := p.Chat(context.Background(), model.Request{Model: "tool-model", Messages: messages, Tools: tools,
		Temperature: "0.1", MaxTokens: 8192})
	if err != nil {
		t.Fatal(err)
	}
	want := []model.ToolCall{
		{ID: "call_1", Name: "search_web", Arguments: json.RawMessage(`{"query": "İstanbul hava durumu"}`)},
		{ID: "call_2", Name: "visit_page", Arguments: json.RawMessage(`{"url": "https://example.com/?a=1&b=2", "limit": 1.5}`)},
	}
	if fmt.Sprintf("%s", turn.Message.ToolCalls) != fmt.Sprintf("%s", want) {
		t.Errorf("tool calls:\n got %s\nwant %s", turn.Message.ToolCalls, want)
	}
	messages = append(messages, turn.Message,
		model.Message{Role: model.RoleTool, ToolCallID: "call_1", Name: "search_web", Content: "[{'title': 'Hava', 'url': 'u'}]"},
		model.Message{Role: model.RoleTool, ToolCallID: "call_2", Name: "visit_page", Content: "Error: \"timeout\""},
	)
	if _, err := p.Chat(context.Background(), model.Request{Model: "tool-model", Messages: messages, Tools: tools,
		Temperature: "0.1", MaxTokens: 8192}); err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "chat_with_tools", append(fake.Requests()[1].Body, '\n'))
}

func TestChatArgumentsNormalization(t *testing.T) {
	for raw, want := range map[string]string{
		`"{\"q\":1}"`: `{"q":1}`,
		`{"q":1}`:     `{"q":1}`,
		`"not json"`:  `not json`,
		`"null"`:      `null`,
		`""`:          `{}`,
		`null`:        `{}`,
		`[1]`:         `{}`,
		`7`:           `{}`,
	} {
		fake := openaifake.New(t)
		fake.Chat().Reply(testkit.Raw(200, "application/json", []byte(
			`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[`+
				`{"id":"c","type":"function","function":{"name":"f","arguments":`+raw+`}}]}}]}`)))
		p, _ := open(t, fake.URL())
		resp, err := p.Chat(context.Background(), ask)
		if err != nil || len(resp.Message.ToolCalls) != 1 || string(resp.Message.ToolCalls[0].Arguments) != want {
			t.Errorf("arguments %s: got %+v, %v; want %s", raw, resp.Message.ToolCalls, err, want)
		}
	}
}

func TestChatTemperatureAndMaxTokens(t *testing.T) {
	for _, tc := range []struct {
		temperature json.Number
		maxTokens   int
		want        string
	}{
		{"", 0, `"temperature":0.5,"max_tokens":1000}`},
		{"0.1", 8192, `"temperature":0.1,"max_tokens":8192}`},
		{"1", 7, `"temperature":1,"max_tokens":7}`},
		{"1.0", 7, `"temperature":1.0,"max_tokens":7}`},
		{"0", 7, `"temperature":0,"max_tokens":7}`},
	} {
		r := ask
		r.Temperature, r.MaxTokens = tc.temperature, tc.maxTokens
		if body := sent(t, r, `"temperature":0.50`, `"max_tokens":1000`); !strings.HasSuffix(string(body), tc.want) {
			t.Errorf("temperature %q, max_tokens %d: body %s, want it to end with %s", tc.temperature, tc.maxTokens, body, tc.want)
		}
	}
}

func TestChatRejectsNaNTemperature(t *testing.T) {
	fake := openaifake.New(t)
	p, _ := open(t, fake.URL())
	r := ask
	r.Temperature = "NaN"
	if _, err := p.Chat(context.Background(), r); err == nil {
		t.Error("got no error")
	}
	fake.AssertCalls(t)
}

func TestChatRetriesEveryFailureThreeTimes(t *testing.T) {
	redirect := testkit.Status(307)
	redirect.Header = http.Header{"Location": {"/elsewhere"}}
	normal, rateLimited := []time.Duration{time.Second, 2 * time.Second}, []time.Duration{5 * time.Second, 10 * time.Second}
	for _, tc := range []struct {
		name  string
		fail  testkit.Response
		waits []time.Duration
		err   string
	}{
		{"connection reset", testkit.ConnReset(), normal, ""},
		{"server error", openaifake.HTTPError(503, "unavailable"), normal, "503 Service Unavailable"},
		{"bad request", openaifake.HTTPError(400, "bad request"), normal, "400 Bad Request"},
		{"bad key", openaifake.HTTPError(401, "invalid key "+key), normal, "401 Unauthorized"},
		{"redirect", redirect, normal, "307 Temporary Redirect"},
		{"rate limited", openaifake.HTTPError(429, "slow down"), rateLimited, "429 Too Many Requests"},
		{"invalid JSON", openaifake.Text("ok").Malformed(), normal, "not valid JSON"},
		{"error payload", openaifake.ErrorPayload("provider returned error", 502), normal, "provider returned error"},
		{"rate-limited payload", openaifake.ErrorPayload("rate-limited upstream", 429), rateLimited, "rate-limited upstream"},
		{"payload code as text", openaifake.ErrorPayload("rate-limited upstream", "429"), rateLimited, "rate-limited upstream"},
		{"payload code not a number", openaifake.ErrorPayload("busy", "rate_limit"), normal, "busy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := openaifake.New(t)
			fake.Chat().Default(tc.fail)
			p, clk := open(t, fake.URL())
			_, err := p.Chat(context.Background(), ask)
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("got %v, want an error containing %q", err, tc.err)
			}
			if err != nil && strings.Contains(err.Error(), key) {
				t.Errorf("error %q contains the key", err)
			}
			fake.AssertCalls(t, "POST /chat/completions", "POST /chat/completions", "POST /chat/completions")
			if got := clk.Waits(); !slices.Equal(got, tc.waits) {
				t.Errorf("waits %v, want %v", got, tc.waits)
			}
		})
	}
}

func TestChatRecoversAfterTwoRateLimits(t *testing.T) {
	fake := openaifake.New(t)
	fake.Chat().Reply(openaifake.HTTPError(429, "slow down"), openaifake.ErrorPayload("rate-limited upstream", 429),
		openaifake.Text("done"))
	p, clk := open(t, fake.URL())
	resp, err := p.Chat(context.Background(), ask)
	if err != nil || resp.Message.Content != "done" {
		t.Fatalf("got %q, %v; want done", resp.Message.Content, err)
	}
	if got, want := len(fake.Requests()), 3; got != want {
		t.Errorf("%d attempts, want %d", got, want)
	}
	if got, want := clk.Waits(), []time.Duration{5 * time.Second, 10 * time.Second}; !slices.Equal(got, want) {
		t.Errorf("waits %v, want %v", got, want)
	}
}

func TestChatReadsTheAnswer(t *testing.T) {
	const hi = `{"choices":[{"message":{"role":"assistant","content":"hi"}}]`
	for _, tc := range []struct {
		body     string
		err      string // empty when the answer is the message "hi"
		attempts int
	}{
		{`{"error":{"message":"rate-limited","code":429}}`, "rate-limited", 3},
		{`{"error":{"code":500,"type":"server_error"}}`, `{"code": 500, "type": "server_error"}`, 3},
		{`{"error":{"message":"","code":500}}`, `{"message": "", "code": 500}`, 3},
		{`{"error":{"message":{"detail":"x"}}}`, `{'detail': 'x'}`, 3},
		{`{"error":"upstream timeout"}`, "upstream timeout", 3},
		{`{"error":["a",1]}`, `['a', 1]`, 3},
		{`{"choices":[],"error":{"message":"no capacity"}}`, "no capacity", 3},
		{`{"choices":null,"error":{"message":"no capacity"}}`, "no capacity", 3},
		{`{"object":"error","error":{"message":"no capacity"}}`, "no capacity", 3},
		{hi + `,"error":null}`, "", 1},
		{hi + `,"error":{"message":"partial failure","code":429}}`, "", 1},
		{`{"id":"x"}`, "response has no choices", 1},
		{`{"choices":[]}`, "response has no choices", 1},
		{`{"choices":null,"error":null}`, "response has no choices", 1},
		{`[]`, "response has no choices", 1},
	} {
		fake := openaifake.New(t)
		fake.Chat().Default(testkit.Raw(200, "application/json", []byte(tc.body)))
		p, _ := open(t, fake.URL())
		resp, err := p.Chat(context.Background(), ask)
		switch {
		case tc.err == "" && (err != nil || resp.Message.Content != "hi"):
			t.Errorf("%s: got %q, %v; want hi", tc.body, resp.Message.Content, err)
		case tc.err != "" && (err == nil || err.Error() != tc.err):
			t.Errorf("%s: got %v, want %q", tc.body, err, tc.err)
		}
		if n := len(fake.Requests()); n != tc.attempts {
			t.Errorf("%s: %d attempts, want %d", tc.body, n, tc.attempts)
		}
	}
}

type step struct {
	after time.Duration
	part  string
}

// paced answers by writing each step's part after its delay and flushing it; the first flush sends the headers.
func paced(fake *openaifake.Fake, steps ...step) testkit.Response {
	return testkit.Response{Handler: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for _, s := range steps {
			select {
			case <-time.After(s.after):
			case <-r.Context().Done():
				return
			case <-fake.Context().Done():
				return
			}
			if _, err := io.WriteString(w, s.part); err != nil {
				return
			}
			http.NewResponseController(w).Flush()
		}
	}}
}

func TestTimeoutBoundsEachPhaseOfAnAttempt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout string
		answer  []step
		waits   []time.Duration
	}{
		{"headers never come", "0.05", []step{{time.Hour, ""}}, []time.Duration{time.Second}},
		{"the body stalls", "0.05",
			[]step{{0, `{"choices":[{"message":`}, {time.Hour, `{"content":"late"}}]}`}}, []time.Duration{time.Second}},
		{"headers and body each come within the timeout", "0.25",
			[]step{{150 * time.Millisecond, ""}, {150 * time.Millisecond, `{"choices":[{"message":{"content":"in time"}}]}`}}, nil},
		{"a steady body outlasts the timeout", "0.5",
			[]step{{0, `{"choices":[`}, {200 * time.Millisecond, `{"message":`},
				{200 * time.Millisecond, `{"content":"in time"`}, {200 * time.Millisecond, `}}]}`}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := openaifake.New(t)
			fake.Chat().Reply(paced(fake, tc.answer...), openaifake.Text("in time"))
			p, clk := open(t, fake.URL(), `"request_timeout":`+tc.timeout)
			resp, err := p.Chat(context.Background(), ask)
			if err != nil || resp.Message.Content != "in time" {
				t.Fatalf("got %q, %v; want in time", resp.Message.Content, err)
			}
			if got := clk.Waits(); !slices.Equal(got, tc.waits) {
				t.Errorf("waits %v, want %v", got, tc.waits)
			}
		})
	}
}
