package chat_test

import (
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
