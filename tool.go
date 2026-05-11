package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/invopop/jsonschema"

	llm "github.com/amit-timalsina/pi-llm-go"
)

// Result is the text-bearing output of a tool handler. Content is what
// the model sees as the tool's response. v1 supports text only; when
// pi-llm-go widens ToolResultBlock to carry rich content, Result will
// gain a Blocks []llm.Block field.
type Result struct {
	Content string
}

// AgentTool is a tool with an executable handler. Embeds llm.Tool so the
// declared schema can be forwarded to the LLM unchanged; adds a Handler
// that the agent loop dispatches when the model issues a matching call.
type AgentTool struct {
	llm.Tool
	Handler func(ctx context.Context, args json.RawMessage) (Result, error)
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
// serialize converts O to the text fed back to the model. For JSON
// output use json.Marshal-and-stringify; for human-readable use
// fmt.Sprintf or any custom format.
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
			return Result{Content: serialize(out)}, nil
		},
	}
}
