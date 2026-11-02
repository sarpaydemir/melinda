package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/port/tool"
	"github.com/sarpaydemir/melinda/internal/prompt"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

// maxLookups bounds the channel membership lookups of one call.
const maxLookups = 100

// Tools are the tools the platform contributes with a settings snapshot, in the order they are offered. Their
// requests go through transport and their texts come from c. A failed request is logged to log and reads as an
// empty result.
func Tools(s *config.Settings, c *prompt.Catalog, transport http.RoundTripper, log *slog.Logger) []tool.Tool {
	var cfg settings
	_ = s.Decode(&cfg) // Open rejects settings that do not decode, so no call is made with them
	r := reader{NewClient(cfg.Endpoint, cfg.Token, transport), c, log}
	return []tool.Tool{
		function{"search_mattermost", `{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`, r.search},
		function{"mattermost_get_teams", `{"type":"object","properties":{}}`, r.teams},
		function{"mattermost_get_channels", `{"type":"object","properties":{"team_id":{"type":"string"}},"required":["team_id"]}`, r.channels},
		function{"mattermost_get_channel_posts", `{"type":"object","properties":{"channel_id":{"type":"string"},"last_n":{"type":"integer"}},"required":["channel_id"]}`, r.channelPosts},
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
		{ID: "tool.mm.no_access", Kind: prompt.KindRaw, Default: "You are not allowed to read this channel."},
	}
}

// function is a tool that runs call.
type function struct {
	name, parameters string
	call             func(context.Context, tool.Call) (string, error)
}

func (f function) Name() string { return f.name }

func (f function) Parameters() json.RawMessage { return json.RawMessage(f.parameters) }

func (f function) Call(ctx context.Context, c tool.Call) (string, error) { return f.call(ctx, c) }

// reader reads Mattermost for the user being answered.
type reader struct {
	client  *Client
	catalog *prompt.Catalog
	log     *slog.Logger
}

func (r reader) search(ctx context.Context, c tool.Call) (string, error) {
	args, err := c.Arguments()
	if err != nil {
		return "", err
	}
	query, err := args.String("query", "q")
	if err != nil {
		return "", err
	}
	if query == "" {
		return "Error: Missing Mattermost search query", nil
	}
	limit := min(max(args.Int("limit", 10), 1), 20)
	found, err := r.client.SearchPosts(ctx, query)
	r.failed(ctx, err)
	readable := r.access(c)
	var posts []Post
	for _, p := range ordered(found) {
		if len(posts) == limit {
			break
		}
		if readable(ctx, p.ChannelID) {
			posts = append(posts, p)
			c.ReadFrom(p.ChannelID)
		}
	}
	if len(posts) == 0 {
		return "No Mattermost results found.", nil
	}
	return r.list(ctx, `Mattermost search results for: "`+query+`"`, posts, 1200, func(p Post) string {
		return "channel_id=" + p.ChannelID + " | post_id=" + p.ID
	}), nil
}

// teams lists the bot's teams that the user being answered belongs to.
func (r reader) teams(ctx context.Context, c tool.Call) (string, error) {
	teams, err := r.client.GetMyTeams(ctx)
	r.failed(ctx, err)
	var lines []string
	for _, t := range teams {
		members, err := r.client.GetTeamMemberIDs(ctx, t.ID, []string{c.UserID})
		r.failed(ctx, err)
		if slices.Contains(members, c.UserID) {
			lines = append(lines, "- "+t.DisplayName+" (id="+t.ID+")")
		}
	}
	if len(lines) == 0 {
		return "No teams found.", nil
	}
	return "Mattermost teams:\n" + strings.Join(lines, "\n"), nil
}

// channels lists the bot's channels in a team that the user being answered may read from the conversation.
func (r reader) channels(ctx context.Context, c tool.Call) (string, error) {
	args, err := c.Arguments()
	if err != nil {
		return "", err
	}
	teamID, err := args.String("team_id")
	if err != nil {
		return "", err
	}
	if teamID == "" {
		return "Error: Missing team_id", nil
	}
	channels, err := r.client.GetMyChannels(ctx, teamID)
	r.failed(ctx, err)
	readable := r.access(c)
	var lines []string
	for _, ch := range channels {
		// The user's one-to-one channel with the bot can only be the current conversation; the others are with
		// other people.
		if ch.Type == "D" && ch.ID != c.ConversationID {
			continue
		}
		if readable(ctx, ch.ID) {
			lines = append(lines, fmt.Sprintf("- %s (id=%s, type=%s)", ch.DisplayName, ch.ID, ch.Type))
		}
	}
	if len(lines) == 0 {
		return "No channels found for this team.", nil
	}
	return "Mattermost channels (team_id=" + teamID + "):\n" + strings.Join(lines, "\n"), nil
}

func (r reader) channelPosts(ctx context.Context, c tool.Call) (string, error) {
	args, err := c.Arguments()
	if err != nil {
		return "", err
	}
	channelID, err := args.String("channel_id")
	if err != nil {
		return "", err
	}
	if channelID == "" {
		return "Error: Missing channel_id", nil
	}
	lastN := min(max(args.Int("last_n", 15), 1), 30)
	if !r.access(c)(ctx, channelID) {
		return r.catalog.Text("tool.mm.no_access"), nil
	}
	found, err := r.client.GetChannelPosts(ctx, channelID, 0, lastN)
	r.failed(ctx, err)
	posts := ordered(found)
	if len(posts) == 0 {
		return "No posts found.", nil
	}
	c.ReadFrom(channelID)
	return r.list(ctx, "Recent posts for channel_id="+channelID+":", posts[:min(lastN, len(posts))], 800, func(p Post) string {
		return "post_id=" + p.ID
	}), nil
}

// access returns whether the user being answered may read a channel from the conversation of c: the conversation
// itself, and from a direct conversation any channel whose members include the user. It asks the server once per
// channel and at most maxLookups times; a failed or refused lookup, or one past the limit, denies.
func (r reader) access(c tool.Call) func(ctx context.Context, channelID string) bool {
	known := map[string]bool{}
	return func(ctx context.Context, channelID string) bool {
		if c.ConversationID != "" && channelID == c.ConversationID {
			return true
		}
		if !c.Direct {
			return false
		}
		allowed, ok := known[channelID]
		if !ok && len(known) < maxLookups {
			members, err := r.client.GetChannelMemberIDs(ctx, channelID, []string{c.UserID})
			r.failed(ctx, err)
			allowed = slices.Contains(members, c.UserID)
			known[channelID] = allowed
		}
		return allowed
	}
}

// list prints posts under title, each as a line of its date, its author, and fields(p), then its message clipped
// to clip code points, separated by blank lines.
func (r reader) list(ctx context.Context, title string, posts []Post, clip int, fields func(Post) string) string {
	ids := make([]string, len(posts))
	for i, p := range posts {
		ids[i] = p.UserID
	}
	slices.Sort(ids)
	found, err := r.client.GetUsersByIDs(ctx, slices.Compact(ids))
	r.failed(ctx, err)
	authors := map[string]User{}
	for _, u := range found {
		authors[u.ID] = u
	}
	var b strings.Builder
	b.WriteString(title + "\n")
	for i, p := range posts {
		author, ok := authors[p.UserID]
		if !ok {
			author = User{ID: p.UserID}
		}
		fmt.Fprintf(&b, "\n[%d] %s | %s | %s", i+1, created(p.CreateAt), chat.User(author).Label(), fields(p))
		if p.RootID != "" {
			b.WriteString(" | root_id=" + p.RootID)
		}
		b.WriteString("\n" + textfmt.Clip(p.Message, clip) + "\n")
	}
	return textfmt.Strip(b.String())
}

// failed logs a request that failed, by its method, path, and status or cause, which hold no request or response
// body. An ID refused before any request is not logged.
func (r reader) failed(ctx context.Context, err error) {
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		r.log.WarnContext(ctx, "mattermost tool request failed; answered as empty", "error", apiErr)
	}
}

// ordered returns the posts of l in its order, skipping IDs it holds no post for.
func ordered(l PostList) []Post {
	var posts []Post
	for _, id := range l.Order {
		if p, ok := l.Posts[id]; ok {
			posts = append(posts, p)
		}
	}
	return posts
}

// created prints a creation time in Unix milliseconds as its UTC minute, "unknown" when it is 0, or the number
// itself when its year is outside 1 to 9999.
func created(ms int64) string {
	if ms == 0 {
		return "unknown"
	}
	t := time.UnixMilli(ms).UTC()
	if t.Year() < 1 || t.Year() > 9999 {
		return strconv.FormatInt(ms, 10)
	}
	return textfmt.Strftime(t, "%Y-%m-%d %H:%M")
}
