package mattermost_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/adapters/mattermost"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/conformance"
	"github.com/sarpaydemir/dost/internal/testkit/mattermostfake"
)

func settings(t testing.TB, doc string) *config.Settings {
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

func open(t *testing.T, mm *mattermostfake.Fake, username string) chat.Platform {
	t.Helper()
	return openLogging(t, mm, username, slog.New(slog.DiscardHandler))
}

func openLogging(t *testing.T, mm *mattermostfake.Fake, username string, log *slog.Logger) chat.Platform {
	t.Helper()
	doc, _ := json.Marshal(map[string]string{
		"model": "m", "mattermost_endpoint": mm.URL(), "mattermost_token": token,
		"mattermost_bot_id": bot.ID, "mattermost_bot_username": username,
	})
	p, err := mattermost.Open(settings(t, string(doc)), transport(t), log)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenRequiresEndpointTokenAndBotID(t *testing.T) {
	tests := []struct{ doc, want string }{
		{`{"model":"m"}`, "mattermost_endpoint: required\nmattermost_token: required\nmattermost_bot_id: required"},
		{`{"model":"m","mattermost_endpoint":"","mattermost_token":null,"mattermost_bot_id":"b"}`,
			"mattermost_endpoint: required\nmattermost_token: required"},
		{`{"model":"m","mattermost_endpoint":"https://chat.example","mattermost_token":"t","mattermost_bot_id":7}`,
			"mattermost_bot_id: expected string"},
	}
	for _, tt := range tests {
		_, err := mattermost.Open(settings(t, tt.doc), http.DefaultTransport, slog.New(slog.DiscardHandler))
		if err == nil || err.Error() != tt.want {
			t.Errorf("Open(%s) error:\n%v\nwant:\n%s", tt.doc, err, tt.want)
		}
	}
}

func TestIdentityIsTheConfiguredAccountWithoutIO(t *testing.T) {
	mm := newFake(t)
	if got, want := open(t, mm, "").Identity(), (chat.User{ID: bot.ID}); got != want {
		t.Errorf("without a username: %+v, want %+v", got, want)
	}
	if got, want := open(t, mm, "asistan").Identity(), (chat.User{ID: bot.ID, Username: "asistan"}); got != want {
		t.Errorf("with a username: %+v, want %+v", got, want)
	}
	mm.AssertCalls(t)
}

func TestLookupsMapMattermostFields(t *testing.T) {
	mm := newFake(t)
	mm.On("GET", "/api/v4/users/{user_id}").Reply(testkit.JSON(200, map[string]any{
		"id": ayse.ID, "username": "ayse", "first_name": "Ayşe", "last_name": "Yılmaz", "full_name": "Ayşe Y.",
		"nickname": "ay", "position": "İK", "roles": "system_user",
	}))
	mm.On("GET", "/api/v4/files/{file_id}/info").Reply(testkit.JSON(200, map[string]any{
		"id": file, "name": "Rapor.PDF", "extension": "pdf", "mime_type": "application/pdf", "size": 2048, "width": 0,
	}))
	p := open(t, mm, "")
	u, err := p.User(t.Context(), ayse.ID)
	if want := (chat.User{ID: ayse.ID, Username: "ayse", FirstName: "Ayşe", LastName: "Yılmaz", FullName: "Ayşe Y.",
		Nickname: "ay", Position: "İK"}); err != nil || u != want {
		t.Errorf("user = %+v, %v; want %+v", u, err, want)
	}
	a, err := p.Attachment(t.Context(), file)
	if want := (chat.Attachment{Name: "Rapor.PDF", Extension: "pdf", MediaType: "application/pdf", Size: 2048}); err != nil || a != want {
		t.Errorf("attachment = %+v, %v; want %+v", a, err, want)
	}
}

func TestChatConformance(t *testing.T) {
	conformance.Chat(t, func(t *testing.T) conformance.ChatHarness {
		mm := newFake(t)
		private := id("ops")
		mm.AddChannel(mattermostfake.Channel{ID: private, TeamID: team, Type: "P", Name: "ops", DisplayName: "Ops", Members: []string{ayse.ID}})
		conversations := map[chat.Kind]string{chat.Public: channel, chat.Private: private, chat.Direct: id("dm")}
		return conformance.ChatHarness{
			Event: func(kind chat.Kind, thread, text string) json.RawMessage {
				return mm.Event(mm.AddPost(mattermostfake.Post{ChannelID: conversations[kind], UserID: ayse.ID, RootID: thread, Message: text}))
			},
			SystemEvents: []json.RawMessage{
				mm.Event(mattermostfake.Post{ID: id("gone"), CreateAt: 1767225600000, DeleteAt: 1767225600999, UserID: ayse.ID, ChannelID: private, Message: "oops"}),
				mm.Event(mattermostfake.Post{ID: id("join"), CreateAt: 1767225600000, UserID: ayse.ID, ChannelID: private, Type: "system_join_channel", Message: "ayse joined the channel."}),
			},
			NonMessages: []json.RawMessage{
				json.RawMessage(`{"event":"hello","data":{"server_version":"10.5.0"},"broadcast":{"user_id":"` + bot.ID + `"},"seq":0}`),
				json.RawMessage(`{"event":"typing","data":{"parent_id":"","user_id":"` + ayse.ID + `"},"broadcast":{"channel_id":"` + channel + `"},"seq":3}`),
				json.RawMessage(`{"status":"OK","seq_reply":1}`),
			},
			Platform:     open(t, mm, ""),
			Agent:        chat.User{ID: bot.ID, Username: bot.Username, FirstName: bot.FirstName, LastName: bot.LastName},
			User:         chat.User{ID: ayse.ID, Username: ayse.Username, FirstName: ayse.FirstName, LastName: ayse.LastName, Nickname: ayse.Nickname, Position: ayse.Position},
			Conversation: channel,
			Sent: func() []chat.Outgoing {
				var out []chat.Outgoing
				for _, p := range mm.Posts(channel) {
					out = append(out, chat.Outgoing{ConversationID: p.ChannelID, ThreadID: p.RootID, Text: p.Message, FileIDs: p.FileIDs})
				}
				return out
			},
			Requests: func() int { return len(mm.Requests()) },
			Fail: func() {
				mm.Guard(func(testkit.Request) (testkit.Response, bool) {
					return testkit.Status(http.StatusInternalServerError), true
				})
			},
			Stall: func() {
				mm.Guard(func(testkit.Request) (testkit.Response, bool) { return testkit.Status(200).WithDelay(time.Hour), true })
			},
		}
	})
}
