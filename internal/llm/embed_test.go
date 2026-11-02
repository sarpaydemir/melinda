package llm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/llm"
)

// embedder answers every embedding request with its vector or error and records the requests.
type embedder struct {
	vector []float32
	err    error
	calls  []string
}

func (e *embedder) Embed(_ context.Context, model, text string) ([]float32, error) {
	e.calls = append(e.calls, model+": "+text)
	return e.vector, e.err
}

func settings(t *testing.T, doc string) *config.Settings {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatal(err)
	}
	s, err := config.Load(m, "agent", nil, func(*config.Settings) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEmbed(t *testing.T) {
	const text = "  Fact:\nthe user likes tea. \n"
	stored := make([]float32, 1536)
	stored[0] = 0.5
	for _, tc := range []struct {
		name      string
		doc       string
		cancelled bool
		embedder  embedder
		want      []float32
		calls     []string
		log       []string // parts of the one line logged, if any
	}{
		{"returns the embedding", `{"model":"m","embedding_model":"embed-1"}`, false, embedder{vector: stored},
			stored, []string{"embed-1: " + text}, nil},
		{"needs an embedding model", `{"model":"m"}`, false, embedder{vector: stored}, nil, nil, nil},
		{"logs a failure", `{"model":"m","embedding_model":"embed-1"}`, false,
			embedder{err: errors.New("HTTP status 503")}, nil, []string{"embed-1: " + text},
			[]string{"level=ERROR", "model=embed-1", `error="HTTP status 503"`}},
		{"does not log a cancellation", `{"model":"m","embedding_model":"embed-1"}`, true,
			embedder{err: context.Canceled}, nil, []string{"embed-1: " + text}, nil},
		{"rejects a length the store cannot hold", `{"model":"m","embedding_model":"embed-small"}`, false,
			embedder{vector: make([]float32, 768)}, nil, []string{"embed-small: " + text},
			[]string{"level=ERROR", "model=embed-small", "length=768", "want=1536"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			got := llm.Embed(ctx, &tc.embedder, settings(t, tc.doc), slog.New(slog.NewTextHandler(&logs, nil)), text)
			if !slices.Equal(got, tc.want) {
				t.Errorf("got a vector of length %d, want %d", len(got), len(tc.want))
			}
			if !slices.Equal(tc.embedder.calls, tc.calls) {
				t.Errorf("requests %q, want %q", tc.embedder.calls, tc.calls)
			}
			if n := strings.Count(logs.String(), "\n"); n != min(len(tc.log), 1) {
				t.Errorf("logged %q, want %d lines", logs.String(), min(len(tc.log), 1))
			}
			for _, part := range tc.log {
				if !strings.Contains(logs.String(), part) {
					t.Errorf("log %q does not contain %s", logs.String(), part)
				}
			}
		})
	}
}
