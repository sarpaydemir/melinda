package tools

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/port/web"
	"github.com/sarpaydemir/dost/internal/store/vector"
)

// Builtin is what the built-in tools work with while answering one message.
type Builtin struct {
	Search web.Searcher
	// Fetch reads the pages visit_page visits.
	Fetch web.Fetcher
	// SearchLimit is the most results search_web reads.
	SearchLimit int
	// Knowledge returns the shared-knowledge entries nearest to a query, nearest first.
	Knowledge interface {
		Search(ctx context.Context, query string, limit int) ([]vector.Hit, error)
	}
	// Chat is the run's outbound path to the chat platform.
	Chat Outbound
	// ReplyThread is the thread the reply goes to, or "" for the conversation root.
	ReplyThread string
	// MaxProgressUpdates is the most updates send_progress_update posts, and ProgressUpdateMaxChars the code
	// points it keeps of each.
	MaxProgressUpdates     int
	ProgressUpdateMaxChars int
	// Clock times the waits between attempts of a failed call.
	Clock clock.Clock
	Log   *slog.Logger
}

// Outbound is the part of the chat platform a run writes through.
type Outbound interface {
	// Send posts a message and returns its ID.
	Send(ctx context.Context, m chat.Outgoing) (string, error)
}

// tools returns the built-in tools offered before the contributed ones and those offered after them, with their own
// state for one reply.
func (b Builtin) tools() (knowledge, conversation []tool.Tool) {
	knowledge = []tool.Tool{
		searchWeb{b},
		visitPage{b},
		searchKB{b},
		declared{"generate_image", `{"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"]}`},
	}
	conversation = []tool.Tool{
		&progressUpdate{b: b},
	}
	return knowledge, conversation
}

// declared is a tool offered by its name and argument schema; calling it is not supported yet.
type declared struct{ name, parameters string }

func (d declared) Name() string { return d.name }

func (d declared) Parameters() json.RawMessage { return json.RawMessage(d.parameters) }

func (declared) Call(context.Context, tool.Call) (string, error) { return "", errors.ErrUnsupported }
