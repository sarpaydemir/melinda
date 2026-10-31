package mattermost

import (
	"cmp"
	"encoding/json"
	"slices"
	"time"

	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/textfmt"
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
	stored, _ := textfmt.Decode([]byte(encoded), false)
	members, ok := stored.(textfmt.Object)
	if !ok {
		return chat.Message{}, false
	}
	for _, f := range []textfmt.Member{
		{Key: "post_id", Value: p.ID}, {Key: "channel_type", Value: channelType}, {Key: "is_dm", Value: kind == chat.Direct},
		{Key: "is_thread", Value: p.RootID != ""}, {Key: "scope", Value: scope}, {Key: "root_id", Value: p.RootID},
		{Key: "reply_root_id", Value: replyRootID}, {Key: "context_id", Value: contextID},
	} {
		if i := slices.IndexFunc(members, func(m textfmt.Member) bool { return m.Key == f.Key }); i >= 0 {
			members[i].Value = f.Value
		} else {
			members = append(members, f)
		}
	}
	post, err := textfmt.EncodeASCII(members)
	if err != nil { // a number too large for a float
		return chat.Message{}, false
	}
	data["post"] = encode(string(post))
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

// encode marshals a string or a map of values decoded from JSON, neither of which can fail.
func encode(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
