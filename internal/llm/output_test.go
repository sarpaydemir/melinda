package llm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/sarpaydemir/melinda/internal/llm"
	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

// provider answers every chat request with resp and err and records the requests.
type provider struct {
	resp model.Response
	err  error
	got  []model.Request
}

func (p *provider) Chat(_ context.Context, r model.Request) (model.Response, error) {
	p.got = append(p.got, r)
	return p.resp, p.err
}

const (
	failure = "Something went wrong."
	note    = "\n\n_(The answer was cut off.)_"
)

// logs holds what a client logged.
type logs struct{ bytes.Buffer }

// records are the logged records without their time.
func (l *logs) records(t *testing.T) []map[string]any {
	t.Helper()
	records := []map[string]any{}
	for line := range strings.Lines(l.String()) {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		delete(r, "time")
		records = append(records, r)
	}
	return records
}

func client(p *provider) (llm.Client, *logs) {
	l := &logs{}
	return llm.Client{Provider: p, Failure: failure, CutOffNote: note, Log: slog.New(slog.NewJSONHandler(l, nil))}, l
}

func text(content string) model.Response {
	return model.Response{Message: model.Message{Role: model.RoleAssistant, Content: content}}
}

func TestGenerateRepliesWithTheContentOrAFailure(t *testing.T) {
	truncated := text("partial")
	truncated.Truncated, truncated.CutOff = true, true
	truncatedEmpty := text("")
	truncatedEmpty.Truncated, truncatedEmpty.CutOff = true, true
	filtered := text("partial")
	filtered.CutOff = true
	malformed := text("partial")
	malformed.Malformed = true
	for _, tc := range []struct {
		name string
		resp model.Response
		err  error
		want string
		logs string
	}{
		{"content", text("Hello"), nil, "Hello", "[]"},
		{"stopped at the token limit", truncated, nil, failure + " [ERR_LLM_RESPONSE_TRUNCATED]", "[]"},
		{"empty at the token limit", truncatedEmpty, nil, failure + " [ERR_LLM_RESPONSE_TRUNCATED]", "[]"},
		{"cut off for another reason", filtered, nil, "partial", "[]"},
		{"malformed tool call", malformed, nil, "partial", "[]"},
		{"empty", text(""), nil, failure + " [ERR_EMPTY_RESPONSE]", "[]"},
		{"request failed", model.Response{}, errors.New(`response has no choices: {"id": "x"}`),
			failure + " [ERR_LLM_INVOKE_FAILED_INITIAL]",
			`[map[error:response has no choices: {"id": "x"} level:ERROR msg:model request failed]]`},
		{"unreadable message at the token limit", truncatedEmpty, errors.New("response cannot be read: message is not an object"),
			failure + " [ERR_LLM_RESPONSE_TRUNCATED]", "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &provider{resp: tc.resp, err: tc.err}
			c, logs := client(p)
			req := model.Request{Model: "m", Messages: []model.Message{
				{Role: model.RoleSystem, Content: "Answer briefly."}, {Role: model.RoleUser, Content: "Hi"}}}
			if got := c.Generate(context.Background(), req); got != tc.want {
				t.Errorf("reply %q, want %q", got, tc.want)
			}
			if got, want := fmt.Sprintf("%+v", p.got), fmt.Sprintf("%+v", []model.Request{req}); got != want {
				t.Errorf("sent %s, want the request unchanged: %s", got, want)
			}
			if got := fmt.Sprint(logs.records(t)); got != tc.logs {
				t.Errorf("logged %s, want %s", got, tc.logs)
			}
		})
	}
}

func TestErrorRepliesAreRecognizedByTheirMarker(t *testing.T) {
	for s, want := range map[string]bool{
		failure + " [ERR_EMPTY_RESPONSE]": true,
		"[ERR_":                           true,
		"x [ERR_X] y":                     true,
		"ERR_EMPTY_RESPONSE":              false,
		"[err_x]":                         false,
		"[ ERR_X]":                        false,
		"[ERR":                            false,
		"":                                false,
	} {
		if got := llm.IsErrorReply(s); got != want {
			t.Errorf("IsErrorReply(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestFailedRecognizesOnlyTheFailureRepliesOfGenerate(t *testing.T) {
	c, _ := client(&provider{})
	for s, want := range map[string]bool{
		failure + " [ERR_LLM_RESPONSE_TRUNCATED]":           true,
		failure + " [ERR_EMPTY_RESPONSE]":                   true,
		failure + " [ERR_LLM_INVOKE_FAILED_INITIAL]":        true,
		failure + " [ERR_FINAL_LLM_FAIL]":                   false,
		"It says: " + failure + " [ERR_EMPTY_RESPONSE]":     false,
		failure + " [ERR_EMPTY_RESPONSE]\n":                 false,
		"Sorry, I could not answer. [ERR_EMPTY_RESPONSE]":   false,
		"The screenshot shows [ERR_LLM_RESPONSE_TRUNCATED]": false,
	} {
		if got := c.Failed(s); got != want {
			t.Errorf("Failed(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestCutOffRepliesGetTheNoteAndAWarning(t *testing.T) {
	for _, tc := range []struct {
		resp          model.Response
		content, want string
		logs          string
	}{
		{model.Response{CutOff: true, Reason: "finish_reason=stop native_finish_reason=RECITATION"}, "Uzun çağrı", "Uzun çağrı" + note,
			"[map[chars:10 level:WARN msg:model response cut off reason:finish_reason=stop native_finish_reason=RECITATION]]"},
		{model.Response{Reason: "finish_reason=stop native_finish_reason=STOP"}, "Complete", "Complete", "[]"},
	} {
		c, logs := client(&provider{})
		if got := c.WithCutOffNote(context.Background(), tc.content, tc.resp); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.resp, got, tc.want)
		}
		if got := fmt.Sprint(logs.records(t)); got != tc.logs {
			t.Errorf("%+v: logged %s, want %s", tc.resp, got, tc.logs)
		}
	}
}

func TestGenerateJSONDecodesTheReplyWithoutFences(t *testing.T) {
	long := strings.Repeat("x", 300)
	for _, tc := range []struct {
		content string
		want    string // the value as textfmt.Repr prints it
		logged  string // the text logged as not JSON, if any
	}{
		{`{"a": 1}`, "{'a': 1}", ""},
		{"```json\n{\"a\": 1}\n```", "{'a': 1}", ""},
		{"```\n[1, 2]\n```", "[1, 2]", ""},
		{"  ```json  {\"b\": [1.0, 2]} ```  ", "{'b': [1.0, 2]}", ""},
		{"```json  {\"a\":1}```", "{'a': 1}", ""},
		{"```json\v{\"a\":1}```", "{'a': 1}", ""},
		{"```json\x1c{\"a\":1}```", "{'a': 1}", ""},
		{"\u0085{\"a\": 1}\u0085", "{'a': 1}", ""},
		{`{"a": "` + "```x" + `"}`, "{'a': 'x'}", ""},
		{`{"a": "` + "```json   y" + `"}`, "{'a': 'y'}", ""},
		{`{"a":1}` + "````", "{'a': 1}", ""},
		{`{"a": 1}` + "`", "{'a': 1}", ""},
		{"[1, 2]```json", "[1, 2]", ""},
		{"{\"a\":1}\n```\n```", "{'a': 1}", ""},
		{`"text"`, "'text'", ""},
		{"[NaN, Infinity, -Infinity]", "[nan, inf, -inf]", ""},
		{"null", "None", ""},
		{"NULL", "None", ""},
		{" nUlL ", "None", ""},
		{"```json\nnull\n```", "None", ""},
		{"```", "None", ""},
		{"``````", "None", ""},
		{"```JSON\n{\"a\": 1}\n```", "None", "JSON\n{\"a\": 1}"},
		{"````{\"a\":1}", "None", "`{\"a\":1}"},
		{"```jsonjson{\"a\":1}```", "None", "json{\"a\":1}"},
		{`{"a": "x"}` + "` `", "None", `{"a": "x"}` + "`"},
		{`{"a": 1} trailing`, "None", `{"a": 1} trailing`},
		{"not json", "None", "not json"},
		{long, "None", long[:200]},
	} {
		c, logs := client(&provider{resp: text(tc.content)})
		if got := textfmt.Repr(c.GenerateJSON(context.Background(), model.Request{Model: "m"})); got != tc.want {
			t.Errorf("%q: got %s, want %s", tc.content, got, tc.want)
		}
		if got, want := notJSON(t, logs), tc.logged; got != want {
			t.Errorf("%q: logged %q as not JSON, want %q", tc.content, got, want)
		}
	}
}

func TestGenerateJSONOfAFailureIsNil(t *testing.T) {
	c, logs := client(&provider{err: errors.New("connection refused")})
	if got := c.GenerateJSON(context.Background(), model.Request{Model: "m"}); got != nil {
		t.Errorf("got %v, want nil", got)
	}
	if got, want := notJSON(t, logs), failure+" [ERR_LLM_INVOKE_FAILED_INITIAL]"; got != want {
		t.Errorf("logged %q as not JSON, want %q", got, want)
	}
}

// notJSON is the text of the warning that the answer is not JSON, or "" when there is none.
func notJSON(t *testing.T, l *logs) string {
	t.Helper()
	for _, r := range l.records(t) {
		if r["level"] == "WARN" && r["msg"] == "model output is not JSON" && r["error"] != nil {
			return r["text"].(string)
		}
	}
	return ""
}
