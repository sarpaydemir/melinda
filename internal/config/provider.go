package config

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/logging"
)

// Provider serves the current settings snapshot. The first read after the snapshot is DOST_SETTINGS_TTL old
// reloads the document from its source and resolves its secret references again; a reload that fails keeps the
// last good snapshot.
type Provider struct {
	env      Env
	keys     []SecretKey
	validate func(*Settings) error
	mask     *logging.Secrets
	clock    clock.Clock
	log      *slog.Logger

	mu        sync.Mutex
	current   *Settings
	expires   time.Time
	reloading bool
	failure   string          // the reload error last logged, until a reload succeeds
	ignored   map[string]bool // fixed keys whose changes have been logged
}

// NewProvider loads the settings from the first configured source. keys are the secret keys the adapters declare,
// and validate checks the selected adapters' settings in every snapshot. The values of the secret keys are added
// to mask before each snapshot is checked.
func NewProvider(env Env, keys []SecretKey, validate func(*Settings) error, mask *logging.Secrets, clk clock.Clock, log *slog.Logger) (*Provider, error) {
	p := &Provider{
		env: env, keys: keys, validate: validate, mask: mask, clock: clk, log: log,
		ignored: map[string]bool{},
	}
	doc, err := p.read()
	if err != nil {
		return nil, err
	}
	if p.current, err = Load(doc, env.AgentID, keys, validate); err != nil {
		return nil, err
	}
	p.expires = clk.Now().Add(env.SettingsTTL)
	return p, nil
}

// Current returns the current snapshot. The first call after it expires reloads it and returns the result;
// calls made during that reload return the previous snapshot.
func (p *Provider) Current(context.Context) *Settings {
	p.mu.Lock()
	prev := p.current
	if p.reloading || p.clock.Now().Before(p.expires) {
		p.mu.Unlock()
		return prev
	}
	p.reloading = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.reloading = false
		p.expires = p.clock.Now().Add(p.env.SettingsTTL)
	}()

	next, ignored, err := p.reload(prev)

	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		if msg := err.Error(); msg != p.failure {
			p.failure = msg
			p.log.Error("settings reload failed; keeping the last good settings", "err", err)
		}
		return prev
	}
	p.failure = ""
	for _, key := range ignored {
		if !p.ignored[key] {
			p.ignored[key] = true
			p.log.Warn("settings change ignored until restart", "setting", key)
		}
	}
	p.current = next
	return next
}

func (p *Provider) reload(prev *Settings) (*Settings, []string, error) {
	doc, err := p.read()
	if err != nil {
		return nil, nil, err
	}
	return prev.Reload(doc, p.validate)
}
