package app

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/dispatch"
	"github.com/sarpaydemir/melinda/internal/gateway"
	"github.com/sarpaydemir/melinda/internal/kb"
	"github.com/sarpaydemir/melinda/internal/logging"
	"github.com/sarpaydemir/melinda/internal/memory"
	"github.com/sarpaydemir/melinda/internal/pipeline"
	"github.com/sarpaydemir/melinda/internal/prompt"
	"github.com/sarpaydemir/melinda/internal/routing"
	"github.com/sarpaydemir/melinda/internal/store/history"
	"github.com/sarpaydemir/melinda/internal/store/journal"
	"github.com/sarpaydemir/melinda/internal/store/kv"
	"github.com/sarpaydemir/melinda/internal/store/pg"
	"github.com/sarpaydemir/melinda/internal/store/vector"
)

// Run starts the agent that env describes and runs it until ctx is cancelled or a component fails. It returns
// before contacting the chat platform when the configuration is invalid or the database cannot be prepared.
func Run(ctx context.Context, env config.Env, version string, secrets *logging.Secrets, log *slog.Logger) error {
	clk := clock.Real()
	return run(ctx, env, version, builtin(http.DefaultTransport, clk, "melinda/"+version, env.SearXNGURL, log), clk, secrets, log)
}

// Check validates the configuration as Run does and, with db, also verifies the database schema without changing
// it. It returns a summary of the configuration with secrets masked.
func Check(ctx context.Context, env config.Env, version string, db bool, secrets *logging.Secrets, log *slog.Logger) (string, error) {
	clk := clock.Real()
	return check(ctx, env, db, builtin(http.DefaultTransport, clk, "melinda/"+version, env.SearXNGURL, log), clk, secrets, log)
}

// ExportPrompts writes the effective prompt catalog to dir, one <id>.txt file per entry: the defaults, the profile
// directory, and, when a settings source is configured, the settings' prompts overrides. Unless force is set, it
// writes nothing when any of those files exists.
func ExportPrompts(ctx context.Context, env config.Env, version, dir string, force bool, secrets *logging.Secrets, log *slog.Logger) error {
	clk := clock.Real()
	return exportPrompts(ctx, env, dir, force, builtin(http.DefaultTransport, clk, "melinda/"+version, env.SearXNGURL, log), clk, secrets, log)
}

// startup is the validated configuration that Run and Check start from.
type startup struct {
	components []string
	dsn        string
	settings   *config.Provider
	// prompts is the catalog before the settings' overrides.
	prompts *prompt.Catalog
}

// load validates the environment and loads the prompt catalog and the settings, reporting every problem at once.
func load(env config.Env, table adapters, clk clock.Clock, secrets *logging.Secrets, log *slog.Logger) (startup, error) {
	for _, name := range env.Unknown {
		log.Warn("unknown environment variable", "name", name)
	}
	components, errComponents := enable(env.Components)
	dsn, errDSN := pg.DSN(env)
	prompts, errPrompts := prompt.Load(table.prompts(), env.PromptsDir)
	settings, errSettings := config.NewProvider(env, table.secretKeys(), table.restartKeys(), validator(table), secrets, clk, log)
	return startup{components: components, dsn: dsn, settings: settings, prompts: prompts},
		errors.Join(errComponents, errDSN, errPrompts, errSettings)
}

// validator checks each settings snapshot: the selected adapters build from it and its prompts overrides apply to
// the catalog's declarations, which do not depend on the profile directory. When the declarations themselves fail
// to load, the caller's catalog load reports it and overrides are not checked.
func validator(table adapters) func(*config.Settings) error {
	defaults, _ := prompt.Load(table.prompts(), "")
	return func(s *config.Settings) error {
		err := table.validate(s)
		if defaults == nil {
			return err
		}
		_, errOverrides := defaults.With(s.Prompts)
		return errors.Join(err, errOverrides)
	}
}

func run(ctx context.Context, env config.Env, version string, table adapters, clk clock.Clock, secrets *logging.Secrets, log *slog.Logger) error {
	cfg, err := load(env, table, clk, secrets, log)
	if err != nil {
		return err
	}
	log.Info("starting", "version", version, "agent", env.AgentID, "settings", env.SettingsSource(), "components", cfg.components)
	if env.DryRun {
		records, err := shadowLog(env.DryRunLog)
		if err != nil {
			return err
		}
		defer records.Close()
		table = table.shadowed(&recorder{w: records, clk: clk})
		log.Warn("shadow mode: chat posts and uploads and tool writes are recorded, not performed",
			"records", cmp.Or(env.DryRunLog, "log output"))
	}
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
	store := kv.New(db, clk)
	identity := routing.NewIdentity(store, s.BotIdentityKey, clk, log)
	if err := identity.Verify(ctx, platform); err != nil {
		return err
	}

	sup := NewSupervisor(log, clk, env.ShutdownTimeout)
	lock := db.NewLock(env.AgentID, clk, log)
	var components []Component
	if env.HTTPAddr != "" {
		components = append(components, newHealth(env.HTTPAddr, db.Ping, sup.NotReady, env.DryRun, log.With(logging.KeyComponent, "http")))
	}
	components = append(components, conversation(ctx, sup.WorkContext(), env, cfg, sel, db, store, identity, lock, clk, log)...)
	err = sup.Run(ctx, components...)
	if !errors.Is(err, ErrShutdownTimeout) {
		// After a shutdown timeout components may still hold connections, and closing would wait for them; the
		// process exit frees them.
		lock.Release()
		db.Close()
	}
	return err
}

// conversation returns the selected conversation components in start order. Runs and other in-flight work derive
// from work. From the moment ctx ends, no pipeline run starts and the flusher takes no buffer.
func conversation(ctx, work context.Context, env config.Env, cfg startup, sel selected, db *pg.DB, store *kv.Store,
	identity *routing.Identity, lock *pg.Lock, clk clock.Clock, log *slog.Logger) []Component {
	s := cfg.settings.Current(ctx)
	named := func(name string) *slog.Logger { return log.With(logging.KeyComponent, name) }
	runs := journal.New(db, env.AgentID, clk)
	vectors := vector.New(db)
	learn := kb.NewTrigger()
	pipe := &pipeline.Pipeline{
		Settings: cfg.settings, Platform: sel.platform, Model: sel.model, Search: sel.search, Fetch: sel.fetch,
		Tools: sel.contributed, Identity: identity,
		History:   memory.NewHistory(history.New(db, clk), clk, s.MemoryKeyPrefix),
		Knowledge: vectors, Facts: vectors, Learning: kb.Batch{Store: store, Learn: learn, Log: named("pipeline")},
		Handlers: sel.handlers, Prompts: cfg.prompts, Zone: env.Timezone, Clock: clk, Log: named("pipeline"),
	}
	scheduler := pipeline.NewScheduler(ctx, work, pipe.Answer, runs, env.PipelineWorkers, env.PipelineRunTimeout, clk, named("pipeline"))
	flusher := dispatch.NewFlusher(s.BufferPrefix, store, runs, lock, scheduler, clk, named("flusher"))
	dispatcher := dispatch.NewDispatcher(work, cfg.settings, sel.platform, identity, store, flusher, clk, named("dispatcher"))
	recovery := dispatch.NewRecovery(dispatcher, store, named("recovery"))
	built := map[string]Component{
		"kb-learner": &kb.Learner{Settings: cfg.settings, Model: sel.model, Batch: store, Store: vectors,
			Prompts: cfg.prompts, Learn: learn, Work: work, Clock: clk, Log: named("kb-learner")},
		"pipeline": scheduler, "flusher": flusher, "dispatcher": dispatcher, "recovery": recovery,
		"gateway": gateway.New(cfg.settings, sel.platform, dispatcher.Enqueue, recovery.Trigger, recovery.Disconnected, clk, named("gateway")),
	}
	var out []Component
	for _, name := range cfg.components {
		if c, ok := built[name]; ok {
			out = append(out, c)
		}
	}
	return out
}

// shadowLog opens the file that shadow-mode records are appended to, or the log output when path is empty.
func shadowLog(path string) (io.WriteCloser, error) {
	if path == "" {
		return nopCloser{os.Stderr}, nil
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func check(ctx context.Context, env config.Env, verifyDB bool, table adapters, clk clock.Clock, secrets *logging.Secrets, log *slog.Logger) (string, error) {
	cfg, err := load(env, table, clk, secrets, log)
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

func exportPrompts(ctx context.Context, env config.Env, dir string, force bool, table adapters, clk clock.Clock, secrets *logging.Secrets, log *slog.Logger) error {
	prompts, err := prompt.Load(table.prompts(), env.PromptsDir)
	if err != nil {
		return err
	}
	if env.SettingsSource() != "" {
		settings, err := config.NewProvider(env, table.secretKeys(), table.restartKeys(), validator(table), secrets, clk, log)
		if err != nil {
			return err
		}
		if prompts, err = prompts.With(settings.Current(ctx).Prompts); err != nil {
			return err
		}
	}
	return prompts.Export(dir, force)
}
