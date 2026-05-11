# Changelog

All notable changes to **pi-agent-go** will be documented in this file. Format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

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

[Unreleased]: https://github.com/amit-timalsina/pi-agent-go/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/amit-timalsina/pi-agent-go/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/amit-timalsina/pi-agent-go/releases/tag/v0.1.0
