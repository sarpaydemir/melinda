package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sarpaydemir/dost/internal/port/chat"
)

const (
	callTimeout = 30 * time.Second
	fileTimeout = 60 * time.Second
)

// Client calls the Mattermost REST API v4 as one bot account. It never retries.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient returns a client for the server at endpoint that authenticates with token and sends every request
// through transport.
func NewClient(endpoint, token string, transport http.RoundTripper) *Client {
	return &Client{
		base:  strings.TrimRight(endpoint, "/") + "/api/v4",
		token: token,
		http: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// APIError is a failed call: an error status, or Err when no usable response arrived.
type APIError struct {
	Method string
	// Path is relative to the API root and includes the query.
	Path string
	// Status is the HTTP status, or 0 when no response arrived.
	Status int
	Err    error
}

func (e *APIError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("mattermost %s %s: %v", e.Method, e.Path, e.Err)
	}
	return fmt.Sprintf("mattermost %s %s: %d %s", e.Method, e.Path, e.Status, http.StatusText(e.Status))
}

func (e *APIError) Unwrap() error { return e.Err }

// User is a Mattermost account.
type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	FullName  string `json:"full_name"`
	Nickname  string `json:"nickname"`
	Position  string `json:"position"`
}

// Team is a team the bot belongs to.
type Team struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// Channel is a channel the bot belongs to.
type Channel struct {
	ID string `json:"id"`
	// Type is O (public), P (private), D (direct), or G (group).
	Type        string `json:"type"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// Post is a Mattermost post.
type Post struct {
	ID        string `json:"id"`
	CreateAt  int64  `json:"create_at"`
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	RootID    string `json:"root_id"`
	Message   string `json:"message"`
	// Raw is the post as the server sent it.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes the post and keeps its encoding in Raw.
func (p *Post) UnmarshalJSON(data []byte) error {
	type fields Post
	if err := json.Unmarshal(data, (*fields)(p)); err != nil {
		return err
	}
	p.Raw = slices.Clone(data)
	return nil
}

// PostList is a page of posts: Order lists post IDs, newest first.
type PostList struct {
	Order []string        `json:"order"`
	Posts map[string]Post `json:"posts"`
}

// FileInfo describes an uploaded file.
type FileInfo struct {
	Name      string `json:"name"`
	Extension string `json:"extension"`
	MimeType  string `json:"mime_type"`
	Size      int64  `json:"size"`
}

// CreatePost posts message in a channel, as a reply to rootID unless it is empty, with the given files.
func (c *Client) CreatePost(ctx context.Context, channelID, message, rootID string, fileIDs []string) (Post, error) {
	body := struct {
		ChannelID string   `json:"channel_id"`
		Message   string   `json:"message"`
		RootID    string   `json:"root_id,omitempty"`
		FileIDs   []string `json:"file_ids,omitempty"`
	}{channelID, message, rootID, fileIDs}
	return call[Post](ctx, c, "POST", "/posts", body)
}

// UploadFile stores f in a channel for a later post and returns its ID.
func (c *Client) UploadFile(ctx context.Context, channelID string, f chat.File) (string, error) {
	var body bytes.Buffer // writes to it cannot fail
	w := multipart.NewWriter(&body)
	_ = w.WriteField("channel_id", channelID)
	part, _ := w.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {multipart.FileContentDisposition("files", f.Name)},
		"Content-Type":        {f.MediaType},
	})
	_, _ = part.Write(f.Data)
	_ = w.Close()
	var id string
	err := c.send(ctx, fileTimeout, "POST", "/files", w.FormDataContentType(), &body, func(r io.Reader) error {
		var out struct {
			FileInfos []struct {
				ID string `json:"id"`
			} `json:"file_infos"`
		}
		if err := json.NewDecoder(r).Decode(&out); err != nil {
			return err
		}
		if len(out.FileInfos) == 0 || out.FileInfos[0].ID == "" {
			return errors.New("no file ID in the response")
		}
		id = out.FileInfos[0].ID
		return nil
	})
	return id, err
}

// GetMe returns the bot's own account.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	return call[User](ctx, c, "GET", "/users/me", nil)
}

// GetUser returns an account.
func (c *Client) GetUser(ctx context.Context, userID string) (User, error) {
	path, err := idPath("/users/%s", userID)
	if err != nil {
		return User{}, err
	}
	return call[User](ctx, c, "GET", path, nil)
}

// GetUsersByIDs returns the accounts with the given IDs that exist; empty IDs are skipped.
func (c *Client) GetUsersByIDs(ctx context.Context, userIDs []string) ([]User, error) {
	ids := nonEmpty(userIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	return call[[]User](ctx, c, "POST", "/users/ids", ids)
}

// GetMyTeams returns the bot's teams.
func (c *Client) GetMyTeams(ctx context.Context) ([]Team, error) {
	return call[[]Team](ctx, c, "GET", "/users/me/teams", nil)
}

// GetMyChannels returns the bot's channels in a team, including its direct and group channels.
func (c *Client) GetMyChannels(ctx context.Context, teamID string) ([]Channel, error) {
	path, err := idPath("/users/me/teams/%s/channels", teamID)
	if err != nil {
		return nil, err
	}
	return call[[]Channel](ctx, c, "GET", path, nil)
}

// GetChannelMemberIDs returns which of the given users belong to a channel; empty IDs are skipped. A channel the
// bot may not see has no members.
func (c *Client) GetChannelMemberIDs(ctx context.Context, channelID string, userIDs []string) ([]string, error) {
	path, err := idPath("/channels/%s/members/ids", channelID)
	if err != nil {
		return nil, err
	}
	ids := nonEmpty(userIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	type member struct {
		UserID string `json:"user_id"`
	}
	members, err := call[[]member](ctx, c, "POST", path, ids)
	if apiErr, ok := errors.AsType[*APIError](err); ok && apiErr.Status == http.StatusForbidden {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]string, len(members))
	for i, m := range members {
		out[i] = m.UserID
	}
	return out, nil
}

// GetChannelPosts returns a channel's posts changed after since (Unix milliseconds), or its latest perPage
// posts. A zero argument is not sent.
func (c *Client) GetChannelPosts(ctx context.Context, channelID string, since int64, perPage int) (PostList, error) {
	path, err := idPath("/channels/%s/posts", channelID)
	if err != nil {
		return PostList{}, err
	}
	var query []string
	if since != 0 {
		query = append(query, "since="+strconv.FormatInt(since, 10))
	}
	if perPage != 0 {
		query = append(query, "per_page="+strconv.Itoa(perPage))
	}
	if len(query) > 0 {
		path += "?" + strings.Join(query, "&")
	}
	return call[PostList](ctx, c, "GET", path, nil)
}

// SearchPosts returns the posts that match all of terms in the bot's teams.
func (c *Client) SearchPosts(ctx context.Context, terms string) (PostList, error) {
	body := struct {
		Terms      string `json:"terms"`
		IsOrSearch bool   `json:"is_or_search"`
	}{Terms: terms}
	return call[PostList](ctx, c, "POST", "/posts/search", body)
}

// GetFileInfo describes an uploaded file.
func (c *Client) GetFileInfo(ctx context.Context, fileID string) (FileInfo, error) {
	path, err := idPath("/files/%s/info", fileID)
	if err != nil {
		return FileInfo{}, err
	}
	return call[FileInfo](ctx, c, "GET", path, nil)
}

// DownloadFile returns a file's content. Past maxBytes it stops reading and returns an error that wraps
// chat.ErrTooLarge.
func (c *Client) DownloadFile(ctx context.Context, fileID string, maxBytes int64) ([]byte, error) {
	path, err := idPath("/files/%s", fileID)
	if err != nil {
		return nil, err
	}
	var data []byte
	err = c.send(ctx, fileTimeout, "GET", path, "", nil, func(r io.Reader) error {
		var err error
		data, err = io.ReadAll(io.LimitReader(r, min(maxBytes, math.MaxInt64-1)+1))
		if err == nil && int64(len(data)) > maxBytes {
			return chat.ErrTooLarge
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// call sends in as a JSON body unless it is nil, and decodes the JSON response.
func call[T any](ctx context.Context, c *Client, method, path string, in any) (T, error) {
	var body io.Reader
	if in != nil {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(in); err != nil {
			var zero T
			return zero, err
		}
		body = bytes.NewReader(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	}
	var out T
	err := c.send(ctx, callTimeout, method, path, "application/json", body, func(r io.Reader) error {
		if err := json.NewDecoder(r).Decode(&out); err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(r, 4<<10)) // reaching the end lets the connection be reused
		return nil
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return out, nil
}

// send makes one request within timeout and hands the body of a 2xx response to read. An empty contentType
// sends no Content-Type header.
func (c *Client) send(ctx context.Context, timeout time.Duration, method, path, contentType string, body io.Reader, read func(io.Reader) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fail := func(status int, err error) error {
		return &APIError{Method: method, Path: path, Status: status, Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fail(0, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err // without the URL
		}
		return fail(0, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fail(resp.StatusCode, nil)
	}
	if err := read(resp.Body); err != nil {
		return fail(resp.StatusCode, err)
	}
	return nil
}

// idPath puts a Mattermost ID, 26 lowercase letters and digits, into a path.
func idPath(format, id string) (string, error) {
	if len(id) != 26 || strings.ContainsFunc(id, func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') }) {
		return "", fmt.Errorf("mattermost: invalid ID %.32q", id)
	}
	return fmt.Sprintf(format, url.PathEscape(id)), nil
}

func nonEmpty(ids []string) []string {
	return slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == "" })
}
