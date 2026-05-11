package agent

import (
	"encoding/json"
	"time"

	llm "github.com/amit-timalsina/pi-llm-go"
)

// RunContext is the read-only view of run state passed to hooks. Hooks
// receive RunContext synchronously while the loop holds its lock; do not
// retain Messages or mutate the slice. For an immutable, async-safe
// snapshot use Agent.Snapshot().
type RunContext struct {
	RunID     string
	Iteration int
	Messages  []llm.Message
}

// RunSnapshot is an immutable point-in-time view of an Agent's state.
// Returned by Agent.Snapshot(). Safe to share across goroutines.
type RunSnapshot struct {
	RunID     string
	Iteration int
	Messages  []llm.Message
	ToolLog   []ToolLogEntry
	LastUsage llm.Usage
	IsRunning bool
}

// ToolLogEntry records one tool invocation for diagnostic / audit
// purposes. Logged for every Handler call (including blocked / errored).
type ToolLogEntry struct {
	Iteration  int
	ToolCallID string
	Name       string
	Arguments  json.RawMessage
	// Result is the bounded Summary fed back to the model — same value
	// surfaced on EventToolEnd.Result.
	Result string
	// FullPayloadHint, when non-empty, is an opaque caller-defined string
	// the tool surfaced alongside its bounded Summary — typically a file
	// path, URL, or storage key. Mirrors EventToolEnd.FullPayloadHint.
	FullPayloadHint string
	IsError         bool
	StartedAt       time.Time
	EndedAt         time.Time
}
