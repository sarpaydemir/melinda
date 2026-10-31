// Package dispatch buffers accepted posts per conversation, flushes them when the debounce window closes, and replays posts missed while disconnected.
package dispatch

import (
	"context"
	"time"

	"github.com/sarpaydemir/dost/internal/routing"
)

// Scheduler is the pipeline's intake as the flusher uses it. The pipeline implements it.
type Scheduler interface {
	// Waiting returns the number of runs for key that were accepted and have not started. It is advisory: the
	// caller enforces any per-conversation cap, so that resubmitted runs can bypass it.
	Waiting(key string) int
	// Reserve blocks until the pipeline can accept one more run and returns ctx.Err() if ctx ends first.
	// After a successful Reserve the caller calls exactly one of submit or cancel; neither blocks, and submit
	// never rejects a job.
	Reserve(ctx context.Context) (submit func(routing.Job), cancel func(), err error)
}

// Notice tells the flusher that the buffer under Key now has Generation and is due at FlushAfter.
type Notice struct {
	Key        string
	Generation string
	FlushAfter time.Time
}
