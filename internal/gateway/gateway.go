package gateway

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/chat"
)

const (
	firstDelay = time.Second
	maxDelay   = time.Minute
	// stable is how long a session must stay connected for the next reconnect to wait firstDelay again.
	stable = 30 * time.Second
)

var errNotConnected = errors.New("not connected")

type settingsSource interface {
	Current(ctx context.Context) *config.Settings
}

// Gateway runs one intake session at a time and starts the next after a backoff delay.
type Gateway struct {
	settings settingsSource
	open     func(*config.Settings) (chat.Platform, error)
	deliver  chat.Deliver
	recovery func()
	clock    clock.Clock
	log      *slog.Logger
	ready    atomic.Bool
}

// New returns a gateway that opens the chat platform from the current settings for each session, passes inbound
// events to deliver, and calls recovery, which must not block, each time a session connects.
func New(settings settingsSource, open func(*config.Settings) (chat.Platform, error), deliver chat.Deliver,
	recovery func(), clk clock.Clock, log *slog.Logger) *Gateway {
	return &Gateway{settings: settings, open: open, deliver: deliver, recovery: recovery, clock: clk, log: log}
}

// Name identifies the component.
func (g *Gateway) Name() string { return "gateway" }

// Ready reports whether a session is connected.
func (g *Gateway) Ready() error {
	if g.ready.Load() {
		return nil
	}
	return errNotConnected
}

// Run keeps a session open until ctx is cancelled and returns nil once that session has ended.
func (g *Gateway) Run(ctx context.Context) error {
	delay := firstDelay
	for {
		connected, err := g.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if connected >= stable {
			delay = firstDelay
		}
		g.log.Warn("disconnected", "err", err, "retry_in", delay)
		if clock.Sleep(ctx, g.clock, delay) != nil {
			return nil
		}
		delay = min(2*delay, maxDelay)
	}
}

// session runs one intake session and returns how long it stayed connected.
func (g *Gateway) session(ctx context.Context) (time.Duration, error) {
	p, err := g.open(g.settings.Current(ctx))
	if err != nil {
		return 0, err
	}
	g.log.Info("connecting")
	var since time.Time
	err = p.Listen(ctx, func() {
		since = g.clock.Now()
		g.ready.Store(true)
		g.log.Info("connected")
		g.recovery()
	}, g.deliver)
	g.ready.Store(false)
	if since.IsZero() {
		return 0, err
	}
	return g.clock.Now().Sub(since), err
}
