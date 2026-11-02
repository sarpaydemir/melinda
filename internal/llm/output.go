package llm

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

// Failure codes. A failure reply ends with its code in brackets.
const (
	CodeInvokeFailedInitial       = "ERR_LLM_INVOKE_FAILED_INITIAL"
	CodeInvokeFailedAfterTools    = "ERR_LLM_INVOKE_FAILED_AFTER_TOOLS"
	CodeFinalFailed               = "ERR_FINAL_LLM_FAIL"
	CodeFinalEmpty                = "ERR_FINAL_RESPONSE_EMPTY"
	CodeBudgetExhaustedFinalEmpty = "ERR_TOOL_BUDGET_EXHAUSTED_FINAL_EMPTY"
	CodeToolInitialEmpty          = "ERR_TOOL_INITIAL_EMPTY_RESPONSE"
	CodeMalformedToolCall         = "ERR_TOOL_MALFORMED_FUNCTION_CALL"
	CodeToolFallbackFailed        = "ERR_TOOL_FALLBACK_LLM_FAIL"
	CodeEmptyResponse             = "ERR_EMPTY_RESPONSE"
	CodeTruncated                 = "ERR_LLM_RESPONSE_TRUNCATED"
)

// Client turns a model's answers into what the agent says.
type Client struct {
	Provider interface {
		Chat(ctx context.Context, r model.Request) (model.Response, error)
	}
	// Failure is the catalog entry error.generic.
	Failure string
	// CutOffNote is the catalog entry reply.truncated_note.
	CutOffNote string
	Log        *slog.Logger
}

// IsErrorReply reports whether s is a failure reply, or contains one.
func IsErrorReply(s string) bool { return strings.Contains(s, "[ERR_") }

func (c Client) failure(code string) string { return c.Failure + " [" + code + "]" }

// Failed reports whether s is one of the failure replies Generate returns, unlike an answer that quotes one.
func (c Client) Failed(s string) bool {
	switch s {
	case c.failure(CodeTruncated), c.failure(CodeInvokeFailedInitial), c.failure(CodeEmptyResponse):
		return true
	}
	return false
}

// WithCutOffNote appends the cut-off note to content when r was cut off, and logs why it was.
func (c Client) WithCutOffNote(ctx context.Context, content string, r model.Response) string {
	if !r.CutOff {
		return content
	}
	c.Log.WarnContext(ctx, "model response cut off", "reason", r.Reason, "chars", utf8.RuneCountInString(content))
	return content + c.CutOffNote
}

// Generate sends r, a request without tools, and returns the answer's content. It returns a failure reply when the
// request fails, the answer stops at the token limit, or its content is empty.
func (c Client) Generate(ctx context.Context, r model.Request) string {
	resp, err := c.Provider.Chat(ctx, r)
	switch {
	case resp.Truncated:
		return c.failure(CodeTruncated)
	case err != nil:
		c.Log.ErrorContext(ctx, "model request failed", "error", err)
		return c.failure(CodeInvokeFailedInitial)
	case resp.Message.Content == "":
		return c.failure(CodeEmptyResponse)
	}
	return resp.Message.Content
}

// fence is a code fence, with or without a json tag, and the whitespace after it.
var fence = regexp.MustCompile(func() string {
	p, err := textfmt.TranslatePattern("```(?:json)?\\s*")
	if err != nil {
		panic(err)
	}
	return p
}())

// GenerateJSON generates as Generate does and decodes the answer, with its code fences removed, as JSON. It returns
// nil for an empty answer, for NULL in any case, and for an answer that is not JSON, such as a failure reply.
func (c Client) GenerateJSON(ctx context.Context, r model.Request) any {
	text := textfmt.Strip(strings.TrimRight(textfmt.Strip(fence.ReplaceAllString(c.Generate(ctx, r), "")), "`"))
	if text == "" || strings.EqualFold(text, "null") {
		return nil
	}
	v, err := textfmt.Decode([]byte(text), true)
	if err != nil {
		c.Log.WarnContext(ctx, "model output is not JSON", "error", err, "text", textfmt.Clip(text, 200))
		return nil
	}
	return v
}
