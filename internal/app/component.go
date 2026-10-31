// Package app wires the agent's components together and runs them under a supervisor.
package app

import "context"

// Component is a long-running part of the process.
type Component interface {
	// Name identifies the component in DOST_COMPONENTS, logs, and readiness reports.
	Name() string
	// Run works until ctx is cancelled, then finishes or persists its in-flight work and returns nil
	// (context.Canceled is accepted as nil). Returning earlier, or with another error, is a failure.
	Run(ctx context.Context) error
}

// Starter is implemented by components whose startup work must finish before later components start.
type Starter interface {
	// Start must return promptly once ctx is cancelled; the shutdown timeout does not bound it. After Start returns
	// nil, Run is always called, even if shutdown has begun, so Run can release what Start acquired. After Start
	// returns an error, Run is not called and Start must have released everything itself.
	Start(ctx context.Context) error
}

// Readier is implemented by components that can be running but unable to work.
type Readier interface {
	// Ready returns nil when the component is ready, or the reason it is not. It is called concurrently with Run.
	Ready() error
}

// ComponentNames returns the names accepted in DOST_COMPONENTS in start order, consumers before producers.
func ComponentNames() []string {
	return []string{"kb-learner", "pipeline", "flusher", "dispatcher", "recovery", "gateway"}
}
