package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/invopop/jsonschema"

	llm "github.com/amit-timalsina/pi-llm-go"
)

// DefaultMaxSummarySize is the default budget for tool Result.Summary text.
// 32 KiB is wide enough for the multi-paragraph reasoning traces, stack
// traces, code reviews, and structured-data summaries that legitimately
// belong in context, while still catching the "I accidentally returned a
// 100k JSON payload" mistake at execution time.
//
// Per-tool override: AgentTool.MaxSummarySize.
// Per-agent override: Config.DefaultMaxSummarySize.
const DefaultMaxSummarySize = 32 * 1024

// Result is what a tool handler returns. Summary is the text the model
// sees as the tool's response; FullPayloadHint, when non-empty, is an
// opaque pointer (file path, URL, storage key) the tool can surface for
// observability and for the model to retrieve via a separately-registered
// retrieval tool (e.g. a read_file or http_get tool the caller wires up).
//
// Summary must satisfy len(Summary) <= the tool's effective
// MaxSummarySize at execution time. The agent loop enforces this and
// returns a clear error if exceeded — caller's bug, surfaced loudly.
//
// Trivially-small tool outputs leave FullPayloadHint empty; Summary IS the
// full output. Large structured outputs write the full payload somewhere
// retrievable (tempfile, S3, durable store) and set FullPayloadHint to
// the locator, with a bounded summary in Summary. Mario's pi-mono uses
// this pattern with `/tmp/bash-<id>.log` paths retrieved via the existing
// Read tool — no framework abstraction needed.
type Result struct {
	// Summary is the bounded text fed back to the model as the tool's
	// response. Must be <= the tool's effective MaxSummarySize.
	Summary string

	// FullPayloadHint, when non-empty, is an opaque caller-defined string
	// surfacing where the tool's full output lives (a tempfile path, S3
	// URL, Postgres row id, etc.). The agent does NOT interpret it; it
	// just surfaces the value on EventToolEnd.FullPayloadHint and
	// ToolLogEntry.FullPayloadHint. The model retrieves the content via
	// whatever existing tool the caller registered for that purpose.
	FullPayloadHint string

	// Content is the deprecated v0.1.x alias for Summary. When set and
	// Summary is empty, the agent loop uses Content. When both are set,
	// Summary wins.
	//
	// Deprecated: use Summary instead. Removed in v0.4.0; the migration
	// is a single sed: `s/Result{Content:/Result{Summary:/g`.
	Content string
}

// effectiveSummary returns the text the model should see. Implements the
// Summary-wins-over-Content rule for the v0.1.x → v0.2.x migration window.
func (r Result) effectiveSummary() string {
	if r.Summary != "" {
		return r.Summary
	}
	return r.Content
}

// AgentTool is a tool with an executable handler. Embeds llm.Tool so the
// declared schema can be forwarded to the LLM unchanged; adds a Handler
// that the agent loop dispatches when the model issues a matching call.
type AgentTool struct {
	llm.Tool
	Handler func(ctx context.Context, args json.RawMessage) (Result, error)

	// MaxSummarySize is the per-tool budget for Result.Summary length in
	// bytes. 0 means "use Config.DefaultMaxSummarySize" (which itself
	// defaults to DefaultMaxSummarySize). A positive value overrides; the
	// agent loop enforces this at execution time and surfaces an error
	// if the tool's Summary exceeds it.
	MaxSummarySize int
}

// Raw constructs an AgentTool from a hand-written JSON Schema and a
// handler that receives the raw argument bytes. Use when the schema is
// dynamic or when reflection-derived schema isn't sufficient.
func Raw(name, description string, schema json.RawMessage, handler func(ctx context.Context, args json.RawMessage) (Result, error)) AgentTool {
	return AgentTool{
		Tool: llm.Tool{
			Name:        name,
			Description: description,
			InputSchema: schema,
		},
		Handler: handler,
	}
}

// Typed constructs an AgentTool whose input is a Go struct I and whose
// output is a Go value O. The InputSchema is derived from I via
// reflection (github.com/invopop/jsonschema); arguments emitted by the
// model are JSON-decoded into I before the handler runs.
//
// serialize converts O to the text fed back to the model as
// Result.Summary. For JSON output use json.Marshal-and-stringify; for
// human-readable use fmt.Sprintf or any custom format.
//
// Panics during construction if the schema cannot be reflected. Schema
// reflection should always succeed for normal Go types, so panicking
// during construction (rather than failing at runtime) is the right
// trade-off — tool registration is at program start, not per-call.
func Typed[I any, O any](
	name, description string,
	handler func(ctx context.Context, in I) (O, error),
	serialize func(O) string,
) AgentTool {
	reflector := &jsonschema.Reflector{
		Anonymous:                 true,
		AllowAdditionalProperties: false,
		DoNotReference:            true,
	}
	var zero I
	schemaDoc := reflector.Reflect(zero)
	schemaBytes, err := json.Marshal(schemaDoc)
	if err != nil {
		panic(fmt.Sprintf("agent.Typed(%s): cannot marshal schema: %v", name, err))
	}

	return AgentTool{
		Tool: llm.Tool{
			Name:        name,
			Description: description,
			InputSchema: schemaBytes,
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (Result, error) {
			var in I
			// An empty Arguments value means "no args"; tolerate as zero I.
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &in); err != nil {
					return Result{}, fmt.Errorf("%s: invalid arguments: %w", name, err)
				}
			}
			out, err := handler(ctx, in)
			if err != nil {
				return Result{}, err
			}
			return Result{Summary: serialize(out)}, nil
		},
	}
}
