# pi-agent-go roadmap

This is the maintainer's working plan. Items aren't promises — they're
ranked by user-value-per-LOC and informed by WWMD audits against Mario
Zechner's [pi-agent](https://github.com/badlogic/pi-mono/tree/main/packages/agent).
Reordering happens when reality changes.

## Status

- **v1.0.0** shipped 2026-06-06 — First stable release. The loop API
  has been additive-only since v0.2.0 (new optional `Config` fields and
  `AgentEvent` variants only, no breaking churn) and dogfooded in
  production at Noumenal (the Actioning Agent drives the loop). From
  here the package follows semver strictly: no breaking change without
  a major-version (module-path) bump. No new code vs v0.8.0 — this tag
  is the stability commitment plus a documentation feature-coverage
  pass (`CacheRetention`, `TransformContext`, dynamic system prompt,
  full examples list). The self-imposed "≥1 external Go consumer" gate
  is **waived**: the Noumenal AA driving the loop in production is the
  adoption signal. Still ships on `pi-llm-go v0.11.2`; the bump to
  `pi-llm-go v1.0.0` lands as a separate deps PR per the lockstep
  convention.
- **v0.8.0** shipped 2026-05-17 — Forwards `Config.CacheRetention`
  into every iteration's `llm.Request`. Closes #25 — Anthropic
  prompt caching (single highest cost lever for tool-heavy agents,
  ~10× input-rate reduction on cache hits) is now reachable from
  pi-agent-go callers. First issue raised by consumer-driven
  production use; matches the "real consumer surfacing real bugs"
  v1.0-gate signal.
- **v0.7.2** shipped 2026-05-14 — Republishes v0.7.1's nil-block fix
  with internal product identifiers scrubbed from comments +
  CHANGELOG. v0.7.1 is retracted (Go proxy had cached it before the
  scrub PR landed). No code-behavior change between v0.7.1 and
  v0.7.2.
- **v0.7.1** shipped 2026-05-14 [RETRACTED] — Defensive nil-block filter on
  `messageAccumulator.final()`. Adaptive thinking + parallel
  tool_use on Anthropic Opus 4.7 sometimes leaves a Content slot
  pre-extended by `ensureBlock` but never finalized (Anthropic's
  stream emits `content_block_start` for "thinking" without the
  closing `content_block_stop`); downstream providers then crashed
  next-iteration request build with `unsupported block type <nil>`.
  Reported in production 2026-05-14 on a multi-iteration agent run
  with ~16 parallel tool calls in a single response.
- **v0.7.0** shipped 2026-05-13 — Two ports from upstream pi-agent
  (Mario v0.67.67 + v0.69.0): `Result.Terminate` for batch-wide early
  exit when a tool's output IS the final answer (saves the
  otherwise-inevitable follow-up "model explains what just happened"
  LLM call), plus a fix where AfterToolCall hook errors become an
  error tool result for that call instead of aborting the run /
  killing other in-flight parallel handlers. New
  `examples/terminate_early/`.
- **v0.6.0** shipped 2026-05-13 — Streaming tool progress via
  `agent.EmitToolDelta(ctx, fragment)` + new `EventToolDelta` variant.
  Tool handlers surface incremental progress without the model seeing
  the intermediate fragments. Context-key plumbing keeps the Handler
  signature unchanged. `Config.ToolDeltaBuffer` (default 64) tunes
  the parallel-mode delta channel.
- **v0.5.0** shipped 2026-05-12 — `agent.Restore(cfg, snap)` for
  snapshot-based resume so long-running agents survive process
  restarts. Pluggable `TranscriptStore` interface deferred to a
  later release pending concrete consumer demand; callers wire
  their own storage on top of `Snapshot()` + `Restore()`.
- **v0.4.0** shipped 2026-05-12 — observability example (slog wiring
  over the AgentEvent iterator + the three hooks) + `RunIDFromContext`
  / `WithRunID` helpers for span correlation from tool handlers. No
  framework deps; observability stays first-class but external.
- **v0.3.0** shipped 2026-05-12 — parallel tool execution
  (`Config.ToolExecution = ToolExecutionParallel`, per-tool opt-out,
  source-order tool_result, finish-order EventToolEnd). Bumps Go floor
  to 1.25 (transitive via `golang.org/x/sync`).
- **v0.2.0** shipped 2026-05-11 — FullPayloadHint + TransformContext +
  SetSystemPrompt (WWMD convergence).
- **v1.0 — shipped 2026-06-06.** Next: the additive Config-hook surface
  below, each released as a v1.x minor when a real consumer asks.

## Mid-term (v0.8+)

Items below are purely additive: each is a new optional Config hook
or new optional method. None churns existing surface. Each waits for
a real consumer ask before landing — adding hooks before the shape
is settled risks signature churn that the Noumenal internal consumer
would feel.

- **`shouldStopAfterTurn` hook** (Mario v0.72.0). Graceful early exit
  before the next LLM call; caller controls the stop condition
  (context-budget guardrail, run-duration cap, custom signal).
  Returns `bool`; pure addition; cheap to ship when a consumer asks.
- **`prepareNextTurn` hook** (Mario). Mutates durable state between
  iterations — can inject a synthetic user message, swap tools, swap
  model. Different from `TransformContext` (which only mutates the
  LLM request, not the durable transcript). Useful for long-running
  agents that need to evolve their tool set or model choice.
- **`prepareArguments` per tool** (Mario). Compatibility shim that
  fixes malformed model-emitted JSON before schema validation. Small
  helper on `AgentTool`. Useful for models that occasionally return
  slightly wrong arg shapes; cheap to add.
- **`getFollowUpMessages` hook** (Mario). Second queue, drains AFTER
  the agent would otherwise stop (vs `Steer` which drains at iteration
  boundary mid-run). Use case: user typed while agent was finishing;
  pick up after. Could potentially fold into existing `Steer` with a
  mode flag.
- **`getApiKey` hook** (Mario). Dynamic API key resolution for
  expiring OAuth tokens. Lower priority — only matters for
  OAuth-backed providers; most consumers bake the key in at provider
  construction. Defer until OAuth is a real ask.
- **Subagent pattern**: a tool whose handler spawns a child
  `Agent.Run` with its own model/tools/prompt and returns the final
  assistant message as the parent's tool result. Ships as a
  `subagent.Tool(...)` constructor, not a Config change. Open design
  questions on event surfacing + cost accounting; warrant punting.
- **Tool-result caching**: agent-level cache keyed on `(name, args)`
  for deterministic tools, opt-in via `AgentTool.CacheKey()`. Saves
  LLM-time on replay. Not in Mario; user-requested.

## Explicitly skipped from Mario

- **Extensible `AgentMessage` via declaration merging.** TS-specific
  pattern; doesn't map cleanly to Go. Our `llm.Block` sum-type
  approach is the Go-native equivalent; adding a parallel custom-
  message surface would dilute it.
- **`agentLoopContinue` (retry from existing context).** Niche; our
  `RunMessage` covers the common case. Revisit if a consumer asks.
- **Harness layer** (`harness/` directory in Mario: shell tools,
  compaction, session repos, skills, system-prompt templates). Caller's
  job. We're the framework, not the harness. Documented in
  `Out of scope`.

## Out of scope (intentionally)

- **Built-in shell / file / browser tools.** Caller's job. We're the
  framework, not the harness. Mario ships these in
  `packages/agent/src/harness/`; we don't ship a harness.
- **Multi-LLM concurrent inference inside one Agent.** One LLM per
  Agent stays. Parallel inference is the consumer's concern (run
  multiple Agents).
- **DSL / declarative tool definition.** `Typed[I, O]` + `Raw` is
  enough; a YAML/JSON layer would be a separate package.
- **Distributed run coordination.** Out of scope for a single-loop
  agent framework.

## v1.0 readiness checklist — closed 2026-06-06

- [x] Parallel tool execution landed (v0.3.0), validated under load
      (production runs with ~16 parallel tool calls — see v0.7.x notes).
- [x] Snapshot resume working end-to-end (`Snapshot()` / `Restore()`, v0.5.0).
- [x] `examples/observability/` shipped (v0.4.0) and referenced from the README.
- [~] External Go consumer — **waived**; the Noumenal Actioning Agent
      driving the loop in production is the adoption signal.
- [x] `pkg.go.dev` `Example_*` tests (`example_test.go`).
- [x] CONTRIBUTING.md present (feature/hook-addition checklist in CLAUDE.md).

## Convergence work — closed

WWMD audits of bounded tool results and PromptBuilder drove the v0.2.0
rewrite. No open WWMD divergence as of 2026-05-11.
