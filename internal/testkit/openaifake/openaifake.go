// Package openaifake fakes an OpenAI-compatible API: chat completions and embeddings.
package openaifake

import (
	"testing"

	"github.com/sarpaydemir/dost/internal/testkit"
)

// Fake serves POST /chat/completions and POST /embeddings at the root of its URL; every answer is scripted.
type Fake struct{ *testkit.Server }

// New starts a fake.
func New(t testing.TB) *Fake { return &Fake{testkit.NewServer(t)} }

// Chat is the chat completions route.
func (f *Fake) Chat() *testkit.Route { return f.On("POST", "/chat/completions") }

// Embeddings is the embeddings route.
func (f *Fake) Embeddings() *testkit.Route { return f.On("POST", "/embeddings") }

// Option sets part of a completion.
type Option func(choice, message map[string]any)

// Content sets message.content: a string, nil for null, or a list of content parts.
func Content(v any) Option { return func(_, m map[string]any) { m["content"] = v } }

// Finish sets finish_reason; "" means null.
func Finish(reason string) Option {
	return func(c, _ map[string]any) {
		if reason == "" {
			c["finish_reason"] = nil
		} else {
			c["finish_reason"] = reason
		}
	}
}

// NativeFinish sets the provider's native_finish_reason.
func NativeFinish(reason string) Option {
	return func(c, _ map[string]any) { c["native_finish_reason"] = reason }
}

// ToolCall is a function call requested by the model. Arguments is encoded as given: providers send a JSON
// string, some send an object.
type ToolCall struct {
	ID        string
	Name      string
	Arguments any
}

// Calls sets message.tool_calls.
func Calls(calls ...ToolCall) Option {
	return func(_, m map[string]any) {
		list := make([]map[string]any, len(calls))
		for i, tc := range calls {
			list[i] = map[string]any{
				"id": tc.ID, "type": "function",
				"function": map[string]any{"name": tc.Name, "arguments": tc.Arguments},
			}
		}
		m["tool_calls"] = list
	}
}

// Completion is a chat completion with one choice: an empty assistant message with finish_reason "stop",
// changed by opts.
func Completion(opts ...Option) testkit.Response {
	message := map[string]any{"role": "assistant", "content": ""}
	choice := map[string]any{"index": 0, "finish_reason": "stop", "message": message}
	for _, o := range opts {
		o(choice, message)
	}
	return testkit.JSON(200, map[string]any{
		"id": "chatcmpl-fake", "object": "chat.completion", "model": "fake", "choices": []any{choice},
	})
}

// Text is a completion whose message content is content.
func Text(content string, opts ...Option) testkit.Response {
	return Completion(append([]Option{Content(content)}, opts...)...)
}

// ToolCalls is a completion that requests calls, with finish_reason "tool_calls".
func ToolCalls(calls ...ToolCall) testkit.Response {
	return Completion(Calls(calls...), Finish("tool_calls"))
}

// ErrorPayload is an HTTP 200 answer without choices that carries an error, as some providers send when an
// upstream model fails.
func ErrorPayload(message string, code any) testkit.Response {
	return testkit.JSON(200, map[string]any{"error": map[string]any{"message": message, "code": code}})
}

// HTTPError is an error answer with an HTTP status, such as 429 or 503.
func HTTPError(status int, message string) testkit.Response {
	return testkit.JSON(status, map[string]any{"error": map[string]any{"message": message, "code": status}})
}

// Embedding answers an embeddings request with one vector per input.
func Embedding(vectors ...[]float64) testkit.Response {
	data := make([]map[string]any, len(vectors))
	for i, v := range vectors {
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": v}
	}
	return testkit.JSON(200, map[string]any{"object": "list", "model": "fake", "data": data})
}
