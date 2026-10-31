package mattermostfake_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/mattermostfake"
)

const token = "bot-token"

var bot = mattermostfake.User{ID: "bot", Username: "helper"}

func call(t *testing.T, mm *mattermostfake.Fake, method, path, body string, out any) int {
	t.Helper()
	req, err := http.NewRequest(method, mm.URL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: %v\n%s", method, path, err, data)
		}
	}
	return resp.StatusCode
}

type postList struct {
	Order []string                       `json:"order"`
	Posts map[string]mattermostfake.Post `json:"posts"`
}

func (l postList) messages() []string {
	var out []string
	for _, id := range l.Order {
		out = append(out, l.Posts[id].Message)
	}
	return out
}

func TestRejectsWrongToken(t *testing.T) {
	mm := mattermostfake.New(t, token, bot)
	resp, err := http.Get(mm.URL() + "/api/v4/users/me")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without token: status %d, want 401", resp.StatusCode)
	}
	var me mattermostfake.User
	if code := call(t, mm, "GET", "/api/v4/users/me", "", &me); code != 200 || me != bot {
		t.Errorf("with token: %d %+v, want 200 %+v", code, me, bot)
	}
	mm.AssertCalls(t, "GET /api/v4/users/me", "GET /api/v4/users/me")
}

func TestUsersTeamsAndChannels(t *testing.T) {
	mm := mattermostfake.New(t, token, bot)
	ayse := mattermostfake.User{ID: "u1", Username: "ayse", FirstName: "Ayşe", Position: "İK"}
	mm.AddUser(ayse, mattermostfake.User{ID: "u2", Username: "can"})
	mm.AddTeam(mattermostfake.Team{ID: "t1", Name: "core"}, mattermostfake.Team{ID: "t2", Name: "ops"})
	mm.AddChannel(
		mattermostfake.Channel{ID: "c1", TeamID: "t1", Type: "O"},
		mattermostfake.Channel{ID: "c2", TeamID: "t2", Type: "P"},
		mattermostfake.Channel{ID: "dm", Type: "D"},
	)

	var u mattermostfake.User
	if code := call(t, mm, "GET", "/api/v4/users/u1", "", &u); code != 200 || u != ayse {
		t.Errorf("get user: %d %+v", code, u)
	}
	if code := call(t, mm, "GET", "/api/v4/users/missing", "", nil); code != 404 {
		t.Errorf("missing user: status %d, want 404", code)
	}
	var users []mattermostfake.User
	call(t, mm, "POST", "/api/v4/users/ids", `["u2","missing","u1"]`, &users)
	if len(users) != 2 || users[0].ID != "u2" || users[1].ID != "u1" {
		t.Errorf("users by IDs = %+v, want u2 then u1", users)
	}
	var teams []mattermostfake.Team
	call(t, mm, "GET", "/api/v4/users/me/teams", "", &teams)
	if len(teams) != 2 {
		t.Errorf("teams = %+v, want 2", teams)
	}
	var channels []mattermostfake.Channel
	call(t, mm, "GET", "/api/v4/users/me/teams/t1/channels", "", &channels)
	var ids []string
	for _, c := range channels {
		ids = append(ids, c.ID)
	}
	if !slices.Equal(ids, []string{"c1", "dm"}) {
		t.Errorf("team channels = %v, want [c1 dm]", ids)
	}
}

func TestCreatedPostsAppearInChannelPosts(t *testing.T) {
	mm := mattermostfake.New(t, token, bot)
	mm.AddChannel(mattermostfake.Channel{ID: "c1", TeamID: "t1", Type: "O"})
	old := mm.AddPost(mattermostfake.Post{ChannelID: "c1", UserID: "u1", Message: "first"})
	mm.On("POST", "/api/v4/posts").Reply(testkit.Status(500))

	if code := call(t, mm, "POST", "/api/v4/posts", `{"channel_id":"c1","message":"lost"}`, nil); code != 500 {
		t.Errorf("scripted failure: status %d, want 500", code)
	}
	var created mattermostfake.Post
	code := call(t, mm, "POST", "/api/v4/posts", `{"channel_id":"c1","message":"second","root_id":"`+old.ID+`"}`, &created)
	if code != 201 || created.ID == "" || created.UserID != "bot" || created.RootID != old.ID || created.CreateAt <= old.CreateAt {
		t.Errorf("create post: %d %+v", code, created)
	}
	call(t, mm, "POST", "/api/v4/posts", `{"channel_id":"c1","message":"third"}`, nil)
	if code := call(t, mm, "POST", "/api/v4/posts", `{"message":"no channel"}`, nil); code != 400 {
		t.Errorf("post without channel: status %d, want 400", code)
	}

	var page postList
	call(t, mm, "GET", "/api/v4/channels/c1/posts?per_page=2", "", &page)
	if got := page.messages(); !slices.Equal(got, []string{"third", "second"}) {
		t.Errorf("per_page=2: %v, want [third second]", got)
	}
	var since postList
	call(t, mm, "GET", "/api/v4/channels/c1/posts?since="+strconv.FormatInt(created.UpdateAt-1, 10), "", &since)
	if got := since.messages(); !slices.Equal(got, []string{"third", "second"}) {
		t.Errorf("since: %v, want [third second]", got)
	}
	call(t, mm, "GET", "/api/v4/channels/c1/posts?since="+strconv.FormatInt(created.UpdateAt, 10), "", &since)
	if got := since.messages(); !slices.Equal(got, []string{"third"}) {
		t.Errorf("since is exclusive: %v, want [third]", got)
	}
}

func TestSearchMatchesEveryTermIgnoringCase(t *testing.T) {
	mm := mattermostfake.New(t, token, bot)
	mm.AddPost(mattermostfake.Post{ChannelID: "c1", Message: "Deploy plan for Friday"})
	mm.AddPost(mattermostfake.Post{ChannelID: "c2", Message: "deploy finished"})

	var res postList
	call(t, mm, "POST", "/api/v4/posts/search", `{"terms":"DEPLOY plan","is_or_search":false}`, &res)
	if got := res.messages(); !slices.Equal(got, []string{"Deploy plan for Friday"}) {
		t.Errorf("search = %v", got)
	}
}

func TestChannelMemberIDs(t *testing.T) {
	mm := mattermostfake.New(t, token, bot)
	mm.AddChannel(mattermostfake.Channel{ID: "c1", Type: "P", Members: []string{"bot", "u1"}})

	var members []map[string]string
	call(t, mm, "POST", "/api/v4/channels/c1/members/ids", `["u1","u2"]`, &members)
	if len(members) != 1 || members[0]["user_id"] != "u1" {
		t.Errorf("members = %v, want only u1", members)
	}
	if code := call(t, mm, "POST", "/api/v4/channels/unknown/members/ids", `["u1"]`, nil); code != 403 {
		t.Errorf("unknown channel: status %d, want 403", code)
	}
}

func TestTeamMemberIDs(t *testing.T) {
	mm := mattermostfake.New(t, token, bot)
	mm.AddTeam(mattermostfake.Team{ID: "t1", Members: []string{"bot", "u1"}})

	var members []map[string]string
	call(t, mm, "POST", "/api/v4/teams/t1/members/ids", `["u1","u2"]`, &members)
	if len(members) != 1 || members[0]["team_id"] != "t1" || members[0]["user_id"] != "u1" {
		t.Errorf("members = %v, want only u1", members)
	}
	if code := call(t, mm, "POST", "/api/v4/teams/unknown/members/ids", `["u1"]`, nil); code != 403 {
		t.Errorf("unknown team: status %d, want 403", code)
	}
}

func TestUploadFiles(t *testing.T) {
	mm := mattermostfake.New(t, token, bot)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("channel_id", "c1")
	part, _ := w.CreateFormFile("files", "Görsel.PNG")
	_, _ = part.Write([]byte("png-bytes"))
	_ = w.Close()
	req, _ := http.NewRequest("POST", mm.URL()+"/api/v4/files", &body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		FileInfos []struct {
			ID, Name, Extension string
			Size                int
		} `json:"file_infos"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 || len(out.FileInfos) != 1 {
		t.Fatalf("upload: %d %+v", resp.StatusCode, out)
	}
	if fi := out.FileInfos[0]; len(fi.ID) != 26 || fi.Name != "Görsel.PNG" || fi.Extension != "png" || fi.Size != 9 {
		t.Errorf("file info = %+v", fi)
	}

	m, err := mm.Requests()[0].Multipart()
	if err != nil || len(m.Files) != 1 || string(m.Files[0].Data) != "png-bytes" {
		t.Errorf("recorded upload = %+v, %v", m, err)
	}
}

func TestServesUploadedFiles(t *testing.T) {
	mm := mattermostfake.New(t, token, bot)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("channel_id", "c1")
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", multipart.FileContentDisposition("files", "chart.png"))
	h.Set("Content-Type", "image/png")
	part, _ := w.CreatePart(h)
	_, _ = part.Write([]byte("png-bytes"))
	_ = w.Close()
	var uploaded struct {
		FileInfos []struct{ ID string } `json:"file_infos"`
	}
	if code := upload(t, mm, &body, w.FormDataContentType(), &uploaded); code != 201 || len(uploaded.FileInfos) != 1 {
		t.Fatalf("upload: %d %+v", code, uploaded)
	}
	id := uploaded.FileInfos[0].ID

	var info struct {
		ID, Name, Extension string
		MimeType            string `json:"mime_type"`
		Size                int
	}
	if code := call(t, mm, "GET", "/api/v4/files/"+id+"/info", "", &info); code != 200 || info.ID != id ||
		info.Name != "chart.png" || info.Extension != "png" || info.MimeType != "image/png" || info.Size != 9 {
		t.Errorf("file info: %d %+v", code, info)
	}
	req, _ := http.NewRequest("GET", mm.URL()+"/api/v4/files/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(data) != "png-bytes" || resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("file content: %d %q %s", resp.StatusCode, data, resp.Header.Get("Content-Type"))
	}
	for _, path := range []string{"/api/v4/files/missing/info", "/api/v4/files/missing"} {
		if code := call(t, mm, "GET", path, "", nil); code != 404 {
			t.Errorf("GET %s: status %d, want 404", path, code)
		}
	}
}

func upload(t *testing.T, mm *mattermostfake.Fake, body io.Reader, contentType string, out any) int {
	t.Helper()
	req, _ := http.NewRequest("POST", mm.URL()+"/api/v4/files", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}
