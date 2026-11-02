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
	"sync"

	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/port/tool"
	"github.com/sarpaydemir/melinda/internal/prompt"
)

// Set is the tools offered while answering one message, with what their calls are told about it. It shares no
// state with the sets of other replies.
type Set struct {
	offered  []tool.Tool
	call     tool.Call
	restrict bool
	crossed  *crossReads
	texts    *prompt.Catalog
	log      *slog.Logger
}

// crossReads are the conversations other than the current one that a reply's calls have read, in first-read order.
type crossReads struct {
	mu  sync.Mutex
	ids []string
}

func (c *crossReads) add(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Contains(c.ids, id) {
		c.ids = append(c.ids, id)
	}
}

func (c *crossReads) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.ids)
}

// Offer returns the tools offered to the model, in this order: the built-in knowledge tools, contributed, and the
// built-in conversation tools; the built-in tools work with b. With enabled not nil, only the tools it names are
// offered, in the same order; a name of no such tool is an error. Each call of a tool gets call with the call's
// arguments. With b.RestrictOutbound set, calls of outbound tools are refused and posts made inert once a call has
// read another conversation.
func Offer(call tool.Call, enabled []string, b Builtin, contributed []tool.Tool) (Set, error) {
	crossed := new(crossReads)
	b.crossed = crossed
	knowledge, conversation := b.tools()
	all := slices.Concat(knowledge, contributed, conversation)
	s := Set{offered: all, call: call, restrict: b.RestrictOutbound, crossed: crossed, texts: b.Catalog, log: b.Log}
	s.call.ReadFrom = func(id string) {
		if id != call.ConversationID {
			crossed.add(id)
		}
	}
	if enabled == nil {
		return s, nil
	}
	var errs []error
	for _, name := range enabled {
		if !slices.ContainsFunc(all, func(t tool.Tool) bool { return t.Name() == name }) {
			errs = append(errs, fmt.Errorf("enabled_tools: unknown tool %q", name))
		}
	}
	s.offered = slices.DeleteFunc(all, func(t tool.Tool) bool { return !slices.Contains(enabled, t.Name()) })
	return s, errors.Join(errs...)
}

// FileIDs are the files the tools uploaded for the reply, in upload order; the reply carries them.
func (s Set) FileIDs() []string {
	for _, t := range s.offered {
		if g, ok := t.(*generateImage); ok {
			return g.uploaded()
		}
	}
	return nil
}

// Post returns text as the reply posts it: unchanged, or inert once a restricted reply has read another conversation.
func (s Set) Post(text string) string {
	return post(s.restrict, s.crossed, text)
}

func post(restrict bool, crossed *crossReads, text string) string {
	if restrict && len(crossed.list()) > 0 {
		return inert(text)
	}
	return text
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
// the model reads: the tool's result, or an error text for an unknown name, a refused outbound call, or a failed or
// panicking call.
func (s Set) Execute(ctx context.Context, name string, args json.RawMessage) (result string) {
	i := slices.IndexFunc(s.offered, func(t tool.Tool) bool { return t.Name() == name })
	if i < 0 {
		return "Error: Unknown tool " + name
	}
	if _, ok := s.offered[i].(tool.Outbound); ok && s.restrict {
		if read := s.crossed.list(); len(read) > 0 {
			cmp.Or(s.log, slog.Default()).WarnContext(ctx, "outbound tool call refused after reading other conversations",
				"tool", name, "conversations", read)
			return s.texts.Text("tool.outbound.refused")
		}
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
