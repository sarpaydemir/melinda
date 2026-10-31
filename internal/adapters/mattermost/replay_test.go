package mattermost_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/mattermostfake"
)

// seenAt is the last-seen time the replays in these tests start after.
var seenAt = time.UnixMilli(1767225600000)

// replay replays p after seenAt and returns the delivered events.
func replay(t *testing.T, p chat.Platform) []json.RawMessage {
	t.Helper()
	var events []json.RawMessage
	err := p.Replay(t.Context(), seenAt, func(_ context.Context, ev json.RawMessage) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// postIDs returns the IDs of the posts in events.
func postIDs(t *testing.T, events []json.RawMessage) []string {
	t.Helper()
	var ids []string
	for _, ev := range events {
		ids = append(ids, decode(t, decoder(t), ev).ID)
	}
	return ids
}

func TestReplayReadsEachChannelOnceSinceTheGivenTime(t *testing.T) {
	mm := newFake(t)
	mm.AddTeam(mattermostfake.Team{ID: id("team2"), Name: "ops", DisplayName: "Ops"})
	dm := mm.AddPost(mattermostfake.Post{ChannelID: id("dm"), UserID: ayse.ID, CreateAt: seenAt.UnixMilli() + 5, Message: "hello"})
	if got := postIDs(t, replay(t, open(t, mm, ""))); !slices.Equal(got, []string{dm.ID}) {
		t.Errorf("replayed %q, want the direct message once", got)
	}
	mm.AssertCalls(t,
		"GET /api/v4/users/me/teams",
		"GET /api/v4/users/me/teams/"+team+"/channels",
		"GET /api/v4/channels/"+channel+"/posts?since=1767225600000",
		"GET /api/v4/channels/"+id("dm")+"/posts?since=1767225600000",
		"GET /api/v4/users/me/teams/"+id("team2")+"/channels",
	)
}

func TestReplayDeliversAChannelsPostsByCreationTime(t *testing.T) {
	mm := newFake(t)
	// post is a listed post; a negative offset leaves out create_at.
	post := func(name string, offset int64) string {
		createAt := ""
		if offset >= 0 {
			createAt = fmt.Sprintf(`,"create_at":%d`, seenAt.UnixMilli()+offset)
		}
		return fmt.Sprintf(`%q:{"id":%q,"channel_id":%q,"user_id":%q%s}`, id(name), id(name), channel, ayse.ID, createAt)
	}
	mm.On("GET", "/api/v4/channels/"+channel+"/posts").Reply(testkit.Raw(200, "application/json", []byte(fmt.Sprintf(
		`{"order":[%q,%q,%q,%q,%q],"posts":{%s,%s,%s,%s}}`, id("z"), id("y"), id("x"), id("w"), id("gone"),
		post("z", 10), post("y", 20), post("x", 10), post("w", -1)))))
	got := postIDs(t, replay(t, open(t, mm, "")))
	if want := []string{id("z"), id("x"), id("y")}; !slices.Equal(got, want) {
		t.Errorf("replayed %q, want %q: oldest first, ties in listed order, none without a creation time", got, want)
	}
}

func TestReplaySkipsPostsCreatedAtOrBeforeTheGivenTime(t *testing.T) {
	mm := newFake(t)
	seen := seenAt.UnixMilli()
	mm.AddPost(mattermostfake.Post{ChannelID: channel, UserID: ayse.ID, CreateAt: seen - 60_000, UpdateAt: seen + 30, Message: "@helper edited"})
	mm.AddPost(mattermostfake.Post{ChannelID: channel, UserID: ayse.ID, CreateAt: seen, UpdateAt: seen + 40, Message: "@helper reacted to"})
	missed := mm.AddPost(mattermostfake.Post{ChannelID: channel, UserID: ayse.ID, CreateAt: seen + 1, Message: "@helper new"})
	if got := postIDs(t, replay(t, open(t, mm, ""))); !slices.Equal(got, []string{missed.ID}) {
		t.Errorf("replayed %q, want only the post created after the given time", got)
	}
}

func TestReplayEventHoldsThePostAsSentAndTheChannelType(t *testing.T) {
	mm := newFake(t)
	mm.On("GET", "/api/v4/users/me/teams/{team_id}/channels").Reply(testkit.JSON(200, []map[string]string{
		{"id": channel}, {"id": id("dm"), "type": "D"},
	}))
	raw := `{ "id": "` + id("p1") + `", "channel_id": "` + channel + `", "create_at": 1767225600001, "props": {"a": "<b>"} }`
	mm.On("GET", "/api/v4/channels/"+channel+"/posts").Reply(testkit.Raw(200, "application/json",
		[]byte(`{"order":["`+id("p1")+`"],"posts":{"`+id("p1")+`":`+raw+`}}`)))
	dm := mm.AddPost(mattermostfake.Post{ChannelID: id("dm"), UserID: ayse.ID, CreateAt: seenAt.UnixMilli() + 2, Message: "hi"})
	dmRaw, _ := json.Marshal(dm)

	events := replay(t, open(t, mm, ""))
	want := []map[string]map[string]string{
		{"data": {"post": raw, "channel_type": "O"}},
		{"data": {"post": string(dmRaw), "channel_type": "D"}},
	}
	if len(events) != len(want) {
		t.Fatalf("replayed %d events, want %d", len(events), len(want))
	}
	for i, ev := range events {
		w, _ := json.Marshal(want[i])
		if eq, err := testkit.JSONEqual(ev, w); err != nil || !eq {
			t.Errorf("event %d = %s, want %s", i, ev, w)
		}
	}
}

func TestReplayLogsAFailingListingAndContinues(t *testing.T) {
	mm := newFake(t)
	news := id("news")
	mm.AddChannel(mattermostfake.Channel{ID: news, TeamID: team, Type: "O", Name: "news", DisplayName: "News"})
	mm.AddTeam(mattermostfake.Team{ID: id("team2"), Name: "ops", DisplayName: "Ops"})
	mm.On("GET", "/api/v4/channels/"+channel+"/posts").Reply(testkit.Status(500))
	mm.On("GET", "/api/v4/users/me/teams/"+id("team2")+"/channels").Reply(testkit.Status(502))
	posted := []string{
		mm.AddPost(mattermostfake.Post{ChannelID: id("dm"), UserID: ayse.ID, CreateAt: seenAt.UnixMilli() + 1, Message: "hi"}).ID,
		mm.AddPost(mattermostfake.Post{ChannelID: news, UserID: ayse.ID, CreateAt: seenAt.UnixMilli() + 2, Message: "@helper news"}).ID,
	}
	var logs bytes.Buffer
	p := openLogging(t, mm, "", slog.New(slog.NewTextHandler(&logs, nil)))

	if got := postIDs(t, replay(t, p)); !slices.Equal(got, posted) {
		t.Errorf("replayed %q, want %q from the channels that could be read", got, posted)
	}
	for _, want := range []string{"channel_id=" + channel, "team_id=" + id("team2"), "teams=2 channels=3"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs do not mention %s:\n%s", want, logs.String())
		}
	}
}
