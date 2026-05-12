# pi-agent-go roadmap

This is the maintainer's working plan. Items aren't promises — they're
ranked by user-value-per-LOC and informed by WWMD audits against Mario
Zechner's [pi-agent](https://github.com/badlogic/pi-mono/tree/main/packages/agent).
Reordering happens when reality changes.

## Status

- **v0.3.0** landing — parallel tool execution
  (`Config.ToolExecution = ToolExecutionParallel`, per-tool opt-out,
  source-order tool_result, finish-order EventToolEnd). Tag stamped on
  merge.
- **v0.2.0** shipped 2026-05-11 — FullPayloadHint + TransformContext +
  SetSystemPrompt (WWMD convergence).
- **v1.0 ETA:** unknown. v1.0 requires ≥4 weeks production use without
  API churn + at least one external Go consumer driving the loop in a
  real workload.

## Near-term (next 1–3 minor releases)

### v0.4.0 — observability example + run-correlation helper

- `examples/observability/` — wires OpenTelemetry spans (run → iter →
  tool) and `slog` structured logs via the three hooks and the
  `AgentEvent` iterator. Zero new framework deps; the events ARE the
  observer surface. Consumer copies and tweaks for their stack.
- New `agent.RunIDFromContext(ctx) string` helper — the agent
  decorates every tool-handler context with the active RunID so
  handlers can correlate their own spans with the parent run without
  threading the ID through tool arguments. Small framework change,
  big telemetry ergonomics win.
- A first-party `Observer` interface or `pi-agent-go/otel` sub-package
  is **deferred** until the example pattern proves insufficient.

### v0.5.0 — snapshot resume

- `agent.Restore(cfg Config, snap RunSnapshot) (*Agent, error)` —
  reconstruct an Agent from a prior `Snapshot()`. Today `Snapshot` is
  observability-only; you can't pick up where a run left off.
- Required for: long-running agents that survive process restarts;
  audit / replay workflows; cheaper recovery after failures.

### v0.6.0 — streaming tool results

- Tool handlers can emit incremental output via a callback parameter
  before returning, surfacing as `EventToolDelta` events. The model
  still sees only the final summary, but UIs and observers get
  per-token progress on long-running tools (shell, sub-agent calls).
- Mario has the equivalent via `AgentToolResult.details`. Ours doesn't.

## Mid-term (v0.7+)

- **`prepareNextTurn` + `shouldStopAfterTurn` hooks.** Mario's loop has
  eight hooks; we have three. Two of his five missing ones are
  load-bearing for long-running agents:
  - `prepareNextTurn` — runs between iterations; can inject a synthetic
    user message, change tools, swap model. Different from
    TransformContext: this mutates durable state, not just the request.
  - `shouldStopAfterTurn` — graceful stop condition; returns true to
    end the run cleanly before the next LLM call. Useful for context-
    budget guardrails.
  - The other three Mario hooks (`onPayload`, `onResponse`, `getApiKey`)
    don't map cleanly to Go idioms; defer indefinitely.
- **Subagent pattern**: a tool whose handler spawns a child `Agent.Run`
  with its own model/tools/prompt and returns the final assistant
  message as the parent's tool result. Ships as a `subagent.Tool(...)`
  constructor, not a Config change. Open design questions on event
  surfacing + cost accounting that warrant punting.
- **Tool-result caching**: agent-level cache keyed on `(name, args)`
  for deterministic tools, opt-in via `AgentTool.CacheKey()`. Saves
  LLM-time on replay. Not in Mario; user-requested.
- **Persistence interface**: pluggable `TranscriptStore` that the agent
  writes to on every state mutation; pairs naturally with v0.5.0
  Restore. Default in-memory matches today's behavior.

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

## v1.0 readiness checklist

- [x] Parallel tool execution landed (v0.3.0); awaiting production
      validation under load.
- [ ] Snapshot resume working end-to-end.
- [ ] `examples/observability/` shipped and referenced from the README.
- [ ] At least one external Go consumer driving the loop in a real
      workload for ≥4 weeks without API churn.
- [ ] `pkg.go.dev` `Example_*` tests for every exported type.
- [ ] CONTRIBUTING.md walks a contributor through adding a new hook.

## Convergence work — closed

WWMD audits of bounded tool results and PromptBuilder drove the v0.2.0
rewrite. No open WWMD divergence as of 2026-05-11.
