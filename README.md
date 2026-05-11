# pi-agent-go

A minimal single-loop agent on top of [`pi-llm-go`](https://github.com/amit-timalsina/pi-llm-go): input → optional tool calls → response → repeat until done. Tool registry with typed handlers, three hooks (`BeforeToolCall` / `AfterToolCall` / `OnSteering`), and a buffered steering channel for mid-run injection.

> Status: **v0.x — pre-1.0**. API may change between minor versions; see [CHANGELOG.md](CHANGELOG.md).

## Why

If you want an agent harness that's small enough to read in one sitting, this is it. ~1kLoC of plain Go that gives you: a loop, a tool registry, three hooks, a steering channel, and a `RunSnapshot` for observability. No multi-agent orchestration, no compaction, no session persistence, no custom-message-type extension points — those belong in your application layer, not the agent core.

## Installation

```bash
go get github.com/amit-timalsina/pi-agent-go
```

Requires Go 1.23 or later (for `iter.Seq2`).

## Quickstart

```go
package main

import (
    "context"
    "fmt"
    "os"

    llm "github.com/amit-timalsina/pi-llm-go"
    "github.com/amit-timalsina/pi-llm-go/providers/anthropic"
    agent "github.com/amit-timalsina/pi-agent-go"
)

type AddArgs struct {
    A int `json:"a" jsonschema:"description=first addend"`
    B int `json:"b" jsonschema:"description=second addend"`
}

func main() {
    p, _ := anthropic.New(anthropic.Options{APIKey: os.Getenv("ANTHROPIC_API_KEY")})

    addTool := agent.Typed[AddArgs, int](
        "add",
        "Add two integers.",
        func(ctx context.Context, in AddArgs) (int, error) { return in.A + in.B, nil },
        func(sum int) string { return fmt.Sprintf("%d", sum) },
    )

    a, _ := agent.New(agent.Config{
        LLM:          p,
        Model:        anthropic.ClaudeSonnet4_6,
        SystemPrompt: "You are a calculator.",
        Tools:        []agent.AgentTool{addTool},
        MaxTokens:    1024,
    })

    for event, err := range a.Run(context.Background(), "what is 137+84?") {
        if err != nil { panic(err) }
        if e, ok := event.(agent.EventLLMStream); ok {
            if d, ok := e.Event.(llm.EventTextDelta); ok {
                fmt.Print(d.Delta)
            }
        }
    }
}
```

## Features

- **Single-loop agent.** Input → optional tool calls → response → repeat. Terminates when the model emits an assistant turn with no tool calls (or `MaxIterations` is hit).
- **Iterator-based events.** `Run` returns `iter.Seq2[AgentEvent, error]`. Type-switch on the concrete events for the granularity you care about. Token-level via `EventLLMStream`, message-level via `EventAssistantMessage`, tool-level via `EventToolStart`/`EventToolEnd`.
- **Typed tools.** `Typed[I, O](name, desc, handler, serialize)` derives the JSON Schema from `I` and decodes raw arguments into the typed input. `Raw(...)` for when you need to ship a hand-written schema.
- **Three hooks.** `BeforeToolCall` (skip with custom error result), `AfterToolCall` (override result), `OnSteering` (drop or rewrite injected messages). Synchronous, error-returning.
- **Steering channel.** `Steer(ctx, msg)` injects a user message; drained at the next iteration boundary. Buffered (capacity 16).
- **Snapshot.** `Snapshot()` returns an immutable view of state for cross-goroutine observation.
- **Production-friendly errors.** Hook errors abort the run; tool errors flow back to the model as `ToolResultBlock{IsError: true}` (the model can recover).

## Hooks

```go
agent.New(agent.Config{
    BeforeToolCall: func(ctx context.Context, rc agent.RunContext, call agent.ToolCallInfo) (skip bool, errorResult string, err error) {
        if call.Name == "bash" { return true, "bash is disabled in this mode", nil }
        return false, "", nil
    },
    AfterToolCall: func(ctx context.Context, rc agent.RunContext, call agent.ToolCallInfo, r agent.Result, isErr bool) (*agent.Result, error) {
        return &agent.Result{Content: redact(r.Content)}, nil
    },
    OnSteering: func(ctx context.Context, rc agent.RunContext, msg llm.Message) (drop bool, err error) {
        if rc.Iteration > 10 { return true, nil } // refuse late steering
        return false, nil
    },
})
```

## Steering

```go
// From another goroutine while a Run is in progress:
agent.Steer(ctx, llm.Message{
    Role:    llm.RoleUser,
    Content: []llm.Block{llm.TextBlock{Text: "actually, do X instead"}},
})
```

The steering message is appended to the transcript before the next LLM call, between iterations. Steering does not interrupt the current LLM call mid-stream — that semantic is reserved for a future major version.

## Example

`examples/hello_agent` is a runnable demo against the real Anthropic API:

```bash
export ANTHROPIC_API_KEY=...
go run ./examples/hello_agent
```

It registers a `get_current_time` tool and asks the model what time it is in two timezones — exercising multi-call iterations.

## Versioning

Pre-1.0. Anything can change between minor versions; refer to [CHANGELOG.md](CHANGELOG.md).

v1.0 lands once the agent has driven real production work for ≥4 weeks without API churn. Post-1.0 is strict semver.

## License

MIT. See [LICENSE](LICENSE).

## Acknowledgements

Designed after [pi-agent](https://github.com/earendil-works/pi/tree/main/packages/agent) by Mario Zechner (TypeScript, MIT). The high-level loop shape and hook ordering follow the upstream's lead; the Go-native surface (iterator events, channel-based steering, minimal hook surface) is a from-scratch redesign.

Built and maintained by Amit Timalsina with Claude Code assistance — all design decisions and release calls are human-owned.
