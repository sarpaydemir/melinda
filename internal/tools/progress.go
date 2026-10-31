package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

// progressUpdate is send_progress_update: an interim message posted in the reply's thread while the reply
// continues.
type progressUpdate struct {
	b Builtin
	// mu is held from the limit check until a post is counted, so concurrent calls cannot exceed the limit.
	mu     sync.Mutex
	posted int
}

func (*progressUpdate) Name() string { return "send_progress_update" }

func (*progressUpdate) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`)
}

// Call posts once, without retrying, so that a post whose outcome is unknown never reaches the user twice.
func (p *progressUpdate) Call(ctx context.Context, c tool.Call) (string, error) {
	args, err := c.Arguments()
	if err != nil {
		return "", err
	}
	message, err := args.String("message")
	if err != nil {
		return "", err
	}
	if message == "" {
		return "Error: Missing progress message", nil
	}
	if c.ConversationID == "" {
		return "Error: send_progress_update requires the current conversation", nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.posted >= p.b.MaxProgressUpdates {
		return "Error: Progress update limit reached for this request. Continue the task and give the final answer.", nil
	}
	m := chat.Outgoing{ConversationID: c.ConversationID, ThreadID: p.b.ReplyThread,
		Text: post(p.b.RestrictOutbound, p.b.crossed, textfmt.Clip(message, p.b.ProgressUpdateMaxChars))}
	id, err := p.b.Chat.Send(ctx, m)
	if err == nil && id == "" {
		err = errors.New("no message ID returned")
	}
	if err != nil {
		p.b.Log.Error("progress update failed", "conversation", c.ConversationID, "error", err)
		return "Error: Progress update could not be posted", nil
	}
	p.posted++
	return "Progress update posted to the user. Continue the task.", nil
}
