package agent

import "context"

// deltaEmitterKey is the unexported context key under which the agent
// loop stows a tool-call-scoped delta emitter for the currently
// executing Handler.
type deltaEmitterKey struct{}

// EmitToolDelta surfaces an incremental progress fragment from inside a
// tool Handler. The agent emits an EventToolDelta on the run's event
// stream so observers (UIs, log shippers, telemetry) can react to
// partial progress without the model seeing the deltas — only the
// final Result.Summary feeds back into the conversation.
//
// Returns true when an emitter was installed by the agent loop (the
// normal case inside a Handler). Returns false when called outside a
// running tool context (e.g. from setup code or a test fixture) — the
// caller can treat false as "no observer wired" and skip the work that
// produced the delta.
//
// Safe under both sequential and parallel ToolExecution modes. Under
// parallel execution, the emitter uses non-blocking sends and may drop
// deltas if the run's consumer falls behind — the contract is "best
// effort observability, never block the Handler." See EventToolDelta
// godoc for the full ordering + drop-on-overflow semantics.
func EmitToolDelta(ctx context.Context, delta string) bool {
	em, ok := ctx.Value(deltaEmitterKey{}).(func(string))
	if !ok {
		return false
	}
	em(delta)
	return true
}

// withDeltaEmitter installs a per-tool-call delta emitter into ctx for
// the agent loop to pass to a Handler. The emitter is a closure
// capturing the tool's call ID + name + the run's yield path so the
// Handler doesn't need to know the wiring.
func withDeltaEmitter(ctx context.Context, em func(string)) context.Context {
	return context.WithValue(ctx, deltaEmitterKey{}, em)
}
