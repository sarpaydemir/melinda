package mattermost_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sarpaydemir/dost/internal/adapters/mattermost"
	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/prompt"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/conformance"
	"github.com/sarpaydemir/dost/internal/testkit/mattermostfake"
)

// Channels as the user "me" sees them while asking in here.
var (
	here      = strings.Repeat("c", 26)
	joined    = strings.Repeat("a", 26)
	notJoined = strings.Repeat("o", 26)
	forbidden = strings.Repeat("f", 26) // the bot cannot see its members
	failing   = strings.Repeat("d", 26) // its membership lookup fails
)

// readers are the authors of the posts in these tests; u3 and u7 cannot be looked up.
var readers = []mattermostfake.User{
	{ID: "u1", Username: " ada ", FirstName: " Ada ", LastName: " Lovelace\t", Nickname: "nick"},
	{ID: "u2", Nickname: " Nicky "},
	{ID: "u4", Username: "bob", LastName: " "},
	{ID: "u5", LastName: "Last"},
}

// mattermostTools returns the platform's tools for mm, logging to log, with tool.mm.no_access set to "Not yours.".
func mattermostTools(t *testing.T, mm *mattermostfake.Fake, log *slog.Logger) []tool.Tool {
	t.Helper()
	doc, _ := json.Marshal(map[string]string{"model": "m", "mattermost_endpoint": mm.URL(), "mattermost_token": token, "mattermost_bot_id": bot.ID})
	catalog, err := prompt.Load(mattermost.Prompts(), "")
	if err == nil {
		catalog, err = catalog.With(map[string]string{"tool.mm.no_access": "Not yours."})
	}
	if err != nil {
		t.Fatal(err)
	}
	return mattermost.Tools(settings(t, string(doc)), catalog, transport(t), log)
}

func named(t *testing.T, tools []tool.Tool, name string) tool.Tool {
	t.Helper()
	i := slices.IndexFunc(tools, func(tl tool.Tool) bool { return tl.Name() == name })
	if i < 0 {
		t.Fatalf("no tool %s", name)
	}
	return tools[i]
}

// asked is a call by user in the conversation here, a direct one when direct is set.
func asked(user string, direct bool) tool.Call {
	return tool.Call{ConversationID: here, UserID: user, Direct: direct, ReadFrom: func(string) {}}
}

func ask(t *testing.T, mm *mattermostfake.Fake, name, args string, c tool.Call) string {
	t.Helper()
	c.Args = json.RawMessage(args)
	out, err := named(t, mattermostTools(t, mm, discard), name).Call(t.Context(), c)
	if err != nil {
		t.Fatalf("%s(%s): %v", name, args, err)
	}
	return out
}

func post(id, channel, user, message string, created int64, root string) mattermostfake.Post {
	return mattermostfake.Post{ID: id, ChannelID: channel, UserID: user, Message: message, CreateAt: created, RootID: root}
}

// postList is a server answer listing posts in order, which may name posts it does not hold.
func postList(order []string, posts ...mattermostfake.Post) testkit.Response {
	byID := map[string]mattermostfake.Post{}
	for _, p := range posts {
		byID[p.ID] = p
	}
	return testkit.JSON(200, map[string]any{"order": order, "posts": byID})
}

func newReadersFake(t *testing.T) *mattermostfake.Fake {
	mm := newFake(t)
	mm.AddUser(readers...)
	return mm
}

func TestSearchListsReadablePostsWithAuthorsAndDates(t *testing.T) {
	found := postList([]string{"p1", "missing", "p2", "p3", "p4", "p5"},
		post("p1", here, "u1", "hello there", 1767225600000, "r1"),
		post("p2", notJoined, "u2", "in other", 1767225660000, ""),
		post("p3", here, "u3", strings.Repeat("ğ", 1199)+"xyz", 0, ""),
		post("p4", here, "u4", "trailing  \n ", -1, ""),
		post("p5", here, "u5", "", 253402300799999, ""),
	)
	head := `Mattermost search results for: "hi"` + "\n\n" +
		"[1] 2026-01-01 00:00 | Ada Lovelace (@ada) | channel_id=" + here + " | post_id=p1 | root_id=r1\nhello there\n\n" +
		"[2] unknown | u3 | channel_id=" + here + " | post_id=p3\n" + strings.Repeat("ğ", 1199) + "x"
	tests := []struct {
		args, want string
		authors    []string
	}{
		{`{"query": "  hi  ", "limit": 20}`, head + "\n\n" +
			"[3] 1969-12-31 23:59 | bob (@bob) | channel_id=" + here + " | post_id=p4\ntrailing  \n \n\n" +
			"[4] 9999-12-31 23:59 | Last | channel_id=" + here + " | post_id=p5", []string{"u1", "u3", "u4", "u5"}},
		{`{"q": "hi", "limit": "2"}`, head, []string{"u1", "u3"}},
	}
	for _, tt := range tests {
		mm := newReadersFake(t)
		mm.On("POST", "/api/v4/posts/search").Reply(found)
		if got := ask(t, mm, "search_mattermost", tt.args, asked("me", false)); got != tt.want {
			t.Errorf("search %s:\n%q\nwant\n%q", tt.args, got, tt.want)
		}
		reqs := mm.Requests()
		if len(reqs) != 2 || string(reqs[0].Body) != `{"terms":"hi","is_or_search":false}` || reqs[1].Call() != "POST /api/v4/users/ids" {
			t.Fatalf("requests: %v", reqs)
		}
		var ids []string
		_ = json.Unmarshal(reqs[1].Body, &ids)
		if slices.Sort(ids); !slices.Equal(ids, tt.authors) {
			t.Errorf("looked up authors %v, want %v", ids, tt.authors)
		}
	}
}

func TestSearchTrimsTheEndAndNamesAuthorsByIDWhenLookupFails(t *testing.T) {
	mm := newReadersFake(t)
	mm.On("POST", "/api/v4/posts/search").Reply(postList([]string{"p5"}, post("p5", here, "u1", "  tail  \n\n", 1767225600000, "")))
	mm.On("POST", "/api/v4/users/ids").Reply(testkit.Status(500))
	want := `Mattermost search results for: "x"` + "\n\n[1] 2026-01-01 00:00 | u1 | channel_id=" + here + " | post_id=p5\n  tail"
	if got := ask(t, mm, "search_mattermost", `{"query": "x"}`, asked("me", false)); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPostDatesAreUTCMinutesOrTheRawNumberOutsideYearsOneTo9999(t *testing.T) {
	stamps := map[int64]string{
		1767225659999:       "2026-01-01 00:00",
		1:                   "1970-01-01 00:00",
		-30610224000001:     "999-12-31 23:59",
		253402300800000:     "253402300800000",
		-62135596800001:     "-62135596800001",
		9223372036854775807: "9223372036854775807",
	}
	for ms, want := range stamps {
		mm := newReadersFake(t)
		mm.On("GET", "/api/v4/channels/{channel_id}/posts").Reply(postList([]string{"p"}, post("p", here, "u7", "m", ms, "")))
		got := ask(t, mm, "mattermost_get_channel_posts", `{"channel_id": "`+here+`"}`, asked("me", false))
		if line := strings.Split(got, "\n")[2]; line != "[1] "+want+" | u7 | post_id=p" {
			t.Errorf("create_at %d: %q, want the date %s", ms, line, want)
		}
	}
}

// dmPosts are search results from channels the asking user has joined, has not, and cannot be checked.
var dmPosts = postList([]string{"q1", "q2", "q3", "q4", "q5"},
	post("q1", joined, "u1", "member", 1767225600000, ""),
	post("q2", notJoined, "u1", "nonmember", 1767225600000, ""),
	post("q3", forbidden, "u1", "forbidden", 1767225600000, ""),
	post("q4", joined, "u1", "member again", 1767225600000, ""),
	post("q5", failing, "u1", "error", 1767225600000, ""),
)

// newMembershipFake knows the members of joined and notJoined, cannot see forbidden's, and fails for failing.
func newMembershipFake(t *testing.T) *mattermostfake.Fake {
	mm := newReadersFake(t)
	mm.AddChannel(
		mattermostfake.Channel{ID: joined, TeamID: team, Type: "P", Name: "ops", DisplayName: "Ops", Members: []string{"me"}},
		mattermostfake.Channel{ID: notJoined, TeamID: team, Type: "O", Name: "sales", DisplayName: "Sales"},
	)
	mm.On("POST", "/api/v4/channels/"+failing+"/members/ids").Default(testkit.Status(500))
	return mm
}

func membersCall(channel string) string { return "POST /api/v4/channels/" + channel + "/members/ids" }

func TestSearchInADirectConversationKeepsChannelsTheUserBelongsTo(t *testing.T) {
	mm := newMembershipFake(t)
	mm.On("POST", "/api/v4/posts/search").Reply(dmPosts)
	want := `Mattermost search results for: "x"` + "\n\n" +
		"[1] 2026-01-01 00:00 | Ada Lovelace (@ada) | channel_id=" + joined + " | post_id=q1\nmember\n\n" +
		"[2] 2026-01-01 00:00 | Ada Lovelace (@ada) | channel_id=" + joined + " | post_id=q4\nmember again"
	if got := ask(t, mm, "search_mattermost", `{"query": "x"}`, asked("me", true)); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	mm.AssertCalls(t, "POST /api/v4/posts/search", membersCall(joined), membersCall(notJoined), membersCall(forbidden),
		membersCall(failing), "POST /api/v4/users/ids")
}

func TestReadsReportEachConversationTheirResultHolds(t *testing.T) {
	tests := []struct {
		name, tool, args string
		want             []string
	}{
		{"search", "search_mattermost", `{"query": "x"}`, []string{joined}},
		{"channel posts", "mattermost_get_channel_posts", `{"channel_id": "` + joined + `"}`, []string{joined}},
		{"refused channel posts", "mattermost_get_channel_posts", `{"channel_id": "` + notJoined + `"}`, nil},
		{"teams", "mattermost_get_teams", `{}`, nil},
		{"channels", "mattermost_get_channels", `{"team_id": "` + team + `"}`, nil},
	}
	for _, tt := range tests {
		mm := newMembershipFake(t)
		mm.On("POST", "/api/v4/posts/search").Reply(dmPosts)
		mm.On("GET", "/api/v4/channels/{channel_id}/posts").Reply(postList([]string{"q1"}, post("q1", joined, "u1", "member", 1, "")))
		var read []string
		c := asked("me", true)
		c.ReadFrom = func(id string) { read = append(read, id) }
		ask(t, mm, tt.tool, tt.args, c)
		if slices.Sort(read); !slices.Equal(slices.Compact(read), tt.want) {
			t.Errorf("%s: reported %q, want %q", tt.name, read, tt.want)
		}
	}
}

func TestSearchFindsNothingReadable(t *testing.T) {
	tests := []struct {
		name   string
		answer testkit.Response
		call   tool.Call
	}{
		{"elsewhere than a direct conversation, other channels", dmPosts, asked("me", false)},
		{"without a user", dmPosts, asked("", true)},
		{"without a conversation, posts without one", postList([]string{"e"}, post("e", "", "u1", "x", 1, "")), tool.Call{UserID: "me"}},
		{"no results", testkit.JSON(200, map[string]any{}), asked("me", true)},
		{"failed search", testkit.Status(500), asked("me", true)},
	}
	for _, tt := range tests {
		mm := newMembershipFake(t)
		mm.On("POST", "/api/v4/posts/search").Reply(tt.answer)
		if got := ask(t, mm, "search_mattermost", `{"query": "x"}`, tt.call); got != "No Mattermost results found." {
			t.Errorf("%s: %q", tt.name, got)
		}
		mm.AssertCalls(t, "POST /api/v4/posts/search")
	}
}

func TestToolsReportMissingArguments(t *testing.T) {
	tests := []struct{ name, args, want string }{
		{"search_mattermost", `{"query": "   ", "q": ""}`, "Error: Missing Mattermost search query"},
		{"mattermost_get_channels", `{"team_id": " "}`, "Error: Missing team_id"},
		{"mattermost_get_channel_posts", `{}`, "Error: Missing channel_id"},
	}
	mm := newFake(t)
	for _, tt := range tests {
		if got := ask(t, mm, tt.name, tt.args, asked("me", true)); got != tt.want {
			t.Errorf("%s(%s) = %q, want %q", tt.name, tt.args, got, tt.want)
		}
	}
	mm.AssertCalls(t)
}

func TestArgumentsThatCannotBeReadFailTheCall(t *testing.T) {
	tests := []struct{ name, args, want string }{
		{"search_mattermost", `{"query": 5}`, "'int' object has no attribute 'strip'"},
		{"mattermost_get_channels", `null`, "'NoneType' object has no attribute 'get'"},
		{"mattermost_get_channel_posts", `{"channel_id": [1]}`, "'list' object has no attribute 'strip'"},
	}
	mm := newFake(t)
	for _, tt := range tests {
		if _, err := named(t, mattermostTools(t, mm, discard), tt.name).Call(t.Context(), tool.Call{Args: json.RawMessage(tt.args)}); err == nil || err.Error() != tt.want {
			t.Errorf("%s(%s): error %v, want %q", tt.name, tt.args, err, tt.want)
		}
	}
	mm.AssertCalls(t)
}

func teamsCall(id string) string { return "POST /api/v4/teams/" + id + "/members/ids" }

func TestTeamsAreThoseTheBotAndTheUserShare(t *testing.T) {
	mm := mattermostfake.New(t, token, bot)
	one, unnamed, hidden := id("one"), id("unnamed"), id("hidden")
	mm.AddTeam(
		mattermostfake.Team{ID: one, Name: "one", DisplayName: "Team One", Members: []string{"me"}},
		mattermostfake.Team{ID: unnamed, Name: "two", Members: []string{"me"}},
		mattermostfake.Team{ID: hidden, Name: "three", DisplayName: "Hidden"},
	)
	want := "Mattermost teams:\n- Team One (id=" + one + ")\n-  (id=" + unnamed + ")"
	if got := ask(t, mm, "mattermost_get_teams", `null`, asked("me", false)); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	mm.AssertCalls(t, "GET /api/v4/users/me/teams", teamsCall(one), teamsCall(unnamed), teamsCall(hidden))
}

func TestNoTeamsFound(t *testing.T) {
	tests := []struct {
		name  string
		fake  func(*mattermostfake.Fake)
		user  string
		calls []string
	}{
		{"the user is in none of the bot's teams", func(mm *mattermostfake.Fake) {
			mm.AddTeam(mattermostfake.Team{ID: team})
		}, "me", []string{teamsCall(team)}},
		{"without a user", func(mm *mattermostfake.Fake) {
			mm.AddTeam(mattermostfake.Team{ID: team})
		}, "", nil},
		{"the bot is in no team", func(*mattermostfake.Fake) {}, "me", nil},
		{"the team listing fails", func(mm *mattermostfake.Fake) {
			mm.On("GET", "/api/v4/users/me/teams").Reply(testkit.Status(500))
		}, "me", nil},
		{"the membership lookup fails", func(mm *mattermostfake.Fake) {
			mm.AddTeam(mattermostfake.Team{ID: team, Members: []string{"me"}})
			mm.On("POST", "/api/v4/teams/{team_id}/members/ids").Reply(testkit.Status(500))
		}, "me", []string{teamsCall(team)}},
	}
	for _, tt := range tests {
		mm := mattermostfake.New(t, token, bot)
		tt.fake(mm)
		if got := ask(t, mm, "mattermost_get_teams", `{}`, asked(tt.user, true)); got != "No teams found." {
			t.Errorf("%s: %q", tt.name, got)
		}
		mm.AssertCalls(t, append([]string{"GET /api/v4/users/me/teams"}, tt.calls...)...)
	}
}

func TestChannelsAreThoseTheUserMayRead(t *testing.T) {
	tests := []struct {
		name    string
		current mattermostfake.Channel
		call    tool.Call
		want    string
		calls   []string
	}{
		// The bot's one-to-one channel with ayse is left out without asking.
		{"in a direct conversation", mattermostfake.Channel{ID: here, Type: "D", Name: bot.ID + "__me", Members: []string{"me"}},
			asked("me", true), "Mattermost channels (team_id=" + team + "):\n- Ops (id=" + joined + ", type=P)\n-  (id=" + here + ", type=D)",
			[]string{membersCall(channel), membersCall(joined), membersCall(notJoined)}},
		{"elsewhere", mattermostfake.Channel{ID: here, TeamID: team, Type: "O", Name: "town-square", DisplayName: "Town Square"},
			asked("me", false), "Mattermost channels (team_id=" + team + "):\n- Town Square (id=" + here + ", type=O)", nil},
	}
	for _, tt := range tests {
		mm := newMembershipFake(t)
		mm.AddChannel(tt.current)
		if got := ask(t, mm, "mattermost_get_channels", `{"team_id": " `+team+` "}`, tt.call); got != tt.want {
			t.Errorf("%s:\n%q\nwant\n%q", tt.name, got, tt.want)
		}
		mm.AssertCalls(t, append([]string{"GET /api/v4/users/me/teams/" + team + "/channels"}, tt.calls...)...)
	}
}

func TestNoChannelsFound(t *testing.T) {
	for _, teamID := range []string{"../../users/me/teams", strings.ToUpper(team)} {
		mm := newFake(t)
		if got := ask(t, mm, "mattermost_get_channels", `{"team_id": "`+teamID+`"}`, asked("me", true)); got != "No channels found for this team." {
			t.Errorf("team %q: %q", teamID, got)
		}
		mm.AssertCalls(t)
	}
	mm := newFake(t)
	mm.On("GET", "/api/v4/users/me/teams/{team_id}/channels").Reply(testkit.Status(500))
	if got := ask(t, mm, "mattermost_get_channels", `{"team_id": "`+team+`"}`, asked("me", true)); got != "No channels found for this team." {
		t.Errorf("failed listing: %q", got)
	}
}

func TestChannelPostsListTheLatestPosts(t *testing.T) {
	recent := postList([]string{"p1", "p3", "p4", "gone"},
		post("p1", here, "u1", "hello there", 1767225600000, "r1"),
		post("p3", here, "u3", strings.Repeat("ğ", 1199)+"xyz", 0, ""),
		post("p4", here, "u4", "trailing  \n ", -1, ""),
	)
	two := "Recent posts for channel_id=" + here + ":\n\n" +
		"[1] 2026-01-01 00:00 | Ada Lovelace (@ada) | post_id=p1 | root_id=r1\nhello there\n\n" +
		"[2] unknown | u3 | post_id=p3\n" + strings.Repeat("ğ", 800)
	all := two + "\n\n[3] 1969-12-31 23:59 | bob (@bob) | post_id=p4\ntrailing"
	tests := []struct{ args, perPage, want string }{
		{`{"channel_id": " ` + here + ` "}`, "15", all},
		{`{"channel_id": "` + here + `", "last_n": 2}`, "2", two},
		{`{"channel_id": "` + here + `", "last_n": 99}`, "30", all},
	}
	for _, tt := range tests {
		mm := newReadersFake(t)
		mm.On("GET", "/api/v4/channels/{channel_id}/posts").Reply(recent)
		if got := ask(t, mm, "mattermost_get_channel_posts", tt.args, asked("me", false)); got != tt.want {
			t.Errorf("%s:\n%q\nwant\n%q", tt.args, got, tt.want)
		}
		if calls := mm.Calls(); len(calls) != 2 || calls[0] != "GET /api/v4/channels/"+here+"/posts?per_page="+tt.perPage {
			t.Errorf("%s: requests %v, want per_page=%s", tt.args, calls, tt.perPage)
		}
	}
}

func TestChannelPostsOfAChannelTheUserMayNotReadAreRefused(t *testing.T) {
	tests := []struct {
		name, channel string
		call          tool.Call
		calls         []string
	}{
		{"elsewhere than a direct conversation", joined, asked("me", false), nil},
		{"not a member", notJoined, asked("me", true), []string{membersCall(notJoined)}},
		{"membership hidden from the bot", forbidden, asked("me", true), []string{membersCall(forbidden)}},
		{"membership lookup failed", failing, asked("me", true), []string{membersCall(failing)}},
		{"without a user", joined, asked("", true), nil},
		{"not a channel ID", "../../users/me", asked("me", true), nil},
	}
	for _, tt := range tests {
		mm := newMembershipFake(t)
		if got := ask(t, mm, "mattermost_get_channel_posts", `{"channel_id": "`+tt.channel+`"}`, tt.call); got != "Not yours." {
			t.Errorf("%s: %q, want the no-access text", tt.name, got)
		}
		mm.AssertCalls(t, tt.calls...)
	}
}

func TestChannelPostsOfAJoinedChannelInADirectConversation(t *testing.T) {
	mm := newMembershipFake(t)
	mm.AddPost(post(id("p"), joined, "u1", "hello", 1767225600000, ""))
	want := "Recent posts for channel_id=" + joined + ":\n\n[1] 2026-01-01 00:00 | Ada Lovelace (@ada) | post_id=" + id("p") + "\nhello"
	if got := ask(t, mm, "mattermost_get_channel_posts", `{"channel_id": "`+joined+`"}`, asked("me", true)); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	mm.AssertCalls(t, membersCall(joined), "GET /api/v4/channels/"+joined+"/posts?per_page=15", "POST /api/v4/users/ids")
}

func TestNoPostsFound(t *testing.T) {
	for _, answer := range []testkit.Response{testkit.JSON(200, map[string]any{}), testkit.Status(http.StatusInternalServerError)} {
		mm := newFake(t)
		mm.On("GET", "/api/v4/channels/{channel_id}/posts").Reply(answer)
		if got := ask(t, mm, "mattermost_get_channel_posts", `{"channel_id": "`+here+`"}`, asked("me", false)); got != "No posts found." {
			t.Errorf("answer %d: %q", answer.Status, got)
		}
	}
}

func TestOneCallLooksUpAtMost100Memberships(t *testing.T) {
	mm := newFake(t)
	for i := range 120 {
		mm.AddChannel(mattermostfake.Channel{ID: id(fmt.Sprintf("x%d", i)), TeamID: team, Type: "O", Members: []string{"me"}})
	}
	ask(t, mm, "mattermost_get_channels", `{"team_id": "`+team+`"}`, asked("me", true))
	lookups := slices.DeleteFunc(mm.Calls(), func(call string) bool { return !strings.HasSuffix(call, "/members/ids") })
	if len(lookups) != 100 {
		t.Errorf("%d membership lookups, want 100", len(lookups))
	}
}

func TestMembershipAnswersNamingOnlyOthersGrantNothing(t *testing.T) {
	mm := newMembershipFake(t)
	others := testkit.JSON(200, []map[string]string{{"user_id": "someone"}})
	mm.On("POST", "/api/v4/channels/"+joined+"/members/ids").Reply(others)
	mm.On("POST", "/api/v4/teams/{team_id}/members/ids").Reply(others)
	if got := ask(t, mm, "mattermost_get_channel_posts", `{"channel_id": "`+joined+`"}`, asked("me", true)); got != "Not yours." {
		t.Errorf("channel posts: %q, want the no-access text", got)
	}
	if got := ask(t, mm, "mattermost_get_teams", `{}`, asked("me", true)); got != "No teams found." {
		t.Errorf("teams: %q", got)
	}
}

func TestFailedRequestsAreLoggedWithoutTheirContent(t *testing.T) {
	mm := newFake(t)
	mm.On("POST", "/api/v4/posts/search").Reply(testkit.Status(500))
	var logs bytes.Buffer
	search := named(t, mattermostTools(t, mm, slog.New(slog.NewTextHandler(&logs, nil))), "search_mattermost")
	out, err := search.Call(t.Context(), tool.Call{Args: json.RawMessage(`{"query": "salary review"}`), ConversationID: here, UserID: "me"})
	if out != "No Mattermost results found." || err != nil {
		t.Errorf("Call = %q, %v; want no results", out, err)
	}
	if s := logs.String(); strings.Count(s, "level=WARN") != 1 || !strings.Contains(s, "POST /posts/search: 500") || strings.Contains(s, "salary") {
		t.Errorf("logs:\n%s\nwant one warning naming the request and not the query", s)
	}
}

func TestToolsKeepTheToolContract(t *testing.T) {
	conformance.Tools(t, mattermostTools(t, newFake(t), discard), mattermost.Prompts())
}
