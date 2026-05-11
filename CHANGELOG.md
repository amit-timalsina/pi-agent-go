# Changelog

All notable changes to **pi-agent-go** will be documented in this file. Format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
