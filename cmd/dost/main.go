// Command dost runs a self-hosted AI agent for Mattermost.
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
	"syscall"

	"github.com/sarpaydemir/dost/internal/app"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/logging"
)

// Build metadata, set with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `usage: dost <command>

commands:
  run                  run the agent until SIGINT or SIGTERM
  config check [--db]  validate the environment and settings; --db also verifies the database schema
  version              print the version
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
		fmt.Fprintf(stdout, "dost %s (commit %s, built %s, %s)\n", version, commit, date, runtime.Version())
		return 0
	case is("-h"), is("--help"), is("help"):
		fmt.Fprint(stdout, usage)
		return 0
	case is("run"):
		env, secrets, log, err := setup(environ, stderr)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		err = app.Run(ctx, env, version, secrets, log)
		if err != nil {
			log.Error("dost stopped", "err", err)
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
