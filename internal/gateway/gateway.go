// Package gateway holds the Mattermost websocket connection and forwards posted events.
package gateway

import (
	"context"
	"encoding/json"
)

// Sink receives posted events in arrival order. The dispatcher implements it.
type Sink interface {
	// Enqueue blocks while the receiver's queue is full and returns ctx.Err() if ctx ends first. It never drops an
	// event. The receiver takes ownership of event; the caller must not reuse its backing array.
	Enqueue(ctx context.Context, event json.RawMessage) error
}
