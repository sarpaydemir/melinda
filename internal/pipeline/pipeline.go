package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/sarpaydemir/dost/internal/automation"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/files"
	"github.com/sarpaydemir/dost/internal/kb"
	"github.com/sarpaydemir/dost/internal/llm"
	"github.com/sarpaydemir/dost/internal/logging"
	"github.com/sarpaydemir/dost/internal/memory"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/prompt"
	"github.com/sarpaydemir/dost/internal/routing"
	"github.com/sarpaydemir/dost/internal/store/vector"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

// fileMemoryLimit is how many code points of the file context a saved turn keeps.
const fileMemoryLimit = 8000

// Pipeline answers flushed conversations. Every field is required.
type Pipeline struct {
	Settings interface {
		Current(ctx context.Context) *config.Settings
	}
	// Platform and Model open the chat platform and the model provider from a settings snapshot.
	Platform func(*config.Settings) (chat.Platform, error)
	Model    func(*config.Settings) (model.Provider, error)
	Identity *routing.Identity
	History  interface {
		Get(ctx context.Context, key string, n int) ([]json.RawMessage, error)
		AddTurn(ctx context.Context, key, user, assistant string, metadata map[string]any) (int, error)
		Scoped(ctx context.Context, key, scope string, n int) ([]json.RawMessage, error)
	}
	// Knowledge holds the shared knowledge entries.
	Knowledge interface {
		Search(ctx context.Context, collection string, vec []float32, filter map[string]string, limit int) ([]vector.Hit, error)
	}
	// Facts holds the facts about each user.
	Facts interface {
		Search(ctx context.Context, collection string, vec []float32, filter map[string]string, limit int) ([]vector.Hit, error)
		Add(ctx context.Context, collection string, docs []vector.Doc) error
	}
	// Learning queues answered conversations for the learning run.
	Learning interface {
		Append(ctx context.Context, s *config.Settings, m chat.Message, reply string) error
	}
	// Handlers run the configured automations, by handler ID.
	Handlers map[string]automation.Handler
	// Prompts is the catalog that each run's settings override.
	Prompts *prompt.Catalog
	// Zone is the time zone of the time the system prompt shows.
	Zone  *time.Location
	Clock clock.Clock
	Log   *slog.Logger
}

// Answer answers j with one settings snapshot: it decodes the first buffered message with the combined text and
// attachments, applies the gate again, and runs the first matching automation in its place or loads the context,
// generates the reply, posts it, saves the turn, extracts the author's facts, and queues the turn for learning.
// beforeWrite is called before each write to the chat platform. Once posting has started, cancelling ctx no
// longer stops the run.
func (p *Pipeline) Answer(ctx context.Context, j routing.Job, beforeWrite func(context.Context) error) error {
	s := p.Settings.Current(ctx)
	log := p.Log.With(logging.KeyConversation, j.Key, "post_ids", j.MessageIDs)
	platform, err := p.Platform(s)
	if err != nil {
		return fmt.Errorf("open chat platform: %w", err)
	}
	m, ok := platform.Decode(j.Payload)
	if !ok {
		log.Debug("buffered payload is not a message")
		return nil
	}
	m.Text = j.Text
	if len(j.AttachmentIDs) > 0 {
		m.AttachmentIDs = j.AttachmentIDs
	}
	agent := platform.Identity()
	if routing.ShouldEnrichIdentity(m, agent.ID, s.NameExcludedChannels) {
		agent = p.Identity.Resolve(ctx, platform)
	}
	if !routing.ShouldRespond(m, agent, s.NameExcludedChannels) {
		if err := ctx.Err(); err != nil {
			return err // the username lookup may have been cut short
		}
		log.Debug("message not answered under the current settings")
		return nil
	}

	cat, err := p.Prompts.With(s.Prompts)
	if err != nil {
		return fmt.Errorf("apply prompt overrides: %w", err)
	}
	if a, args, ok := automation.Match(s.Automations, m.Text); ok {
		return p.automate(ctx, s, cat, m, a, args, &outbound{platform: platform, beforeWrite: beforeWrite}, log)
	}
	provider, err := p.Model(s)
	if err != nil {
		return fmt.Errorf("open model provider: %w", err)
	}
	client := llm.Client{Provider: provider, Failure: cat.Text("error.generic"), CutOffNote: cat.Text("reply.truncated_note"), Log: log}
	facts := memory.Facts{Settings: s, Model: client, Embedder: provider, Store: p.Facts, History: p.History,
		Prompt: cat.Text("memory.facts"), Log: log}
	c, err := p.loader(s, cat, platform, provider, client, facts, log).Load(ctx, m)
	if err != nil {
		return fmt.Errorf("load context: %w", err)
	}
	reply := p.generate(ctx, s, cat, client, c, m.Text, log)
	if err := ctx.Err(); err != nil {
		return err
	}

	ctx = context.WithoutCancel(ctx)
	out := &outbound{platform: platform, beforeWrite: beforeWrite}
	if err := send(ctx, out, chat.Outgoing{ConversationID: m.ConversationID, ThreadID: routing.ReplyThread(m), Text: reply}, log); err != nil {
		return fmt.Errorf("post reply: %w", err)
	}
	count, err := p.save(ctx, cat, m, c, reply)
	if err != nil {
		return fmt.Errorf("save turn: %w", err)
	}
	facts.Extract(ctx, m, reply, count)
	if err := p.Learning.Append(ctx, s, m, reply); err != nil {
		return fmt.Errorf("queue turn for learning: %w", err)
	}
	return nil
}

// automate runs the automation a in place of the reply: its handler posts, and the pipeline saves the handler's
// reply and queues it for learning unless it is a failure reply. A handler error ends the run without saving.
func (p *Pipeline) automate(ctx context.Context, s *config.Settings, cat *prompt.Catalog, m chat.Message,
	a config.Automation, args map[string]string, out *outbound, log *slog.Logger) error {
	log.Info("running automation", "automation", a.Name)
	reply, err := p.Handlers[a.Handler](ctx, automation.Run{Message: m, Settings: s, Args: args, Chat: out})
	if err != nil {
		return fmt.Errorf("automation %s: %w", a.Name, err)
	}
	if llm.IsErrorReply(reply) {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	if _, err := p.save(ctx, cat, m, prompt.Context{}, reply); err != nil {
		return fmt.Errorf("save turn: %w", err)
	}
	if err := p.Learning.Append(ctx, s, m, reply); err != nil {
		return fmt.Errorf("queue turn for learning: %w", err)
	}
	return nil
}

// loader loads a message's context with the run's snapshot, catalog, platform, model, and facts.
func (p *Pipeline) loader(s *config.Settings, cat *prompt.Catalog, platform chat.Platform, provider model.Provider,
	client llm.Client, facts memory.Facts, log *slog.Logger) Loader {
	return Loader{
		History: p.History,
		Users:   platform,
		Facts:   facts,
		Knowledge: kb.Retriever{Settings: s, Model: client, Embedder: provider, Store: p.Knowledge,
			QueryPrompt: cat.Text("kb.query"), Clock: p.Clock, Log: log},
		Files:      files.Describer{Platform: platform, Settings: s, Catalog: cat, Model: client, Log: log},
		UserPrefix: cat.Text("context.user_prefix"),
		Log:        log,
	}
}

// generate returns the model's reply to text in the context c, without tools.
func (p *Pipeline) generate(ctx context.Context, s *config.Settings, cat *prompt.Catalog, client llm.Client,
	c prompt.Context, text string, log *slog.Logger) string {
	system := prompt.System(cat, s, c, false, p.Clock.Now(), p.Zone)
	user := prompt.UserMessage(cat, c, text)
	log.DebugContext(ctx, "model input", "system_chars", utf8.RuneCountInString(system),
		"user_chars", utf8.RuneCountInString(user), "history_items", len(c.History))
	reply := client.Generate(ctx, model.Request{Model: s.Model, Messages: llm.Messages(system, c.History, user)})
	log.DebugContext(ctx, "model reply", "reply", logging.Clip(reply, debugClip))
	return reply
}

// save appends the turn to every history m's conversation writes to, the author's first. The user text is the
// combined message, then the attachment summary and the clipped file context when there are any. It returns the
// message count of the author's turn.
func (p *Pipeline) save(ctx context.Context, cat *prompt.Catalog, m chat.Message, c prompt.Context, reply string) (int, error) {
	contextID, direct := routing.ContextID(m), m.Kind == chat.Direct
	metadata := map[string]any{"scope": routing.Scope(m), "context_id": contextID, "user_id": m.AuthorID, "is_dm": direct}
	if m.ThreadID != "" {
		metadata["thread_id"] = contextID
	}
	user := m.Text
	if c.FileSummary != "" {
		user += cat.Render("memory.files_marker", map[string]string{"file_summary": c.FileSummary})
	}
	if c.FileContext != "" {
		user += cat.Text("memory.file_analysis_marker") + textfmt.Clip(c.FileContext, fileMemoryLimit)
	}
	_, keys := memory.HistoryKeys(m.AuthorID, contextID, direct)
	var count int
	for _, key := range keys {
		n, err := p.History.AddTurn(ctx, key, user, reply, metadata)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", key, err)
		}
		if key == m.AuthorID {
			count = n
		}
	}
	return count, nil
}

// outbound is a run's path to the chat platform: each write is preceded by beforeWrite. Until the first write, a
// cancelled run writes nothing, so it keeps its in-flight record.
type outbound struct {
	platform    chat.Platform
	beforeWrite func(context.Context) error
	started     atomic.Bool
}

func (o *outbound) permit(ctx context.Context) error {
	if !o.started.Load() {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	o.started.Store(true)
	return o.beforeWrite(ctx)
}

func (o *outbound) Send(ctx context.Context, m chat.Outgoing) (string, error) {
	if err := o.permit(ctx); err != nil {
		return "", err
	}
	return o.platform.Send(ctx, m)
}

func (o *outbound) Upload(ctx context.Context, conversationID string, f chat.File) (string, error) {
	if err := o.permit(ctx); err != nil {
		return "", err
	}
	return o.platform.Upload(ctx, conversationID, f)
}

// send posts a reply, and posts it once more without its files when posting it with them fails, unless it timed
// out: the post may exist. A failed post is logged, not returned; only a run that may not write fails.
func send(ctx context.Context, out *outbound, m chat.Outgoing, log *slog.Logger) error {
	_, err := out.Send(ctx, m)
	if err != nil && len(m.FileIDs) > 0 && !errors.Is(err, errTakenOver) && !errors.Is(err, context.DeadlineExceeded) {
		log.Warn("reply with files not posted; posting it without them", "error", err)
		m.FileIDs = nil
		_, err = out.Send(ctx, m)
	}
	switch {
	case errors.Is(err, errTakenOver):
		return err
	case err != nil:
		log.Error("reply not posted", "error", err)
	}
	return nil
}
