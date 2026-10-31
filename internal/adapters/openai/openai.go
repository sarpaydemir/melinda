package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

const (
	attempts       = 3
	defaultTimeout = 60 * time.Second
)

type provider struct {
	client    *http.Client
	clock     clock.Clock
	userAgent string
	url, key  string
	// timeout bounds each phase of an attempt: connecting, sending, waiting for the headers, and each read of
	// the body.
	timeout     time.Duration
	temperature json.Number
	maxTokens   int
}

// New builds the provider from one settings snapshot. transport sends its requests, clk times the waits between
// attempts, and userAgent identifies it to the API.
func New(s *config.Settings, transport http.RoundTripper, clk clock.Clock, userAgent string) (model.Provider, error) {
	var c struct {
		APIBase string      `json:"api_base"`
		Key     string      `json:"key"`
		Timeout json.Number `json:"request_timeout"`
	}
	problems := []error{s.Decode(&c)}
	if c.APIBase == "" {
		problems = append(problems, errors.New("api_base: required"))
	}
	if c.Key == "" {
		problems = append(problems, errors.New("key: required"))
	}
	seconds, _ := strconv.ParseFloat(string(c.Timeout), 64) // 0 when absent; Decode accepts only finite numbers
	switch {
	case seconds < 0:
		problems = append(problems, errors.New("request_timeout: must not be negative"))
	case seconds >= time.Duration(math.MaxInt64).Seconds():
		problems = append(problems, errors.New("request_timeout: too large"))
	}
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}
	url := strings.TrimRight(c.APIBase, "/")
	if !strings.HasSuffix(url, "/chat/completions") {
		url += "/chat/completions"
	}
	timeout := defaultTimeout
	if seconds > 0 {
		timeout = time.Duration(seconds * float64(time.Second))
	}
	return &provider{
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		clock:       clk,
		userAgent:   userAgent,
		url:         url,
		key:         c.Key,
		timeout:     timeout,
		temperature: s.Temperature,
		maxTokens:   s.MaxTokens,
	}, nil
}

// Chat sends one chat completion request. Every failure is retried, up to three attempts, after 1 s and 2 s, or
// 5 s and 10 s when the API reports a rate limit.
func (p *provider) Chat(ctx context.Context, r model.Request) (model.Response, error) {
	body, err := p.body(r)
	if err != nil {
		return model.Response{}, err
	}
	var completion map[string]json.RawMessage
	for attempt := 0; ; attempt++ {
		completion, err = p.post(ctx, body)
		if err == nil || attempt == attempts-1 {
			break
		}
		wait := time.Second << attempt
		if rateLimited(err) {
			wait = 5 * time.Second << attempt
		}
		if err := clock.Sleep(ctx, p.clock, wait); err != nil {
			return model.Response{}, err
		}
	}
	if err != nil {
		return model.Response{}, err
	}
	return response(completion)
}

// Embed is not available yet.
func (p *provider) Embed(context.Context, string, string) ([]float32, error) {
	return nil, fmt.Errorf("embeddings: %w", errors.ErrUnsupported)
}

// Image is not available yet.
func (p *provider) Image(context.Context, string, string) (model.Image, error) {
	return model.Image{}, fmt.Errorf("image generation: %w", errors.ErrUnsupported)
}

var errStalled = errors.New("stalled")

// post makes one attempt and returns the members of the answer; an answer that is not an object has none.
func (p *provider) post(ctx context.Context, body []byte) (map[string]json.RawMessage, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stall := time.AfterFunc(p.timeout, func() { cancel(errStalled) })
	defer stall.Stop()
	progress := func() { stall.Reset(p.timeout) }
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn:              func(httptrace.GotConnInfo) { progress() },
		WroteRequest:         func(httptrace.WroteRequestInfo) { progress() },
		GotFirstResponseByte: progress,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = http.Header{
		"Authorization": {"Bearer " + p.key},
		"Content-Type":  {"application/json"},
		"User-Agent":    {p.userAgent},
	}
	resp, err := p.client.Do(req)
	var data []byte
	if err == nil {
		defer resp.Body.Close()
		data, err = io.ReadAll(progressReader{resp.Body, progress})
	}
	if err != nil && errors.Is(context.Cause(ctx), errStalled) {
		return nil, fmt.Errorf("timed out after %v without progress", p.timeout)
	}
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &apiError{message: "HTTP status " + resp.Status, code: resp.StatusCode}
	}
	if !json.Valid(data) {
		return nil, errors.New("response is not valid JSON")
	}
	var completion map[string]json.RawMessage
	_ = json.Unmarshal(data, &completion) // leaves it nil for an answer that is not an object
	if !truthy(completion["choices"]) && truthy(completion["error"]) {
		return nil, errorPayload(completion["error"])
	}
	return completion, nil
}

type progressReader struct {
	r        io.Reader
	progress func()
}

func (p progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.progress()
	return n, err
}

// apiError is a failure the API reported, with an HTTP status or as an error in a successful answer.
type apiError struct {
	message string
	// code is the HTTP status, or the error's code when it is an integer, else 0.
	code int
}

func (e *apiError) Error() string { return e.message }

func rateLimited(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.code == http.StatusTooManyRequests
}

// errorPayload reads an error member: its message, or else the error as JSON, or a value that is not an object
// as text.
func errorPayload(raw json.RawMessage) error {
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil {
		return &apiError{message: textfmt.Str(value(raw))}
	}
	e := &apiError{message: spaced(raw), code: textfmt.Int(value(members["code"]), 0)}
	if m := value(members["message"]); textfmt.Truthy(m) {
		e.message = textfmt.Str(m)
	}
	return e
}

// spaced is raw, valid JSON, compacted and then given a space after each separator: {"code": 500, "type": "x"}.
func spaced(raw json.RawMessage) string {
	var compact bytes.Buffer
	_ = json.Compact(&compact, raw)
	var b strings.Builder
	inString, escaped := false, false
	for _, c := range compact.Bytes() {
		b.WriteByte(c)
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case !inString && (c == ',' || c == ':'):
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// value decodes raw, valid JSON or empty, into a textfmt value; empty is nil.
func value(raw json.RawMessage) any {
	v, _ := textfmt.Decode(raw, false)
	return v
}

func truthy(raw json.RawMessage) bool { return textfmt.Truthy(value(raw)) }

// response is the first choice's assistant message.
func response(completion map[string]json.RawMessage) (model.Response, error) {
	var choices []struct {
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(completion["choices"], &choices) != nil || len(choices) == 0 {
		return model.Response{}, errors.New("response has no choices")
	}
	raw := choices[0].Message
	var m struct {
		Content   json.RawMessage `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return model.Response{}, fmt.Errorf("response message: %w", err)
	}
	msg := model.Message{Role: model.RoleAssistant, Raw: raw}
	if content, ok := value(m.Content).(string); ok {
		msg.Content = textfmt.Strip(content)
	}
	for _, c := range m.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, model.ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: arguments(c.Function.Arguments)})
	}
	return model.Response{Message: msg}, nil
}

// arguments is the JSON text of a call's arguments: an object as sent, the content of a non-empty string, or {}.
func arguments(raw json.RawMessage) json.RawMessage {
	if bytes.HasPrefix(raw, []byte("{")) {
		return raw
	}
	if text, ok := value(raw).(string); ok && text != "" {
		return json.RawMessage(text)
	}
	return json.RawMessage("{}")
}
