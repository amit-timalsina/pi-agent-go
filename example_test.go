package agent_test

// These Example functions render on pkg.go.dev next to the documented
// types. They're compiled by `go test` but never executed (no
// // Output: lines), so they don't need API keys to verify — they're
// here so coding agents and humans land on a runnable, copy-pasteable
// snippet for every public-API entry point.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	agent "github.com/amit-timalsina/pi-agent-go"
	llm "github.com/amit-timalsina/pi-llm-go"
	"github.com/amit-timalsina/pi-llm-go/providers/anthropic"
)

// Example shows the smallest useful pi-agent-go program: a single-tool
// calculator agent backed by Anthropic Claude. Replace the provider /
// model to switch backends; replace the tool to change the task.
func Example() {
	p, _ := anthropic.New(anthropic.Options{APIKey: os.Getenv("ANTHROPIC_API_KEY")})

	type AddArgs struct {
		A int `json:"a" jsonschema:"description=first addend"`
		B int `json:"b" jsonschema:"description=second addend"`
	}
	addTool := agent.Typed[AddArgs, int](
		"add", "Add two integers.",
		func(ctx context.Context, in AddArgs) (int, error) { return in.A + in.B, nil },
		func(sum int) string { return fmt.Sprintf("%d", sum) },
	)

	a, _ := agent.New(agent.Config{
		LLM:          p,
		Model:        anthropic.ClaudeSonnet4_6,
		SystemPrompt: "You are a calculator. Use the add tool.",
		Tools:        []agent.AgentTool{addTool},
		MaxTokens:    1024,
	})

	for event, err := range a.Run(context.Background(), "what is 137+84?") {
		if err != nil {
			return
		}
		if e, ok := event.(agent.EventLLMStream); ok {
			if d, ok := e.Event.(llm.EventTextDelta); ok {
				fmt.Print(d.Delta)
			}
		}
	}
}

// ExampleTyped shows constructing a typed tool with a schema derived
// from a Go struct via reflection. Use this when the tool's input is
// a normal Go value; use agent.Raw for hand-written JSON schemas.
func ExampleTyped() {
	type WeatherArgs struct {
		City  string `json:"city" jsonschema:"description=Target city"`
		Units string `json:"units,omitempty" jsonschema:"description=metric or imperial,enum=metric,enum=imperial"`
	}
	type WeatherResult struct {
		Temp float64 `json:"temp"`
		Unit string  `json:"unit"`
	}
	tool := agent.Typed[WeatherArgs, WeatherResult](
		"get_weather",
		"Look up the current weather for a city.",
		func(ctx context.Context, in WeatherArgs) (WeatherResult, error) {
			return WeatherResult{Temp: 22.0, Unit: "C"}, nil
		},
		func(r WeatherResult) string {
			b, _ := json.Marshal(r)
			return string(b)
		},
	)
	_ = tool
}

// ExampleRaw shows a tool with a hand-written JSON schema. Use when
// schema reflection isn't sufficient — dynamic schemas, conditional
// required fields, enum values driven by runtime data.
func ExampleRaw() {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"command":{"type":"string"}},
		"required":["command"],
		"additionalProperties":false
	}`)
	tool := agent.Raw("shell", "Run a shell command.", schema,
		func(ctx context.Context, args json.RawMessage) (agent.Result, error) {
			return agent.Result{Summary: "command output here"}, nil
		},
	)
	_ = tool
}

// ExampleConfig_toolExecutionParallel shows enabling parallel tool
// execution. Source order is preserved for tool_result blocks and for
// BeforeToolCall hook invocations; EventToolEnd fires in finish order.
// Per-tool opt-out via AgentTool.ExecutionMode = ToolExecutionSequential.
func ExampleConfig_toolExecutionParallel() {
	a, _ := agent.New(agent.Config{
		LLM:           nil, // your provider
		Model:         anthropic.ClaudeSonnet4_6,
		ToolExecution: agent.ToolExecutionParallel,
		Tools:         []agent.AgentTool{ /* multiple tools — model may call several per turn */ },
	})
	_ = a
}

// ExampleEmitToolDelta shows surfacing incremental progress from a
// long-running tool handler. The model never sees deltas — only the
// Handler's final Result.Summary. Observers (UIs, log shippers, trace
// spans) receive EventToolDelta between EventToolStart and
// EventToolEnd. Drop-on-overflow under parallel execution; tune via
// Config.ToolDeltaBuffer.
func ExampleEmitToolDelta() {
	tool := agent.Raw("download", "Download a URL with progress.",
		json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`),
		func(ctx context.Context, args json.RawMessage) (agent.Result, error) {
			for i := 1; i <= 5; i++ {
				agent.EmitToolDelta(ctx, fmt.Sprintf("downloaded chunk %d/5", i))
			}
			return agent.Result{Summary: "download complete"}, nil
		},
	)
	_ = tool
}

// ExampleAgent_Steer shows mid-run steering — injecting a user
// message while the loop is running. Drained at the next iteration
// boundary; buffered (capacity 16). Use to interrupt an agent that's
// going off-track or to escalate a clarifying message.
func ExampleAgent_Steer() {
	a, _ := agent.New(agent.Config{
		LLM:   nil, // your provider
		Model: anthropic.ClaudeSonnet4_6,
	})

	go func() {
		time.Sleep(time.Second)
		_ = a.Steer(context.Background(), llm.Message{
			Role:    llm.RoleUser,
			Content: []llm.Block{llm.TextBlock{Text: "Actually, focus on the second item."}},
		})
	}()

	for event, err := range a.Run(context.Background(), "Analyze these three items...") {
		_ = event
		if err != nil {
			return
		}
	}
}

// ExampleAgent_Snapshot shows saving an agent's run state so a process
// restart can resume the conversation. Pair with agent.Restore.
func ExampleAgent_Snapshot() {
	a, _ := agent.New(agent.Config{LLM: nil, Model: anthropic.ClaudeSonnet4_6})

	// Run for a while, then snapshot
	snap := a.Snapshot()

	// Persist snap somewhere durable (JSON, Postgres, S3, ...)
	persisted, _ := json.Marshal(snap)
	_ = persisted

	// Later, after a process restart:
	var restored agent.RunSnapshot
	_ = json.Unmarshal(persisted, &restored)
	a2, _ := agent.Restore(agent.Config{LLM: nil, Model: anthropic.ClaudeSonnet4_6}, restored)
	for event, err := range a2.Run(context.Background(), "continue") {
		_ = event
		if err != nil {
			return
		}
	}
}

// ExampleConfig_hooks shows the three available hooks: BeforeToolCall
// (skip with custom result), AfterToolCall (override result),
// OnSteering (drop or rewrite injected messages).
func ExampleConfig_hooks() {
	_ = agent.Config{
		BeforeToolCall: func(ctx context.Context, rc agent.RunContext, call agent.ToolCallInfo) (skip bool, errorResult string, err error) {
			if call.Name == "shell" {
				return true, "shell is disabled in this mode", nil
			}
			return false, "", nil
		},
		AfterToolCall: func(ctx context.Context, rc agent.RunContext, call agent.ToolCallInfo, r agent.Result, isErr bool) (*agent.Result, error) {
			// Truncate or redact before the model sees it
			return &agent.Result{Summary: r.Summary, FullPayloadHint: r.FullPayloadHint}, nil
		},
		OnSteering: func(ctx context.Context, rc agent.RunContext, msg llm.Message) (drop bool, err error) {
			if rc.Iteration > 20 {
				return true, nil // refuse late steering
			}
			return false, nil
		},
	}
}

// ExampleRunIDFromContext shows correlating tool-handler work with the
// run's RunID — useful for OpenTelemetry spans and structured logs.
func ExampleRunIDFromContext() {
	tool := agent.Raw("log_event", "Log an event with run correlation.",
		json.RawMessage(`{"type":"object","properties":{"event":{"type":"string"}},"required":["event"]}`),
		func(ctx context.Context, args json.RawMessage) (agent.Result, error) {
			runID := agent.RunIDFromContext(ctx)
			fmt.Printf("run=%s event recorded\n", runID)
			return agent.Result{Summary: "logged"}, nil
		},
	)
	_ = tool
}
