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
// sees as the tool's response; FullPayloadRef, when non-nil, points to
// durable storage holding the full output for retrieval via the built-in
// fetch_tool_result meta-tool (see Config.EnableFetchToolResult).
//
// Summary must satisfy len(Summary) <= the tool's effective
// MaxSummarySize at execution time. The agent loop enforces this and
// returns a clear error if exceeded — caller's bug, surfaced loudly.
//
// Trivially-small tool outputs leave FullPayloadRef nil; Summary IS the
// full output. Large structured outputs (correlation matrices, frame
// analyses, multi-trajectory data) set FullPayloadRef and put a bounded
// summary in Summary.
type Result struct {
	// Summary is the bounded text fed back to the model as the tool's
	// response. Must be <= the tool's effective MaxSummarySize.
	Summary string

	// FullPayloadRef, when non-nil, points to durable storage holding the
	// full tool output. NOT injected into model context. Retrievable via
	// the built-in fetch_tool_result meta-tool when the agent has
	// Config.EnableFetchToolResult + Config.PayloadResolver set.
	FullPayloadRef *PayloadRef

	// Content is the deprecated v0.1.x alias for Summary. When set and
	// Summary is empty, the agent loop uses Content. When both are set,
	// Summary wins.
	//
	// Deprecated: use Summary instead. Removed in v0.3.0; the migration
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

// PayloadRef points at durably-stored full tool output. Caller-supplied
// values flow opaquely through the agent loop to the PayloadResolver,
// which interprets them.
type PayloadRef struct {
	// Backend is a free-form string the PayloadResolver dispatches on
	// (e.g. "postgres", "minio", "filesystem", "memory"). pi-agent-go
	// does not interpret it.
	Backend string

	// Key is the backend-specific lookup key (a Postgres row id, a MinIO
	// object key, a filesystem path, etc.).
	Key string

	// Size is the best-effort byte size of the full payload. 0 means
	// unknown — some backends can't cheaply compute size without a HEAD.
	Size int64

	// MimeType describes the payload's content type. Used by review UIs
	// and by Resolver implementations that vary their behavior by type
	// (e.g. JSON gets field_path slicing; binary gets returned whole).
	MimeType string
}

// PayloadResolver fetches the full payload (or a sub-tree of it) from
// durable storage when the model invokes the fetch_tool_result meta-tool.
// Backends are caller-owned — pi-agent-go is storage-agnostic.
//
// fieldPath is passed verbatim to the Resolver; pi-agent-go does NOT
// parse it. The Resolver decides the dialect (JSONPath, JMESPath,
// dotted, ignored entirely). Document the per-consumer format
// expectation in the Resolver's docs.
//
// On error (payload not found, TTL expired, network failure), return a
// non-nil error. The agent loop surfaces it via the standard tool-error
// path: ToolResultBlock{IsError: true, Content: err.Error()}. The model
// sees the failure and adapts.
type PayloadResolver interface {
	Resolve(ctx context.Context, ref PayloadRef, fieldPath string) (string, error)
}

// MemoryPayloadResolver is an in-memory PayloadResolver suitable for
// tests and trivial demos. Maps {Backend: "memory" (or empty), Key: "..."}
// to a stored string. Not concurrency-safe; protect externally if shared.
type MemoryPayloadResolver struct {
	Payloads map[string]string // keyed by PayloadRef.Key
}

// Resolve returns the stored payload for the given key. fieldPath is
// IGNORED in this implementation — extend with your own logic if you
// want path slicing.
func (m *MemoryPayloadResolver) Resolve(_ context.Context, ref PayloadRef, _ string) (string, error) {
	if m.Payloads == nil {
		return "", fmt.Errorf("memory resolver: no payloads stored")
	}
	if ref.Backend != "" && ref.Backend != "memory" {
		return "", fmt.Errorf("memory resolver: cannot resolve backend %q", ref.Backend)
	}
	v, ok := m.Payloads[ref.Key]
	if !ok {
		return "", fmt.Errorf("memory resolver: key %q not found", ref.Key)
	}
	return v, nil
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
