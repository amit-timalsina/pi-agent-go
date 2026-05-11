package agent

import (
	"context"
	"encoding/json"
	"fmt"

	llm "github.com/amit-timalsina/pi-llm-go"
)

// fetchToolResultName is the name the built-in fetch meta-tool registers
// under. Reserved when Config.EnableFetchToolResult is true; collisions
// with caller-supplied tools error at New().
const fetchToolResultName = "fetch_tool_result"

// fetchToolResultSchema is the JSON Schema the model sees. call_index is
// 1-based to match the way models count things; field_path is opaque to
// pi-agent-go (the PayloadResolver decides its dialect).
var fetchToolResultSchema = json.RawMessage(`{
    "type": "object",
    "properties": {
        "call_index": {
            "type": "integer",
            "minimum": 1,
            "description": "1-based index of the prior tool call in this run whose full payload to retrieve. Count tool calls from the start of the run."
        },
        "field_path": {
            "type": "string",
            "description": "Optional. Passed verbatim to the payload backend; format is backend-specific (e.g. JSONPath for JSON payloads, ignored for image payloads). Omit to retrieve the entire payload."
        }
    },
    "required": ["call_index"],
    "additionalProperties": false
}`)

const fetchToolResultDescription = "Retrieve the full payload of a prior tool call by its 1-based index. " +
	"Use when the Summary you saw earlier was insufficient for the decision you need to make. " +
	"Returns the full payload (or a sub-tree if field_path is supplied) as text. " +
	"Errors if the call_index is out of range, the prior tool had no FullPayloadRef, or the payload backend rejects the lookup."

type fetchArgs struct {
	CallIndex int    `json:"call_index"`
	FieldPath string `json:"field_path,omitempty"`
}

// newFetchToolResultTool returns the built-in fetch meta-tool with a
// handler closed over the agent. Called at New() time when
// Config.EnableFetchToolResult is set.
func newFetchToolResultTool(a *Agent) AgentTool {
	return AgentTool{
		Tool: llm.Tool{
			Name:        fetchToolResultName,
			Description: fetchToolResultDescription,
			InputSchema: fetchToolResultSchema,
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (Result, error) {
			var args fetchArgs
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &args); err != nil {
					return Result{}, fmt.Errorf("fetch_tool_result: invalid arguments: %w", err)
				}
			}
			if args.CallIndex < 1 {
				return Result{}, fmt.Errorf("fetch_tool_result: call_index must be >= 1 (got %d)", args.CallIndex)
			}

			// Snapshot the toolLog under the read lock. The fetch tool
			// inspects only PRIOR calls — its own log entry is appended
			// after the handler returns, so we never resolve "myself."
			a.mu.RLock()
			log := make([]ToolLogEntry, len(a.toolLog))
			copy(log, a.toolLog)
			a.mu.RUnlock()

			// Filter to entries that have a FullPayloadRef. The
			// call_index refers to position in this filtered list
			// (matches the description "tool calls" — fetch itself
			// excluded, and calls without payloads are invisible to the
			// model anyway since they have nothing to fetch).
			var withPayloads []ToolLogEntry
			for _, e := range log {
				if e.Name == fetchToolResultName {
					continue
				}
				if e.FullPayloadRef != nil {
					withPayloads = append(withPayloads, e)
				}
			}
			if args.CallIndex > len(withPayloads) {
				return Result{}, fmt.Errorf("fetch_tool_result: call_index %d out of range (only %d prior tool calls have full payloads)",
					args.CallIndex, len(withPayloads))
			}
			entry := withPayloads[args.CallIndex-1]

			payload, err := a.cfg.PayloadResolver.Resolve(ctx, *entry.FullPayloadRef, args.FieldPath)
			if err != nil {
				return Result{}, fmt.Errorf("fetch_tool_result: resolve %s/%s: %w",
					entry.FullPayloadRef.Backend, entry.FullPayloadRef.Key, err)
			}
			return Result{Summary: payload}, nil
		},
		// Allow large summaries — the whole point of this tool is to surface
		// big payloads. Default to ~256 KiB so a single fetch can return
		// most reasonable structured outputs. Tool authors can override at
		// the consumer level if they want tighter or looser bounds.
		MaxSummarySize: 256 * 1024,
	}
}

