// Command melinda runs a self-hosted AI agent for Mattermost.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/sarpaydemir/melinda/internal/app"
	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/logging"
)

// Build metadata, set with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `usage: melinda <command>

commands:
  run                             run the agent until SIGINT or SIGTERM
  config check [--db]             validate the environment and settings; --db also verifies the database schema
  prompts export <dir> [--force]  write the effective prompt catalog to dir, one file per ID; --force overwrites
  health                          exit 0 when /readyz on MELINDA_HTTP_ADDR answers 200, otherwise 1
  version                         print the version
`

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go stopOnSignals(signals, cancel, os.Exit)
	os.Exit(run(ctx, os.Args[1:], os.Environ(), os.Stdout, os.Stderr))
}

// stopOnSignals calls stop at the first signal and exit(1) at the second.
func stopOnSignals(signals <-chan os.Signal, stop func(), exit func(int)) {
	<-signals
	stop()
	<-signals
	exit(1)
}

func run(ctx context.Context, args, environ []string, stdout, stderr io.Writer) int {
	is := func(command ...string) bool { return slices.Equal(args, command) }
	switch {
	case is("version"):
		fmt.Fprintf(stdout, "melinda %s (commit %s, built %s, %s)\n", version, commit, date, runtime.Version())
		return 0
	case is("-h"), is("--help"), is("help"):
		fmt.Fprint(stdout, usage)
		return 0
	case is("health"):
		env, err := config.LoadEnv(environ)
		if err == nil {
			err = checkReady(ctx, env.HTTPAddr)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case is("run"):
		env, secrets, log, err := setup(environ, stderr)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if limit := limitMemory(environ, os.DirFS("/sys/fs/cgroup")); limit > 0 {
			log.Info("GOMEMLIMIT set from the container memory limit", "bytes", limit)
		}
		err = app.Run(ctx, env, version, secrets, log)
		if err != nil {
			log.Error("melinda stopped", "err", err)
		}
		return exitCode(err)
	case is("config", "check"), is("config", "check", "--db"):
		env, secrets, log, err := setup(environ, stderr)
		if err == nil {
			var summary string
			summary, err = app.Check(ctx, env, version, len(args) == 3, secrets, log)
			fmt.Fprint(stdout, summary)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case len(args) > 2 && !strings.HasPrefix(args[2], "-") && (is("prompts", "export", args[2]) || is("prompts", "export", args[2], "--force")):
		env, secrets, log, err := setup(environ, stderr)
		if err == nil {
			err = app.ExportPrompts(ctx, env, version, args[2], len(args) == 4, secrets, log)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}

// setup reads the environment and builds the logger, which writes to w and masks the environment's secrets.
func setup(environ []string, w io.Writer) (config.Env, *logging.Secrets, *slog.Logger, error) {
	env, err := config.LoadEnv(environ)
	if err != nil {
		return env, nil, nil, err
	}
	secrets := new(logging.Secrets)
	secrets.Add(env.Secrets()...)
	return env, secrets, logging.New(w, env.LogLevel, env.LogJSON, secrets), nil
}

// exitCode is 1 when the agent failed to start or a component failed, and 0 after a requested shutdown, even one
// that interrupted startup or overran its timeout.
func exitCode(err error) int {
	var failed *app.ComponentError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &failed):
		return 1
	case errors.Is(err, app.ErrShutdownTimeout), errors.Is(err, context.Canceled):
		return 0
	}
	return 1
}
