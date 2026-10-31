package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/sarpaydemir/dost/internal/pipeline"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/prompt"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

const userPrefix = "Talking to: "

// history serves the last n items stored under a key.
type history map[string][]json.RawMessage

func (h history) Get(_ context.Context, key string, n int) ([]json.RawMessage, error) {
	return textfmt.Tail(h[key], n), nil
}

type failingHistory struct{}

func (failingHistory) Get(context.Context, string, int) ([]json.RawMessage, error) {
	return nil, errors.New("database unavailable")
}

// users answers every lookup with one profile or error and records the IDs asked for.
type users struct {
	user  chat.User
	err   error
	asked []string
}

func (u *users) User(_ context.Context, id string) (chat.User, error) {
	u.asked = append(u.asked, id)
	return u.user, u.err
}

type historyReader interface {
	Get(ctx context.Context, key string, n int) ([]json.RawMessage, error)
}

func loader(h historyReader, u *users, log *slog.Logger) pipeline.Loader {
	return pipeline.Loader{History: h, Users: u, UserPrefix: userPrefix, Log: log}
}

var discard = slog.New(slog.DiscardHandler)

func item(user string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"user": %q, "assistant": "ok"}`, user))
}

func TestLoadDescribesTheUser(t *testing.T) {
	full := chat.User{FirstName: "Ada", LastName: "Lovelace", Username: "ada", Position: "Engineering Lead"}
	cases := []struct {
		name   string
		user   chat.User
		err    error
		userID string
		want   string
	}{
		{"name, handle, ID, and position", full, nil, "uid_123", "Talking to: Ada Lovelace (@ada, ID: uid_123) - Engineering Lead"},
		{"without a position", chat.User{FirstName: "Ada", LastName: "Lovelace", Username: "ada"}, nil, "uid_123", "Talking to: Ada Lovelace (@ada, ID: uid_123)"},
		{"without a username", chat.User{FirstName: "Ada", LastName: "Lovelace"}, nil, "uid_123", "Talking to: Ada Lovelace (ID: uid_123)"},
		{"without a user ID", chat.User{FirstName: "Ada", LastName: "Lovelace", Username: "ada"}, nil, "", "Talking to: Ada Lovelace (@ada)"},
		{"empty profile", chat.User{}, nil, "uid_123", ""},
		{"failed lookup", full, errors.New("not found"), "uid_123", ""},
		{"first name alone", chat.User{FirstName: "Ada", LastName: " "}, nil, "uid_123", "Talking to: Ada (ID: uid_123)"},
		{"last name alone", chat.User{LastName: "Lovelace"}, nil, "uid_123", "Talking to: Lovelace (ID: uid_123)"},
		{"full name when first and last are blank", chat.User{FirstName: " ", FullName: " Ada L. ", Nickname: "al"}, nil, "uid_123", "Talking to: Ada L. (ID: uid_123)"},
		{"nickname after the full name", chat.User{FullName: "\t", Nickname: " al ", Username: "ada"}, nil, "uid_123", "Talking to: al (@ada, ID: uid_123)"},
		{"username as the name is not repeated as a handle", chat.User{Username: " ada "}, nil, "uid_123", "Talking to: ada (ID: uid_123)"},
		{"user ID as the name", chat.User{ID: "uid_123", Position: "Lead"}, nil, "uid_123", "Talking to: uid_123 (ID: uid_123) - Lead"},
		{"nothing to name the user by", chat.User{Position: "Lead"}, nil, "", ""},
		{"values are stripped", chat.User{FirstName: "\u3000Ada", LastName: "Lovelace\n", Username: " ada\t", Position: " Lead \u001f"}, nil, "uid_123", "Talking to: Ada Lovelace (@ada, ID: uid_123) - Lead"},
		{"blank position", chat.User{FirstName: "Ada", Position: " \t "}, nil, "uid_123", "Talking to: Ada (ID: uid_123)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := &users{user: c.user, err: c.err}
			got, err := loader(history{}, u, discard).Load(context.Background(), chat.Message{ID: "p1", AuthorID: c.userID})
			if err != nil {
				t.Fatal(err)
			}
			if got.UserContext != c.want {
				t.Errorf("UserContext = %q, want %q", got.UserContext, c.want)
			}
			if len(u.asked) != 1 || u.asked[0] != c.userID {
				t.Errorf("looked up %q, want [%q]", u.asked, c.userID)
			}
		})
	}
}

func TestLoadReadsTheConversationHistory(t *testing.T) {
	h := history{
		"u1":           {item("direct")},
		"thread:p1":    {item("channel post")},
		"thread:root1": {item("thread")},
	}
	u := chat.User{FirstName: "Ada", Username: "ada"}
	cases := []struct {
		name string
		msg  chat.Message
		want prompt.Context
	}{
		{
			"direct message",
			chat.Message{ID: "p1", AuthorID: "u1", Kind: chat.Direct},
			prompt.Context{History: h["u1"], UserContext: "Talking to: Ada (@ada, ID: u1)", UserID: "u1"},
		},
		{
			"thread in a direct message",
			chat.Message{ID: "p2", ThreadID: "root1", AuthorID: "u1", Kind: chat.Direct},
			prompt.Context{History: h["u1"], UserContext: "Talking to: Ada (@ada, ID: u1)", UserID: "u1"},
		},
		{
			"channel post",
			chat.Message{ID: "p1", AuthorID: "u1", Kind: chat.Public},
			prompt.Context{History: h["thread:p1"], UserContext: "Talking to: Ada (@ada, ID: u1)", UserID: "u1"},
		},
		{
			"thread reply in a private channel",
			chat.Message{ID: "p2", ThreadID: "root1", AuthorID: "u1", Kind: chat.Private},
			prompt.Context{History: h["thread:root1"], UserContext: "Talking to: Ada (@ada, ID: u1)", UserID: "u1"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := loader(h, &users{user: u}, discard).Load(context.Background(), c.msg)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Load = %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestLoadKeepsTheNewestTenOfThirtyStoredItems(t *testing.T) {
	var stored []json.RawMessage
	for i := 1; i <= 30; i++ {
		stored = append(stored, item(fmt.Sprint("m", i)))
	}
	got, err := loader(history{"thread:p1": stored}, &users{}, discard).Load(context.Background(), chat.Message{ID: "p1", AuthorID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.History, stored[20:]) {
		t.Errorf("History = %s, want items m21..m30", got.History)
	}
}

func TestLoadFailsWhenTheHistoryCannotBeRead(t *testing.T) {
	_, err := loader(failingHistory{}, &users{}, discard).Load(context.Background(), chat.Message{ID: "p1", AuthorID: "u1"})
	if err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Errorf("Load error = %v, want the history read error", err)
	}
}

func TestLoadLogsTheContextAtDebug(t *testing.T) {
	h := history{"thread:p1": {item(strings.Repeat("x", 5000))}}
	u := &users{user: chat.User{FirstName: "Ada", Username: "ada"}}
	msg := chat.Message{ID: "p1", AuthorID: "u1"}

	var logs bytes.Buffer
	debug := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if _, err := loader(h, u, debug).Load(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"read_key=thread:p1",
		"write_keys=\"[u1 thread:p1]\"",
		`user_context="Talking to: Ada (@ada, ID: u1)"`,
		"; showing head/tail] ...",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("debug log lacks %s:\n%s", want, logs.String())
		}
	}

	logs.Reset()
	info := slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := loader(h, u, info).Load(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "read_key") {
		t.Errorf("context logged above debug level:\n%s", logs.String())
	}
}
