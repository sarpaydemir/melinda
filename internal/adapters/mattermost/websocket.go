package mattermost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/sarpaydemir/melinda/internal/port/chat"
)

const (
	openTimeout  = 10 * time.Second
	pingInterval = 20 * time.Second
	pongTimeout  = 20 * time.Second
	maxFrame     = 8 << 20
)

var errPongTimeout = fmt.Errorf("websocket keepalive: no pong within %v", pongTimeout)

// wsURL is the websocket address of the server at endpoint.
func wsURL(endpoint string) string {
	u := strings.ReplaceAll(strings.TrimRight(endpoint, "/"), "http://", "ws://")
	return strings.ReplaceAll(u, "https://", "wss://") + "/api/v4/websocket"
}

// Listen runs one websocket session: it authenticates with the bot's token and passes every posted event, as the
// frame the server sent, to deliver.
func (p *platform) Listen(ctx context.Context, connected func(), deliver chat.Deliver) error {
	conn, err := p.dial(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	session, end := context.WithCancelCause(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { end(p.read(ctx, conn, connected, deliver)) })
	wg.Go(func() { end(p.keepalive(session, conn)) })
	<-session.Done()
	if ctx.Err() != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
	_ = conn.CloseNow()
	wg.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return context.Cause(session)
}

// dial connects without credentials and sends the authentication challenge.
func (p *platform) dial(ctx context.Context) (*websocket.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, p.wsURL, &websocket.DialOptions{HTTPClient: &http.Client{Transport: p.transport}})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(maxFrame)
	challenge, err := json.Marshal(struct {
		Seq    int               `json:"seq"`
		Action string            `json:"action"`
		Data   map[string]string `json:"data"`
	}{1, "authentication_challenge", map[string]string{"token": p.client.token}})
	if err == nil {
		err = conn.Write(ctx, websocket.MessageText, challenge)
	}
	if err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	return conn, nil
}

// read passes posted events to deliver until the connection fails, the challenge is refused, or deliver fails.
func (p *platform) read(ctx context.Context, conn *websocket.Conn, connected func(), deliver chat.Deliver) error {
	for {
		// Cancelling a read drops the connection, so reads ignore ctx and Listen closes the connection instead.
		_, frame, err := conn.Read(context.WithoutCancel(ctx))
		if err != nil {
			return err
		}
		var f struct {
			SeqReply any `json:"seq_reply"`
			Status   any `json:"status"`
			Event    any `json:"event"`
		}
		if err := json.Unmarshal(frame, &f); err != nil {
			p.log.Warn("websocket frame skipped", "err", err, "bytes", len(frame))
			continue
		}
		switch {
		case f.SeqReply == 1.0 && f.Status == "OK":
			connected()
		case f.SeqReply == 1.0:
			return fmt.Errorf("websocket authentication failed: %v", f.Status)
		case f.Event == "posted":
			if err := deliver(ctx, frame); err != nil {
				return err
			}
		}
	}
}

// keepalive pings every pingInterval and fails when a pong takes longer than pongTimeout.
func (p *platform) keepalive(ctx context.Context, conn *websocket.Conn) error {
	timer := p.clock.NewTimer(pingInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C():
		}
		timer.Reset(pongTimeout)
		pingCtx, cancel := context.WithCancel(ctx)
		pong := make(chan error, 1)
		go func() { pong <- conn.Ping(pingCtx) }()
		select {
		case err := <-pong:
			cancel()
			if err != nil {
				return err
			}
			timer.Reset(pingInterval)
		case <-timer.C():
			cancel()
			<-pong
			return errPongTimeout
		}
	}
}
