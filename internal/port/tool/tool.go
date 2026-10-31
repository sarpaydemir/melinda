// Package tool is the contract for functions the model can call, whether built in or contributed by an adapter.
package tool

import (
	"context"
	"encoding/json"
)

// Tool is a function the model can call. A value is built from one settings snapshot and is safe for concurrent
// use.
type Tool interface {
	// Name is the function name. The description offered to the model is the catalog entry tools.desc.<Name>.
	Name() string
	// Parameters is the JSON schema of the arguments, with properties in the order they are offered.
	Parameters() json.RawMessage
	// Call runs the tool. The result, and a returned error's text, are read by the model.
	Call(ctx context.Context, c Call) (string, error)
}

// Outbound is a Tool whose calls send what the model gives them outside the engine, such as a web request or a
// work-tracker write.
type Outbound interface {
	Tool
	Outbound()
}

// Call is one invocation, made while answering a message.
type Call struct {
	Args           json.RawMessage
	ConversationID string
	// UserID is the user being answered.
	UserID string
	// Direct is set when the conversation is one-to-one between the user being answered and the agent.
	Direct bool
	// Requester names the user being answered in records the tool writes, or returns "" when there is none.
	Requester func(context.Context) string
	// BeforeWrite must be called, and its error returned, before the tool's first change outside the engine.
	BeforeWrite func(context.Context) error
	// ReadFrom must be called with each conversation whose content the result holds.
	ReadFrom func(conversationID string)
}
