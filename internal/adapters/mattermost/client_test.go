package mattermost_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"mime"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/adapters/mattermost"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/mattermostfake"
)

const token = "s3cret-token"

// id pads s to a 26-character Mattermost ID.
func id(s string) string { return s + strings.Repeat("0", 26-len(s)) }

var (
	bot     = mattermostfake.User{ID: id("bot"), Username: "helper", FirstName: "Help", LastName: "Desk"}
	ayse    = mattermostfake.User{ID: id("ayse"), Username: "ayse", FirstName: "Ayşe", LastName: "Yılmaz", Nickname: "ay", Position: "İK"}
	team    = id("team")
	channel = id("chan")
	file    = id("file")
)

func newFake(t *testing.T) *mattermostfake.Fake {
	mm := mattermostfake.New(t, token, bot)
	mm.AddUser(ayse)
	mm.AddTeam(mattermostfake.Team{ID: team, Name: "core", DisplayName: "Core", Members: []string{ayse.ID}})
	mm.AddChannel(
		mattermostfake.Channel{ID: channel, TeamID: team, Type: "O", Name: "town", DisplayName: "Town", Members: []string{ayse.ID}},
		mattermostfake.Channel{ID: id("dm"), Type: "D", Name: bot.ID + "__" + ayse.ID},
	)
	return mm
}

// newClient returns a client for mm over its own transport, with the endpoint given with trailing slashes.
func newClient(t *testing.T, mm *mattermostfake.Fake) *mattermost.Client {
	return mattermost.NewClient(mm.URL()+"//", token, transport(t))
}

func transport(t *testing.T) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

// wire is what a request puts on the wire: the request line, the headers the client sets, and the body.
type wire struct {
	call, authorization, contentType, body string
}

func onWire(r testkit.Request) wire {
	return wire{r.Call(), r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(r.Body)}
}

func TestRequestsOnTheWire(t *testing.T) {
	const auth, js = "Bearer " + token, "application/json"
	fileInfo := testkit.JSON(200, map[string]any{"id": file, "name": "a.png"})
	tests := []struct {
		name string
		do   func(context.Context, *mattermost.Client, *mattermostfake.Fake) error
		want wire
	}{
		{"post at the root", func(ctx context.Context, c *mattermost.Client, _ *mattermostfake.Fake) error {
			_, err := c.CreatePost(ctx, channel, "Merhaba <dünya> & co", "", []string{})
			return err
		}, wire{"POST /api/v4/posts", auth, js, `{"channel_id":"` + channel + `","message":"Merhaba <dünya> & co"}`}},
		{"post in a thread with files", func(ctx context.Context, c *mattermost.Client, _ *mattermostfake.Fake) error {
			_, err := c.CreatePost(ctx, channel, "ok", id("root"), []string{id("f1"), id("f2")})
			return err
		}, wire{"POST /api/v4/posts", auth, js, `{"channel_id":"` + channel + `","message":"ok","root_id":"` + id("root") +
			`","file_ids":["` + id("f1") + `","` + id("f2") + `"]}`}},
		{"me", func(ctx context.Context, c *mattermost.Client, _ *mattermostfake.Fake) error {
			_, err := c.GetMe(ctx)
			return err
		}, wire{"GET /api/v4/users/me", auth, js, ""}},
		{"user", func(ctx context.Context, c *mattermost.Client, _ *mattermostfake.Fake) error {
			_, err := c.GetUser(ctx, ayse.ID)
			return err
		}, wire{"GET /api/v4/users/" + ayse.ID, auth, js, ""}},
		{"teams", func(ctx context.Context, c *mattermost.Client, _ *mattermostfake.Fake) error {
			_, err := c.GetMyTeams(ctx)
			return err
		}, wire{"GET /api/v4/users/me/teams", auth, js, ""}},
		{"channels", func(ctx context.Context, c *mattermost.Client, _ *mattermostfake.Fake) error {
			_, err := c.GetMyChannels(ctx, team)
			return err
		}, wire{"GET /api/v4/users/me/teams/" + team + "/channels", auth, js, ""}},
		{"file info", func(ctx context.Context, c *mattermost.Client, mm *mattermostfake.Fake) error {
			mm.On("GET", "/api/v4/files/{file_id}/info").Reply(fileInfo)
			_, err := c.GetFileInfo(ctx, file)
			return err
		}, wire{"GET /api/v4/files/" + file + "/info", auth, js, ""}},
		{"file content", func(ctx context.Context, c *mattermost.Client, mm *mattermostfake.Fake) error {
			mm.On("GET", "/api/v4/files/{file_id}").Reply(testkit.Raw(200, "image/png", []byte("png")))
			_, err := c.DownloadFile(ctx, file, 10)
			return err
		}, wire{"GET /api/v4/files/" + file, auth, "", ""}},
		{"posts since", func(ctx context.Context, c *mattermost.Client, _ *mattermostfake.Fake) error {
			_, err := c.GetChannelPosts(ctx, channel, 1767225600001, 0)
			return err
		}, wire{"GET /api/v4/channels/" + channel + "/posts?since=1767225600001", auth, js, ""}},
		{"latest posts", func(ctx context.Context, c *mattermost.Client, _ *mattermostfake.Fake) error {
			_, err := c.GetChannelPosts(ctx, channel, 0, 15)
			return err
		}, wire{"GET /api/v4/channels/" + channel + "/posts?per_page=15", auth, js, ""}},
		{"posts since with a page size", func(ctx context.Context, c *mattermost.Client, _ *mattermostfake.Fake) error {
			_, err := c.GetChannelPosts(ctx, channel, 5, 15)
			return err
		}, wire{"GET /api/v4/channels/" + channel + "/posts?since=5&per_page=15", auth, js, ""}},
		{"posts without parameters", func(ctx context.Context, c *mattermost.Client, _ *mattermostfake.Fake) error {
			_, err := c.GetChannelPosts(ctx, channel, 0, 0)
			return err
		}, wire{"GET /api/v4/channels/" + channel + "/posts", auth, js, ""}},
		{"search", func(ctx context.Context, c *mattermost.Client, _ *mattermostfake.Fake) error {
			_, err := c.SearchPosts(ctx, `deploy "prod" <eu>`)
			return err
		}, wire{"POST /api/v4/posts/search", auth, js, `{"terms":"deploy \"prod\" <eu>","is_or_search":false}`}},
		{"users by IDs", func(ctx context.Context, c *mattermost.Client, _ *mattermostfake.Fake) error {
			_, err := c.GetUsersByIDs(ctx, []string{ayse.ID, "", bot.ID})
			return err
		}, wire{"POST /api/v4/users/ids", auth, js, `["` + ayse.ID + `","` + bot.ID + `"]`}},
		{"channel members", func(ctx context.Context, c *mattermost.Client, _ *mattermostfake.Fake) error {
			_, err := c.GetChannelMemberIDs(ctx, channel, []string{"", ayse.ID})
			return err
		}, wire{"POST /api/v4/channels/" + channel + "/members/ids", auth, js, `["` + ayse.ID + `"]`}},
		{"team members", func(ctx context.Context, c *mattermost.Client, _ *mattermostfake.Fake) error {
			_, err := c.GetTeamMemberIDs(ctx, team, []string{"", ayse.ID})
			return err
		}, wire{"POST /api/v4/teams/" + team + "/members/ids", auth, js, `["` + ayse.ID + `"]`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mm := newFake(t)
			if err := tt.do(t.Context(), newClient(t, mm), mm); err != nil {
				t.Fatal(err)
			}
			reqs := mm.Requests()
			if len(reqs) != 1 {
				t.Fatalf("sent %d requests, want 1: %v", len(reqs), mm.Calls())
			}
			if got := onWire(reqs[0]); got != tt.want {
				t.Errorf("sent\n  %+v\nwant\n  %+v", got, tt.want)
			}
		})
	}
}

func TestUploadFileSendsAMultipartForm(t *testing.T) {
	mm := newFake(t)
	mm.On("POST", "/api/v4/files").Reply(testkit.JSON(201, map[string]any{
		"file_infos": []map[string]string{{"id": file}, {"id": id("other")}}, "client_ids": []string{},
	}))
	got, err := newClient(t, mm).UploadFile(t.Context(), channel, chat.File{Name: "Görsel 1.png", MediaType: "image/png", Data: []byte("\x89PNG")})
	if err != nil || got != file {
		t.Fatalf("returned %q, %v; want the first file info's ID", got, err)
	}
	r := mm.Requests()[0]
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		t.Fatalf("content type %q", r.Header.Get("Content-Type"))
	}
	b := params["boundary"]
	want := wire{"POST /api/v4/files", "Bearer " + token, "multipart/form-data; boundary=" + b,
		"--" + b + "\r\n" +
			`Content-Disposition: form-data; name="channel_id"` + "\r\n\r\n" +
			channel + "\r\n" +
			"--" + b + "\r\n" +
			`Content-Disposition: form-data; name="files"; filename="Görsel 1.png"` + "\r\n" +
			"Content-Type: image/png\r\n\r\n" +
			"\x89PNG\r\n" +
			"--" + b + "--\r\n"}
	if got := onWire(r); got != want {
		t.Errorf("sent\n  %q\nwant\n  %q", got, want)
	}
}

func TestLookupsDecodeResponses(t *testing.T) {
	mm := newFake(t)
	c := newClient(t, mm)
	ctx := t.Context()
	mm.AddPost(mattermostfake.Post{ID: id("p1"), UserID: ayse.ID, ChannelID: channel, Message: "first"})
	mm.AddPost(mattermostfake.Post{ID: id("p2"), UserID: ayse.ID, ChannelID: channel, RootID: id("p1"), Message: "second deploy"})

	user := func(u mattermostfake.User) mattermost.User {
		return mattermost.User{ID: u.ID, Username: u.Username, FirstName: u.FirstName, LastName: u.LastName, Nickname: u.Nickname, Position: u.Position}
	}
	check := func(name string, got, want any, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", name, err)
		} else if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, want)
		}
	}
	me, err := c.GetMe(ctx)
	check("me", me, user(bot), err)
	u, err := c.GetUser(ctx, ayse.ID)
	check("user", u, user(ayse), err)
	users, err := c.GetUsersByIDs(ctx, []string{ayse.ID, id("nobody")})
	check("users by IDs", users, []mattermost.User{user(ayse)}, err)
	teams, err := c.GetMyTeams(ctx)
	check("teams", teams, []mattermost.Team{{ID: team, Name: "core", DisplayName: "Core"}}, err)
	channels, err := c.GetMyChannels(ctx, team)
	check("channels", channels, []mattermost.Channel{
		{ID: channel, Type: "O", Name: "town", DisplayName: "Town"},
		{ID: id("dm"), Type: "D", Name: bot.ID + "__" + ayse.ID},
	}, err)
	members, err := c.GetChannelMemberIDs(ctx, channel, []string{bot.ID, ayse.ID})
	check("members", members, []string{ayse.ID}, err)
	members, err = c.GetTeamMemberIDs(ctx, team, []string{bot.ID, ayse.ID})
	check("team members", members, []string{ayse.ID}, err)

	list, err := c.GetChannelPosts(ctx, channel, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(list.Order, []string{id("p2"), id("p1")}) {
		t.Errorf("order = %v, want newest first", list.Order)
	}
	p := list.Posts[id("p2")]
	if p.ID != id("p2") || p.UserID != ayse.ID || p.ChannelID != channel || p.RootID != id("p1") ||
		p.Message != "second deploy" || p.CreateAt != list.Posts[id("p1")].CreateAt+1 {
		t.Errorf("post = %+v", p)
	}
	found, err := c.SearchPosts(ctx, "deploy")
	check("search order", found.Order, []string{id("p2")}, err)

	sent, err := c.CreatePost(ctx, channel, "hi", "", nil)
	if err != nil || len(sent.ID) != 26 || sent.Message != "hi" {
		t.Errorf("created post = %+v, %v", sent, err)
	}
}

func TestSequentialCallsReuseOneConnection(t *testing.T) {
	mm := newFake(t)
	teams := make([]mattermostfake.Team, 200) // large enough to be sent chunked
	for i := range teams {
		teams[i] = mattermostfake.Team{ID: id(fmt.Sprint(i)), Name: "team", DisplayName: "Team"}
	}
	mm.On("GET", "/api/v4/users/me/teams").Default(testkit.JSON(200, teams))
	tr := transport(t)
	var dials atomic.Int32
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return dial(ctx, network, addr)
	}
	c := mattermost.NewClient(mm.URL(), token, tr)
	for range 20 {
		if got, err := c.GetMyTeams(t.Context()); err != nil || len(got) != len(teams) {
			t.Fatalf("teams = %d, %v", len(got), err)
		}
	}
	if n := dials.Load(); n != 1 {
		t.Errorf("20 calls opened %d connections, want 1", n)
	}
}

func TestDownloadWithoutALimitReadsTheWholeFile(t *testing.T) {
	mm := newFake(t)
	mm.On("GET", "/api/v4/files/{file_id}").Reply(testkit.Raw(200, "application/pdf", []byte("%PDF-1.7")))
	if data, err := newClient(t, mm).DownloadFile(t.Context(), file, math.MaxInt64); err != nil || string(data) != "%PDF-1.7" {
		t.Errorf("download = %q, %v", data, err)
	}
}

func TestPostsKeepTheirRawJSON(t *testing.T) {
	mm := newFake(t)
	raw := `{ "id": "` + id("p1") + `", "message": "hi", "create_at": 1767225600000, "props": {"from_bot": "true"} }`
	mm.On("GET", "/api/v4/channels/{channel_id}/posts").Reply(testkit.Raw(200, "application/json",
		[]byte(`{"order":["`+id("p1")+`"],"posts":{"`+id("p1")+`":`+raw+`},"next_post_id":"","has_next":false}`)))
	list, err := newClient(t, mm).GetChannelPosts(t.Context(), channel, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := list.Posts[id("p1")]
	if string(p.Raw) != raw {
		t.Errorf("raw post = %s, want the server's bytes %s", p.Raw, raw)
	}
	if p.Message != "hi" || p.CreateAt != 1767225600000 {
		t.Errorf("decoded post = %+v", p)
	}
}

func TestErrorStatusesAreReturned(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound,
		http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			mm := newFake(t)
			resp := testkit.JSON(status, map[string]any{"id": "app.error", "status_code": status})
			resp.Header.Set("Location", "/api/v4/users/"+ayse.ID)
			mm.On("GET", "/api/v4/users/me").Reply(resp)
			_, err := newClient(t, mm).GetMe(t.Context())
			want := &mattermost.APIError{Method: "GET", Path: "/users/me", Status: status}
			var got *mattermost.APIError
			if !errors.As(err, &got) || !reflect.DeepEqual(got, want) {
				t.Errorf("error = %#v, want %#v", err, want)
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("error %q shows the token", err)
			}
			mm.AssertCalls(t, "GET /api/v4/users/me") // no retry, no redirect
		})
	}
}

func TestTransportAndDecodeFailuresAreReturned(t *testing.T) {
	tests := []struct {
		name   string
		resp   testkit.Response
		status int
	}{
		{"connection reset", testkit.ConnReset(), 0},
		{"truncated body", testkit.JSON(200, mattermostfake.User{ID: ayse.ID}).Malformed(), 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mm := newFake(t)
			mm.On("GET", "/api/v4/users/{user_id}").Reply(tt.resp)
			_, err := newClient(t, mm).GetUser(t.Context(), ayse.ID)
			var got *mattermost.APIError
			if !errors.As(err, &got) || got.Method != "GET" || got.Path != "/users/"+ayse.ID || got.Status != tt.status || got.Err == nil {
				t.Fatalf("error = %#v", err)
			}
			if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), mm.URL()) {
				t.Errorf("error %q shows the token or the endpoint", err)
			}
		})
	}
}

func TestCallsEndAtTheCallersDeadline(t *testing.T) {
	mm := newFake(t)
	mm.On("GET", "/api/v4/users/me").Reply(testkit.JSON(200, bot).WithDelay(time.Hour))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := newClient(t, mm).GetMe(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want a deadline error", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("returned after %v", d)
	}
}

// deadlines is a transport that records how far away each request's deadline is.
type deadlines struct {
	base http.RoundTripper
	mu   sync.Mutex
	left map[string]time.Duration
}

func (d *deadlines) RoundTrip(r *http.Request) (*http.Response, error) {
	var left time.Duration
	if dl, ok := r.Context().Deadline(); ok {
		left = time.Until(dl)
	}
	d.mu.Lock()
	d.left[r.Method+" "+r.URL.Path] = left
	d.mu.Unlock()
	return d.base.RoundTrip(r)
}

func TestEveryCallUsesTheSharedTransportWithItsTimeout(t *testing.T) {
	mm := newFake(t)
	rec := &deadlines{base: transport(t), left: map[string]time.Duration{}}
	c := mattermost.NewClient(mm.URL(), token, rec)
	ctx := t.Context()
	fid, err := c.UploadFile(ctx, channel, chat.File{Name: "a.png", MediaType: "image/png", Data: []byte("png")})
	_, err2 := c.CreatePost(ctx, channel, "hi", "", nil)
	_, err3 := c.GetMe(ctx)
	_, err4 := c.GetUser(ctx, ayse.ID)
	_, err5 := c.GetMyTeams(ctx)
	_, err6 := c.GetMyChannels(ctx, team)
	_, err7 := c.GetFileInfo(ctx, fid)
	_, err8 := c.DownloadFile(ctx, fid, 100)
	_, err9 := c.GetChannelPosts(ctx, channel, 0, 5)
	_, err10 := c.SearchPosts(ctx, "hi")
	_, err11 := c.GetUsersByIDs(ctx, []string{ayse.ID})
	_, err12 := c.GetChannelMemberIDs(ctx, channel, []string{ayse.ID})
	_, err13 := c.GetTeamMemberIDs(ctx, team, []string{ayse.ID})
	if err := errors.Join(err, err2, err3, err4, err5, err6, err7, err8, err9, err10, err11, err12, err13); err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Duration{
		"POST /api/v4/files":                                time.Minute,
		"POST /api/v4/posts":                                30 * time.Second,
		"GET /api/v4/users/me":                              30 * time.Second,
		"GET /api/v4/users/" + ayse.ID:                      30 * time.Second,
		"GET /api/v4/users/me/teams":                        30 * time.Second,
		"GET /api/v4/users/me/teams/" + team + "/channels":  30 * time.Second,
		"GET /api/v4/files/" + fid + "/info":                30 * time.Second,
		"GET /api/v4/files/" + fid:                          time.Minute,
		"GET /api/v4/channels/" + channel + "/posts":        30 * time.Second,
		"POST /api/v4/posts/search":                         30 * time.Second,
		"POST /api/v4/users/ids":                            30 * time.Second,
		"POST /api/v4/channels/" + channel + "/members/ids": 30 * time.Second,
		"POST /api/v4/teams/" + team + "/members/ids":       30 * time.Second,
	}
	if len(rec.left) != len(want) || len(mm.Requests()) != len(want) {
		t.Errorf("transport saw %d calls and the server %d, want %d each", len(rec.left), len(mm.Requests()), len(want))
	}
	for call, timeout := range want {
		if left, ok := rec.left[call]; !ok || left <= timeout-5*time.Second || left > timeout {
			t.Errorf("%s: deadline in %v, want %v", call, left, timeout)
		}
	}
}

func TestInvalidIDsAreRejectedWithoutARequest(t *testing.T) {
	calls := map[string]func(context.Context, *mattermost.Client, string) error{
		"user": func(ctx context.Context, c *mattermost.Client, id string) error {
			_, err := c.GetUser(ctx, id)
			return err
		},
		"channels": func(ctx context.Context, c *mattermost.Client, id string) error {
			_, err := c.GetMyChannels(ctx, id)
			return err
		},
		"file info": func(ctx context.Context, c *mattermost.Client, id string) error {
			_, err := c.GetFileInfo(ctx, id)
			return err
		},
		"file content": func(ctx context.Context, c *mattermost.Client, id string) error {
			_, err := c.DownloadFile(ctx, id, 10)
			return err
		},
		"posts": func(ctx context.Context, c *mattermost.Client, id string) error {
			_, err := c.GetChannelPosts(ctx, id, 0, 10)
			return err
		},
		"members": func(ctx context.Context, c *mattermost.Client, id string) error {
			_, err := c.GetChannelMemberIDs(ctx, id, []string{ayse.ID})
			return err
		},
		"team members": func(ctx context.Context, c *mattermost.Client, id string) error {
			_, err := c.GetTeamMemberIDs(ctx, id, []string{ayse.ID})
			return err
		},
	}
	invalid := []string{"", "me", id("x")[1:], id("x") + "0", strings.ToUpper(id("abc")), "../../../users/me/teams/xx",
		"abc%2f..%2f..%2fusers00000", "abcdefghijklmnopqrstuvwx/y", "abcdefghijklmnopqrstuvwxy?", strings.Repeat("x", 5000)}
	mm := newFake(t)
	c := newClient(t, mm)
	for name, call := range calls {
		for _, bad := range invalid {
			if err := call(t.Context(), c, bad); err == nil || !strings.Contains(err.Error(), "invalid ID") {
				t.Errorf("%s(%.40q): error %.100v, want an invalid ID error", name, bad, err)
			} else if len(err.Error()) > 100 {
				t.Errorf("%s: error repeats %d bytes of the rejected value", name, len(err.Error()))
			}
		}
	}
	mm.AssertCalls(t)
}

func TestEmptyInputsSendNoRequest(t *testing.T) {
	mm := newFake(t)
	c := newClient(t, mm)
	users, err := c.GetUsersByIDs(t.Context(), []string{"", ""})
	if err != nil || len(users) != 0 {
		t.Errorf("users by no IDs = %v, %v", users, err)
	}
	members, err := c.GetChannelMemberIDs(t.Context(), channel, nil)
	if err != nil || len(members) != 0 {
		t.Errorf("members among no users = %v, %v", members, err)
	}
	members, err = c.GetTeamMemberIDs(t.Context(), team, []string{""})
	if err != nil || len(members) != 0 {
		t.Errorf("team members among no users = %v, %v", members, err)
	}
	mm.AssertCalls(t)
}

func TestChannelMembersForbiddenIsEmpty(t *testing.T) {
	mm := newFake(t)
	members, err := newClient(t, mm).GetChannelMemberIDs(t.Context(), id("private"), []string{ayse.ID})
	if err != nil || len(members) != 0 {
		t.Errorf("members of a forbidden channel = %v, %v; want none and no error", members, err)
	}
	mm.AssertCalls(t, "POST /api/v4/channels/"+id("private")+"/members/ids")
}

func TestUploadWithoutFileInfoFails(t *testing.T) {
	for _, body := range []string{`{"file_infos":[],"client_ids":[]}`, `{"file_infos":[{"name":"a.png"}]}`, `{}`} {
		mm := newFake(t)
		mm.On("POST", "/api/v4/files").Reply(testkit.Raw(201, "application/json", []byte(body)))
		if fid, err := newClient(t, mm).UploadFile(t.Context(), channel, chat.File{Name: "a.png", Data: []byte("x")}); err == nil {
			t.Errorf("upload answered %s: returned %q and no error", body, fid)
		}
	}
}

func TestDownloadStopsReadingPastTheLimit(t *testing.T) {
	mm := newFake(t)
	served := make(chan int64, 1)
	mm.On("GET", "/api/v4/files/{file_id}").Reply(testkit.Response{Handler: func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 32<<10)
		var n int64
		for r.Context().Err() == nil && n < 1<<30 {
			m, err := w.Write(chunk)
			n += int64(m)
			if err != nil {
				break
			}
		}
		served <- n
	}})
	start := time.Now()
	_, err := newClient(t, mm).DownloadFile(t.Context(), file, 1<<20)
	if !errors.Is(err, chat.ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
	if n := <-served; n >= 1<<30 {
		t.Errorf("the client read the whole %d-byte body", n)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("returned after %v", d)
	}
}
