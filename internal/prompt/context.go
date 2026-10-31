package prompt

import "encoding/json"

// Context is what the reply to a message is generated from.
type Context struct {
	// History is the newest items of the conversation's history, oldest first, as stored.
	History []json.RawMessage
	// UserContext describes the message's author in one line, or is empty when the profile is unavailable.
	UserContext    string
	UserID         string
	LongTermMemory string
	Knowledge      string
	FileContext    string
	FileSummary    string
}
