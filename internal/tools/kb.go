package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sarpaydemir/dost/internal/kb"
	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

// kbMaxEntries is the most entries search_public_kb returns, whatever limit the model asks for.
const kbMaxEntries = 5

// searchKB is search_public_kb: the shared-knowledge entries nearest to the model's query, numbered.
type searchKB struct{ b Builtin }

func (searchKB) Name() string { return "search_public_kb" }

func (searchKB) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`)
}

// Call retries a failed search.
func (k searchKB) Call(ctx context.Context, c tool.Call) (string, error) {
	args, err := c.Arguments()
	if err != nil {
		return "", err
	}
	query, err := args.String("query", "q")
	if err != nil {
		return "", err
	}
	if query == "" {
		return "Error: Missing public KB query", nil
	}
	limit := max(1, min(kbMaxEntries, args.Int("limit", kb.Entries)))
	return tool.Retry(ctx, k.b.Clock, k.b.Log, k.Name(), func() (string, error) {
		hits, err := k.b.Knowledge.Search(ctx, query, limit)
		switch {
		case errors.Is(err, kb.ErrNoEmbeddingModel):
			return "Error: Public KB search requires embedding_model", nil
		case errors.Is(err, kb.ErrNotEmbedded):
			return "Error: Could not embed public KB query", nil
		case err != nil:
			return "", err
		case len(hits) == 0:
			return "No public KB results found.", nil
		}
		entries := make([]string, len(hits))
		for i, h := range hits {
			entries[i] = fmt.Sprintf("[%d] %s", i+1, textfmt.Str(value(h.Document)))
		}
		return strings.Join(entries, "\n\n"), nil
	})
}
