package llm_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sarpaydemir/dost/internal/llm"
	"github.com/sarpaydemir/dost/internal/port/model"
)

func TestMessagesLayOutTheSystemPromptHistoryAndUserMessage(t *testing.T) {
	history := []json.RawMessage{
		json.RawMessage(`{"user": "Hi", "assistant": "Hello!", "ts": "2026-10-31T21:00:00+00:00", "scope": "public"}`),
		json.RawMessage(`{"user": "", "assistant": "Only the reply"}`),
		json.RawMessage(`{"user": "Only the question", "assistant": ""}`),
		json.RawMessage(`{"user": "Broken?", "assistant": "Sorry. [ERR_EMPTY_RESPONSE]"}`),
		json.RawMessage(`{"user": "Read this\n\n(files: a.txt)\n\nAnalysis:\nnumbers", "assistant": "Done"}`),
		json.RawMessage(`{"message_count": 3}`),
	}
	tests := []struct {
		name, system string
		history      []json.RawMessage
		user         string
		want         []model.Message
	}{
		{
			name: "with history", system: "Be brief.", history: history, user: "What changed?",
			want: []model.Message{
				{Role: model.RoleSystem, Content: "Be brief."},
				{Role: model.RoleUser, Content: "Hi"},
				{Role: model.RoleAssistant, Content: "Hello!"},
				{Role: model.RoleAssistant, Content: "Only the reply"},
				{Role: model.RoleUser, Content: "Only the question"},
				{Role: model.RoleUser, Content: "Broken?"},
				{Role: model.RoleAssistant, Content: "Sorry. [ERR_EMPTY_RESPONSE]"},
				{Role: model.RoleUser, Content: "Read this\n\n(files: a.txt)\n\nAnalysis:\nnumbers"},
				{Role: model.RoleAssistant, Content: "Done"},
				{Role: model.RoleUser, Content: "What changed?"},
			},
		},
		{
			name: "without a system prompt", user: "Just this",
			want: []model.Message{{Role: model.RoleUser, Content: "Just this"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := llm.Messages(tt.system, tt.history, tt.user); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("messages:\n%+v\nwant:\n%+v", got, tt.want)
			}
		})
	}
}
