package tool_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/port/tool"
	"github.com/sarpaydemir/melinda/internal/testkit/fakeclock"
)

// logger writes text lines without timestamps to logs.
func logger(logs *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

type result struct {
	out string
	err error
}

// retry runs tool.Retry in the background with a call that fails until its attempt number reaches succeedOn.
func retry(ctx context.Context, clk *fakeclock.Clock, logs *bytes.Buffer, succeedOn int32) (<-chan result, *atomic.Int32) {
	var attempts atomic.Int32
	done := make(chan result, 1)
	go func() {
		out, err := tool.Retry(ctx, clk, logger(logs), "lookup", func() (string, error) {
			n := attempts.Add(1)
			if n == succeedOn {
				return "found", nil
			}
			return "", fmt.Errorf("attempt %d failed", n)
		})
		done <- result{out, err}
	}()
	return done, &attempts
}

func TestRetryWaitsHalfASecondThenASecondBetweenAttempts(t *testing.T) {
	clk := fakeclock.New(time.Unix(0, 0))
	var logs bytes.Buffer
	done, attempts := retry(context.Background(), clk, &logs, 3)
	for i, wait := range []time.Duration{500 * time.Millisecond, time.Second} {
		clk.AwaitTimers(t, 1)
		clk.Advance(wait - time.Nanosecond)
		if n := attempts.Load(); n != int32(i+1) || clk.Timers() != 1 {
			t.Fatalf("before %v had passed: %d attempts, %d timers; want %d attempts and a waiting timer", wait, n, clk.Timers(), i+1)
		}
		clk.Advance(time.Nanosecond)
	}
	if r := <-done; r.out != "found" || r.err != nil {
		t.Fatalf("Retry = %q, %v; want the third attempt's result", r.out, r.err)
	}
	want := `level=WARN msg="tool call failed" tool=lookup attempt=1/3 error="attempt 1 failed"` + "\n" +
		`level=WARN msg="tool call failed" tool=lookup attempt=2/3 error="attempt 2 failed"` + "\n"
	if logs.String() != want {
		t.Errorf("logs:\n%s\nwant:\n%s", logs.String(), want)
	}
}

func TestRetryReturnsTheLastFailureAfterThreeAttempts(t *testing.T) {
	clk := fakeclock.New(time.Unix(0, 0))
	var logs bytes.Buffer
	done, attempts := retry(context.Background(), clk, &logs, 0)
	for range 2 {
		clk.AwaitTimers(t, 1)
		clk.Advance(time.Second)
	}
	r := <-done
	if r.err == nil || r.err.Error() != "attempt 3 failed" || attempts.Load() != 3 {
		t.Fatalf("Retry = %q, %v after %d attempts; want the third failure", r.out, r.err, attempts.Load())
	}
	lines := strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n")
	want := `level=ERROR msg="tool call failed on every attempt" tool=lookup attempts=3`
	if len(lines) != 4 || !strings.Contains(lines[2], "attempt=3/3") || lines[3] != want {
		t.Errorf("logs:\n%s\nwant three failures, then %s", logs.String(), want)
	}
}

func TestRetryStopsWaitingWhenTheContextEnds(t *testing.T) {
	clk := fakeclock.New(time.Unix(0, 0))
	ctx, cancel := context.WithCancel(context.Background())
	var logs bytes.Buffer
	done, attempts := retry(ctx, clk, &logs, 0)
	clk.AwaitTimers(t, 1)
	cancel()
	select {
	case r := <-done:
		if r.err == nil || r.err.Error() != "attempt 1 failed" || attempts.Load() != 1 {
			t.Errorf("Retry = %q, %v after %d attempts; want the first failure", r.out, r.err, attempts.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Retry kept waiting after its context ended")
	}
}

func TestRetryReturnsTheFirstSuccessAtOnce(t *testing.T) {
	clk := fakeclock.New(time.Unix(0, 0))
	var logs bytes.Buffer
	done, _ := retry(context.Background(), clk, &logs, 1)
	if r := <-done; r.out != "found" || r.err != nil || logs.Len() != 0 {
		t.Errorf("Retry = %q, %v with logs %q; want the result without waiting or logging", r.out, r.err, logs.String())
	}
}
