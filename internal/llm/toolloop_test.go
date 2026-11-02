package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/llm"
	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/port/tool"
	"github.com/sarpaydemir/melinda/internal/prompt"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

type answer struct {
	resp model.Response
	err  error
}

// scripted gives the scripted answers in order and records the requests.
type scripted struct {
	answers []answer
	got     []model.Request
}

func (s *scripted) Chat(_ context.Context, r model.Request) (model.Response, error) {
	s.got = append(s.got, r)
	if len(s.answers) == 0 {
		return model.Response{}, errors.New("no answer scripted")
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	return a.resp, a.err
}

func call(id string) model.ToolCall {
	return model.ToolCall{ID: id, Name: "search_web", Arguments: json.RawMessage(`{"query": "q"}`)}
}

func toolTurn(calls ...model.ToolCall) answer {
	return answer{resp: model.Response{Message: model.Message{Role: model.RoleAssistant, ToolCalls: calls}}}
}

func reply(content string) answer { return answer{resp: text(content)} }

func cutOff(a answer, truncated bool) answer {
	a.resp.CutOff, a.resp.Truncated = true, truncated
	return a
}

var (
	failedRequest = answer{err: errors.New("HTTP status 429 Too Many Requests")}
	unreadable    = answer{err: fmt.Errorf("%w: message is not an object", model.ErrUnreadable)}
	malformed     = answer{resp: model.Response{Malformed: true, Message: model.Message{Role: model.RoleAssistant}}}
)

// echo is a tool that returns its name and arguments.
func echo(_ context.Context, name string, args json.RawMessage) string {
	return "out " + name + " " + textfmt.Repr(tool.Decode(args))
}

// returning is a tool that returns outs in turn.
func returning(outs ...string) func(context.Context, string, json.RawMessage) string {
	return func(context.Context, string, json.RawMessage) string {
		out := outs[0]
		outs = outs[1:]
		return out
	}
}

// loop runs the tool loop with the settings document members doc and the script on msgs, with the tool execute.
// It returns the reply, each request as describe prints it, and the logs.
func loop(t *testing.T, doc string, msgs []model.Message, answers []answer,
	execute func(context.Context, string, json.RawMessage) string) (string, []string, *logs) {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"model": "primary"`+doc+`}`), &members); err != nil {
		t.Fatal(err)
	}
	s, err := config.Load(members, "agent", nil, func(*config.Settings) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	cat, err := prompt.Load(prompt.Builtin(), "")
	if err == nil {
		cat, err = cat.With(map[string]string{
			"tools.format_guard": "GUARD", "toolloop.context_header": "HEADER", "toolloop.limit_reached": "LIMIT",
			"toolloop.final_retry": "FINAL_RETRY", "toolloop.malformed_retry": "MALFORMED_RETRY",
			"toolloop.unavailable": "UNAVAILABLE {reason}", "toolloop.done_prefix": "DONE",
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	p := &scripted{answers: answers}
	l := &logs{}
	client := llm.Client{Provider: p, Failure: failure, CutOffNote: note, Log: slog.New(slog.NewJSONHandler(l, nil))}
	specs := []model.ToolSpec{{Name: "search_web", Parameters: json.RawMessage(`{}`)}}
	got := llm.ToolLoop{Client: client, Settings: s, Catalog: cat, Tools: specs, Execute: execute}.
		Answer(context.Background(), msgs)
	requests := make([]string, len(p.got))
	for i, r := range p.got {
		requests[i] = describe(r)
	}
	return got, requests, l
}

// describe prints a request as its model, temperature, and max tokens, whether it offers tools, and a line per
// message.
func describe(r model.Request) string {
	lines := []string{fmt.Sprintf("%s %s %d", r.Model, r.Temperature, r.MaxTokens)}
	if len(r.Tools) > 0 {
		lines[0] += " tools"
	}
	for _, m := range r.Messages {
		switch {
		case m.Role == model.RoleTool:
			lines = append(lines, fmt.Sprintf("tool %s %s: %s", m.ToolCallID, m.Name, m.Content))
		case len(m.ToolCalls) > 0:
			var ids []string
			for _, c := range m.ToolCalls {
				ids = append(ids, c.ID)
			}
			lines = append(lines, fmt.Sprintf("assistant: %s %v", m.Content, ids))
		default:
			lines = append(lines, fmt.Sprintf("%s: %s", m.Role, m.Content))
		}
	}
	return strings.Join(lines, "\n")
}

const (
	firstTurn = "primary 0.1 8192 tools\nsystem: SYS\nsystem: GUARD\nuser: hi"
	output    = "out search_web {'query': 'q'}"
	done      = "DONE " + output
)

// called is the transcript of a turn that made calls of echo with the IDs.
func called(ids ...string) string {
	s := fmt.Sprintf("\nassistant:  %v", ids)
	for _, id := range ids {
		s += "\ntool " + id + " search_web: " + output
	}
	return s
}

// finalRequest is a request without tools that gives the summaries of n calls of echo and the instruction.
func finalRequest(n int, instruction string) string {
	s := "primary 0.7 8192\nsystem: SYS\nuser: hi"
	if n > 0 {
		s += "\nuser: HEADER" + strings.Repeat("\n- search_web({'query': 'q'}): "+output, n)
	}
	return s + "\nsystem: " + instruction
}

type loopCase struct {
	name     string
	doc      string
	answers  []answer
	execute  func(context.Context, string, json.RawMessage) string
	want     string
	requests []string
}

func run(t *testing.T, cases []loopCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			execute := tc.execute
			if execute == nil {
				execute = echo
			}
			got, requests, _ := loop(t, tc.doc, llm.Messages("SYS", nil, "hi"), tc.answers, execute)
			check(t, got, requests, tc.want, tc.requests)
		})
	}
}

func check(t *testing.T, got string, requests []string, want string, wantRequests []string) {
	t.Helper()
	if got != want {
		t.Errorf("reply %q, want %q", got, want)
	}
	if !slices.Equal(requests, wantRequests) {
		t.Errorf("requests:\n%s\n\nwant:\n%s", strings.Join(requests, "\n--\n"), strings.Join(wantRequests, "\n--\n"))
	}
}

func TestToolLoopAnswersWithTheContentOfTheTurnAfterTheCalls(t *testing.T) {
	history := []json.RawMessage{json.RawMessage(`{"user": "u0", "assistant": "a0"}`)}
	first := "primary 0.1 8192 tools\nsystem: SYS\nsystem: GUARD\nuser: u0\nassistant: a0\nuser: hi"
	for _, tc := range []struct {
		name  string
		final answer
		want  string
	}{
		{"complete", reply("Long"), "Long"},
		{"cut off at the token limit", cutOff(reply("Long"), true), "Long" + note},
		{"cut off by a filter", cutOff(reply("Long"), false), "Long" + note},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, requests, _ := loop(t, "", llm.Messages("SYS", history, "hi"),
				[]answer{toolTurn(call("call_1")), tc.final}, echo)
			check(t, got, requests, tc.want, []string{first, first + called("call_1")})
		})
	}
}

func TestToolLoopStopsAtTheCallBudgetMidBatchAndAnswersFromTheSummaries(t *testing.T) {
	var batch []model.ToolCall
	for i := range 30 {
		batch = append(batch, model.ToolCall{ID: fmt.Sprintf("call_%d", i), Name: "search_web",
			Arguments: json.RawMessage(`{"query": "joke search"}`)})
	}
	line := "\n- search_web({'query': 'joke search'}): result for search_web: joke search"
	summary := "HEADER" + strings.Repeat(line, 20)
	for _, tc := range []struct{ doc, role string }{{"", "user"}, {`, "tool_summary_role": "system"`, "system"}} {
		t.Run(tc.role, func(t *testing.T) {
			executed := 0
			execute := func(_ context.Context, name string, args json.RawMessage) string {
				executed++
				a, _ := tool.Call{Args: args}.Arguments()
				query, _ := a.String("query")
				return "result for " + name + ": " + query
			}
			got, requests, _ := loop(t, tc.doc, llm.Messages("", nil, "hi"),
				[]answer{toolTurn(batch...), reply("Final answer")}, execute)
			check(t, got, requests, "Final answer", []string{
				"primary 0.1 8192 tools\nsystem: GUARD\nuser: hi",
				"primary 0.7 8192\nuser: hi\n" + tc.role + ": " + summary + "\nsystem: LIMIT",
			})
			if executed != 20 {
				t.Errorf("executed %d calls, want 20", executed)
			}
		})
	}
}

func TestToolLoopSendsOneMoreToolTurnAtTheLimits(t *testing.T) {
	run(t, []loopCase{
		{name: "budget spent at the end of a batch, then an answer", doc: `, "max_tool_calls": 2`,
			answers: []answer{toolTurn(call("call_1"), call("call_2")), reply("done")},
			want:    "done", requests: []string{firstTurn, firstTurn + called("call_1", "call_2")}},
		{name: "budget spent at the end of a batch, then more calls", doc: `, "max_tool_calls": 2`,
			answers:  []answer{toolTurn(call("call_1"), call("call_2")), toolTurn(call("call_3")), reply("final")},
			want:     "final",
			requests: []string{firstTurn, firstTurn + called("call_1", "call_2"), finalRequest(2, "LIMIT")}},
		{name: "iterations spent, then more calls", doc: `, "max_tool_iterations": 2`,
			answers: []answer{toolTurn(call("call_1")), toolTurn(call("call_2")), toolTurn(call("call_3")), reply("final")},
			want:    "final",
			requests: []string{firstTurn, firstTurn + called("call_1"), firstTurn + called("call_1") + called("call_2"),
				finalRequest(2, "LIMIT")}},
	})
}

func TestToolLoopSendsAFailedToolTurnToTheFallbackModel(t *testing.T) {
	fallback := `, "tool_fallback_model": "fallback"`
	fallbackTurn := strings.Replace(firstTurn, "primary", "fallback", 1)
	run(t, []loopCase{
		{name: "fallback answers", doc: fallback, answers: []answer{failedRequest, reply("fallback answer")},
			want: "fallback answer", requests: []string{firstTurn, fallbackTurn}},
		{name: "next turn goes to the primary model", doc: fallback,
			answers: []answer{failedRequest, toolTurn(call("call_1")), reply("primary again")},
			want:    "primary again", requests: []string{firstTurn, fallbackTurn, firstTurn + called("call_1")}},
		{name: "no fallback model", answers: []answer{failedRequest},
			want: failure + " [ERR_LLM_INVOKE_FAILED_INITIAL]", requests: []string{firstTurn}},
		{name: "an answer that cannot be read is not a failed request", doc: fallback, answers: []answer{unreadable},
			want: failure + " [ERR_TOOL_MALFORMED_FUNCTION_CALL]", requests: []string{firstTurn}},
	})
}

func TestToolLoopAnswersFromTheResultsWhenATurnAfterCallsFails(t *testing.T) {
	answers := func(final ...answer) []answer {
		return append([]answer{toolTurn(call("call_1")), failedRequest}, final...)
	}
	before := []string{firstTurn, firstTurn + called("call_1")}
	retries := append(before, finalRequest(1, "FINAL_RETRY"), finalRequest(1, "FINAL_RETRY"))
	run(t, []loopCase{
		{name: "final answer", answers: answers(reply("summary answer")),
			want: "summary answer", requests: append(before, finalRequest(1, "FINAL_RETRY"))},
		{name: "empty final answers", answers: answers(reply(""), reply("")), want: done, requests: retries},
		{name: "failed final answers", answers: answers(failedRequest, failedRequest), want: done, requests: retries},
	})
}

func TestToolLoopEndsWithATurnWithoutCalls(t *testing.T) {
	run(t, []loopCase{
		{name: "empty at the token limit", answers: []answer{toolTurn(call("call_1")), cutOff(reply(""), true)},
			want: failure + " [ERR_LLM_RESPONSE_TRUNCATED]", requests: []string{firstTurn, firstTurn + called("call_1")}},
		{name: "empty after calls, then a final answer",
			answers:  []answer{toolTurn(call("call_1")), reply(""), cutOff(reply("retry answer"), true)},
			want:     "retry answer" + note,
			requests: []string{firstTurn, firstTurn + called("call_1"), finalRequest(1, "FINAL_RETRY")}},
		{name: "empty after calls, then empty final answers",
			answers: []answer{toolTurn(call("call_1")), reply(""), reply(""), reply("")}, want: done,
			requests: []string{firstTurn, firstTurn + called("call_1"), finalRequest(1, "FINAL_RETRY"),
				finalRequest(1, "FINAL_RETRY")}},
	})

	got, requests, logs := loop(t, "", llm.Messages("SYS", nil, "hi"), []answer{reply("")}, echo)
	check(t, got, requests, failure+" [ERR_TOOL_INITIAL_EMPTY_RESPONSE]", []string{firstTurn})
	records := logs.records(t)
	if len(records) != 1 || records[0]["msg"] != "model answered the first tool turn with nothing" {
		t.Errorf("logged %v, want the empty first turn", records)
	}
}

func TestToolLoopAnswersAtTheLimitWithoutTools(t *testing.T) {
	limited := []string{firstTurn, firstTurn + called("call_1"), finalRequest(1, "LIMIT")}
	twoCalls := []answer{toolTurn(call("call_1")), toolTurn(call("call_2"))}
	long := strings.Repeat("x", 900)
	failed := "Error: " + strings.Repeat("y", 900)
	run(t, []loopCase{
		{name: "empty, then a final answer", doc: `, "max_tool_iterations": 1`,
			answers: append(twoCalls, reply(""), reply(""), reply("second retry")), want: "second retry",
			requests: append(limited, finalRequest(1, "FINAL_RETRY"), finalRequest(1, "FINAL_RETRY"))},
		{name: "empty at the token limit", doc: `, "max_tool_iterations": 1`,
			answers: append(twoCalls, cutOff(reply(""), true)), want: failure + " [ERR_LLM_RESPONSE_TRUNCATED]",
			requests: limited},
		{name: "failed, then empty final answers", doc: `, "max_tool_iterations": 1`,
			answers: append(twoCalls, failedRequest, reply(""), reply("")), want: done,
			requests: append(limited, finalRequest(1, "FINAL_RETRY"), finalRequest(1, "FINAL_RETRY"))},
		{name: "mid-batch, then the last successful output", doc: `, "max_tool_calls": 1`,
			answers: []answer{toolTurn(call("call_1"), call("call_2")), reply(""), reply(""), reply("")},
			execute: returning(long), want: "DONE " + long[:300],
			requests: []string{firstTurn,
				strings.ReplaceAll(finalRequest(1, "LIMIT"), output, long[:800]),
				strings.ReplaceAll(finalRequest(1, "FINAL_RETRY"), output, long[:800]),
				strings.ReplaceAll(finalRequest(1, "FINAL_RETRY"), output, long[:800])}},
		{name: "mid-batch, then the last summary after its first separator", doc: `, "max_tool_calls": 1`,
			answers: []answer{toolTurn(call("call_1"), call("call_2")), reply(""), reply(""), reply("")},
			execute: returning(failed), want: textfmt.Clip("'q'}): "+failed, 500),
			requests: []string{firstTurn,
				strings.ReplaceAll(finalRequest(1, "LIMIT"), output, failed[:800]),
				strings.ReplaceAll(finalRequest(1, "FINAL_RETRY"), output, failed[:800]),
				strings.ReplaceAll(finalRequest(1, "FINAL_RETRY"), output, failed[:800])}},
	})
}

func TestToolLoopFallbackTextIsTheLastOutputThatIsNotAnError(t *testing.T) {
	for _, tc := range []struct {
		outs []string
		want string
	}{
		{[]string{"ERROR: x", "error:y", " Error: z", "Errors: w", "fine"}, "DONE fine"},
		{[]string{"fine", "ERROR: x", "error:y"}, "DONE fine"},
		{[]string{"ERROR: x", "error:y"}, "'q'}): error:y"},
		{[]string{""}, "DONE "},
	} {
		var batch []model.ToolCall
		for i := range tc.outs {
			batch = append(batch, call(fmt.Sprint("call_", i)))
		}
		got, _, _ := loop(t, "", llm.Messages("SYS", nil, "hi"),
			[]answer{toolTurn(batch...), reply(""), reply(""), reply("")}, returning(tc.outs...))
		if got != tc.want {
			t.Errorf("%q: reply %q, want %q", tc.outs, got, tc.want)
		}
	}
}

func TestToolLoopRetriesAMalformedFirstTurn(t *testing.T) {
	fallback := `, "tool_fallback_model": "fallback"`
	retry := firstTurn + "\nsystem: MALFORMED_RETRY"
	fallbackTurn := "fallback 0.1 4096 tools\nsystem: SYS\nsystem: GUARD\nuser: hi"
	withoutTools := func(code string) string {
		return "primary 0.7 8192\nsystem: SYS\nuser: hi\nsystem: UNAVAILABLE " + code
	}
	unavailable := withoutTools("ERR_TOOL_MALFORMED_FUNCTION_CALL")
	twice := func(next ...answer) []answer { return append([]answer{malformed, malformed}, next...) }
	withCalls := toolTurn(call("call_1"))
	withCalls.resp.Malformed = true
	badArgs := toolTurn(model.ToolCall{ID: "call_1", Name: "search_web", Arguments: json.RawMessage("{bad")})
	noID := toolTurn(model.ToolCall{Name: "search_web", Arguments: json.RawMessage("{}")})
	run(t, []loopCase{
		{name: "retry makes calls", answers: []answer{malformed, toolTurn(call("call_1")), reply("done")},
			want: "done", requests: []string{firstTurn, retry, firstTurn + called("call_1")}},
		{name: "calls of a malformed turn are not run", answers: []answer{withCalls, toolTurn(call("call_2")), reply("ok")},
			want: "ok", requests: []string{firstTurn, retry, firstTurn + called("call_2")}},
		{name: "retry fails", answers: []answer{malformed, failedRequest},
			want: failure + " [ERR_LLM_INVOKE_FAILED_INITIAL]", requests: []string{firstTurn, retry}},
		{name: "malformed twice", answers: twice(reply("answer without tools")),
			want: "answer without tools", requests: []string{firstTurn, retry, unavailable}},
		{name: "malformed twice, then empty", answers: twice(reply("")),
			want: failure + " [ERR_FINAL_RESPONSE_EMPTY]", requests: []string{firstTurn, retry, unavailable}},
		{name: "malformed twice, then empty at the token limit", answers: twice(cutOff(reply(""), true)),
			want: failure + " [ERR_LLM_RESPONSE_TRUNCATED]", requests: []string{firstTurn, retry, unavailable}},
		{name: "malformed twice, then failed", answers: twice(failedRequest),
			want: failure + " [ERR_FINAL_LLM_FAIL]", requests: []string{firstTurn, retry, unavailable}},
		{name: "fallback makes calls", doc: fallback, answers: twice(toolTurn(call("call_1")), reply("done")),
			want: "done", requests: []string{firstTurn, retry, fallbackTurn, firstTurn + called("call_1")}},
		{name: "fallback makes no calls", doc: fallback, answers: twice(reply("prose"), reply("answer")),
			want: "answer", requests: []string{firstTurn, retry, fallbackTurn, unavailable}},
		{name: "fallback fails", doc: fallback, answers: twice(failedRequest, reply("answer")), want: "answer",
			requests: []string{firstTurn, retry, fallbackTurn, withoutTools("ERR_TOOL_FALLBACK_LLM_FAIL")}},
		{name: "fallback call with arguments that are not JSON", doc: fallback, answers: twice(badArgs, reply("answer")),
			want: "answer", requests: []string{firstTurn, retry, fallbackTurn, unavailable}},
		{name: "fallback call without an ID", doc: fallback, answers: twice(noID, reply("answer")),
			want: "answer", requests: []string{firstTurn, retry, fallbackTurn, unavailable}},
	})
}

func TestToolLoopTreatsATurnItCannotUseAsMalformed(t *testing.T) {
	nameless := model.ToolCall{ID: "call_2", Arguments: json.RawMessage("{}")}
	run(t, []loopCase{
		{name: "first turn cannot be read", answers: []answer{unreadable},
			want: failure + " [ERR_TOOL_MALFORMED_FUNCTION_CALL]", requests: []string{firstTurn}},
		{name: "turn after calls cannot be read", answers: []answer{toolTurn(call("call_1")), unreadable, reply("answer")},
			want: "answer", requests: []string{firstTurn, firstTurn + called("call_1"), finalRequest(1, "FINAL_RETRY")}},
		{name: "first call without an ID", answers: []answer{toolTurn(model.ToolCall{Name: "search_web"})},
			execute: func(context.Context, string, json.RawMessage) string { panic("executed") },
			want:    failure + " [ERR_TOOL_MALFORMED_FUNCTION_CALL]", requests: []string{firstTurn}},
		{name: "call without a name after a call", answers: []answer{toolTurn(call("call_1"), nameless), reply("answer")},
			want: "answer", requests: []string{firstTurn, finalRequest(1, "FINAL_RETRY")}},
	})
}

func TestToolLoopSummarizesTheDecodedArguments(t *testing.T) {
	var batch []model.ToolCall
	var got []string
	for i, args := range []string{`{"b": 1.0, "a": [true, null]}`, "{bad", "null", `"s"`, "[1]", `{"q": "it's"}`} {
		batch = append(batch, model.ToolCall{ID: fmt.Sprint("call_", i), Name: "search_web",
			Arguments: json.RawMessage(args)})
	}
	execute := func(_ context.Context, _ string, args json.RawMessage) string {
		got = append(got, string(args))
		return "ok"
	}
	_, requests, _ := loop(t, "", llm.Messages("SYS", nil, "hi"),
		[]answer{toolTurn(batch...), reply(""), reply("x")}, execute)
	want := "HEADER\n- search_web({'b': 1.0, 'a': [True, None]}): ok\n- search_web({}): ok\n- search_web(None): ok\n" +
		"- search_web('s'): ok\n- search_web([1]): ok\n- search_web({'q': \"it's\"}): ok"
	if len(requests) != 3 || !strings.Contains(requests[2], "\nuser: "+want+"\n") {
		t.Errorf("requests:\n%s\nwant the summaries:\n%s", strings.Join(requests, "\n--\n"), want)
	}
	for i, c := range batch {
		if got[i] != string(c.Arguments) {
			t.Errorf("call %d executed with %s, want the arguments as written: %s", i, got[i], c.Arguments)
		}
	}
}
