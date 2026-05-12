// observability: reference wiring for structured logging + tracing
// over a pi-agent-go run. Uses stdlib log/slog so the example stays
// dep-free; comment blocks show where OpenTelemetry spans would
// attach if you wire a tracer.
//
//	export ANTHROPIC_API_KEY=...
//	go run ./examples/observability
//
// The pattern:
//
//   - Range over Agent.Run's events to log per-iteration / per-tool
//     lifecycle. The AgentEvent sum is your tracing-event stream.
//   - Wire the three hooks (BeforeToolCall, AfterToolCall, OnSteering)
//     to capture pre-/post-handler state — especially useful for
//     attaching span attributes (input args, output summary, errors).
//   - Tool handlers call agent.RunIDFromContext(ctx) to correlate
//     their own spans with the parent run, without threading IDs
//     through tool arguments.
//   - Snapshot() gives you the durable transcript + ToolLog if you
//     want to emit a post-run summary or periodic heartbeat.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	agent "github.com/amit-timalsina/pi-agent-go"
	llm "github.com/amit-timalsina/pi-llm-go"
	"github.com/amit-timalsina/pi-llm-go/providers/anthropic"
)

// echoArgs is the typed input for the toy "echo" tool. Real tools
// would have richer schemas; this one is intentionally trivial so the
// example focuses on observability, not the tool.
type echoArgs struct {
	Text string `json:"text"`
}

func main() {
	// JSON-formatted slog handler — production-friendly, easily piped
	// into Loki / Cloudwatch / Datadog. The KEY trick on every log
	// line: include "run_id" so the lines correlate when you re-pull
	// them out of your log store later.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		logger.Error("ANTHROPIC_API_KEY required")
		os.Exit(2)
	}
	provider, err := anthropic.New(anthropic.Options{APIKey: key})
	if err != nil {
		logger.Error("anthropic.New", "err", err)
		os.Exit(1)
	}

	// A trivial tool whose handler demonstrates RunIDFromContext.
	// Real tools attach OTel spans here using the run ID; we emit a
	// structured log line instead so the example stays dep-free.
	echoTool := agent.Typed[echoArgs, string](
		"echo",
		"echoes back the input text",
		func(ctx context.Context, in echoArgs) (string, error) {
			runID := agent.RunIDFromContext(ctx)

			// === OpenTelemetry would go here ===
			//
			// ctx, span := otel.Tracer("my-app").Start(ctx, "tool.echo",
			//     trace.WithAttributes(
			//         attribute.String("pi.run_id", runID),
			//         attribute.String("tool.name", "echo"),
			//         attribute.String("tool.input", in.Text),
			//     ))
			// defer span.End()
			//
			// The run_id attribute is what links this tool span to the
			// parent run-level span you started outside Agent.Run.

			logger.Info("tool.echo executing",
				"run_id", runID,
				"input", in.Text,
			)
			return "echoed: " + in.Text, nil
		},
		func(s string) string { return s },
	)

	// Hooks: capture pre-/post-handler state. In production these are
	// where OTel attributes go; here we use slog. Note that hooks
	// receive RunContext (immutable view of the run), and the ctx
	// they see also carries the active RunID via RunIDFromContext.
	beforeHook := func(ctx context.Context, rc agent.RunContext, info agent.ToolCallInfo) (bool, string, error) {
		logger.Debug("tool.before",
			"run_id", agent.RunIDFromContext(ctx),
			"iteration", rc.Iteration,
			"tool", info.Name,
			"call_id", info.ToolCallID,
			"args", string(info.Arguments),
		)
		// Return (skip=false, errMsg="", err=nil) — no policy intervention.
		return false, "", nil
	}

	afterHook := func(ctx context.Context, rc agent.RunContext, info agent.ToolCallInfo, result agent.Result, isError bool) (*agent.Result, error) {
		logger.Debug("tool.after",
			"run_id", agent.RunIDFromContext(ctx),
			"iteration", rc.Iteration,
			"tool", info.Name,
			"call_id", info.ToolCallID,
			"summary_bytes", len(result.Summary),
			"is_error", isError,
		)
		// No override — return nil to keep the handler's result.
		return nil, nil
	}

	a, err := agent.New(agent.Config{
		LLM:            provider,
		Model:          anthropic.ClaudeSonnet4_6,
		SystemPrompt:   "You have an echo tool. Use it once with the text 'hello world' and then tell me what you got back.",
		Tools:          []agent.AgentTool{echoTool},
		BeforeToolCall: beforeHook,
		AfterToolCall:  afterHook,
		MaxTokens:      512,
	})
	if err != nil {
		logger.Error("agent.New", "err", err)
		os.Exit(1)
	}

	// === OpenTelemetry: this is where you'd start the run-level span ===
	//
	// ctx, runSpan := otel.Tracer("my-app").Start(ctx, "agent.Run")
	// defer runSpan.End()
	//
	// You don't need to inject the RunID into the ctx yourself — the
	// agent loop will decorate ctx automatically once it generates
	// the RunID inside RunMessage. Just consume EventRunStart below
	// and attach the run id as a span attribute.

	runStartedAt := time.Now()
	var runID string

	for event, err := range a.Run(context.Background(), "Use the echo tool.") {
		if err != nil {
			logger.Error("agent.run error",
				"run_id", runID,
				"err", err,
			)
			os.Exit(1)
		}
		switch e := event.(type) {

		case agent.EventRunStart:
			runID = e.RunID
			logger.Info("run.start", "run_id", runID)
			// === OTel: runSpan.SetAttributes(attribute.String("pi.run_id", e.RunID))

		case agent.EventIterationStart:
			logger.Debug("iteration.start",
				"run_id", runID,
				"iteration", e.Iteration,
			)
			// === OTel: start an "iteration N" child span here.

		case agent.EventLLMStream:
			// Token-level events. In production: avoid logging each
			// delta (rate too high); attach to a span and emit only
			// summary spans. Here: counting deltas as a sanity sample.
			switch e.Event.(type) {
			case llm.EventMessageStart, llm.EventMessageEnd:
				logger.Debug("llm.message",
					"run_id", runID,
					"iteration", e.Iteration,
					"event_type", fmt.Sprintf("%T", e.Event),
				)
			}

		case agent.EventAssistantMessage:
			logger.Info("assistant.message",
				"run_id", runID,
				"iteration", e.Iteration,
				"input_tokens", e.Message.Usage.InputTokens,
				"output_tokens", e.Message.Usage.OutputTokens,
				"stop_reason", e.Message.StopReason,
			)

		case agent.EventToolStart:
			logger.Info("tool.start",
				"run_id", runID,
				"call_id", e.ToolCallID,
				"tool", e.Name,
			)

		case agent.EventToolEnd:
			logger.Info("tool.end",
				"run_id", runID,
				"call_id", e.ToolCallID,
				"tool", e.Name,
				"is_error", e.IsError,
				"result_bytes", len(e.Result),
				"has_hint", e.FullPayloadHint != "",
			)

		case agent.EventSteering:
			logger.Info("steering",
				"run_id", runID,
			)

		case agent.EventRunEnd:
			logger.Info("run.end",
				"run_id", runID,
				"iterations", e.Iterations,
				"elapsed_ms", time.Since(runStartedAt).Milliseconds(),
				"final_stop_reason", e.FinalMessage.StopReason,
			)
		}
	}

	// Post-run heartbeat: dump the audit log. Real consumers might
	// emit this on a goroutine while the run is in flight via
	// Snapshot(), or persist on completion as a structured event.
	snap := a.Snapshot()
	for i, entry := range snap.ToolLog {
		logger.Info("tool_log.entry",
			"run_id", snap.RunID,
			"index", i,
			"iteration", entry.Iteration,
			"tool", entry.Name,
			"call_id", entry.ToolCallID,
			"is_error", entry.IsError,
			"duration_ms", entry.EndedAt.Sub(entry.StartedAt).Milliseconds(),
		)
	}

	logger.Info("run.summary",
		"run_id", snap.RunID,
		"final_iteration", snap.Iteration,
		"tool_calls", len(snap.ToolLog),
		"final_input_tokens", snap.LastUsage.InputTokens,
		"final_output_tokens", snap.LastUsage.OutputTokens,
	)
}
