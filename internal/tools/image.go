package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/port/tool"
)

// generateImage is generate_image: an image from the image model, uploaded to the conversation and attached to
// the reply.
type generateImage struct {
	b   Builtin
	mu  sync.Mutex
	ids []string // of the uploaded files, in upload order
}

func (*generateImage) Name() string { return "generate_image" }

func (*generateImage) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"]}`)
}

// Call reports a failed generation or upload in its result, which the model reads as a success.
func (g *generateImage) Call(ctx context.Context, c tool.Call) (string, error) {
	args, err := c.Arguments()
	if err != nil {
		return "", err
	}
	prompt, err := args.String("prompt")
	if err != nil {
		return "", err
	}
	texts := g.b.Catalog
	failed := func(detail string) string {
		return texts.Render("tool.image.failed", map[string]string{"detail": detail})
	}
	switch {
	case prompt == "":
		return failed("Missing prompt"), nil
	case g.b.ImageModel == "":
		return failed("image_generation_model setting is missing"), nil
	}
	img, err := tool.Retry(ctx, g.b.Clock, g.b.Log, g.Name(), func() (model.Image, error) {
		img, err := g.b.Images.Image(ctx, g.b.ImageModel, prompt)
		if err != nil && err.Error() == "" {
			err = errors.New(texts.Text("tool.image.unknown_error"))
		}
		return img, err
	})
	if err != nil {
		return failed(err.Error()), nil
	}
	extension := "jpg"
	if img.MediaType == "image/png" {
		extension = "png"
	}
	f := chat.File{Name: fmt.Sprintf("generated_%d.%s", len(g.uploaded())+1, extension), MediaType: img.MediaType, Data: img.Data}
	id, err := g.b.Chat.Upload(ctx, c.ConversationID, f)
	if err != nil {
		g.b.Log.WarnContext(ctx, "generated image upload failed", "conversation", c.ConversationID, "error", err)
		return texts.Text("tool.image.upload_failed"), nil
	}
	g.mu.Lock()
	g.ids = append(g.ids, id)
	g.mu.Unlock()
	return texts.Text("tool.image.attached"), nil
}

// uploaded returns the IDs of the files uploaded so far, in upload order.
func (g *generateImage) uploaded() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.ids)
}
