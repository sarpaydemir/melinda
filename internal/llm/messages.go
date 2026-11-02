package llm

import (
	"encoding/json"

	"github.com/sarpaydemir/melinda/internal/port/model"
)

// Messages lays out a reply's request: the system prompt unless it is empty, the non-empty user and assistant
// texts of each history item, oldest first, then the user's message.
func Messages(system string, history []json.RawMessage, user string) []model.Message {
	var msgs []model.Message
	if system != "" {
		msgs = append(msgs, model.Message{Role: model.RoleSystem, Content: system})
	}
	for _, item := range history {
		var turn map[string]any
		_ = json.Unmarshal(item, &turn) // an item that is not an object adds nothing
		if text, _ := turn["user"].(string); text != "" {
			msgs = append(msgs, model.Message{Role: model.RoleUser, Content: text})
		}
		if text, _ := turn["assistant"].(string); text != "" {
			msgs = append(msgs, model.Message{Role: model.RoleAssistant, Content: text})
		}
	}
	return append(msgs, model.Message{Role: model.RoleUser, Content: user})
}
