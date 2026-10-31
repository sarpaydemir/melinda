package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/logging"
	"github.com/sarpaydemir/dost/internal/routing"
	"github.com/sarpaydemir/dost/internal/store/kv"
	"github.com/sarpaydemir/dost/internal/store/pg"
)

// Run starts the agent that env describes and runs it until ctx is cancelled or a component fails. It returns
// before contacting the chat platform when the configuration is invalid or the database cannot be prepared.
func Run(ctx context.Context, env config.Env, version string, secrets *logging.Secrets, log *slog.Logger) error {
	clk := clock.Real()
	return run(ctx, env, version, builtin(http.DefaultTransport, clk, "dost/"+version), clk, secrets, log)
}

// Check validates the configuration as Run does and, with db, also verifies the database schema without changing
// it. It returns a summary of the configuration with secrets masked.
func Check(ctx context.Context, env config.Env, version string, db bool, secrets *logging.Secrets, log *slog.Logger) (string, error) {
	clk := clock.Real()
	return check(ctx, env, db, builtin(http.DefaultTransport, clk, "dost/"+version), clk, secrets, log)
}

// startup is the validated configuration that Run and Check start from.
type startup struct {
	components []string
	dsn        string
	settings   *config.Provider
}

// load validates the environment and loads the settings, reporting every problem at once.
func load(ctx context.Context, env config.Env, table adapters, clk clock.Clock, secrets *logging.Secrets, log *slog.Logger) (startup, error) {
	for _, name := range env.Unknown {
		log.Warn("unknown environment variable", "name", name)
	}
	components, errComponents := enable(env.Components)
	dsn, errDSN := pg.DSN(env)
	settings, errSettings := config.NewProvider(ctx, env, table.secretKeys(), table.validate, secrets, clk, log)
	return startup{components: components, dsn: dsn, settings: settings}, errors.Join(errComponents, errDSN, errSettings)
}

func run(ctx context.Context, env config.Env, version string, table adapters, clk clock.Clock, secrets *logging.Secrets, log *slog.Logger) error {
	cfg, err := load(ctx, env, table, clk, secrets, log)
	if err != nil {
		return err
	}
	log.Info("starting", "version", version, "agent", env.AgentID, "settings", env.SettingsSource(), "components", cfg.components)
	s := cfg.settings.Current(ctx)
	sel, err := table.choose(selection(s))
	if err != nil {
		return err
	}
	platform, err := sel.platform(s)
	if err != nil {
		return err
	}
	db, err := pg.Open(ctx, cfg.dsn, clk, log)
	if err != nil {
		return err
	}
	identity := routing.NewIdentity(kv.New(db, clk), s.BotIdentityKey, clk, log)
	identity.Verify(ctx, platform)

	sup := NewSupervisor(log, clk, env.ShutdownTimeout)
	var components []Component
	if env.HTTPAddr != "" {
		components = append(components, newHealth(env.HTTPAddr, db.Ping, sup.NotReady, log.With(logging.KeyComponent, "http")))
	}
	err = sup.Run(ctx, components...)
	if !errors.Is(err, ErrShutdownTimeout) {
		// After a shutdown timeout components may still hold connections, and Close would wait for them.
		db.Close()
	}
	return err
}

func check(ctx context.Context, env config.Env, verifyDB bool, table adapters, clk clock.Clock, secrets *logging.Secrets, log *slog.Logger) (string, error) {
	cfg, err := load(ctx, env, table, clk, secrets, log)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "agent: %s\ncomponents: %s\n", env.AgentID, strings.Join(cfg.components, ", "))
	if verifyDB {
		if err := pg.Verify(ctx, cfg.dsn, clk, log); err != nil {
			return "", err
		}
		b.WriteString("database: schema verified\n")
	}
	fmt.Fprintf(&b, "settings (%s):\n", env.SettingsSource())
	_ = json.Indent(&b, cfg.settings.Current(ctx).Redacted(), "", "  ") // Redacted is valid JSON
	b.WriteByte('\n')
	return b.String(), nil
}
