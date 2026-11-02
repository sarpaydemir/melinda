package llm

import (
	"context"
	"log/slog"

	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/store/pg"
)

// embedder is the part of a model provider that Embed uses.
type embedder interface {
	Embed(ctx context.Context, model, text string) ([]float32, error)
}

// Embed returns the embedding of text by the embedding model. It is nil when no embedding model is set, and when
// the embedding fails or has a length the store cannot hold, which is logged unless ctx is done.
func Embed(ctx context.Context, p embedder, s *config.Settings, log *slog.Logger, text string) []float32 {
	if s.EmbeddingModel == "" {
		return nil
	}
	vector, err := p.Embed(ctx, s.EmbeddingModel, text)
	if err != nil {
		if ctx.Err() == nil {
			log.ErrorContext(ctx, "embedding failed", "model", s.EmbeddingModel, "error", err)
		}
		return nil
	}
	if len(vector) != pg.EmbeddingDims {
		log.ErrorContext(ctx, "embedding has the wrong length", "model", s.EmbeddingModel, "length", len(vector),
			"want", pg.EmbeddingDims)
		return nil
	}
	return vector
}
