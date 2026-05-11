# Contributing to pi-agent-go

Thanks for considering a contribution. This package is maintained by a single person; the bar is "things one maintainer can sustain forever."

## Quick orientation

- The `Agent` type, loop, and hook plumbing live at the module root (`agent.go`).
- Event types are in `event.go`; tool helpers in `tool.go`; hook signatures in `hooks.go`; run state types in `run.go`.
- Examples are runnable `package main` programs under `examples/`.

For deeper context on design decisions (why three hooks, why iterator events, why steering at iteration boundaries), see the per-repo `CLAUDE.md`.

## Before opening an issue

- **Bug report**: minimal reproduction with a fake `LLM` (we have one in `agent_test.go`) is ideal. If the bug requires a real provider, include the prompt + a redacted transcript.
- **Feature request**: describe the use case first. We deliberately keep the surface minimal — `BeforeToolCall` / `AfterToolCall` / `OnSteering` are the only hooks, and we resist adding more without a concrete consumer.

## Before opening a PR

Required:
- Tests for new behavior, using the fake-`LLM` pattern in `agent_test.go`.
- `CHANGELOG.md` entry under `[Unreleased]`, classified `Added` / `Changed` / `Deprecated` / `Removed` / `Fixed` / `Security`.
- `go test -race ./...` green.
- `go vet ./...` green.
- For new public API: a paragraph in the PR description explaining the use case and any rejected alternatives.

Style:
- `gofmt` enforced via CI.
- One Agent value, one consumer — please keep that invariant. The mutable state guards rely on it.

## Hook-surface posture

We have three hooks. Adding a fourth requires a real consumer with a real use case. We removed the upstream pi-agent's eight hooks (TS) deliberately. Don't add them back.

## Tool execution mode

v1 is **sequential**. There's a planned `Config.ToolExecution ExecutionMode` field with a `Parallel` option that uses `errgroup` and assembles results in source order. If you implement that, please file an issue first so we can align on the public-API shape before code review.

## Adding examples

Examples in `examples/` are runnable `main` packages in snake_case directories. Each should:
- Demonstrate a specific feature or pattern (one focused thing per example).
- Be runnable with `go run ./examples/<name>` given the documented env vars.
- Include a top-of-file comment explaining what it shows and how to run it.

## Review cadence

- Issues acknowledged within 7 days.
- PRs reviewed within 14 days for an initial response. Larger PRs may take longer.

## License

By contributing, you agree your contributions will be licensed under the project's [MIT License](LICENSE).
