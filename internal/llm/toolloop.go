package llm

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/port/tool"
	"github.com/sarpaydemir/melinda/internal/prompt"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

// ToolLoop answers a message with tools: the model calls them one at a time and reads their results until it
// answers, and an answer is made from the results gathered when the limits are reached or the model fails.
type ToolLoop struct {
	Client   Client
	Settings *config.Settings
	// Catalog holds the tool loop's instructions to the model.
	Catalog *prompt.Catalog
	// Tools are offered to the model; Execute runs a call of one and returns the text the model reads.
	Tools   []model.ToolSpec
	Execute func(ctx context.Context, name string, args json.RawMessage) string
}

// toolRun is the state of one answer.
type toolRun struct {
	ToolLoop
	// base is the request's messages before any tool call.
	base []model.Message
	// summaries are a line per executed call, and successes the outputs of those that did not fail.
	summaries, successes []string
}

// Answer returns the reply to msgs, the messages laid out for the request, or a failure reply.
func (l ToolLoop) Answer(ctx context.Context, msgs []model.Message) string {
	msgs = slices.Clip(msgs)
	r := &toolRun{ToolLoop: l, base: msgs}
	s, log := l.Settings, l.Client.Log
	resp, err := r.turn(ctx, msgs)
	if failed(err) {
		log.ErrorContext(ctx, "model request failed", "error", err)
		return l.Client.failure(CodeInvokeFailedInitial)
	}
	if resp.Malformed {
		log.WarnContext(ctx, "malformed tool call; retrying once", "reason", resp.Reason)
		resp, err = r.turn(ctx, append(msgs, system(l.Catalog.Text("toolloop.malformed_retry"))))
		if failed(err) {
			log.ErrorContext(ctx, "model request failed", "error", err)
			return l.Client.failure(CodeInvokeFailedInitial)
		}
		if resp.Malformed {
			if s.ToolFallbackModel == "" {
				return r.withoutTools(ctx, CodeMalformedToolCall)
			}
			resp, err = r.chat(ctx, s.ToolFallbackModel, msgs, s.MaxTokens)
			switch {
			case failed(err):
				log.ErrorContext(ctx, "fallback model request failed", "error", err)
				return r.withoutTools(ctx, CodeToolFallbackFailed)
			case err != nil || !validCalls(resp.Message.ToolCalls):
				log.WarnContext(ctx, "fallback model made no valid tool calls", "reason", resp.Reason)
				return r.withoutTools(ctx, CodeMalformedToolCall)
			}
		}
	}

	iterations, calls := 0, 0
	for {
		switch {
		case err != nil:
			return r.malformed(ctx, err)
		case len(resp.Message.ToolCalls) == 0:
			return r.end(ctx, resp, iterations)
		case iterations >= s.ToolIterations() || calls >= s.ToolCalls():
			return r.limit(ctx, CodeFinalEmpty)
		}
		iterations++
		msgs = append(msgs, resp.Message)
		for i, call := range resp.Message.ToolCalls {
			if calls >= s.ToolCalls() {
				log.WarnContext(ctx, "tool call budget spent mid-batch; skipping the rest",
					"calls", calls, "skipped", len(resp.Message.ToolCalls)-i)
				return r.limit(ctx, CodeBudgetExhaustedFinalEmpty)
			}
			if call.ID == "" || call.Name == "" {
				return r.malformed(ctx, errors.New("tool call without an ID or a name"))
			}
			out := l.Execute(ctx, call.Name, call.Arguments)
			msgs = append(msgs, model.Message{Role: model.RoleTool, ToolCallID: call.ID, Name: call.Name, Content: out})
			r.summaries = append(r.summaries,
				"- "+call.Name+"("+textfmt.Repr(tool.Decode(call.Arguments))+"): "+textfmt.Clip(out, 800))
			if !strings.HasPrefix(textfmt.Lower(out), "error:") {
				r.successes = append(r.successes, out)
			}
			calls++
		}
		resp, err = r.turn(ctx, msgs)
		if failed(err) {
			log.ErrorContext(ctx, "model request failed after tool calls", "error", err)
			return r.recover(ctx, CodeInvokeFailedAfterTools)
		}
	}
}

// failed reports whether err failed a request, unlike an answer that came but cannot be read.
func failed(err error) bool { return err != nil && !errors.Is(err, model.ErrUnreadable) }

func system(text string) model.Message { return model.Message{Role: model.RoleSystem, Content: text} }

// turn sends msgs with the tools to the model, and to the fallback model when that request fails.
func (r *toolRun) turn(ctx context.Context, msgs []model.Message) (model.Response, error) {
	s := r.Settings
	resp, err := r.chat(ctx, s.Model, msgs, s.ToolFinalTokens())
	if !failed(err) || s.ToolFallbackModel == "" {
		return resp, err
	}
	r.Client.Log.WarnContext(ctx, "model request failed; retrying on the fallback model", "error", err)
	return r.chat(ctx, s.ToolFallbackModel, msgs, s.ToolFinalTokens())
}

// chat sends msgs with the tools and the format guard to the model named name.
func (r *toolRun) chat(ctx context.Context, name string, msgs []model.Message, maxTokens int) (model.Response, error) {
	return r.Client.Provider.Chat(ctx, model.Request{
		Model:       name,
		Messages:    withFormatGuard(msgs, r.Catalog.Text("tools.format_guard")),
		Tools:       r.Tools,
		Temperature: r.Settings.ToolCallTemperature,
		MaxTokens:   maxTokens,
	})
}

// withFormatGuard is msgs with the system message guard inserted after the first message when that is a system
// message, or else first.
func withFormatGuard(msgs []model.Message, guard string) []model.Message {
	at := 0
	if len(msgs) > 0 && msgs[0].Role == model.RoleSystem {
		at = 1
	}
	return slices.Insert(slices.Clone(msgs), at, system(guard))
}

// validCalls reports whether calls has a call and each has an ID, a name, and JSON arguments.
func validCalls(calls []model.ToolCall) bool {
	for _, c := range calls {
		if _, err := textfmt.Decode(c.Arguments, true); err != nil || c.ID == "" || c.Name == "" {
			return false
		}
	}
	return len(calls) > 0
}

// end answers with the content of a turn without tool calls.
func (r *toolRun) end(ctx context.Context, resp model.Response, iterations int) string {
	switch {
	case resp.Message.Content != "":
		return r.Client.WithCutOffNote(ctx, resp.Message.Content, resp)
	case resp.Truncated:
		return r.Client.failure(CodeTruncated)
	case iterations == 0:
		r.Client.Log.WarnContext(ctx, "model answered the first tool turn with nothing", "reason", resp.Reason)
		return r.Client.failure(CodeToolInitialEmpty)
	}
	return r.recover(ctx, CodeEmptyResponse)
}

// malformed answers after a turn whose message or tool calls cannot be used.
func (r *toolRun) malformed(ctx context.Context, err error) string {
	r.Client.Log.WarnContext(ctx, "model turn cannot be used", "error", err)
	if len(r.summaries) == 0 {
		return r.Client.failure(CodeMalformedToolCall)
	}
	return r.recover(ctx, CodeMalformedToolCall)
}

// limit answers when no more tool calls are allowed.
func (r *toolRun) limit(ctx context.Context, code string) string {
	resp, err := r.final(ctx, r.Catalog.Text("toolloop.limit_reached"))
	switch {
	case err != nil:
		r.Client.Log.ErrorContext(ctx, "final model request failed", "error", err)
	case resp.Message.Content != "":
		return r.Client.WithCutOffNote(ctx, resp.Message.Content, resp)
	case resp.Truncated:
		return r.Client.failure(CodeTruncated)
	}
	return r.recover(ctx, code)
}

// withoutTools answers without tools after the model failed to call them, for the reason code.
func (r *toolRun) withoutTools(ctx context.Context, code string) string {
	resp, err := r.final(ctx, r.Catalog.Render("toolloop.unavailable", map[string]string{"reason": code}))
	switch {
	case err != nil:
		r.Client.Log.ErrorContext(ctx, "final model request failed", "error", err)
		return r.Client.failure(CodeFinalFailed)
	case resp.Message.Content != "":
		return r.Client.WithCutOffNote(ctx, resp.Message.Content, resp)
	case resp.Truncated:
		return r.Client.failure(CodeTruncated)
	}
	return r.Client.failure(CodeFinalEmpty)
}

// recover answers from the tool results: the model's answer to up to two final requests, or else the fallback
// text: the last successful output, else the last summary, else the failure reply for code.
func (r *toolRun) recover(ctx context.Context, code string) string {
	for attempt := 1; attempt <= 2; attempt++ {
		resp, err := r.final(ctx, r.Catalog.Text("toolloop.final_retry"))
		if err != nil {
			r.Client.Log.WarnContext(ctx, "final answer request failed", "attempt", attempt, "error", err)
			continue
		}
		if resp.Message.Content != "" {
			return r.Client.WithCutOffNote(ctx, resp.Message.Content, resp)
		}
	}
	if n := len(r.successes); n > 0 {
		return r.Catalog.Text("toolloop.done_prefix") + " " + textfmt.Clip(r.successes[n-1], 300)
	}
	if n := len(r.summaries); n > 0 {
		last := r.summaries[n-1]
		if _, after, ok := strings.Cut(last, ": "); ok {
			last = after
		}
		return textfmt.Clip(last, 500)
	}
	return r.Client.failure(code)
}

// final asks the model, without tools, to answer from the messages before any tool call, the tool results, and
// the system message instruction.
func (r *toolRun) final(ctx context.Context, instruction string) (model.Response, error) {
	s := r.Settings
	msgs := slices.Clone(r.base)
	if len(r.summaries) > 0 {
		msgs = append(msgs, model.Message{
			Role:    model.Role(s.ToolSummaryRole),
			Content: r.Catalog.Text("toolloop.context_header") + "\n" + strings.Join(r.summaries, "\n"),
		})
	}
	msgs = append(msgs, system(instruction))
	return r.Client.Provider.Chat(ctx, model.Request{
		Model: s.Model, Messages: msgs, Temperature: s.Temperature, MaxTokens: s.ToolFinalTokens(),
	})
}
