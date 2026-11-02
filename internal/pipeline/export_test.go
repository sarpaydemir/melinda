package pipeline

import (
	"context"
	"log/slog"

	"github.com/sarpaydemir/melinda/internal/port/chat"
)

// Send posts m through a run's outbound path to platform.
func Send(ctx context.Context, platform chat.Platform, beforeWrite func(context.Context) error, m chat.Outgoing, log *slog.Logger) error {
	return send(ctx, &outbound{platform: platform, beforeWrite: beforeWrite}, m, log)
}

// Upload uploads f through a run's outbound path to platform.
func Upload(ctx context.Context, platform chat.Platform, beforeWrite func(context.Context) error, conversationID string, f chat.File) (string, error) {
	return (&outbound{platform: platform, beforeWrite: beforeWrite}).Upload(ctx, conversationID, f)
}
