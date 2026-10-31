package tools

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/port/web"
	"github.com/sarpaydemir/dost/internal/store/vector"
)

// Builtin is what the built-in tools work with while answering one message.
type Builtin struct {
	Search web.Searcher
	// SearchLimit is the most results search_web reads.
	SearchLimit int
	// Knowledge returns the shared-knowledge entries nearest to a query, nearest first.
	Knowledge interface {
		Search(ctx context.Context, query string, limit int) ([]vector.Hit, error)
	}
	// Clock times the waits between attempts of a failed call.
	Clock clock.Clock
	Log   *slog.Logger
}

// tools returns the built-in tools offered before the contributed ones and those offered after them.
func (b Builtin) tools() (knowledge, conversation []tool.Tool) {
	knowledge = []tool.Tool{
		searchWeb{b},
		declared{"visit_page", `{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`},
		searchKB{b},
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
