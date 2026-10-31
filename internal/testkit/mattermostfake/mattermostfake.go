// Package mattermostfake fakes a Mattermost server: REST API v4 routes and the websocket event stream.
package mattermostfake

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"github.com/sarpaydemir/dost/internal/testkit"
)

// User is the part of a Mattermost user that clients read.
type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Nickname  string `json:"nickname"`
	Position  string `json:"position"`
}

// Team is a Mattermost team.
type Team struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// Channel is a Mattermost channel.
type Channel struct {
	ID          string   `json:"id"`
	TeamID      string   `json:"team_id"` // empty for direct and group channels
	Type        string   `json:"type"`    // O, P, D, or G
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Members     []string `json:"-"`
}

// Post is a Mattermost post.
type Post struct {
	ID        string   `json:"id"`
	CreateAt  int64    `json:"create_at"`
	UpdateAt  int64    `json:"update_at"`
	DeleteAt  int64    `json:"delete_at"`
	UserID    string   `json:"user_id"`
	ChannelID string   `json:"channel_id"`
	RootID    string   `json:"root_id"`
	Message   string   `json:"message"`
	Type      string   `json:"type"`
	FileIDs   []string `json:"file_ids,omitempty"`
}

// Fake is a Mattermost server as seen by one bot account. REST routes answer from the users, teams, channels,
// and posts added to it, and posts created through the API are added too; files uploaded through the API are
// served back. Requests without the bot's bearer token get 401.
type Fake struct {
	*testkit.Server
	t       testing.TB
	token   string
	bot     User
	changed testkit.Notifier

	mu       sync.Mutex
	users    map[string]User
	teams    []Team
	channels []Channel
	posts    []Post
	files    map[string]file
	lastID   int
	lastMS   int64
	ws       wsState
}

// file is an uploaded file.
type file struct {
	info fileInfo
	data []byte
}

type fileInfo struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	Name      string `json:"name"`
	Extension string `json:"extension"`
	Size      int    `json:"size"`
	MimeType  string `json:"mime_type"`
}

// startMS is the creation time, in Unix milliseconds, of the first post that gets one from the fake.
const startMS = 1767225600000 // 2026-01-01T00:00:00Z

// New starts a fake that accepts token and authenticates it as bot.
func New(t testing.TB, token string, bot User) *Fake {
	f := &Fake{
		Server: testkit.NewServer(t), t: t, token: token, bot: bot,
		users: map[string]User{bot.ID: bot}, files: map[string]file{}, lastMS: startMS - 1,
		ws: wsState{conns: map[*websocket.Conn]struct{}{}},
	}
	f.Guard(func(r testkit.Request) (testkit.Response, bool) {
		return appError(http.StatusUnauthorized, "api.context.session_expired.app_error"),
			r.Path != wsPath && r.Header.Get("Authorization") != "Bearer "+token
	})
	f.On("GET", "/api/v4/users/me").DefaultFunc(func(testkit.Request) testkit.Response { return testkit.JSON(200, f.bot) })
	f.On("GET", "/api/v4/users/{user_id}").DefaultFunc(f.getUser)
	f.On("POST", "/api/v4/users/ids").DefaultFunc(f.getUsersByIDs)
	f.On("GET", "/api/v4/users/me/teams").DefaultFunc(f.myTeams)
	f.On("GET", "/api/v4/users/me/teams/{team_id}/channels").DefaultFunc(f.myChannels)
	f.On("POST", "/api/v4/channels/{channel_id}/members/ids").DefaultFunc(f.memberIDs)
	f.On("GET", "/api/v4/channels/{channel_id}/posts").DefaultFunc(f.channelPosts)
	f.On("POST", "/api/v4/posts").DefaultFunc(f.createPost)
	f.On("POST", "/api/v4/posts/search").DefaultFunc(f.search)
	f.On("POST", "/api/v4/files").DefaultFunc(f.uploadFiles)
	f.On("GET", "/api/v4/files/{file_id}/info").DefaultFunc(f.fileInfo)
	f.On("GET", "/api/v4/files/{file_id}").DefaultFunc(f.fileContent)
	f.On("GET", wsPath).Default(testkit.Response{Handler: f.serveWS})
	return f
}

// AddUser adds users.
func (f *Fake) AddUser(users ...User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range users {
		f.users[u.ID] = u
	}
}

// AddTeam adds teams the bot belongs to.
func (f *Fake) AddTeam(teams ...Team) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teams = append(f.teams, teams...)
}

// AddChannel adds channels the bot belongs to.
func (f *Fake) AddChannel(channels ...Channel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels = append(f.channels, channels...)
}

// AddPost stores p without pushing an event, filling in a missing ID and timestamps, and returns it.
func (f *Fake) AddPost(p Post) Post {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.ID == "" {
		p.ID = f.newID()
	}
	if p.CreateAt == 0 {
		f.lastMS++
		p.CreateAt = f.lastMS
	}
	if p.UpdateAt == 0 {
		p.UpdateAt = p.CreateAt
	}
	f.posts = append(f.posts, p)
	return p
}

// Posts returns the posts in a channel, oldest first.
func (f *Fake) Posts(channelID string) []Post {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Post
	for _, p := range f.posts {
		if p.ChannelID == channelID {
			out = append(out, p)
		}
	}
	return out
}

// newID returns a 26-character ID, the length of Mattermost IDs; f.mu must be held.
func (f *Fake) newID() string {
	f.lastID++
	return fmt.Sprintf("%026d", f.lastID)
}

func (f *Fake) channel(id string) (Channel, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.channels, func(c Channel) bool { return c.ID == id })
	if i < 0 {
		return Channel{}, false
	}
	return f.channels[i], true
}

func (f *Fake) getUser(r testkit.Request) testkit.Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[r.Params["user_id"]]
	if !ok {
		return appError(http.StatusNotFound, "app.user.missing_account.const")
	}
	return testkit.JSON(200, u)
}

func (f *Fake) getUsersByIDs(r testkit.Request) testkit.Response {
	var ids []string
	if err := json.Unmarshal(r.Body, &ids); err != nil {
		return appError(http.StatusBadRequest, "api.context.invalid_body_param.app_error")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	users := []User{}
	for _, id := range ids {
		if u, ok := f.users[id]; ok {
			users = append(users, u)
		}
	}
	return testkit.JSON(200, users)
}

func (f *Fake) myTeams(testkit.Request) testkit.Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	return testkit.JSON(200, append([]Team{}, f.teams...))
}

// myChannels lists the team's channels plus every direct and group channel, as Mattermost does.
func (f *Fake) myChannels(r testkit.Request) testkit.Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	channels := []Channel{}
	for _, c := range f.channels {
		if c.TeamID == r.Params["team_id"] || c.TeamID == "" {
			channels = append(channels, c)
		}
	}
	return testkit.JSON(200, channels)
}

func (f *Fake) memberIDs(r testkit.Request) testkit.Response {
	c, ok := f.channel(r.Params["channel_id"])
	if !ok {
		return appError(http.StatusForbidden, "api.context.permissions.app_error")
	}
	var ids []string
	if err := json.Unmarshal(r.Body, &ids); err != nil {
		return appError(http.StatusBadRequest, "api.context.invalid_body_param.app_error")
	}
	members := []map[string]string{}
	for _, id := range ids {
		if slices.Contains(c.Members, id) {
			members = append(members, map[string]string{"channel_id": c.ID, "user_id": id})
		}
	}
	return testkit.JSON(200, members)
}

// channelPosts answers with posts updated after `since` (milliseconds), or else with the newest `per_page`
// posts (default 60).
func (f *Fake) channelPosts(r testkit.Request) testkit.Response {
	q := r.Query()
	posts := f.Posts(r.Params["channel_id"])
	if s := q.Get("since"); s != "" {
		since, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return appError(http.StatusBadRequest, "api.context.invalid_url_param.app_error")
		}
		posts = slices.DeleteFunc(posts, func(p Post) bool { return p.UpdateAt <= since })
		return postList(posts)
	}
	perPage := 60
	if s := q.Get("per_page"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return appError(http.StatusBadRequest, "api.context.invalid_url_param.app_error")
		}
		perPage = n
	}
	posts = sortNewestFirst(posts)
	return postList(posts[:min(perPage, len(posts))])
}

func (f *Fake) createPost(r testkit.Request) testkit.Response {
	var in struct {
		ChannelID string   `json:"channel_id"`
		Message   string   `json:"message"`
		RootID    string   `json:"root_id"`
		FileIDs   []string `json:"file_ids"`
	}
	if err := json.Unmarshal(r.Body, &in); err != nil || in.ChannelID == "" {
		return appError(http.StatusBadRequest, "api.context.invalid_body_param.app_error")
	}
	p := f.AddPost(Post{UserID: f.bot.ID, ChannelID: in.ChannelID, RootID: in.RootID, Message: in.Message, FileIDs: in.FileIDs})
	return testkit.JSON(http.StatusCreated, p)
}

// search matches posts whose message contains every term, ignoring case.
func (f *Fake) search(r testkit.Request) testkit.Response {
	var in struct {
		Terms string `json:"terms"`
	}
	if err := json.Unmarshal(r.Body, &in); err != nil {
		return appError(http.StatusBadRequest, "api.context.invalid_body_param.app_error")
	}
	terms := strings.Fields(strings.ToLower(in.Terms))
	f.mu.Lock()
	defer f.mu.Unlock()
	var matches []Post
	for _, p := range f.posts {
		msg := strings.ToLower(p.Message)
		if len(terms) > 0 && !slices.ContainsFunc(terms, func(term string) bool { return !strings.Contains(msg, term) }) {
			matches = append(matches, p)
		}
	}
	return postList(matches)
}

func (f *Fake) uploadFiles(r testkit.Request) testkit.Response {
	m, err := r.Multipart()
	if err != nil || len(m.Values["channel_id"]) == 0 {
		return appError(http.StatusBadRequest, "api.file.upload_file.storage.app_error")
	}
	infos := []fileInfo{}
	f.mu.Lock()
	for _, part := range m.Files {
		if part.Field != "files" {
			continue
		}
		info := fileInfo{
			ID: f.newID(), UserID: f.bot.ID, ChannelID: m.Values["channel_id"][0],
			Name: part.Filename, Extension: strings.ToLower(strings.TrimPrefix(path.Ext(part.Filename), ".")),
			Size: len(part.Data), MimeType: part.ContentType,
		}
		f.files[info.ID] = file{info: info, data: part.Data}
		infos = append(infos, info)
	}
	f.mu.Unlock()
	return testkit.JSON(http.StatusCreated, map[string]any{"file_infos": infos, "client_ids": []string{}})
}

func (f *Fake) fileInfo(r testkit.Request) testkit.Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := f.files[r.Params["file_id"]]
	if !ok {
		return appError(http.StatusNotFound, "app.file_info.get.app_error")
	}
	return testkit.JSON(200, file.info)
}

func (f *Fake) fileContent(r testkit.Request) testkit.Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := f.files[r.Params["file_id"]]
	if !ok {
		return appError(http.StatusNotFound, "app.file_info.get.app_error")
	}
	return testkit.Raw(200, file.info.MimeType, file.data)
}

func sortNewestFirst(posts []Post) []Post {
	slices.SortStableFunc(posts, func(a, b Post) int { return cmp.Compare(b.CreateAt, a.CreateAt) })
	return posts
}

// postList encodes posts as a Mattermost post list, newest first.
func postList(posts []Post) testkit.Response {
	order := []string{}
	byID := map[string]Post{}
	for _, p := range sortNewestFirst(posts) {
		order = append(order, p.ID)
		byID[p.ID] = p
	}
	return testkit.JSON(200, map[string]any{"order": order, "posts": byID})
}

func appError(status int, id string) testkit.Response {
	return testkit.JSON(status, map[string]any{"id": id, "message": id, "status_code": status})
}
