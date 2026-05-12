# pi-agent-go roadmap

This is the maintainer's working plan. Items aren't promises — they're
ranked by user-value-per-LOC and informed by WWMD audits against Mario
Zechner's [pi-agent](https://github.com/badlogic/pi-mono/tree/main/packages/agent).
Reordering happens when reality changes.

## Status

- **v0.5.0** landing — `agent.Restore(cfg, snap)` for snapshot-based
  resume so long-running agents survive process restarts. Pluggable
  `TranscriptStore` interface deferred to a later release pending
  concrete consumer demand; callers wire their own storage on top of
  `Snapshot()` + `Restore()`. Tag stamped on merge.
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
- **v1.0 ETA:** unknown. v1.0 requires ≥4 weeks production use without
  API churn + at least one external Go consumer driving the loop in a
  real workload.

## Near-term (next 1–3 minor releases)

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
