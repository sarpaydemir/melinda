package tools

import (
	"context"
	"log/slog"

	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/port/tool"
	"github.com/sarpaydemir/melinda/internal/port/web"
	"github.com/sarpaydemir/melinda/internal/prompt"
	"github.com/sarpaydemir/melinda/internal/store/vector"
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
	Images interface {
		Image(ctx context.Context, name, prompt string) (model.Image, error)
	}
	// ImageModel is the model Images generates with; "" when none is configured.
	ImageModel string
	// Chat is the run's outbound path to the chat platform.
	Chat Outbound
	// ReplyThread is the thread the reply goes to, or "" for the conversation root.
	ReplyThread string
	// MaxProgressUpdates is the most updates send_progress_update posts, and ProgressUpdateMaxChars the code
	// points it keeps of each.
	MaxProgressUpdates     int
	ProgressUpdateMaxChars int
	// RestrictOutbound refuses outbound calls for the rest of a reply once it has read another conversation.
	RestrictOutbound bool
	// Catalog holds the texts of the tools' results.
	Catalog *prompt.Catalog
	// Clock times the waits between attempts of a failed call.
	Clock clock.Clock
	Log   *slog.Logger

	crossed *crossReads
}

// Outbound is the part of the chat platform a run writes through.
type Outbound interface {
	// Send posts a message and returns its ID.
	Send(ctx context.Context, m chat.Outgoing) (string, error)
	// Upload stores a file in a conversation for a later Send and returns its ID.
	Upload(ctx context.Context, conversationID string, f chat.File) (string, error)
}

// tools returns the built-in tools offered before the contributed ones and those offered after them, with their own
// state for one reply.
func (b Builtin) tools() (knowledge, conversation []tool.Tool) {
	knowledge = []tool.Tool{
		searchWeb{b},
		visitPage{b},
		searchKB{b},
		&generateImage{b: b},
	}
	conversation = []tool.Tool{
		&progressUpdate{b: b},
	}
	return knowledge, conversation
}
