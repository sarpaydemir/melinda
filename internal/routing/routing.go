// Package routing decides whether the agent responds to a message and defines the work handed from dispatch to the pipeline.
package routing

import (
	"context"
	"encoding/json"
)

// Job is one flushed conversation for the pipeline to answer.
type Job struct {
	// Key is the conversation key the messages were buffered under.
	Key string
	// Generation is the taken buffer's generation; it identifies the job's in-flight record.
	Generation string
	// Payload is the first buffered message as its platform adapter encoded it. The reply goes to its
	// conversation and thread.
	Payload json.RawMessage
	// Text is the combined text of every buffered message.
	Text string
	// AttachmentIDs is the ordered union of the buffered messages' attachments.
	AttachmentIDs []string
	// MessageIDs lists the buffered messages in append order.
	MessageIDs []string
}

type jobKey struct{}

// WithJob returns ctx carrying j, the job being answered.
func WithJob(ctx context.Context, j Job) context.Context { return context.WithValue(ctx, jobKey{}, j) }

// JobFrom returns the job ctx carries, if any.
func JobFrom(ctx context.Context) (Job, bool) {
	j, ok := ctx.Value(jobKey{}).(Job)
	return j, ok
}
