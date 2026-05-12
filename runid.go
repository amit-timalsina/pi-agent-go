package agent

import "context"

// runIDCtxKey is the typed key under which the agent loop stashes the
// active RunID on every ctx it hands to a hook or tool Handler. The
// unexported type prevents accidental collisions with other packages
// that also stuff values into the same ctx.
type runIDCtxKey struct{}

// WithRunID returns a derived ctx carrying the given RunID. The agent
// loop calls this internally before invoking BeforeToolCall,
// AfterToolCall, OnSteering, TransformContext, and the tool Handler,
// so callers normally don't need to call it themselves. Exported for
// test scaffolding and for consumers building their own RunContext-
// aware infrastructure on top of pi-agent-go.
//
// Returns ctx unchanged when runID is empty — there's no value in
// stashing an empty string.
func WithRunID(ctx context.Context, runID string) context.Context {
	if runID == "" {
		return ctx
	}
	return context.WithValue(ctx, runIDCtxKey{}, runID)
}

// RunIDFromContext extracts the active RunID from a ctx the agent
// loop passed into a hook or Handler. Returns "" if no RunID has been
// attached (e.g. the ctx came from outside the loop).
//
// The intended use is span correlation: a tool Handler attaching its
// own OpenTelemetry span as a child of the run-level span, without
// having to thread the RunID through tool arguments.
//
//	func myHandler(ctx context.Context, args json.RawMessage) (agent.Result, error) {
//	    runID := agent.RunIDFromContext(ctx)
//	    ctx, span := tracer.Start(ctx, "my_tool",
//	        trace.WithAttributes(attribute.String("pi.run_id", runID)))
//	    defer span.End()
//	    // ... do work ...
//	}
func RunIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(runIDCtxKey{}).(string)
	return v
}
