# pi-agent-go

[![CI](https://github.com/amit-timalsina/pi-agent-go/actions/workflows/ci.yml/badge.svg)](https://github.com/amit-timalsina/pi-agent-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/amit-timalsina/pi-agent-go.svg)](https://pkg.go.dev/github.com/amit-timalsina/pi-agent-go)
[![Go Report Card](https://goreportcard.com/badge/github.com/amit-timalsina/pi-agent-go)](https://goreportcard.com/report/github.com/amit-timalsina/pi-agent-go)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**Minimal Go agent framework for LLM tool-calling loops.** Single-loop agent on top of [`pi-llm-go`](https://github.com/amit-timalsina/pi-llm-go): input → optional tool calls → response → repeat until done. Typed tool handlers with reflection-derived JSON schemas, parallel tool execution, three hooks for control, streaming tool progress, mid-run steering, snapshot-based resume. Works with **Anthropic Claude**, **OpenAI** (GPT-5 family), **Google Gemini**, and any OpenAI-compatible endpoint through pi-llm-go.

> Status: **v0.6.0, pre-1.0.** API may change between minor versions; see [CHANGELOG.md](CHANGELOG.md). Used internally at [Noumenal](https://noumenalai.com).

## Install

```bash
go get github.com/amit-timalsina/pi-agent-go
```

Requires Go 1.25+ (transitively, via `golang.org/x/sync`).

## Capability matrix

| Capability | Status |
|---|---|
| Single-loop agent (input → tools → response → repeat) | ✅ |
| Typed tool handlers (`agent.Typed[I, O]`, schema via reflection) | ✅ |
| Raw tool handlers (`agent.Raw`, hand-written schema) | ✅ |
| Parallel tool execution (`Config.ToolExecution = ToolExecutionParallel`) | ✅ |
| Streaming tool progress (`agent.EmitToolDelta`) | ✅ |
| Batch early-exit (`Result.Terminate`) — skip the follow-up LLM call when a tool's output IS the final answer | ✅ |
| Three hooks: `BeforeToolCall`, `AfterToolCall`, `OnSteering` | ✅ |
| Mid-run steering (`Steer(ctx, msg)`) | ✅ |
| Snapshot resume (`Snapshot()` / `Restore()`) | ✅ |
| Iterator-based events (`iter.Seq2[AgentEvent, error]`) | ✅ |
| Cancellation via `context.Context` | ✅ |

Works with any provider implementing [pi-llm-go's `LLM` interface](https://pkg.go.dev/github.com/amit-timalsina/pi-llm-go#LLM): Anthropic Claude, OpenAI (Chat + Responses), Google Gemini, OpenAI-compatible hosts.

## Quickstart — calculator agent (Anthropic)

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
        SystemPrompt: "You are a calculator. Use the add tool.",
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

## When to pick `pi-agent-go`

| You want | Pick |
|---|---|
| A single-loop tool-calling agent in Go, multi-provider, ~1kLoC to read | **pi-agent-go** |
| Multi-agent orchestration, chains, memory, vector stores, retrievers | [tmc/langchaingo](https://github.com/tmc/langchaingo) |
| Just the LLM client, no agent loop | [pi-llm-go](https://github.com/amit-timalsina/pi-llm-go) directly |
| Hand-rolled loop on top of a vendor SDK | Vendor SDK + your own switch |

`pi-agent-go` deliberately stops where the agent loop stops. Multi-agent orchestration, session persistence, context compaction, and custom-message-type extension points belong in the application layer.

## Features

- **Single-loop agent.** Input → optional tool calls → response → repeat. Terminates when the model emits an assistant turn with no tool calls (or `MaxIterations` is hit).
- **Iterator-based events.** `Run` returns `iter.Seq2[AgentEvent, error]`. Type-switch on the concrete events for the granularity you care about. Token-level via `EventLLMStream`, message-level via `EventAssistantMessage`, tool-level via `EventToolStart`/`EventToolDelta`/`EventToolEnd`.
- **Streaming tool progress.** Tool handlers call `agent.EmitToolDelta(ctx, "fragment")` to surface incremental progress via `EventToolDelta`. The model still sees only the final `Result.Summary`; UIs and observers get per-token progress on long-running tools (shell, sub-agent calls, multi-step HTTP).
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

## Observability

Observability is first-class but external: the `AgentEvent` iterator IS the
event stream, the three hooks expose pre/post-tool state, and `Snapshot()`
gives a coherent post-run dump. Zero framework deps; wire your tracer of
choice on top.

`agent.RunIDFromContext(ctx) string` lets a tool handler attach its own
spans as children of the run-level span without threading the RunID
through tool arguments:

```go
func myHandler(ctx context.Context, args json.RawMessage) (agent.Result, error) {
    runID := agent.RunIDFromContext(ctx)
    ctx, span := tracer.Start(ctx, "my_tool",
        trace.WithAttributes(attribute.String("pi.run_id", runID)))
    defer span.End()
    // ... do work ...
}
```

The agent loop decorates ctx with the active RunID before invoking any
hook or Handler — the same ID you see on `EventRunStart.RunID` and
`Snapshot().RunID`. See [`examples/observability`](examples/observability)
for a complete reference wiring `log/slog` over the event iterator + the
three hooks, with marker comments showing where OpenTelemetry spans
attach.

## Snapshot resume

`Snapshot()` already gives a coherent post-run dump; `Restore(cfg, snap)`
brings the dual back — reconstruct an Agent from a prior snapshot so a
long-running agent can survive process restarts.

```go
snap := agent.Snapshot()             // persist however you want

// ... process restart ...

restored, err := agent.Restore(agent.Config{
    LLM:   newProvider,
    Model: snap.Model, // or your config default
}, snap)
// restored is ready to receive Run / RunMessage; the prior transcript,
// ToolLog, system prompt, and last usage are preserved. Next Run
// generates a fresh RunID.
```

Constraints: `snap.IsRunning=true` is rejected (can't resume a run that
was streaming mid-LLM-call); steering channel contents are not
preserved (re-inject any pending steering after restore); cfg goes
through the same validation as `New()`.

Serialization of `RunSnapshot` is left to the caller — `gob` works
out-of-the-box on the interface-typed `llm.Block` slice, while native
JSON support lands in a future pi-llm-go release. See
[`examples/snapshot_resume`](examples/snapshot_resume) for the
end-to-end pattern.

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
