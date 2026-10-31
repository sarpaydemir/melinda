package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/sarpaydemir/dost/internal/app"
)

func TestCommandLine(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		environ  []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{name: "version", args: []string{"version"}, wantOut: "dost dev (commit none, built unknown, " + runtime.Version() + ")\n"},
		{name: "help", args: []string{"-h"}, wantOut: "usage: dost <command>"},
		{name: "no command", args: nil, wantCode: 2, wantErr: "usage: dost <command>"},
		{name: "unknown command", args: []string{"serve"}, wantCode: 2, wantErr: "usage:"},
		{name: "extra argument", args: []string{"run", "now"}, wantCode: 2, wantErr: "usage:"},
		{name: "unknown flag", args: []string{"config", "check", "--verbose"}, wantCode: 2, wantErr: "usage:"},
		{name: "invalid environment", args: []string{"run"}, environ: []string{"DOST_SHUTDOWN_TIMEOUT=soon"}, wantCode: 1,
			wantErr: `invalid environment: DOST_SHUTDOWN_TIMEOUT: "soon" is not a duration`},
		{name: "invalid configuration", args: []string{"config", "check", "--db"}, environ: []string{"DOST_COMPONENTS=gateway"}, wantCode: 1,
			wantErr: "DOST_COMPONENTS: gateway needs dispatcher\n"},
		{name: "startup failure", args: []string{"run"}, environ: []string{"DOST_COMPONENTS=gateway"}, wantCode: 1,
			wantErr: `msg="dost stopped" err="DOST_COMPONENTS: gateway needs dispatcher\n`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), tt.args, tt.environ, &stdout, &stderr); got != tt.wantCode {
				t.Fatalf("exit code = %d, want %d; stderr:\n%s", got, tt.wantCode, &stderr)
			}
			if !strings.Contains(stdout.String(), tt.wantOut) {
				t.Errorf("stdout = %q, want substring %q", stdout.String(), tt.wantOut)
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want substring %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

func TestExitCode(t *testing.T) {
	failed := &app.ComponentError{Name: "pipeline", Err: errors.New("boom")}
	timeout := fmt.Errorf("%w after 30s while stopping pipeline", app.ErrShutdownTimeout)
	for _, tc := range []struct {
		err  error
		want int
	}{
		{nil, 0},
		{timeout, 0},
		{errors.Join(timeout, failed), 1},
		{failed, 1},
		{errors.New("invalid settings"), 1},
		{fmt.Errorf("connect to database: %w", context.Canceled), 0},
		{&app.ComponentError{Name: "gateway", Err: context.Canceled}, 1},
	} {
		if got := exitCode(tc.err); got != tc.want {
			t.Errorf("exitCode(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestFirstSignalStopsAndSecondExits(t *testing.T) {
	signals := make(chan os.Signal)
	stopped, exited := make(chan struct{}), make(chan int, 1)
	go stopOnSignals(signals, func() { close(stopped) }, func(code int) { exited <- code })

	signals <- os.Interrupt
	<-stopped
	if len(exited) > 0 {
		t.Fatal("exited after the first signal")
	}
	signals <- os.Interrupt
	if code := <-exited; code != 1 {
		t.Errorf("exit code after the second signal = %d, want 1", code)
	}
}
