package pipeline

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/sarpaydemir/dost/internal/logging"
	"github.com/sarpaydemir/dost/internal/memory"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/prompt"
	"github.com/sarpaydemir/dost/internal/routing"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

const (
	// historyWindow is how many of the newest history items a reply is generated from.
	historyWindow = 10
	// debugClip bounds each long value in debug logs, in characters.
	debugClip = 4000
)

// Loader loads a message's context from the conversation history, the shared knowledge, the author's profile, and
// the attachments. Every field is required.
type Loader struct {
	History interface {
		Get(ctx context.Context, key string, n int) ([]json.RawMessage, error)
	}
	Users interface {
		User(ctx context.Context, id string) (chat.User, error)
	}
	// Knowledge finds the shared knowledge relevant to a message's text.
	Knowledge interface {
		Preload(ctx context.Context, text string) string
	}
	// Files describes the message's attachments: the file context and the file summary.
	Files interface {
		Describe(ctx context.Context, m chat.Message) (fileCtx, summary string)
	}
	// UserPrefix introduces the author's name in UserContext.
	UserPrefix string
	Log        *slog.Logger
}

// Load returns the context for m: the newest items of the history its conversation reads, the shared knowledge
// relevant to its text, its author, and its attachments. It fails only when the history cannot be read.
func (l Loader) Load(ctx context.Context, m chat.Message) (prompt.Context, error) {
	read, write := memory.HistoryKeys(m.AuthorID, routing.ContextID(m), m.Kind == chat.Direct)
	items, err := l.History.Get(ctx, read, historyWindow)
	if err != nil {
		return prompt.Context{}, fmt.Errorf("read history %s: %w", read, err)
	}
	c := prompt.Context{History: items, UserID: m.AuthorID}
	c.Knowledge = l.Knowledge.Preload(ctx, m.Text)
	c.UserContext = l.userContext(ctx, m.AuthorID)
	c.FileContext, c.FileSummary = l.Files.Describe(ctx, m)
	if l.Log.Enabled(ctx, slog.LevelDebug) {
		l.Log.DebugContext(ctx, "context loaded",
			"read_key", read,
			"write_keys", write,
			"history", logging.Clip(fmt.Sprintf("%s", c.History), debugClip),
			"user_context", c.UserContext,
			"knowledge", logging.Clip(c.Knowledge, debugClip),
			"long_term_memory", logging.Clip(c.LongTermMemory, debugClip),
			"file_context", logging.Clip(c.FileContext, debugClip),
		)
	}
	return c, nil
}

// userContext is UserPrefix and the user's name, then the handle and ID when known, then the position. It is
// empty when the lookup fails, the profile is empty, or nothing names the user.
func (l Loader) userContext(ctx context.Context, id string) string {
	u, err := l.Users.User(ctx, id)
	if err != nil {
		l.Log.WarnContext(ctx, "user lookup failed", "user_id", id, "error", err)
		return ""
	}
	if u == (chat.User{}) {
		return ""
	}
	first, last := textfmt.Strip(u.FirstName), textfmt.Strip(u.LastName)
	fullName := first + " " + last
	if first == "" || last == "" {
		fullName = first + last
	}
	username := textfmt.Strip(u.Username)
	name := cmp.Or(fullName, textfmt.Strip(u.FullName), textfmt.Strip(u.Nickname), username, id)
	if name == "" {
		return ""
	}
	text := l.UserPrefix + name
	switch {
	case username != "" && username != name:
		text += " (@" + username
		if id != "" {
			text += ", ID: " + id
		}
		text += ")"
	case id != "":
		text += " (ID: " + id + ")"
	}
	if position := textfmt.Strip(u.Position); position != "" {
		text += " - " + position
	}
	return text
}
