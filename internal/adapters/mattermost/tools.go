package mattermost

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/prompt"
)

// Tools are the tools the platform contributes, in the order they are offered.
func Tools() []tool.Tool {
	return []tool.Tool{
		declared{"search_mattermost", `{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`},
		declared{"mattermost_get_teams", `{"type":"object","properties":{}}`},
		declared{"mattermost_get_channels", `{"type":"object","properties":{"team_id":{"type":"string"}},"required":["team_id"]}`},
		declared{"mattermost_get_channel_posts", `{"type":"object","properties":{"channel_id":{"type":"string"},"last_n":{"type":"integer"}},"required":["channel_id"]}`},
	}
}

// Prompts are the catalog entries of the platform's tools, with their default texts.
func Prompts() []prompt.Entry {
	return []prompt.Entry{
		{ID: "tools.desc.search_mattermost", Kind: prompt.KindRaw, Default: "Search chat messages by keywords. " +
			"Only conversations the user can read from here are searched; limit is at most 20."},
		{ID: "tools.desc.mattermost_get_teams", Kind: prompt.KindRaw, Default: "List the chat teams, with their IDs."},
		{ID: "tools.desc.mattermost_get_channels", Kind: prompt.KindRaw, Default: "List the channels of a team, " +
			"with their IDs and types. Get team_id from mattermost_get_teams."},
		{ID: "tools.desc.mattermost_get_channel_posts", Kind: prompt.KindRaw, Default: "Read the recent messages " +
			"of a channel the user can read. last_n sets how many, at most 30."},
	}
}

// declared is a tool offered by its name and argument schema; calling it is not supported yet.
type declared struct{ name, parameters string }

func (d declared) Name() string { return d.name }

func (d declared) Parameters() json.RawMessage { return json.RawMessage(d.parameters) }

func (declared) Call(context.Context, tool.Call) (string, error) { return "", errors.ErrUnsupported }
