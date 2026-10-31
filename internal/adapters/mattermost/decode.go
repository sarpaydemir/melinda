package mattermost

import (
	"cmp"
	"encoding/json"
	"time"

	"github.com/sarpaydemir/dost/internal/port/chat"
)

// post is the part of a Mattermost post that routing reads.
type post struct {
	ID, UserID, ChannelID, RootID, Message, Type, ChannelType string
	CreateAt, DeleteAt                                        int64
	FileIDs                                                   []string
}

// Decode reads a websocket event or a stored payload whose data.post is a JSON object encoded as a non-empty
// string. A non-empty data.channel_type overrides the post's own. The payload is the event with data.post
// re-encoded to hold every original field plus the routing fields stored buffers have always carried.
func (*platform) Decode(event json.RawMessage) (chat.Message, bool) {
	var frame, data, fields map[string]json.RawMessage
	var encoded string
	if json.Unmarshal(event, &frame) != nil || json.Unmarshal(frame["data"], &data) != nil ||
		json.Unmarshal(data["post"], &encoded) != nil || encoded == "" ||
		json.Unmarshal([]byte(encoded), &fields) != nil || fields == nil {
		return chat.Message{}, false
	}
	var p post
	// Keys match exactly, as the post's keys are case-sensitive.
	for key, v := range map[string]any{
		"id": &p.ID, "user_id": &p.UserID, "channel_id": &p.ChannelID, "root_id": &p.RootID, "message": &p.Message,
		"type": &p.Type, "channel_type": &p.ChannelType, "create_at": &p.CreateAt, "delete_at": &p.DeleteAt,
		"file_ids": &p.FileIDs,
	} {
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, v) != nil {
			return chat.Message{}, false
		}
	}
	var channelType string
	_ = json.Unmarshal(data["channel_type"], &channelType) // absent or not a string: the post's own applies
	channelType = cmp.Or(channelType, p.ChannelType)

	kind, scope := chat.Public, "public"
	switch channelType {
	case "D":
		kind, scope = chat.Direct, "private"
	case "P", "G":
		kind = chat.Private
	}
	contextID := cmp.Or(p.RootID, p.ID)
	replyRootID := contextID
	if kind == chat.Direct {
		replyRootID = ""
	}
	for key, v := range map[string]any{
		"channel_type": channelType, "post_id": p.ID, "is_dm": kind == chat.Direct, "is_thread": p.RootID != "",
		"scope": scope, "root_id": p.RootID, "context_id": contextID, "reply_root_id": replyRootID,
	} {
		fields[key] = encode(v)
	}
	data["post"] = encode(string(encode(fields)))
	frame["data"] = encode(data)

	var created time.Time
	if p.CreateAt != 0 {
		created = time.UnixMilli(p.CreateAt)
	}
	return chat.Message{
		ID: p.ID, ConversationID: p.ChannelID, ThreadID: p.RootID, AuthorID: p.UserID, Text: p.Message,
		AttachmentIDs: p.FileIDs, Created: created, Kind: kind, System: p.DeleteAt > 0 || p.Type != "",
		Payload: encode(frame),
	}, true
}

// encode marshals a string, a bool, or a map of values decoded from JSON, none of which can fail.
func encode(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
