// Package model is the contract between the engine and a model provider adapter.
package model

import (
	"context"
	"encoding/json"
)

// Provider is a model provider as the engine uses it. A value is built from one settings snapshot and is safe for
// concurrent use. Retries are the adapter's; a returned error is final.
type Provider interface {
	Chat(ctx context.Context, r Request) (Response, error)
	Embed(ctx context.Context, model, text string) ([]float32, error)
	// Image generates one image. The error's text is shown to the model.
	Image(ctx context.Context, model, prompt string) (Image, error)
}

// Request is one chat completion request.
type Request struct {
	Model    string
	Messages []Message
	Tools    []ToolSpec
	// Temperature keeps the number as written in the settings; empty omits it.
	Temperature json.Number
	MaxTokens   int
}

// Role is the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one message of a chat transcript.
type Message struct {
	Role    Role
	Content string
	// Parts are files that follow Content in a user message.
	Parts []Part
	// ToolCalls are the calls an assistant message made.
	ToolCalls []ToolCall
	// ToolCallID and Name identify the call a tool message answers.
	ToolCallID string
	Name       string
	// Raw is the provider's own encoding of an assistant message it returned. The adapter replays it when the
	// message is part of a later request, so the provider gets back exactly what it sent.
	Raw json.RawMessage
}

// Part is a file in a user message. Images are sent as images, anything else as a file.
type Part struct {
	Filename  string
	MediaType string
	Data      []byte
}

// ToolCall is one call the model requested.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ToolSpec is a tool offered to the model.
type ToolSpec struct {
	Name        string
	Description string
	// Parameters is a JSON schema; its bytes are sent as they are.
	Parameters json.RawMessage
}

// Response is the model's answer.
type Response struct {
	// Message is the assistant message, with Content stripped of surrounding whitespace and Raw set.
	Message Message
	// Truncated reports a stop at the token limit.
	Truncated bool
	// CutOff reports a stop before a natural end: truncated, filtered, or failed by the provider.
	CutOff bool
	// Malformed reports a tool call the provider could not parse.
	Malformed bool
	// Reason is the provider's stop reason, for logs.
	Reason string
}

// Image is a generated image.
type Image struct {
	MediaType string
	Data      []byte
}
