// Package routing parses posted events into routable posts, decides whether the agent responds, and defines the work handed from dispatch to the pipeline.
package routing

import "encoding/json"

// Job is one flushed conversation for the pipeline to answer.
type Job struct {
	// Key is the conversation key the posts were buffered under.
	Key string
	// Generation is the taken buffer's generation; it identifies the job's in-flight record.
	Generation string
	// Payload is the first buffered event, with data.post carrying the combined message and file IDs.
	Payload json.RawMessage
	// PostIDs lists the buffered posts in append order.
	PostIDs []string
}
