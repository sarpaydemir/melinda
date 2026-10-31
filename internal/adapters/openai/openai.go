package openai

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

const (
	attempts         = 3
	defaultTimeout   = 60 * time.Second
	embeddingTimeout = 30 * time.Second
)

type provider struct {
	client    *http.Client
	clock     clock.Clock
	userAgent string
	log       *slog.Logger
	// base is api_base without trailing slashes or a /chat/completions suffix.
	base, key string
	// timeout and imageTimeout bound each phase of a chat and an image attempt.
	timeout, imageTimeout time.Duration
	temperature           json.Number
	maxTokens             int
}

// New builds the provider from one settings snapshot. transport sends its requests, clk times the waits between
// attempts, userAgent identifies it to the API, and it logs to log.
func New(s *config.Settings, transport http.RoundTripper, clk clock.Clock, userAgent string, log *slog.Logger) (model.Provider, error) {
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
	timeout := defaultTimeout
	if seconds > 0 {
		timeout = time.Duration(seconds * float64(time.Second))
	}
	return &provider{
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		clock:        clk,
		userAgent:    userAgent,
		log:          log,
		base:         strings.TrimSuffix(strings.TrimRight(c.APIBase, "/"), "/chat/completions"),
		key:          c.Key,
		timeout:      timeout,
		imageTimeout: 2 * time.Minute,
		temperature:  s.Temperature,
		maxTokens:    s.MaxTokens,
	}, nil
}

// Chat sends one chat completion request, with retries. An answer without choices that carries an error fails the
// attempt; an answer without a readable first choice fails the request.
func (p *provider) Chat(ctx context.Context, r model.Request) (model.Response, error) {
	body, err := p.body(r)
	if err != nil {
		return model.Response{}, err
	}
	var answer []byte
	err = p.retry(ctx, func() (err error) {
		answer, err = p.post(ctx, "/chat/completions", body, p.timeout)
		if completion := members(answer); err == nil && !truthy(completion["choices"]) && truthy(completion["error"]) {
			err = errorPayload(completion["error"])
		}
		return err
	})
	if err != nil {
		return model.Response{}, err
	}
	return response(answer)
}

// Embed returns the answer's first embedding, with retries. An answer without one fails the attempt, whatever error
// it carries.
func (p *provider) Embed(ctx context.Context, model, text string) ([]float32, error) {
	body, err := textfmt.Encode(embeddingRequest{model, text})
	if err != nil {
		return nil, err
	}
	var vector []float32
	err = p.retry(ctx, func() error {
		answer, err := p.post(ctx, "/embeddings", body, embeddingTimeout)
		if err == nil {
			vector, err = embedding(members(answer))
		}
		return err
	})
	return vector, err
}

// Image asks the chat completions API once, without retries, and returns the answer's first image sent as a data
// URL. Its errors are worded for the model: Timeout, API error: <status> - <body>, or what was wrong with the answer.
func (p *provider) Image(ctx context.Context, name, prompt string) (model.Image, error) {
	body, err := textfmt.Encode(imageRequest{
		Model:      name,
		Messages:   []message{{Role: model.RoleUser, Content: prompt}},
		Modalities: []string{"image", "text"},
		MaxTokens:  4096,
	})
	if err != nil {
		return model.Image{}, err
	}
	resp, answer, err := p.exchange(ctx, "/chat/completions", body, p.imageTimeout)
	var stalled timeoutError
	var unsent *url.Error
	switch {
	case errors.As(err, &stalled):
		return model.Image{}, imageError("Timeout")
	case errors.As(err, &unsent):
		// The model reads the error, so the API's URL is left to the log.
		if ctx.Err() == nil {
			p.log.WarnContext(ctx, "image request failed", "error", err)
		}
		if unsent.Timeout() {
			return model.Image{}, imageError("Timeout")
		}
		return model.Image{}, unsent.Err
	case err != nil:
		return model.Image{}, err
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		text := "API error: " + strconv.Itoa(resp.StatusCode)
		if len(answer) > 0 {
			text += " - " + textfmt.Clip(string(answer), 250)
		}
		return model.Image{}, imageError(text)
	}
	if kind := textfmt.Lower(resp.Header.Get("Content-Type")); !strings.Contains(kind, "application/json") {
		return model.Image{}, imageError("Unexpected content-type: " + cmp.Or(kind, "unknown"))
	}
	return image(answer)
}

// retry runs attempt until it succeeds, up to three times, waiting 1 s and then 2 s after a failure, or 5 s and then
// 10 s after a rate limit.
func (p *provider) retry(ctx context.Context, attempt func() error) error {
	for n := 0; ; n++ {
		err := attempt()
		if err == nil || n == attempts-1 {
			return err
		}
		wait := time.Second << n
		if rateLimited(err) {
			wait = 5 * time.Second << n
		}
		if err := clock.Sleep(ctx, p.clock, wait); err != nil {
			return err
		}
	}
}

// post makes one attempt at the API path and returns the answer, which is valid JSON.
func (p *provider) post(ctx context.Context, path string, body []byte, timeout time.Duration) ([]byte, error) {
	resp, data, err := p.exchange(ctx, path, body, timeout)
	switch {
	case err != nil:
		return nil, err
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &apiError{message: "HTTP status " + resp.Status, code: resp.StatusCode}
	case !json.Valid(data):
		return nil, errors.New("response is not valid JSON")
	}
	return data, nil
}

var errStalled = errors.New("stalled")

// timeoutError fails an attempt that made no progress for the duration.
type timeoutError time.Duration

func (e timeoutError) Error() string {
	return fmt.Sprintf("timed out after %v without progress", time.Duration(e))
}

// exchange makes one attempt at the API path and returns the response with its body read. timeout bounds each
// phase: connecting, sending, waiting for the headers, and each read of the body.
func (p *provider) exchange(ctx context.Context, path string, body []byte, timeout time.Duration) (*http.Response, []byte, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stall := time.AfterFunc(timeout, func() { cancel(errStalled) })
	defer stall.Stop()
	progress := func() { stall.Reset(timeout) }
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn:              func(httptrace.GotConnInfo) { progress() },
		WroteRequest:         func(httptrace.WroteRequestInfo) { progress() },
		GotFirstResponseByte: progress,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
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
		return nil, nil, timeoutError(timeout)
	}
	if err != nil {
		return nil, nil, err
	}
	return resp, data, nil
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
	fields := members(raw)
	if fields == nil {
		return &apiError{message: textfmt.Str(value(raw))}
	}
	e := &apiError{message: spaced(raw), code: textfmt.Int(value(fields["code"]), 0)}
	if m := value(fields["message"]); textfmt.Truthy(m) {
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

// members are the members of raw, valid JSON or empty, when it is an object, and nil otherwise.
func members(raw []byte) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	return m
}

// cutOffFinish and cutOffNative are the finish_reason and native_finish_reason values of an answer that stopped
// before its natural end.
var (
	cutOffFinish = []string{"length", "max_tokens", "content_filter", "error"}
	cutOffNative = []string{"max_tokens", "recitation", "safety", "blocklist", "prohibited_content", "spii", "other"}
)

// response reads the first choice of an answer: the assistant message and why the model stopped.
func response(answer []byte) (model.Response, error) {
	var choices []json.RawMessage
	if json.Unmarshal(members(answer)["choices"], &choices) != nil || len(choices) == 0 {
		text := spaced(answer)
		if clipped := textfmt.Clip(text, 2000); clipped != text {
			text = clipped + "... [truncated]"
		}
		return model.Response{}, fmt.Errorf("response has no choices: %s", text)
	}
	choice := members(choices[0])
	if choice == nil {
		return model.Response{}, errors.New("response choice is not an object")
	}
	finish, native := reason(choice["finish_reason"]), reason(choice["native_finish_reason"])
	resp := model.Response{
		Truncated: finish == "length" || finish == "max_tokens",
		CutOff:    slices.Contains(cutOffFinish, finish) || slices.Contains(cutOffNative, native),
		Malformed: native == "malformed_function_call",
		Reason: "finish_reason=" + textfmt.Str(value(choice["finish_reason"])) +
			" native_finish_reason=" + textfmt.Str(value(choice["native_finish_reason"])),
	}
	raw := choice["message"]
	message := members(raw)
	if message == nil {
		return resp, errors.New("response message is not an object")
	}
	text, err := content(message["content"])
	if err != nil {
		return resp, err
	}
	var calls []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	// A member of an unexpected type is left empty; calls without an ID or a name are the tool loop's to reject.
	_ = json.Unmarshal(message["tool_calls"], &calls)
	resp.Message = model.Message{Role: model.RoleAssistant, Content: text, Raw: raw}
	for _, c := range calls {
		resp.Message.ToolCalls = append(resp.Message.ToolCalls,
			model.ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: arguments(c.Function.Arguments)})
	}
	return resp, nil
}

// content is a message's text, stripped: a string as sent, the texts of a list's text parts joined by newlines,
// "" for any other falsy value, and any other value in its str form.
func content(raw json.RawMessage) (string, error) {
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		if v := value(raw); textfmt.Truthy(v) {
			return textfmt.Strip(textfmt.Str(v)), nil
		}
		return "", nil
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		p := members(part)
		if value(p["type"]) != "text" {
			continue
		}
		member, present := p["text"]
		text, ok := value(member).(string)
		if present && !ok {
			return "", errors.New("response content has a text part whose text is not a string")
		}
		texts = append(texts, text)
	}
	return textfmt.Strip(strings.Join(texts, "\n")), nil
}

// reason is a finish reason in lowercase, or "" when it is falsy.
func reason(raw json.RawMessage) string {
	if v := value(raw); textfmt.Truthy(v) {
		return textfmt.Lower(textfmt.Str(v))
	}
	return ""
}

// embedding is data[0].embedding: numbers, or nil for null.
func embedding(answer map[string]json.RawMessage) ([]float32, error) {
	var data []struct {
		Embedding json.RawMessage `json:"embedding"`
	}
	if json.Unmarshal(answer["data"], &data) != nil || len(data) == 0 || data[0].Embedding == nil {
		return nil, errors.New("response has no embedding")
	}
	var vector []float32
	if err := json.Unmarshal(data[0].Embedding, &vector); err != nil {
		return nil, fmt.Errorf("response embedding: %w", err)
	}
	return vector, nil
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
