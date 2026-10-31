package tool

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sarpaydemir/dost/internal/clock"
)

const attempts = 3

// Retry runs call until it succeeds, at most three times, waiting 0.5 s and then 1 s on clk between attempts. It
// logs each failure and returns the last one, or the failure before an end of ctx cut a wait short.
func Retry(ctx context.Context, clk clock.Clock, log *slog.Logger, name string, call func() (string, error)) (string, error) {
	wait := 500 * time.Millisecond
	for n := 1; ; n++ {
		out, err := call()
		if err == nil {
			return out, nil
		}
		log.Warn("tool call failed", "tool", name, "attempt", fmt.Sprintf("%d/%d", n, attempts), "error", err)
		if n == attempts {
			log.Error("tool call failed on every attempt", "tool", name, "attempts", attempts)
			return "", err
		}
		if clock.Sleep(ctx, clk, wait) != nil {
			return "", err
		}
		wait *= 2
	}
}
