// Package conformance holds the suites that every adapter of a port must pass. An adapter's test runs a suite
// with a harness: the adapter wired to its testkit fake, and hooks that script the fake.
package conformance

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/testkit"
)

// ModelHarness is a model provider wired to a fake API.
type ModelHarness struct {
	// Provider is the adapter under test. Its retries must not wait.
	Provider model.Provider
	// Text makes the API answer the next chat request with an assistant message whose content is text.
	Text func(text string)
	// ToolCall makes the API answer the next chat request with one call of a tool; arguments is a JSON object.
	ToolCall func(id, name, arguments string)
	// Embedding makes the API answer the next embeddings request with vector.
	Embedding func(vector []float32)
	// Image makes the API answer the next image request with an image of mediaType, image/png or image/jpeg.
	Image func(mediaType string, data []byte)
	// Fail makes the API fail every later request.
	Fail func()
	// Hang makes the API hold back its answer to every later request until the client gives up.
	Hang func()
}

// Model runs the model provider suite; harness builds a fresh harness for each case.
func Model(t *testing.T, harness func(t *testing.T) ModelHarness) {
	user := model.Message{Role: model.RoleUser, Content: "What is the weather in Paris?"}
	ask := model.Request{Model: "m", Messages: []model.Message{user}}
	// requests are one request of each kind.
	requests := []struct {
		name string
		do   func(context.Context, model.Provider) error
	}{
		{"chat", func(ctx context.Context, p model.Provider) error { _, err := p.Chat(ctx, ask); return err }},
		{"embed", func(ctx context.Context, p model.Provider) error {
			_, err := p.Embed(ctx, "e", user.Content)
			return err
		}},
		{"image", func(ctx context.Context, p model.Provider) error {
			_, err := p.Image(ctx, "i", "A sunny street in Paris")
			return err
		}},
	}

	t.Run("answers with stripped text", func(t *testing.T) {
		h := harness(t)
		h.Text("  It is sunny.\n")
		resp, err := h.Provider.Chat(context.Background(), ask)
		if err != nil {
			t.Fatal(err)
		}
		m := resp.Message
		if m.Role != model.RoleAssistant || m.Content != "It is sunny." || len(m.ToolCalls) != 0 || len(m.Raw) == 0 {
			t.Errorf("message = %+v, want an assistant message %q without tool calls, with Raw set", m, "It is sunny.")
		}
	})

	t.Run("accepts its tool calls fed back", func(t *testing.T) {
		h := harness(t)
		h.ToolCall("call_1", "lookup", `{"query":"Paris weather"}`)
		h.Text("It is sunny.")
		req := ask
		req.Tools = []model.ToolSpec{{
			Name:        "lookup",
			Description: "Look up a fact.",
			Parameters:  []byte(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		}}
		first, err := h.Provider.Chat(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		calls := first.Message.ToolCalls
		if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Name != "lookup" {
			t.Fatalf("tool calls = %+v, want call_1 of lookup", calls)
		}
		if eq, err := testkit.JSONEqual(calls[0].Arguments, []byte(`{"query":"Paris weather"}`)); err != nil || !eq {
			t.Errorf("arguments = %s (%v), want {\"query\":\"Paris weather\"}", calls[0].Arguments, err)
		}
		req.Messages = []model.Message{user, first.Message,
			{Role: model.RoleTool, ToolCallID: "call_1", Name: "lookup", Content: "Sunny, 24 °C"}}
		second, err := h.Provider.Chat(context.Background(), req)
		if err != nil || second.Message.Content != "It is sunny." {
			t.Errorf("follow-up: got %q, %v; want %q", second.Message.Content, err, "It is sunny.")
		}
	})

	t.Run("sends image and file parts", func(t *testing.T) {
		h := harness(t)
		h.Text("A chart and a report.")
		req := model.Request{Model: "m", Messages: []model.Message{{
			Role: model.RoleUser, Content: "Describe these.", Parts: []model.Part{
				{Filename: "chart.png", MediaType: "image/png", Data: []byte("\x89PNG\r\n")},
				{Filename: "report.pdf", MediaType: "application/pdf", Data: []byte("%PDF-1.7")},
			},
		}}}
		resp, err := h.Provider.Chat(context.Background(), req)
		if err != nil || resp.Message.Content != "A chart and a report." {
			t.Errorf("got %q, %v; want the answer", resp.Message.Content, err)
		}
	})

	t.Run("embeds text", func(t *testing.T) {
		h := harness(t)
		want := []float32{0.0123, -0.5, 1, 0}
		h.Embedding(want)
		got, err := h.Provider.Embed(context.Background(), "e", user.Content)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("got %v, %v; want %v", got, err, want)
		}
	})

	t.Run("generates images", func(t *testing.T) {
		h := harness(t)
		for _, want := range []model.Image{
			{MediaType: "image/png", Data: []byte("\x89PNG\r\n\x1a\n")},
			{MediaType: "image/jpeg", Data: []byte("\xff\xd8\xff\xe0")},
		} {
			h.Image(want.MediaType, want.Data)
			got, err := h.Provider.Image(context.Background(), "i", "A sunny street in Paris")
			if err != nil || got.MediaType != want.MediaType || !bytes.Equal(got.Data, want.Data) {
				t.Errorf("got %s %q, %v; want %s %q", got.MediaType, got.Data, err, want.MediaType, want.Data)
			}
		}
	})

	t.Run("reports server errors", func(t *testing.T) {
		h := harness(t)
		h.Fail()
		for _, r := range requests {
			if err := r.do(context.Background(), h.Provider); err == nil {
				t.Errorf("%s: got no error", r.name)
			}
		}
	})

	t.Run("stops when cancelled", func(t *testing.T) {
		h := harness(t)
		h.Hang()
		for _, r := range requests {
			ctx, cancel := context.WithCancel(context.Background())
			stop := time.AfterFunc(50*time.Millisecond, cancel)
			if err := r.do(ctx, h.Provider); !errors.Is(err, context.Canceled) {
				t.Errorf("%s: got %v, want context.Canceled", r.name, err)
			}
			stop.Stop()
			cancel()
		}
	})
}
