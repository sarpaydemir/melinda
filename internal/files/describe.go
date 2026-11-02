package files

import (
	"cmp"
	"context"
	"log/slog"
	"strings"

	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/llm"
	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/prompt"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

// Describer tells the model about a message's attachments, for one reply.
type Describer struct {
	Platform chat.Platform
	Settings *config.Settings
	Catalog  *prompt.Catalog
	// Model answers the vision requests.
	Model llm.Client
	Log   *slog.Logger
}

// Describe returns the file context of m's attachments, its notes joined by blank lines, and their summary. The
// vision model analyses each file in a request of its own.
func (d Describer) Describe(ctx context.Context, m chat.Message) (fileCtx, summary string) {
	c := Ingest(ctx, d.Platform, d.Settings, d.Log, m.AttachmentIDs, func(ctx context.Context, f File) Note {
		return d.analyse(ctx, f, m.Text)
	})
	notes := make([]string, len(c.Notes))
	for i, n := range c.Notes {
		notes[i] = d.Catalog.Render(n.ID, n.Values)
	}
	parts := make([]string, len(c.Parts))
	for i, p := range c.Parts {
		parts[i] = p.Name + " (" + cmp.Or(p.Kind, d.Catalog.Text("files.kind_fallback")) + ")"
	}
	return textfmt.Strip(strings.Join(notes, "\n\n")), strings.Join(parts, ", ")
}

// analyse asks the vision model about f. When the answer is or quotes a failure reply, the model is asked about an
// office document's text instead, and the text itself is the analysis when that fails too. A failure without
// text is noted as such, so a failure reply never reaches the model.
func (d Describer) analyse(ctx context.Context, f File, message string) Note {
	question := d.Catalog.Render("files.vision", map[string]string{"user_message": message, "file_list": f.Name})
	part := model.Part{Filename: f.Name, MediaType: mediaType(f.Attachment), Data: f.Data}
	analysis := d.ask(ctx, model.Message{Role: model.RoleUser, Content: question, Parts: []model.Part{part}})
	if llm.IsErrorReply(analysis) {
		text, err := officeText(ctx, f)
		if err != nil {
			d.Log.WarnContext(ctx, "document text extraction failed", "filename", f.Name, "error", err)
		}
		switch {
		case text != "":
			content := question + "\n\n" + d.Catalog.Text("files.vision_office_label") + "\n" + text
			if analysis = d.ask(ctx, model.Message{Role: model.RoleUser, Content: content}); llm.IsErrorReply(analysis) {
				analysis = d.Catalog.Render("files.office_fallback", map[string]string{"text": text})
			}
		case d.Model.Failed(analysis):
			return Note{ID: "files.analysis_unavailable", Values: map[string]string{"name": f.Name}}
		}
	}
	return Note{ID: "files.section", Values: map[string]string{"filename": f.Name, "analysis": analysis}}
}

func (d Describer) ask(ctx context.Context, m model.Message) string {
	return d.Model.Generate(ctx, model.Request{Model: d.Settings.VisionModel, Messages: []model.Message{m}})
}
