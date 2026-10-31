package mattermost

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/chat"
)

// settings are the adapter's keys in the settings document.
type settings struct {
	Endpoint    string `json:"mattermost_endpoint"`
	Token       string `json:"mattermost_token"`
	BotID       string `json:"mattermost_bot_id"`
	BotUsername string `json:"mattermost_bot_username"`
}

// platform is Mattermost as a chat platform.
type platform struct {
	client   *Client
	identity chat.User
}

// Open builds the platform from a settings snapshot; its requests go through transport.
func Open(s *config.Settings, transport http.RoundTripper) (chat.Platform, error) {
	var c settings
	if err := s.Decode(&c); err != nil {
		return nil, err
	}
	var missing []error
	for _, k := range []struct{ key, value string }{
		{"mattermost_endpoint", c.Endpoint}, {"mattermost_token", c.Token}, {"mattermost_bot_id", c.BotID},
	} {
		if k.value == "" {
			missing = append(missing, errors.New(k.key+": required"))
		}
	}
	if len(missing) > 0 {
		return nil, errors.Join(missing...)
	}
	return &platform{
		client:   NewClient(c.Endpoint, c.Token, transport),
		identity: chat.User{ID: c.BotID, Username: c.BotUsername},
	}, nil
}

// Listen is not supported yet.
func (p *platform) Listen(context.Context, func(), chat.Deliver) error { return errors.ErrUnsupported }

// Replay is not supported yet.
func (p *platform) Replay(context.Context, time.Time, chat.Deliver) error {
	return errors.ErrUnsupported
}

func (p *platform) Identity() chat.User { return p.identity }

func (p *platform) Self(ctx context.Context) (chat.User, error) {
	u, err := p.client.GetMe(ctx)
	if err != nil {
		return chat.User{}, err
	}
	return chat.User(u), nil
}

func (p *platform) User(ctx context.Context, id string) (chat.User, error) {
	u, err := p.client.GetUser(ctx, id)
	if err != nil {
		return chat.User{}, err
	}
	return chat.User(u), nil
}

func (p *platform) Attachment(ctx context.Context, id string) (chat.Attachment, error) {
	info, err := p.client.GetFileInfo(ctx, id)
	if err != nil {
		return chat.Attachment{}, err
	}
	return chat.Attachment{Name: info.Name, Extension: info.Extension, MediaType: info.MimeType, Size: info.Size}, nil
}

func (p *platform) Download(ctx context.Context, id string, max int64) ([]byte, error) {
	return p.client.DownloadFile(ctx, id, max)
}

func (p *platform) Send(ctx context.Context, m chat.Outgoing) (string, error) {
	post, err := p.client.CreatePost(ctx, m.ConversationID, m.Text, m.ThreadID, m.FileIDs)
	if err != nil {
		return "", err
	}
	return post.ID, nil
}

func (p *platform) Upload(ctx context.Context, conversationID string, f chat.File) (string, error) {
	return p.client.UploadFile(ctx, conversationID, f)
}
