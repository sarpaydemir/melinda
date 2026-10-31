package routing

import (
	"cmp"
	"strings"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

// ShouldRespond reports whether the agent answers m. agent is the agent's account, with an empty Username when
// it could not be resolved; excluded is name_excluded_channels.
func ShouldRespond(m chat.Message, agent chat.User, excluded config.Channels) bool {
	if respond, decided := decideByFacts(m, agent.ID, excluded); decided {
		return respond
	}
	if agent.Username == "" {
		return false
	}
	text, name := textfmt.Lower(m.Text), textfmt.Lower(agent.Username)
	return strings.Contains(text, "@"+name) || (m.ThreadID != "" && strings.Contains(text, name))
}

// ShouldEnrichIdentity reports whether ShouldRespond needs the agent's username to decide m.
func ShouldEnrichIdentity(m chat.Message, agentID string, excluded config.Channels) bool {
	_, decided := decideByFacts(m, agentID, excluded)
	return !decided
}

// decideByFacts applies the rules that need no username and reports whether one of them decided.
func decideByFacts(m chat.Message, agentID string, excluded config.Channels) (respond, decided bool) {
	switch {
	case m.System, m.AuthorID == agentID:
		return false, true
	case m.Kind == chat.Direct, excluded.Has(m.ConversationID):
		return true, true
	}
	return false, false
}

// ContextID is the thread m belongs to: its thread root, or m itself at the conversation root.
func ContextID(m chat.Message) string { return cmp.Or(m.ThreadID, m.ID) }

// ReplyThread is the thread the reply to m goes to: the conversation root for a direct message, else ContextID.
func ReplyThread(m chat.Message) string {
	if m.Kind == chat.Direct {
		return ""
	}
	return ContextID(m)
}

// Scope is "private" for a direct message and "public" for any other, private conversations included.
func Scope(m chat.Message) string {
	if m.Kind == chat.Direct {
		return "private"
	}
	return "public"
}

// ConversationKey is the debounce buffer key for m under prefix: one per author in a direct conversation, and
// one per author and thread elsewhere, where a top-level message is its own thread.
func ConversationKey(prefix string, m chat.Message) string {
	if m.Kind == chat.Direct {
		return prefix + "dm:" + m.AuthorID
	}
	return prefix + "thread:" + ContextID(m) + ":" + m.AuthorID
}
