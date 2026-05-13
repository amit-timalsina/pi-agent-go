# Changelog

All notable changes to **pi-agent-go** will be documented in this file. Format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **`Result.Terminate`** — boolean field on `agent.Result`. When EVERY
  finalized tool result in a batch sets `Terminate=true`, the agent
  stops without making the otherwise-inevitable follow-up "model
  explains what just happened" LLM call. Saves a turn (and the cost
  of one) when a tool's output IS the final answer: `write_file`,
  `send_message`, `render_artifact`, etc.
  - AND-reduce semantics: a single `false` anywhere in the batch
    means the agent continues with another turn so the model can
    react to the mixed signal.
  - Internal error results (unknown tool, BeforeToolCall-skip,
    handler error, budget violation, AfterToolCall hook error)
    naturally leave `Terminate=false` — they don't silently skip the
    model's chance to recover from an internal failure.
  - The `AfterToolCall` hook can force-terminate by setting
    `Terminate=true` on its override; useful for guardrails above
    the tool layer.
  - Mirrors Mario Zechner's pi-agent v0.69.0 / #3525.
  - New `examples/terminate_early/` demo (Anthropic-backed).

### Fixed

- **AfterToolCall hook error no longer aborts the run.** A hook that
  returns a non-nil error now produces an error tool result for THAT
  tool call only — the run continues so other in-flight parallel
  calls aren't aborted mid-execution. Symmetric in sequential mode
  for consistency. Mirrors Mario's pi-agent v0.67.67 (#3084).
  - BeforeToolCall errors still abort the run (asymmetric intentional:
    Before runs PRE-execution and leaves the agent uncertain whether
    to skip or execute; After runs POST-execution with output we can
    surface). Documented in `hooks.go`.
  - Cascading cleanup: `executeOneToolCall` no longer has a path
    that returns a non-nil error; signature simplified.

## [0.6.0] - 2026-05-13

Closes the streaming-tool-results roadmap slot. Tool handlers can now
emit incremental progress fragments while running; observers see
deltas in real time, but the model still sees only the final
`Result.Summary`. Mario Zechner's `AgentToolResult.details` analog,
mapped to Go's context-key idiom so the Handler signature stays
unchanged.

### Added

- **Streaming tool progress** via `agent.EmitToolDelta(ctx, fragment)`.
  Tool handlers call this from inside their Handler to surface
  incremental progress; the agent emits an `EventToolDelta{...}` on
  the run's event stream. The model NEVER sees deltas — only the
  Handler's final `Result.Summary` feeds back into the conversation.
  - `EventToolDelta` carries `ToolCallID`, `Name`, `Delta`.
  - Ordering: deltas for a given call arrive between its
    `EventToolStart` and `EventToolEnd` in emit order. Under parallel
    execution, deltas from different concurrent calls interleave
    non-deterministically; tag observers by `ToolCallID`.
  - Drop-on-overflow under parallel mode: a non-blocking channel
    backs the parallel emitter so a slow consumer cannot stall a
    Handler. Deltas are best-effort, not guaranteed.
  - Outside a running tool context, `EmitToolDelta` returns false and
    is a no-op — callers can branch on the return to skip work that
    produced the delta.
- `examples/streaming_tool` — end-to-end demo (Anthropic-backed) of a
  count_to tool emitting one delta per second; the example prints
  deltas as they arrive and shows the model receiving only the final
  collected summary.

## [0.5.0] - 2026-05-12

Snapshot-based resume. The biggest production unblock on the roadmap:
long-running agents can now survive process restarts via
Snapshot → persist → Restore.

### Added

- `agent.Restore(cfg Config, snap RunSnapshot) (*Agent, error)` —
  reconstructs an Agent from a prior `Snapshot()` so long-running
  agents can survive process restarts. Preserves transcript, ToolLog,
  system prompt (snap wins over cfg when non-empty), last usage,
  and the prior RunID (metadata only — the next `Run` generates a
  fresh ID).
- Constraints: `snap.IsRunning=true` is rejected (can't resume mid-
  flight); `cfg` goes through `New`-style validation; cfg tool
  registry can differ from the original (adding / removing tools
  is allowed).
- Defensive copy semantics: mutating the snap's slices after Restore
  does not affect the restored agent's state. Locked by
  `TestRestore_DefensiveCopySliceMutationDoesntLeakBack`.
- New `examples/snapshot_resume` — demonstrates the
  Snapshot → Restore pattern end-to-end. Live-verified against the
  Anthropic API: turn 1 ("capital of France?" → "Paris"); turn 2 on
  the restored agent ("its population?" → "~2,161,000") proves the
  prior transcript carries through.

### Deferred (planned for v0.6.0 or later)

- **`TranscriptStore` interface** for pluggable auto-persistence.
  v0.5.0 ships the load-bearing Restore primitive; callers wire
  their own storage (gob, custom JSON, S3, Postgres, etc.) on top
  of `Snapshot()` + `Restore()`. Real-consumer-driven shape will
  inform the interface design.
- **Native JSON serialization** for `llm.Message.Content` — the
  `llm.Block` interface needs a discriminated-union encoder in
  pi-llm-go before `encoding/json` can round-trip a snapshot
  unchanged. `gob` works today (register concrete block types).

## [0.4.0] - 2026-05-12

Observability helpers + reference example. No API breakage — the
new `agent.RunIDFromContext` / `agent.WithRunID` helpers add a
small ctx-based surface; the agent loop transparently decorates ctx
with the active RunID before invoking any hook or Handler.

### Added

- `agent.RunIDFromContext(ctx) string` — extracts the active RunID
  from a ctx the agent loop passed into a hook or tool Handler. The
  intended use is span correlation: a tool Handler can attach its
  own OpenTelemetry span as a child of the run-level span without
  having to thread the RunID through tool arguments. Returns `""`
  cleanly when the ctx didn't come from an agent loop.
- `agent.WithRunID(ctx, runID) context.Context` — the inverse helper.
  Normally callers don't need it (the loop decorates ctx
  automatically inside `RunMessage`), but it's exported for test
  scaffolding and for consumers building higher-level RunContext-
  aware infrastructure on top of pi-agent-go. Empty `runID` returns
  the input ctx unchanged.
- Hooks (BeforeToolCall / AfterToolCall / OnSteering),
  TransformContext, and tool Handlers all now receive a ctx
  pre-decorated with the active RunID. No API change to the hook
  signatures.
- New `examples/observability` — reference wiring for structured
  logging (stdlib `log/slog`) over the AgentEvent iterator and the
  three hooks. Marker comments call out where OpenTelemetry spans
  attach if you want to add a tracer. Zero new framework deps —
  observability stays first-class but external.

## [0.3.0] - 2026-05-12

Opt-in parallel tool execution. WWMD-aligned with Mario Zechner's
pi-agent. Sequential remains the default; existing callers see no
behavior change. Bumps Go floor to 1.25 (transitive requirement of
`golang.org/x/sync v0.20.0`, which provides `errgroup`).

### Added

- `agent.ToolExecutionMode` enum (`ToolExecutionUnspecified`,
  `ToolExecutionSequential`, `ToolExecutionParallel`) on `Config.ToolExecution`
  and `AgentTool.ExecutionMode`. Default is sequential (preserves v0.2.x
  behavior). Set `Config.ToolExecution = ToolExecutionParallel` to run
  the Handler + AfterToolCall phases of a batch concurrently when the
  model issues multiple tool calls in one assistant turn.
- Two-phase contract, mirroring Mario Zechner's pi-agent:
  - **Preflight (sequential, source order):** BeforeToolCall hook +
    EventToolStart emission. Inserts immediate outcomes (skipped /
    unknown tool) into the result channel in this phase so they
    interleave with parallel outcomes by source position.
  - **Execute (parallel):** Handler + AfterToolCall hook run in
    goroutines via `golang.org/x/sync/errgroup`, fed by a buffered
    `done` channel sized to `len(calls)`. `EventToolEnd` is yielded
    from the main goroutine as outcomes arrive — **finish order**, so
    observers see real concurrency. After all outcomes are drained,
    `tool_result` blocks + `ToolLog` entries are appended in **source
    order** so the wire transcript and audit log stay stable. Sort by
    `ToolCallID` or read from `Snapshot().ToolLog` if you need source
    order on the event stream.
- Per-tool opt-out: declaring `AgentTool.ExecutionMode = ToolExecutionSequential`
  on any tool in the batch forces the entire batch sequential — safety
  valve for handlers that aren't thread-safe with themselves or with
  other handlers.
- Single-tool batches stay sequential regardless of config (no point
  spinning up a goroutine for one call).
- `golang.org/x/sync` (v0.20.0) added as a transitive dependency for
  `errgroup`. No `import` change for callers.

### Contract notes

- Hook authors are responsible for thread-safety under
  ToolExecutionParallel. BeforeToolCall + AfterToolCall may be
  invoked concurrently from multiple goroutines. Protect shared state
  externally.
- Handler errors are converted to `IsError=true` results just like the
  sequential path; the run continues. Only hook errors abort the run
  (and propagate via `errgroup` cancellation to in-flight handlers).

## [0.2.0] - 2026-05-11

First breaking change since v0.1.x. Two PRs landed: storage-policy is now
the tool's job (FullPayloadHint replaces PayloadResolver/fetch_tool_result),
and a per-iteration TransformContext hook + mutable SystemPrompt let
long-running agents reshape context without rebuilding the Agent. Both
changes are WWMD-aligned with Mario Zechner's pi-mono.

Requires `github.com/amit-timalsina/pi-llm-go` ≥ v0.2.0.

### Added

- `Config.TransformContext func(ctx, []llm.Message) ([]llm.Message, error)`
  — optional hook called at the top of every iteration with a copy of
  the current transcript. The returned slice is used in place of the
  original for that iteration's LLM call. Use for context-window
  management (pruning old turns), late-injecting context that should
  not persist in the durable transcript, or summarizing prior turns.
  Mirrors Mario Zechner's pi-mono `transformContext` (closes #5).
- `Agent.SetSystemPrompt(string)` and `Agent.SystemPrompt() string`
  — mutate/read the live system prompt from any goroutine while a run
  is in progress. The change takes effect at the next `buildRequest`
  (top of the next iteration). The system prompt now lives on mutable
  agent state initialized from `Config.SystemPrompt` at `New()`.
  Note: calling `SetSystemPrompt` from inside `TransformContext` lands
  on iteration N+1, not N — see the godoc on `TransformContext` for
  the precise ordering contract.
- `RunSnapshot.SystemPrompt` — the live system prompt at snapshot
  time, so review UIs and audit consumers see the value that will be
  used on the next iteration.
- `ErrTransformContext` sentinel — `errors.Is`-matchable wrapper for
  caller errors out of `Config.TransformContext` (or for the "returned
  nil slice" contract violation). The underlying error is preserved
  via `%w` for `errors.Unwrap` / `errors.As` inspection.

- `Result.FullPayloadHint string` (opaque) — tools surface a free-form
  locator (file path, URL, storage key) alongside their bounded `Summary`.
  pi-agent-go does not interpret it; it just propagates the value onto
  `EventToolEnd.FullPayloadHint` and `ToolLogEntry.FullPayloadHint` for
  observability. The model retrieves full content via a separately-
  registered tool the caller wires up (e.g. a `read_file` tool that
  reads the hint path). See `examples/bounded_results` for the pattern.

### Removed (breaking)

- `PayloadRef` struct.
- `PayloadResolver` interface and `MemoryPayloadResolver` implementation.
- `Config.EnableFetchToolResult` and `Config.PayloadResolver`.
- Built-in `fetch_tool_result` meta-tool (`fetch.go` deleted).
- `Result.FullPayloadRef`, `EventToolEnd.FullPayloadRef`,
  `ToolLogEntry.FullPayloadRef`.

  These were introduced unreleased on `main` (PR #7, never tagged) as a
  framework-side storage-indirection abstraction. WWMD audit against
  Mario Zechner's upstream pi-mono found Mario keeps payload-storage
  policy out of the agent core entirely — tools write overflow to a
  tempfile and the agent retrieves it via the existing `Read` tool, no
  framework abstraction. The simpler design covers the same use case
  with less surface and matches his "if I don't need it, it won't be
  built" principle. Closes #8.

  Migration:

  ```go
  // before — agent owns storage indirection
  return agent.Result{
      Summary: "top correlations: ...",
      FullPayloadRef: &agent.PayloadRef{
          Backend: "memory", Key: "k1",
          Size: 12345, MimeType: "application/json",
      },
  }, nil
  // ...with agent.New(agent.Config{
  //     EnableFetchToolResult: true,
  //     PayloadResolver:       &MemoryPayloadResolver{...},
  //     ...
  // })

  // after — tool owns storage; agent just propagates the hint
  f, _ := os.CreateTemp("", "corr-matrix-*.json")
  _, _ = f.Write(fullBytes)
  _ = f.Close()
  return agent.Result{
      Summary:         "top correlations: ... full matrix at " + f.Name(),
      FullPayloadHint: f.Name(),
  }, nil

  // ...and register a sibling read tool the model can call with
  // {"path": "..."} when the summary is insufficient. Wide the budget
  // because the whole point of this tool is to return the unbounded
  // payload.
  readTool := agent.Raw(
      "read_file",
      "Read the file at the given path. Returns its raw bytes as text.",
      json.RawMessage(`{"type":"object",
                        "properties":{"path":{"type":"string"}},
                        "required":["path"],
                        "additionalProperties":false}`),
      func(_ context.Context, raw json.RawMessage) (agent.Result, error) {
          var args struct{ Path string `json:"path"` }
          _ = json.Unmarshal(raw, &args)
          body, err := os.ReadFile(args.Path)
          if err != nil { return agent.Result{}, err }
          return agent.Result{Summary: string(body)}, nil
      },
  )
  readTool.MaxSummarySize = 256 * 1024

  agent.New(agent.Config{
      Tools: []agent.AgentTool{correlationTool, readTool},
      // No Config flags needed for hints.
  })
  ```

  Production consumers should confine `read_file`-style tools to a
  known-safe path prefix instead of accepting any model-supplied path.
  See `examples/bounded_results` for the working pattern.

## [0.1.1] - 2026-05-11

CI + lint cleanup. No user-visible API changes vs v0.1.0.

### Added

- Dependabot config for `gomod` + `github-actions` ecosystems (weekly).
- README badges: CI status, Go Reference (pkg.go.dev), Go Report Card, MIT license.

### Changed

- Bumped go.mod Go floor to **1.24** (transitively required by
  `github.com/invopop/jsonschema` v0.14.0). Users on Go 1.23 should
  stay on the older release or upgrade.
- Pinned `golangci-lint-action` to v8 and the linter binary to v2.12.2.
- Internal: dropped unused `assistantMsg` parameter from
  `executeToolCalls`. Method is unexported; no public-API impact.
- gofmt -s normalizations across the tree.

## [0.1.0] - 2026-05-11

Initial public release. Real-API verified against Anthropic across
four end-to-end demos (hello_agent, with_hooks, steering, multi_tool).

### Added

- Initial release skeleton: `Agent`, `Config`, `New`, `Run`, `RunMessage`,
  `Steer`, `Snapshot`, `Reset`.
- `AgentEvent` sealed sum type: `EventRunStart`, `EventIterationStart`,
  `EventLLMStream` (wraps every `llm.StreamEvent`), `EventAssistantMessage`,
  `EventSteering`, `EventToolStart`, `EventToolEnd`, `EventRunEnd`.
- `AgentTool` with `Raw` and `Typed[I, O]` constructors; `Typed` derives the
  JSON Schema from `I` via `github.com/invopop/jsonschema`.
- Hooks: `BeforeToolCallHook`, `AfterToolCallHook`, `OnSteeringHook`.
- `RunContext` (read-only, passed to hooks) and `RunSnapshot` (immutable,
  returned by `Snapshot()`).
- `ToolLogEntry` audit record per invocation.
- Sentinels: `ErrMaxIterations`, `ErrAlreadyRunning`, `ErrSteeringClosed`.
- Run IDs in the form `run_<unix-ns-hex>_<8-rand-hex>` (sortable, dep-free).
- Buffered steering channel (capacity 16) drained at iteration boundaries.
- Sequential tool execution.
- Examples (all verified end-to-end against the Anthropic API):
  - `examples/hello_agent` — Typed[I,O] tool, BeforeToolCall hook
    logging.
  - `examples/with_hooks` — all three hooks live: BeforeToolCall
    denying dangerous commands, AfterToolCall redacting secrets,
    OnSteering dropping prompt-injection attempts.
  - `examples/steering` — cross-goroutine Steer injection from a
    watcher goroutine; the agent picks up the steering at the next
    iteration boundary and adjusts behavior.
  - `examples/multi_tool` — three typed tools chained across
    iterations, with Snapshot().ToolLog audit trail and per-call
    latencies printed at the end.
- Tests covering: lifecycle events, tool execution, hook short-circuiting
  (skip/override/error), steering inject + drop, MaxIterations cap, Snapshot,
  Reset-while-running panic, ErrAlreadyRunning concurrent-Run rejection,
  duplicate-tool registration rejection, Typed argument unmarshaling. All
  pass under `-race`.

### Dependencies

- `github.com/invopop/jsonschema v0.14.0` for `Typed[I, O]` schema derivation.
- `github.com/amit-timalsina/pi-llm-go` (sibling package).

[Unreleased]: https://github.com/amit-timalsina/pi-agent-go/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/amit-timalsina/pi-agent-go/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/amit-timalsina/pi-agent-go/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/amit-timalsina/pi-agent-go/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/amit-timalsina/pi-agent-go/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/amit-timalsina/pi-agent-go/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/amit-timalsina/pi-agent-go/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/amit-timalsina/pi-agent-go/releases/tag/v0.1.0
