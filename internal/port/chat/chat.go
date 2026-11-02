// Package chat is the contract between the engine and a chat platform adapter.
package chat

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/sarpaydemir/melinda/internal/textfmt"
)

// ErrTakenOver refuses a run's write after another process took the run over; the run must stop writing.
var ErrTakenOver = errors.New("in-flight record released by another process; nothing written")

// Platform is a chat platform as the engine uses it. A value is built from one settings snapshot and is safe for
// concurrent use.
type Platform interface {
	// Listen runs one intake session. It calls connected once the session receives events, then passes each
	// inbound event to deliver in arrival order. It returns the error that ended the session, or nil after ctx is
	// cancelled.
	Listen(ctx context.Context, connected func(), deliver Deliver) error
	// Replay passes to deliver, once each, the messages created after the given time that an intake session may
	// have missed, oldest first within each conversation. It returns early with the error of deliver.
	Replay(ctx context.Context, after time.Time, deliver Deliver) error
	// Decode reads an event passed to deliver, or a Message.Payload. It reports false for events that are not
	// messages, and does no I/O.
	Decode(event json.RawMessage) (Message, bool)

	// Identity is the agent's account as configured: ID is always set, Username only when configured.
	Identity() User
	// Self is the agent's account as the platform reports it.
	Self(ctx context.Context) (User, error)
	User(ctx context.Context, id string) (User, error)
	Attachment(ctx context.Context, id string) (Attachment, error)
	// Download returns an attachment's content. Past max bytes it stops reading and returns ErrTooLarge.
	Download(ctx context.Context, id string, max int64) ([]byte, error)

	// Send posts a message and returns its ID.
	Send(ctx context.Context, m Outgoing) (string, error)
	// Upload stores a file in a conversation for a later Send and returns its ID.
	Upload(ctx context.Context, conversationID string, f File) (string, error)
}

// Deliver hands one inbound event to the engine. It blocks while the engine's intake is full and returns
// ctx.Err() if ctx ends first. The engine takes ownership of event.
type Deliver func(ctx context.Context, event json.RawMessage) error

// ErrUnauthorized reports that the platform rejected the agent's credentials.
var ErrUnauthorized = errors.New("credentials rejected")

// ErrTooLarge reports an attachment larger than the requested limit.
var ErrTooLarge = errors.New("attachment exceeds the size limit")

// Message is an inbound message with the facts the engine's routing policy decides on.
type Message struct {
	ID             string
	ConversationID string
	// ThreadID is the thread's root message, or "" for a message at the conversation root.
	ThreadID      string
	AuthorID      string
	Text          string
	AttachmentIDs []string
	// Created is zero when the platform did not report it.
	Created time.Time
	Kind    Kind
	// System marks deleted messages and events that are not a person's message.
	System bool
	// Payload is the event as the adapter stores it in the debounce buffer. Its shape belongs to the adapter and
	// stays readable by Decode across versions.
	Payload json.RawMessage
}

// Kind tells who can read a conversation.
type Kind int

const (
	Public  Kind = iota // open to its team
	Private             // invite-only, including group conversations
	Direct              // the author and the agent only
)

// Outgoing is a message the agent sends.
type Outgoing struct {
	ConversationID string
	// ThreadID is the thread to reply in, or "" for the conversation root.
	ThreadID string
	Text     string
	FileIDs  []string
}

// File is content to upload.
type File struct {
	Name      string
	MediaType string
	Data      []byte
}

// Attachment describes a file attached to an inbound message.
type Attachment struct {
	Name      string
	Extension string
	MediaType string
	Size      int64
}

// User is an account on the platform. Fields the platform does not have are empty.
type User struct {
	ID        string
	Username  string
	FirstName string
	LastName  string
	FullName  string
	Nickname  string
	Position  string
}

// Label names the user as "First Last (@username)": the full name, else the nickname, the username, or the ID,
// followed by the username when there is one, each without surrounding whitespace.
func (u User) Label() string {
	username := textfmt.Strip(u.Username)
	name := cmp.Or(textfmt.Strip(textfmt.Strip(u.FirstName)+" "+textfmt.Strip(u.LastName)), textfmt.Strip(u.Nickname), username, u.ID)
	if username == "" {
		return name
	}
	return name + " (@" + username + ")"
}

// Requester returns the function that names the user id in records a tool writes: the Label of the user as users
// knows them, the id alone when the lookup fails, or "" when there is no user.
func Requester(users interface {
	User(ctx context.Context, id string) (User, error)
}, id string) func(context.Context) string {
	return func(ctx context.Context) string {
		if id == "" {
			return ""
		}
		u, err := users.User(ctx, id)
		if err != nil {
			u = User{}
		}
		u.ID = id
		return u.Label()
	}
}
