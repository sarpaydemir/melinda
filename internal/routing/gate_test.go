package routing_test

import (
	"testing"

	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/routing"
)

const (
	author = "user-a"
	other  = "user-b"
	town   = "town-square"
	root   = "root-post"
	postID = "this-post"
)

var (
	agent      = chat.User{ID: agentID, Username: "Helper"}
	noUsername = chat.User{ID: agentID}
	excluded   = config.Channels{IDs: []string{"lobby", "support"}}
)

// message is a person's message in town at the conversation root, unless changed.
func message(change func(*chat.Message)) chat.Message {
	m := chat.Message{ID: postID, ConversationID: town, AuthorID: author, Kind: chat.Public}
	change(&m)
	return m
}

func in(kind chat.Kind) func(*chat.Message) {
	return func(m *chat.Message) { m.Kind = kind }
}

func text(s string) func(*chat.Message) {
	return func(m *chat.Message) { m.Text = s }
}

func all(changes ...func(*chat.Message)) func(*chat.Message) {
	return func(m *chat.Message) {
		for _, c := range changes {
			c(m)
		}
	}
}

var (
	inThread   = func(m *chat.Message) { m.ThreadID = root }
	system     = func(m *chat.Message) { m.System = true }
	byAgent    = func(m *chat.Message) { m.AuthorID = agentID }
	inExcluded = func(m *chat.Message) { m.ConversationID = "support" }
)

func TestShouldRespond(t *testing.T) {
	tests := []struct {
		name     string
		m        chat.Message
		agent    chat.User
		excluded config.Channels
		want     bool
	}{
		// 1. System messages are never answered, whatever else holds.
		{"system mention in a public channel", message(all(system, text("@helper hi"))), agent, excluded, false},
		{"system message in a direct conversation", message(all(system, in(chat.Direct))), agent, excluded, false},
		{"system message in an excluded channel", message(all(system, inExcluded)), agent, excluded, false},

		// 2. The agent's own messages are never answered.
		{"own direct message", message(all(byAgent, in(chat.Direct))), agent, excluded, false},
		{"own message mentioning itself", message(all(byAgent, text("@helper ping"))), agent, excluded, false},
		{"own message in an excluded channel", message(all(byAgent, inExcluded)), agent, excluded, false},
		{"own thread reply naming itself", message(all(byAgent, inThread, text("helper here"))), agent, excluded, false},

		// 3. Direct messages are always answered.
		{"direct message without a mention", message(all(in(chat.Direct), text("hi"))), agent, excluded, true},
		{"direct thread reply without a mention", message(all(in(chat.Direct), inThread, text("and?"))), agent, excluded, true},
		{"direct message without a known username", message(in(chat.Direct)), noUsername, excluded, true},
		{"empty direct message", message(in(chat.Direct)), agent, excluded, true},

		// 4. Excluded conversations are always answered.
		{"excluded channel without a mention", message(all(inExcluded, text("anyone?"))), agent, excluded, true},
		{"excluded channel without a known username", message(inExcluded), noUsername, excluded, true},
		{"excluded private channel", message(all(inExcluded, in(chat.Private))), agent, excluded, true},
		{"excluded list matches whole IDs only", message(func(m *chat.Message) { m.ConversationID = "lob" }), agent, excluded, false},
		{"excluded string matches a substring", message(func(m *chat.Message) { m.ConversationID = "lob" }), agent, config.Channels{Text: "lobby,support"}, true},
		{"empty excluded string matches nothing", message(text("hi")), agent, config.Channels{}, false},

		// 5. Without a username nothing else is answered.
		{"mention without a known username", message(text("@helper hi")), noUsername, excluded, false},
		{"thread reply without a known username", message(all(inThread, text("helper?"))), noUsername, excluded, false},

		// 6. A mention anywhere in the text, as a plain substring after lowercasing.
		{"mention in a public channel", message(text("@helper deploy please")), agent, excluded, true},
		{"mention in a private channel", message(all(in(chat.Private), text("@helper deploy"))), agent, excluded, true},
		{"mention in a thread", message(all(inThread, text("cc @helper"))), agent, excluded, true},
		{"mention in different case", message(text("@HELPER status?")), agent, excluded, true},
		{"mention as a longer name's prefix", message(text("@helperbot status?")), agent, excluded, true},
		{"mention inside an email address", message(text("mail x@helper.com")), agent, excluded, true},
		{"mention from another bot", message(func(m *chat.Message) { m.AuthorID = "other-bot"; m.Text = "@helper" }), agent, excluded, true},

		// 7. In a thread, the bare name is enough, anywhere in the text.
		{"bare name in a thread", message(all(inThread, text("helper can you check"))), agent, excluded, true},
		{"bare name inside a word in a thread", message(all(inThread, text("thehelpers"))), agent, excluded, true},
		{"bare name in a private thread", message(all(in(chat.Private), inThread, text("HELPER?"))), agent, excluded, true},

		// 8. Otherwise not answered.
		{"bare name at the channel root", message(text("helper can you check")), agent, excluded, false},
		{"no mention at the channel root", message(text("deploy please")), agent, excluded, false},
		{"no mention in a thread", message(all(inThread, text("thanks"))), agent, excluded, false},
		{"no mention in a private channel", message(all(in(chat.Private), text("hi"))), agent, excluded, false},
		{"empty message", message(text("")), agent, excluded, false},
		{"name split by a space", message(text("@help er")), agent, excluded, false},
	}
	for _, tt := range tests {
		if got := routing.ShouldRespond(tt.m, tt.agent, tt.excluded); got != tt.want {
			t.Errorf("%s: ShouldRespond(%+v, %+v) = %v, want %v", tt.name, tt.m, tt.agent, got, tt.want)
		}
	}
}

func TestShouldRespondLowercasesAsFullUnicode(t *testing.T) {
	tests := []struct {
		username, text string
		want           bool
	}{
		// İ lowercases to i followed by a combining dot above, so a plain i does not match it.
		{"İlkay", "@İLKAY bak", true},
		{"İlkay", "@i̇lkay bak", true},
		{"İlkay", "@ilkay bak", false},
		{"ilkay", "@İlkay bak", false},
		{"ILGAZ", "@ılgaz", false},
		{"ılgaz", "@ILGAZ", false},
		{"Şule", "@ŞULE", true},
		// A word-final capital sigma lowercases to ς, any other to σ.
		{"Κωστας", "γεια @ΚΩΣΤΑΣ", true},
		{"ΚΩΣΤΑΣ", "@κωστασ", false},
		{"ΣΑΣ", "@σας", true},
	}
	for _, tt := range tests {
		m := message(text(tt.text))
		if got := routing.ShouldRespond(m, chat.User{ID: agentID, Username: tt.username}, excluded); got != tt.want {
			t.Errorf("username %q, text %q: ShouldRespond = %v, want %v", tt.username, tt.text, got, tt.want)
		}
	}
}

func TestShouldEnrichIdentityOnlyWhenTheUsernameCanDecide(t *testing.T) {
	tests := []struct {
		name string
		m    chat.Message
		want bool
	}{
		{"system message", message(all(system, text("@helper"))), false},
		{"own message", message(all(byAgent, text("@helper"))), false},
		{"direct message", message(in(chat.Direct)), false},
		{"direct thread reply", message(all(in(chat.Direct), inThread)), false},
		{"excluded channel", message(inExcluded), false},
		{"public message without a mention", message(text("hi")), true},
		{"public mention", message(text("@helper")), true},
		{"private thread reply", message(all(in(chat.Private), inThread)), true},
	}
	for _, tt := range tests {
		if got := routing.ShouldEnrichIdentity(tt.m, agentID, excluded); got != tt.want {
			t.Errorf("%s: ShouldEnrichIdentity = %v, want %v", tt.name, got, tt.want)
		}
	}
	if routing.ShouldEnrichIdentity(message(func(m *chat.Message) { m.ConversationID = "lob" }), agentID, config.Channels{Text: "lobby"}) {
		t.Error("a channel contained in the excluded string needs no username")
	}
}

func TestDerivedConversationFacts(t *testing.T) {
	tests := []struct {
		name                       string
		m                          chat.Message
		context, reply, scope, key string
	}{
		{"public root", message(in(chat.Public)), postID, postID, "public", "agent:buffer:thread:" + postID + ":" + author},
		{"public thread", message(all(in(chat.Public), inThread)), root, root, "public", "agent:buffer:thread:" + root + ":" + author},
		{"private root", message(in(chat.Private)), postID, postID, "public", "agent:buffer:thread:" + postID + ":" + author},
		{"private thread", message(all(in(chat.Private), inThread)), root, root, "public", "agent:buffer:thread:" + root + ":" + author},
		{"direct root", message(in(chat.Direct)), postID, "", "private", "agent:buffer:dm:" + author},
		{"direct thread", message(all(in(chat.Direct), inThread)), root, "", "private", "agent:buffer:dm:" + author},
	}
	for _, tt := range tests {
		if got := routing.ContextID(tt.m); got != tt.context {
			t.Errorf("%s: ContextID = %q, want %q", tt.name, got, tt.context)
		}
		if got := routing.ReplyThread(tt.m); got != tt.reply {
			t.Errorf("%s: ReplyThread = %q, want %q", tt.name, got, tt.reply)
		}
		if got := routing.Scope(tt.m); got != tt.scope {
			t.Errorf("%s: Scope = %q, want %q", tt.name, got, tt.scope)
		}
		if got := routing.ConversationKey("agent:buffer:", tt.m); got != tt.key {
			t.Errorf("%s: ConversationKey = %q, want %q", tt.name, got, tt.key)
		}
	}
}

func TestConversationKeysSeparateAuthorsAndTopLevelPosts(t *testing.T) {
	key := func(change func(*chat.Message)) string {
		return routing.ConversationKey("agent:buffer:", message(change))
	}
	a := key(func(m *chat.Message) { m.ID, m.ThreadID = "p2", root })
	b := key(func(m *chat.Message) { m.ID, m.ThreadID, m.AuthorID = "p3", root, other })
	a2 := key(func(m *chat.Message) { m.ID, m.ThreadID = "p3", root })
	if a == b {
		t.Errorf("two authors in one thread share key %q", a)
	}
	if a != a2 {
		t.Errorf("one author's thread replies have keys %q and %q", a, a2)
	}
	if top1, top2 := key(func(m *chat.Message) { m.ID = "p1" }), key(func(m *chat.Message) { m.ID = "p2" }); top1 == top2 {
		t.Errorf("two top-level posts by one author share key %q", top1)
	}
}
