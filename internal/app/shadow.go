package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/routing"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

// recorder suppresses the writes of shadow mode, writing one JSON line to w for each.
type recorder struct {
	mu  sync.Mutex
	w   io.Writer
	clk clock.Clock
}

// record is one suppressed write. ID is the synthetic ID returned in place of the one the write would have created.
type record struct {
	Time            string          `json:"time"`
	Kind            string          `json:"kind"`
	ConversationKey string          `json:"conversation_key,omitempty"`
	PostIDs         []string        `json:"post_ids,omitempty"`
	ID              string          `json:"id,omitempty"`
	ChannelID       string          `json:"channel_id,omitempty"`
	RootID          string          `json:"root_id,omitempty"`
	Message         string          `json:"message,omitempty"`
	FileIDs         []string        `json:"file_ids,omitempty"`
	File            *file           `json:"file,omitempty"`
	Method          string          `json:"method,omitempty"`
	Path            string          `json:"path,omitempty"`
	Body            json.RawMessage `json:"body,omitempty"`
}

type file struct {
	Name string `json:"name"`
	Size int    `json:"size"`
}

// write stamps r with the time and the job that ctx carries, and writes it.
func (rec *recorder) write(ctx context.Context, r record) error {
	r.Time = rec.clk.Now().UTC().Format(time.RFC3339Nano)
	if j, ok := routing.JobFrom(ctx); ok {
		r.ConversationKey, r.PostIDs = j.Key, j.MessageIDs
	}
	var line bytes.Buffer
	e := json.NewEncoder(&line)
	e.SetEscapeHTML(false)
	if err := e.Encode(r); err != nil {
		return err
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	_, err := rec.w.Write(line.Bytes())
	return err
}

// platform returns p with its posts and uploads recorded instead of performed.
func (rec *recorder) platform(p chat.Platform) chat.Platform { return shadowPlatform{p, rec} }

// shadowPlatform forwards each read explicitly, so a method added to chat.Platform fails to compile until shadow
// mode handles it.
type shadowPlatform struct {
	p   chat.Platform
	rec *recorder
}

func (s shadowPlatform) Listen(ctx context.Context, connected func(), deliver chat.Deliver) error {
	return s.p.Listen(ctx, connected, deliver)
}

func (s shadowPlatform) Replay(ctx context.Context, after time.Time, deliver chat.Deliver) error {
	return s.p.Replay(ctx, after, deliver)
}

func (s shadowPlatform) Decode(event json.RawMessage) (chat.Message, bool) { return s.p.Decode(event) }

func (s shadowPlatform) Identity() chat.User { return s.p.Identity() }

func (s shadowPlatform) Self(ctx context.Context) (chat.User, error) { return s.p.Self(ctx) }

func (s shadowPlatform) User(ctx context.Context, id string) (chat.User, error) {
	return s.p.User(ctx, id)
}

func (s shadowPlatform) Attachment(ctx context.Context, id string) (chat.Attachment, error) {
	return s.p.Attachment(ctx, id)
}

func (s shadowPlatform) Download(ctx context.Context, id string, max int64) ([]byte, error) {
	return s.p.Download(ctx, id, max)
}

func (p shadowPlatform) Send(ctx context.Context, m chat.Outgoing) (string, error) {
	id := newPlatformID()
	err := p.rec.write(ctx, record{Kind: "post", ID: id, ChannelID: m.ConversationID, RootID: m.ThreadID,
		Message: m.Text, FileIDs: m.FileIDs})
	if err != nil {
		return "", err
	}
	return id, nil
}

func (p shadowPlatform) Upload(ctx context.Context, conversationID string, f chat.File) (string, error) {
	id := newPlatformID()
	err := p.rec.write(ctx, record{Kind: "upload", ID: id, ChannelID: conversationID, File: &file{f.Name, len(f.Data)}})
	if err != nil {
		return "", err
	}
	return id, nil
}

// newPlatformID returns a random ID in the form of a Mattermost ID: 26 lowercase letters and digits.
func newPlatformID() string { return strings.ToLower(rand.Text()) }

// transport returns next with every request but GET and HEAD recorded instead of sent. A recorded POST is answered
// 201 with a JSON object holding a synthetic UUID as its id, any other 200 with an empty object.
func (rec *recorder) transport(next http.RoundTripper) http.RoundTripper {
	return roundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet || req.Method == http.MethodHead {
			return next.RoundTrip(req)
		}
		var body []byte
		if req.Body != nil {
			var err error
			body, err = io.ReadAll(req.Body)
			req.Body.Close()
			if err != nil {
				return nil, err
			}
		}
		r := record{Kind: "request", Method: req.Method, Path: req.URL.Path}
		if len(body) > 0 {
			if r.Body = body; !json.Valid(body) {
				r.Body, _ = json.Marshal(string(body)) // a string always marshals
			}
		}
		status, answer := http.StatusOK, "{}"
		if req.Method == http.MethodPost {
			r.ID = textfmt.NewUUID()
			status, answer = http.StatusCreated, `{"id":"`+r.ID+`"}`
		}
		if err := rec.write(req.Context(), r); err != nil {
			return nil, err
		}
		return &http.Response{
			Status: strconv.Itoa(status) + " " + http.StatusText(status), StatusCode: status, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(answer)),
			ContentLength: int64(len(answer)), Request: req,
		}, nil
	})
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// shadowed returns a with rec suppressing the writes of shadow mode: the posts and uploads of every chat platform,
// and the requests of every tool source but its reads. Platform tools only read and keep the plain transport, since
// some of their reads are POST requests.
func (a adapters) shadowed(rec *recorder) adapters {
	a.platforms = maps.Clone(a.platforms)
	for name, p := range a.platforms {
		open := p.open
		p.open = func(s *config.Settings) (chat.Platform, error) {
			c, err := open(s)
			if err != nil {
				return nil, err
			}
			return rec.platform(c), nil
		}
		a.platforms[name] = p
	}
	a.tools = slices.Clone(a.tools)
	for i := range a.tools {
		a.tools[i].transport = rec.transport(a.tools[i].transport)
	}
	return a
}
