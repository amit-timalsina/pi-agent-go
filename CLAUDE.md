# pi-agent-go — maintainer working agreement

This file is the **maintainer's working agreement** for `pi-agent-go`. Keep it short. When something grows past a few lines, move it to a dedicated doc and link from here.

## What this repo is

A single-loop agent built on [`pi-llm-go`](https://github.com/amit-timalsina/pi-llm-go): input → optional tool calls → response → repeat until done. Tool registry with typed handlers, three hooks (`BeforeToolCall` / `AfterToolCall` / `OnSteering`), and a buffered steering channel for mid-run injection.

Depends on pi-llm-go's `LLM` interface — track its API and bump in lockstep.

## Stability

- **v1.0.0 — stable (since 2026-06-06).** Strict semver: the exported API is frozen; breaking changes require a `vN+1` module-path bump per Go's major-version policy. Adding a method to a public interface is a major bump; new structs / new functions / new optional `Config` fields / new `AgentEvent` variants are minor; bugfixes are patches. CHANGELOG documents every change.

## Hard rules

- **Atomic commits.** Each logical unit of work = one commit. Conventional commits with scope. HEREDOC bodies, `Co-Authored-By` trailer when AI-assisted. Each commit must build + test green.
- **Push, force-push, repo-creation, opening PRs require explicit human OK.** Atomic commits are local-by-default.
- **One Agent value, one consumer.** `Agent.Run` is not safe for concurrent calls. Callers wanting parallel sessions instantiate multiple Agents. `Steer()` and `Snapshot()` are the only methods safe from other goroutines while `Run` is in progress.
- **Hook surface is intentionally minimal.** Three hooks only (`BeforeToolCall`, `AfterToolCall`, `OnSteering`). Resist adding more without a concrete use case from a real consumer. Upstream pi-agent (TS) has eight; we chose the smallest workable subset and have lived to defend it.
- **Sequential tool execution is the default.** Parallel is opt-in via `Config.ToolExecution = ToolExecutionParallel` (shipped v0.3.0; `errgroup.Group` with `tool_result` blocks reassembled in source order). The default stays `Sequential` — do not flip it.

## Code conventions

- `context.Context` is the first parameter of every call. Cancelling ctx terminates the run and yields `ctx.Err()` from the iterator.
- Steering is a buffered `chan llm.Message` (capacity 16) drained at iteration boundaries — never mid-LLM-call.
- Events flow through `iter.Seq2[AgentEvent, error]`. Each Run returns a fresh iterator; tearing down a run = cancel ctx or break out of the range loop.
- `Snapshot()` returns an immutable copy (defensive). Never expose live mutable state across goroutines.
- Tool errors are converted to `ToolResultBlock{IsError: true}` and fed back to the model — the model can recover. Hook errors abort the run.

## Currency

- Track pi-llm-go's API. When it adds a new `Block` type, decide whether `AgentTool.Handler` should surface it through `Result`.
- Schema-derivation depends on `github.com/invopop/jsonschema`. Replace if upstream stops being maintained.

## Adding a feature checklist

1. Update the [LLD](../docs/lld-pi-agent-go.md) (in the parent monorepo) first. Lock the decision before code.
2. Implementation + tests using a fake `LLM` that emits scripted `StreamEvent`s.
3. Update `examples/hello_agent/main.go` if the feature is user-visible at the entry point.
4. CHANGELOG entry.
5. Verify the example builds and runs against the real Anthropic API before merging.

## Releases

Tag `vX.Y.Z` (signed). GitHub Actions `release.yml` picks up the tag and publishes release notes from the matching CHANGELOG block.

When pi-llm-go bumps a minor, the agent's `require` line in `go.mod` updates in a separate PR with its own changelog entry.

## License

MIT. Attribution to upstream `pi-agent` (Mario Zechner, MIT) in README and LICENSE.

## See also

- [README.md](README.md) — user-facing intro and quickstart.
- [CHANGELOG.md](CHANGELOG.md) — per-release changes.
- Parent monorepo's [lld-pi-agent-go.md](../docs/lld-pi-agent-go.md) — full design rationale.
