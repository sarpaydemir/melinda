package tools

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/port/web"
)

// Builtin is what the built-in tools work with while answering one message.
type Builtin struct {
	Search web.Searcher
	// SearchLimit is the most results search_web reads.
	SearchLimit int
	Log         *slog.Logger
}

// tools returns the built-in tools offered before the contributed ones and those offered after them.
func (b Builtin) tools() (knowledge, conversation []tool.Tool) {
	knowledge = []tool.Tool{
		searchWeb{b},
		declared{"visit_page", `{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`},
		declared{"search_public_kb", `{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`},
		declared{"generate_image", `{"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"]}`},
	}
	conversation = []tool.Tool{
		declared{"send_progress_update", `{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`},
	}
	return knowledge, conversation
}

// declared is a tool offered by its name and argument schema; calling it is not supported yet.
type declared struct{ name, parameters string }

func (d declared) Name() string { return d.name }

func (d declared) Parameters() json.RawMessage { return json.RawMessage(d.parameters) }

func (declared) Call(context.Context, tool.Call) (string, error) { return "", errors.ErrUnsupported }
