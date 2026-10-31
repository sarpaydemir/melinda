package chat_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sarpaydemir/dost/internal/port/chat"
)

func TestLabelIsTheBestNameFollowedByTheUsername(t *testing.T) {
	for _, tt := range []struct {
		user chat.User
		want string
	}{
		{chat.User{ID: "u1", Username: " ada ", FirstName: " Ada ", LastName: " Lovelace\t", Nickname: "nick"}, "Ada Lovelace (@ada)"},
		{chat.User{ID: "u6", Username: "dee", LastName: "Dee"}, "Dee (@dee)"},
		{chat.User{ID: "u5", FirstName: " Ann "}, "Ann"},
		{chat.User{ID: "u3", Username: "carl", Nickname: "Carlito"}, "Carlito (@carl)"},
		{chat.User{ID: "u2", Nickname: " Nicky "}, "Nicky"},
		{chat.User{ID: "u4", Username: "bob", LastName: " "}, "bob (@bob)"},
		{chat.User{ID: "u4", Username: "   "}, "u4"},
		{chat.User{ID: "u7"}, "u7"},
	} {
		if got := tt.user.Label(); got != tt.want {
			t.Errorf("%+v: Label() = %q, want %q", tt.user, got, tt.want)
		}
	}
}

// users knows the users of known; looking up anyone else fails. It records each lookup.
type users struct {
	known   map[string]chat.User
	lookups []string
}

func (u *users) User(_ context.Context, id string) (chat.User, error) {
	u.lookups = append(u.lookups, id)
	if user, ok := u.known[id]; ok {
		return user, nil
	}
	return chat.User{}, errors.New("no such user")
}

func TestRequesterIsTheLabelOfTheUserOrTheirID(t *testing.T) {
	u := &users{known: map[string]chat.User{"u1": {ID: "u1", Username: "ada", FirstName: "Ada", LastName: "Lovelace"},
		"u2": {Username: "bob"}}}
	for id, want := range map[string]string{"u1": "Ada Lovelace (@ada)", "u2": "bob (@bob)", "u9": "u9", "": ""} {
		if got := chat.Requester(u, id)(t.Context()); got != want {
			t.Errorf("Requester(%q) = %q, want %q", id, got, want)
		}
	}
	if len(u.lookups) != 3 {
		t.Errorf("lookups %q, want one for each user", u.lookups)
	}
}
