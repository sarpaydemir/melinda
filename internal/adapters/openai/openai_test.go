package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
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
	p, err := New(settings(t, doc+"}"), http.DefaultTransport, clk, "dost/1.2.3", slog.New(slog.DiscardHandler))
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
			Embedding: func(vector []float32) { fake.Embeddings().Reply(openaifake.Embedding(vector)) },
			Image: func(mediaType string, data []byte) {
				url := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
				fake.Chat().Reply(openaifake.Completion(openaifake.Images(url)))
			},
			Fail: func() {
				fake.Chat().Default(openaifake.HTTPError(500, "unavailable"))
				fake.Embeddings().Default(openaifake.HTTPError(500, "unavailable"))
			},
			Hang: func() {
				fake.Chat().Default(openaifake.Text("late").WithDelay(time.Hour))
				fake.Embeddings().Default(openaifake.Embedding([]float32{0.5}).WithDelay(time.Hour))
			},
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
		_, err := New(settings(t, tc.doc), http.DefaultTransport, &instantClock{}, "dost/1.2.3", slog.New(slog.DiscardHandler))
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

func TestURLs(t *testing.T) {
	for _, tc := range []struct{ base, chat, embeddings string }{
		{"", "/chat/completions", "/embeddings"},
		{"/", "/chat/completions", "/embeddings"},
		{"/v1", "/v1/chat/completions", "/v1/embeddings"},
		{"/v1/", "/v1/chat/completions", "/v1/embeddings"},
		{"/v1//", "/v1/chat/completions", "/v1/embeddings"},
		{"/v1/chat/completions", "/v1/chat/completions", "/v1/embeddings"},
		{"/v1/chat/completions/", "/v1/chat/completions", "/v1/embeddings"},
		{"/api/chat/completions/v1", "/api/chat/completions/v1/chat/completions", "/api/chat/completions/v1/embeddings"},
	} {
		fake := openaifake.New(t)
		fake.On("POST", tc.chat).Reply(openaifake.Text("ok"))
		fake.On("POST", tc.embeddings).Reply(openaifake.Embedding([]float32{0.5}))
		p, _ := open(t, fake.URL()+tc.base)
		if _, err := p.Chat(context.Background(), ask); err != nil {
			t.Errorf("%q: chat: %v", tc.base, err)
		}
		if _, err := p.Embed(context.Background(), "e", "Hello"); err != nil {
			t.Errorf("%q: embed: %v", tc.base, err)
		}
		fake.AssertCalls(t, "POST "+tc.chat, "POST "+tc.embeddings)
	}
}

func TestSendsOnlyTheContractHeaders(t *testing.T) {
	fake := openaifake.New(t)
	fake.Chat().Reply(openaifake.Text("ok"))
	fake.Embeddings().Reply(openaifake.Embedding([]float32{0.5}))
	p, _ := open(t, fake.URL())
	if _, err := p.Chat(context.Background(), ask); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Embed(context.Background(), "e", "Hello"); err != nil {
		t.Fatal(err)
	}
	fake.Chat().Reply(openaifake.Completion(openaifake.Images("data:image/png;base64,QUJD")))
	if _, err := p.Image(context.Background(), "img-1", "a cat"); err != nil {
		t.Fatal(err)
	}
	for _, req := range fake.Requests() {
		want := http.Header{
			"Accept-Encoding": {"gzip"}, // added by the transport, which then decompresses the answer
			"Authorization":   {"Bearer " + key},
			"Content-Length":  {strconv.Itoa(len(req.Body))},
			"Content-Type":    {"application/json"},
			"User-Agent":      {"dost/1.2.3"},
		}
		if fmt.Sprint(req.Header) != fmt.Sprint(want) {
			t.Errorf("%s headers:\n got %v\nwant %v", req.Call(), req.Header, want)
		}
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

func TestChatBodyIsTheOnlyCopyOfTheFilesItCarries(t *testing.T) {
	data := make([]byte, 8<<20)
	r := model.Request{Model: "m", Messages: []model.Message{{Role: model.RoleUser, Content: "?", Parts: []model.Part{
		{Filename: "a.png", MediaType: "image/png", Data: data}, {Filename: "a.pdf", Data: data},
	}}}}
	p := &provider{temperature: "0.7"}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	body, err := p.body(r)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > uint64(len(body))+1<<20 {
		t.Errorf("building a %d-byte body allocated %d bytes", len(body), allocated)
	}
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
		{`{"id":"x"}`, `response has no choices: {"id": "x"}`, 1},
		{`{"choices":[]}`, `response has no choices: {"choices": []}`, 1},
		{`{"choices":null,"error":null}`, `response has no choices: {"choices": null, "error": null}`, 1},
		{`{"choices":"abc"}`, `response has no choices: {"choices": "abc"}`, 1},
		{`{"Choices":[{"message":{"content":"hi"}}]}`, `response has no choices: {"Choices": [{"message": {"content": "hi"}}]}`, 1},
		{`[]`, "response has no choices: []", 1},
		{`{"id":"` + strings.Repeat("ü", 2500) + `"}`,
			`response has no choices: {"id": "` + strings.Repeat("ü", 1992) + `... [truncated]`, 1},
		{`{"choices":[{"message":{"content":"hi"}},5]}`, "", 1},
		{`{"choices":[null]}`, "response cannot be read: choice is not an object", 1},
		{`{"choices":["hi"]}`, "response cannot be read: choice is not an object", 1},
		{`{"choices":[{"finish_reason":"stop"}]}`, "response cannot be read: message is not an object", 1},
		{`{"choices":[{"message":null}]}`, "response cannot be read: message is not an object", 1},
		{`{"choices":[{"message":"hi"}]}`, "response cannot be read: message is not an object", 1},
		{`{"choices":[{"message":{"content":"hi","tool_calls":"x"}}]}`, "", 1},
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
		case strings.HasPrefix(tc.err, "response cannot be read") != errors.Is(err, model.ErrUnreadable):
			t.Errorf("%s: %v wraps ErrUnreadable: %t", tc.body, err, errors.Is(err, model.ErrUnreadable))
		}
		if n := len(fake.Requests()); n != tc.attempts {
			t.Errorf("%s: %d attempts, want %d", tc.body, n, tc.attempts)
		}
	}
}

// answer runs a chat request against an API answering with one choice made of members.
func answer(t *testing.T, members string) (model.Response, error) {
	t.Helper()
	fake := openaifake.New(t)
	fake.Chat().Reply(testkit.Raw(200, "application/json", []byte(`{"choices":[{`+members+`}]}`)))
	p, _ := open(t, fake.URL())
	return p.Chat(context.Background(), ask)
}

func TestChatKeepsACallOfAnUnexpectedShapeWithoutItsID(t *testing.T) {
	resp, err := answer(t, `"message":{"content":"hi","tool_calls":[{"id":5}]}`)
	if calls := resp.Message.ToolCalls; err != nil || resp.Message.Content != "hi" || len(calls) != 1 || calls[0].ID != "" {
		t.Errorf("got %+v, %v; want hi with one call without an ID", resp.Message, err)
	}
}

func TestChatReportsStopFactsWithAnUnreadableMessage(t *testing.T) {
	for message, want := range map[string]string{
		`,"message":"hi"`: "response cannot be read: message is not an object",
		``:                "response cannot be read: message is not an object",
		`,"message":null`: "response cannot be read: message is not an object",
		`,"message":{"content":[{"type":"text","text":5}]}`: "response cannot be read: content has a text part whose text is not a string",
	} {
		resp, err := answer(t, `"finish_reason":"length"`+message)
		if !errors.Is(err, model.ErrUnreadable) || err.Error() != want || !resp.Truncated || !resp.CutOff {
			t.Errorf("%s: got %+v, %v; want truncated with %q", message, resp, err, want)
		}
	}
}

func TestChatReadsMessageContent(t *testing.T) {
	for _, tc := range []struct {
		content, want, err string
	}{
		{`"\u001c x \u0085"`, "x", ""},
		{`null`, "", ""},
		{`""`, "", ""},
		{`0`, "", ""},
		{`0.0`, "", ""},
		{`false`, "", ""},
		{`[]`, "", ""},
		{`{}`, "", ""},
		{`5`, "5", ""},
		{`-0.5`, "-0.5", ""},
		{`1e2`, "100.0", ""},
		{`true`, "True", ""},
		{`{"text": " x "}`, "{'text': ' x '}", ""},
		{`[{"type":"text","text":" a "},{"type":"image_url","image_url":{"url":"u"}},{"type":"text","text":"b "}]`, "a \nb", ""},
		{`[{"type":"text"},{"type":"text","text":"x"}]`, "x", ""},
		{`[{"type":"TEXT","text":"x"},"text",{"text":"y"},null]`, "", ""},
		{`[{"type":"text","text":""},{"type":"text","text":""}]`, "", ""},
		{`[{"type":"text","text":null}]`, "", "response cannot be read: content has a text part whose text is not a string"},
		{`[{"type":"text","text":5}]`, "", "response cannot be read: content has a text part whose text is not a string"},
	} {
		resp, err := answer(t, `"message":{"content":`+tc.content+`}`)
		switch {
		case tc.err == "" && (err != nil || resp.Message.Content != tc.want):
			t.Errorf("content %s: got %q, %v; want %q", tc.content, resp.Message.Content, err, tc.want)
		case tc.err != "" && (err == nil || err.Error() != tc.err):
			t.Errorf("content %s: got %v, want %q", tc.content, err, tc.err)
		}
	}
}

func TestChatReportsWhyTheModelStopped(t *testing.T) {
	type facts struct {
		truncated, cutOff, malformed bool
		reason                       string
	}
	for _, tc := range []struct {
		finish, native string // JSON values; empty leaves the member out
		want           facts
	}{
		{`"stop"`, ``, facts{reason: "finish_reason=stop native_finish_reason=None"}},
		{`"tool_calls"`, ``, facts{reason: "finish_reason=tool_calls native_finish_reason=None"}},
		{`null`, `null`, facts{reason: "finish_reason=None native_finish_reason=None"}},
		{``, ``, facts{reason: "finish_reason=None native_finish_reason=None"}},
		{`1`, `0`, facts{reason: "finish_reason=1 native_finish_reason=0"}},
		{`"length"`, ``, facts{true, true, false, "finish_reason=length native_finish_reason=None"}},
		{`"max_tokens"`, ``, facts{true, true, false, "finish_reason=max_tokens native_finish_reason=None"}},
		{`"LENGTH"`, `"STOP"`, facts{true, true, false, "finish_reason=LENGTH native_finish_reason=STOP"}},
		{`"length "`, ``, facts{reason: "finish_reason=length  native_finish_reason=None"}},
		{`"content_filter"`, `"RECITATION"`, facts{false, true, false, "finish_reason=content_filter native_finish_reason=RECITATION"}},
		{`"error"`, ``, facts{false, true, false, "finish_reason=error native_finish_reason=None"}},
		{`"stop"`, `"RECITATION"`, facts{false, true, false, "finish_reason=stop native_finish_reason=RECITATION"}},
		{`"stop"`, `"STOP"`, facts{reason: "finish_reason=stop native_finish_reason=STOP"}},
		{`"stop"`, `"MAX_TOKENS"`, facts{false, true, false, "finish_reason=stop native_finish_reason=MAX_TOKENS"}},
		{`"stop"`, `"SAFETY"`, facts{false, true, false, "finish_reason=stop native_finish_reason=SAFETY"}},
		{`"stop"`, `"blocklist"`, facts{false, true, false, "finish_reason=stop native_finish_reason=blocklist"}},
		{`"stop"`, `"PROHIBITED_CONTENT"`, facts{false, true, false, "finish_reason=stop native_finish_reason=PROHIBITED_CONTENT"}},
		{`"stop"`, `"SPII"`, facts{false, true, false, "finish_reason=stop native_finish_reason=SPII"}},
		{`"stop"`, `"OTHER"`, facts{false, true, false, "finish_reason=stop native_finish_reason=OTHER"}},
		{`"stop"`, `"image_safety"`, facts{reason: "finish_reason=stop native_finish_reason=image_safety"}},
		{`"stop"`, `"MALFORMED_FUNCTION_CALL"`, facts{false, false, true, "finish_reason=stop native_finish_reason=MALFORMED_FUNCTION_CALL"}},
		{`"length"`, `"malformed_function_call"`, facts{true, true, true, "finish_reason=length native_finish_reason=malformed_function_call"}},
	} {
		members := `"message":{"content":"x"}`
		if tc.finish != "" {
			members += `,"finish_reason":` + tc.finish
		}
		if tc.native != "" {
			members += `,"native_finish_reason":` + tc.native
		}
		resp, err := answer(t, members)
		if err != nil {
			t.Fatalf("%s: %v", members, err)
		}
		if got := (facts{resp.Truncated, resp.CutOff, resp.Malformed, resp.Reason}); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", members, got, tc.want)
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

func TestEmbedSendsTheTextUnmodified(t *testing.T) {
	fake := openaifake.New(t)
	fake.Embeddings().Reply(openaifake.Embedding([]float32{0.5}))
	p, _ := open(t, fake.URL())
	text := "  Şu <b>kod</b> & \"alıntı\" ne işe yarar?\tC:\\yol\x01\b\f\x7f\r\n" +
		"Satır\u2028ayırıcı, paragraf\u2029ayırıcı, düz metin \\u2028 ve emoji 🙂\n\n"
	if _, err := p.Embed(context.Background(), "text-embedding-3-small", text); err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "embedding", append(fake.Requests()[0].Body, '\n'))
}

func TestEmbedReadsTheFirstEmbedding(t *testing.T) {
	for _, tc := range []struct {
		body     string
		want     []float32
		err      string // empty when the answer is want
		attempts int
	}{
		{`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.25,-1.5,3]},` +
			`{"object":"embedding","index":1,"embedding":[9]}],"model":"x"}`, []float32{0.25, -1.5, 3}, "", 1},
		{`{"data":[{"embedding":[1,2]}],"error":{"message":"partial failure","code":429}}`, []float32{1, 2}, "", 1},
		{`{"data":[{"embedding":[]}]}`, nil, "", 1},
		{`{"data":[{"embedding":null}]}`, nil, "", 1},
		{`{"object":"list"}`, nil, "response has no embedding", 3},
		{`{"data":null}`, nil, "response has no embedding", 3},
		{`{"data":[]}`, nil, "response has no embedding", 3},
		{`{"data":[{}]}`, nil, "response has no embedding", 3},
		{`[]`, nil, "response has no embedding", 3},
		{`{"error":{"message":"rate-limited upstream","code":429}}`, nil, "response has no embedding", 3},
		{`{"data":[{"embedding":"0.5"}]}`, nil, "response embedding: json: cannot unmarshal string", 3},
	} {
		fake := openaifake.New(t)
		fake.Embeddings().Default(testkit.Raw(200, "application/json", []byte(tc.body)))
		p, clk := open(t, fake.URL())
		got, err := p.Embed(context.Background(), "e", "Hello")
		switch {
		case tc.err == "" && (err != nil || !slices.Equal(got, tc.want)):
			t.Errorf("%s: got %v, %v; want %v", tc.body, got, err, tc.want)
		case tc.err != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.err)):
			t.Errorf("%s: got %v, want an error starting %q", tc.body, err, tc.err)
		}
		if n := len(fake.Requests()); n != tc.attempts {
			t.Errorf("%s: %d attempts, want %d", tc.body, n, tc.attempts)
		}
		if got, want := clk.Waits(), []time.Duration{time.Second, 2 * time.Second}[:tc.attempts-1]; !slices.Equal(got, want) {
			t.Errorf("%s: waits %v, want %v", tc.body, got, want)
		}
	}
}

func TestEmbedRecoversAfterTwoFailures(t *testing.T) {
	vector := []float32{0.25, -1.5, 3}
	for _, tc := range []struct {
		name     string
		failures testkit.Response
		waits    []time.Duration
	}{
		{"server errors", openaifake.HTTPError(503, "unavailable"), []time.Duration{time.Second, 2 * time.Second}},
		{"rate limits", openaifake.HTTPError(429, "slow down"), []time.Duration{5 * time.Second, 10 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := openaifake.New(t)
			fake.Embeddings().Reply(tc.failures, tc.failures, openaifake.Embedding(vector))
			p, clk := open(t, fake.URL())
			got, err := p.Embed(context.Background(), "e", "Hello")
			if err != nil || !slices.Equal(got, vector) {
				t.Errorf("got %v, %v; want %v", got, err, vector)
			}
			fake.AssertCalls(t, "POST /embeddings", "POST /embeddings", "POST /embeddings")
			if got := clk.Waits(); !slices.Equal(got, tc.waits) {
				t.Errorf("waits %v, want %v", got, tc.waits)
			}
		})
	}
}

func TestEmbedTimeoutIsNotTheRequestTimeout(t *testing.T) {
	fake := openaifake.New(t)
	fake.Embeddings().Reply(openaifake.Embedding([]float32{0.5}).WithDelay(200 * time.Millisecond))
	p, _ := open(t, fake.URL(), `"request_timeout":0.05`)
	if got, err := p.Embed(context.Background(), "e", "Hello"); err != nil || !slices.Equal(got, []float32{0.5}) {
		t.Errorf("got %v, %v; want [0.5] from the first attempt", got, err)
	}
}

func TestChatResendsToolCallsWithoutHTMLEscapes(t *testing.T) {
	fake := openaifake.New(t)
	answer := `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function",` +
		`"function":{"name":"create_issue","arguments":"{\"description\": \"Seen on <mobile> & web.\"}"}}]}}]}`
	fake.Chat().Reply(testkit.Raw(200, "application/json", []byte(answer)), openaifake.Text("done"))
	p, _ := open(t, fake.URL())
	tools := []model.ToolSpec{{Name: "create_issue", Parameters: json.RawMessage(`{"type":"object"}`)}}
	messages := []model.Message{{Role: model.RoleUser, Content: "file it"}}
	turn, err := p.Chat(context.Background(), model.Request{Model: "m", Messages: messages, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	messages = append(messages, turn.Message,
		model.Message{Role: model.RoleTool, ToolCallID: "call_1", Name: "create_issue", Content: "ok"})
	if _, err := p.Chat(context.Background(), model.Request{Model: "m", Messages: messages, Tools: tools}); err != nil {
		t.Fatal(err)
	}
	want := `"tool_calls":[{"id":"call_1","type":"function","function":{"name":"create_issue",` +
		`"arguments":"{\"description\": \"Seen on <mobile> & web.\"}"}}]`
	if body := string(fake.Requests()[1].Body); !strings.Contains(body, want) {
		t.Errorf("body %s\nlacks %s", body, want)
	}
}
