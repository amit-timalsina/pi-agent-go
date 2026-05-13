package agent

import (
	"context"
	"encoding/json"

	llm "github.com/amit-timalsina/pi-llm-go"
)

// ToolCallInfo describes a pending tool invocation passed to hooks.
type ToolCallInfo struct {
	ToolCallID string
	Name       string
	Arguments  json.RawMessage
}

// BeforeToolCallHook runs after argument validation and before the tool's
// Handler executes. Returning skip=true causes the loop to skip Handler
// execution and feed the supplied errorResult (or a default "blocked"
// message) back to the model as the tool result. Returning err != nil
// aborts the run.
//
// Use to enforce policies, redact arguments, or short-circuit destructive
// tools in test mode.
type BeforeToolCallHook func(
	ctx context.Context,
	rc RunContext,
	call ToolCallInfo,
) (skip bool, errorResult string, err error)

// AfterToolCallHook runs after the tool's Handler completes (success or
// error), before the result is appended to the transcript and the
// EventToolEnd event is emitted. The hook may override the result by
// returning a non-nil pointer. Returning err != nil produces an error
// tool result for THIS tool call only — the run continues so other
// in-flight parallel calls aren't aborted mid-execution.
//
// This is asymmetric with BeforeToolCallHook, which aborts the run on
// hook error. The asymmetry is intentional: a failed Before hook runs
// PRE-execution and leaves the agent uncertain whether to skip or
// execute; aborting is safe. AfterToolCall runs POST-execution; the
// tool already produced output we can surface, so the safer default
// is "convert the hook failure to an error tool result and let the
// batch finish."
//
// Use to inject post-processing (annotation, logging) or to mask
// sensitive fields from the model.
type AfterToolCallHook func(
	ctx context.Context,
	rc RunContext,
	call ToolCallInfo,
	result Result,
	isError bool,
) (override *Result, err error)

// OnSteeringHook runs once per steering message dequeued from the
// channel, before the message is appended to the transcript. Returning
// drop=true discards the message. Returning err != nil aborts the run.
//
// Use to filter, rate-limit, or rewrite steering messages — e.g. to
// reject steering after a certain iteration or to redact unsafe content.
type OnSteeringHook func(
	ctx context.Context,
	rc RunContext,
	msg llm.Message,
) (drop bool, err error)
