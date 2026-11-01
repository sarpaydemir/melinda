package mattermost

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/sarpaydemir/dost/internal/port/chat"
)

// Replay reads the posts of every channel the bot belongs to, visiting a channel listed under several teams once,
// and delivers those created after after, oldest first per channel. A team or channel whose listing fails is
// logged and skipped, and the replay then returns an error once the others are delivered.
func (p *platform) Replay(ctx context.Context, after time.Time, deliver chat.Deliver) error {
	teams, err := p.client.GetMyTeams(ctx)
	if err != nil {
		return err
	}
	// The server lists the posts updated after since, so posts edited, reacted to, or replied to since then come
	// back too; those created at or before it were already read.
	since := after.UnixMilli()
	visited := map[string]bool{}
	skipped := 0
	for _, team := range teams {
		channels, err := p.client.GetMyChannels(ctx, team.ID)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.log.Warn("cannot list a team's channels to replay; skipped", "team_id", team.ID, "error", err)
			skipped++
			continue
		}
		for _, c := range channels {
			if visited[c.ID] {
				continue
			}
			visited[c.ID] = true
			list, err := p.client.GetChannelPosts(ctx, c.ID, since, 0)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				p.log.Warn("cannot read a channel's posts to replay; skipped", "channel_id", c.ID, "error", err)
				skipped++
				continue
			}
			for _, post := range createdAfter(list, since) {
				if err := deliver(ctx, replayEvent(post, cmp.Or(c.Type, "O"))); err != nil {
					return err
				}
			}
		}
	}
	p.log.Info("replay scanned the bot's channels", "teams", len(teams), "channels", len(visited))
	if skipped > 0 {
		return fmt.Errorf("%d of the listings to replay could not be read", skipped)
	}
	return nil
}

// createdAfter returns the listed posts created after since (Unix milliseconds), oldest first, keeping the list's
// order among posts created at the same time.
func createdAfter(list PostList, since int64) []Post {
	var posts []Post
	for _, id := range list.Order {
		if post, ok := list.Posts[id]; ok && post.CreateAt > since {
			posts = append(posts, post)
		}
	}
	slices.SortStableFunc(posts, func(a, b Post) int { return cmp.Compare(a.CreateAt, b.CreateAt) })
	return posts
}

// replayEvent is the event for a replayed post: the post as the server sent it, and its channel's type.
func replayEvent(post Post, channelType string) json.RawMessage {
	var ev struct {
		Data struct {
			Post        string `json:"post"`
			ChannelType string `json:"channel_type"`
		} `json:"data"`
	}
	ev.Data.Post, ev.Data.ChannelType = string(post.Raw), channelType
	b, _ := json.Marshal(ev) // strings only
	return b
}
