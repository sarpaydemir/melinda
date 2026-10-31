// Package app wires the agent's components together and runs them under a supervisor.
package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

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

// consumer returns the component that a producer hands its work to in process, or "" for a component that hands
// work to none.
func consumer(name string) string {
	switch name {
	case "gateway", "recovery":
		return "dispatcher"
	case "dispatcher":
		return "flusher"
	case "flusher":
		return "pipeline"
	}
	return ""
}

// enable returns the components named in DOST_COMPONENTS in start order, or all of them when names is empty. It
// reports every unknown name and every producer whose consumer is missing, following the chain to its end.
func enable(names []string) ([]string, error) {
	all := ComponentNames()
	if len(names) == 0 {
		return all, nil
	}
	var errs []error
	for _, n := range names {
		if !slices.Contains(all, n) {
			errs = append(errs, fmt.Errorf("DOST_COMPONENTS: unknown component %q (available: %s)", n, strings.Join(all, ", ")))
		}
	}
	needed := map[string]bool{}
	for _, n := range slices.Backward(all) {
		if c := consumer(n); c != "" && (needed[n] || slices.Contains(names, n)) {
			needed[c] = true
			if !slices.Contains(names, c) {
				errs = append(errs, fmt.Errorf("DOST_COMPONENTS: %s needs %s", n, c))
			}
		}
	}
	return slices.DeleteFunc(all, func(n string) bool { return !slices.Contains(names, n) }), errors.Join(errs...)
}
