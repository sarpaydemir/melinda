package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"

	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/prompt"
)

// Set is the tools offered while answering one message, with what their calls are told about it. It shares no
// state with the sets of other replies.
type Set struct {
	offered []tool.Tool
	call    tool.Call
	log     *slog.Logger
}

// Offer returns the tools offered to the model, in this order: the built-in knowledge tools, contributed, and the
// built-in conversation tools; the built-in tools work with b. With enabled not nil, only the tools it names are
// offered, in the same order; a name of no such tool is an error. Each call of a tool gets call with the call's
// arguments.
func Offer(call tool.Call, enabled []string, b Builtin, contributed []tool.Tool) (Set, error) {
	knowledge, conversation := b.tools()
	all := slices.Concat(knowledge, contributed, conversation)
	if enabled == nil {
		return Set{all, call, b.Log}, nil
	}
	var errs []error
	for _, name := range enabled {
		if !slices.ContainsFunc(all, func(t tool.Tool) bool { return t.Name() == name }) {
			errs = append(errs, fmt.Errorf("enabled_tools: unknown tool %q", name))
		}
	}
	offered := slices.DeleteFunc(all, func(t tool.Tool) bool { return !slices.Contains(enabled, t.Name()) })
	return Set{offered, call, b.Log}, errors.Join(errs...)
}

// Specs are the offered tools as the model is offered them, each described by its catalog entry
// tools.desc.<name>.
func (s Set) Specs(c *prompt.Catalog) []model.ToolSpec {
	specs := make([]model.ToolSpec, 0, len(s.offered))
	for _, t := range s.offered {
		specs = append(specs, model.ToolSpec{Name: t.Name(), Description: c.Text("tools.desc." + t.Name()), Parameters: t.Parameters()})
	}
	return specs
}

// Execute runs the model's call of the offered tool named name with the JSON arguments args and returns the text
// the model reads: the tool's result, or an error text for an unknown name or a failed or panicking call.
func (s Set) Execute(ctx context.Context, name string, args json.RawMessage) (result string) {
	i := slices.IndexFunc(s.offered, func(t tool.Tool) bool { return t.Name() == name })
	if i < 0 {
		return "Error: Unknown tool " + name
	}
	call := s.call
	call.Args = args
	defer func() {
		if v := recover(); v != nil {
			cmp.Or(s.log, slog.Default()).Error("tool call panicked", "tool", name, "panic", v, "stack", string(debug.Stack()))
			result = fmt.Sprintf("Error: Could not execute %s: %v", name, v)
		}
	}()
	out, err := s.offered[i].Call(ctx, call)
	if err != nil {
		return fmt.Sprintf("Error: Could not execute %s: %v", name, err)
	}
	return out
}
