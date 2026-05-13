package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	agent "github.com/amit-timalsina/pi-agent-go"
	llm "github.com/amit-timalsina/pi-llm-go"
)

// terminateTool returns a tool whose handler returns Result.Terminate=t
// alongside the given summary. Used to drive the AND-reduce semantics.
func terminateTool(name, summary string, t bool) agent.AgentTool {
	return agent.Raw(name, "terminating tool",
		json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
			return agent.Result{Summary: summary, Terminate: t}, nil
		},
	)
}

// TestTerminate_AllToolsOptInSkipsNextLLMCall verifies the cost-saving
// path: when every tool result in a batch sets Terminate=true, the
// run ends after the tool results land in the transcript — NO follow-up
// "model explains what happened" LLM call.
func TestTerminate_AllToolsOptInSkipsNextLLMCall(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			// Iteration 1: assistant calls write_file with terminate=true
			// in the result. If terminate were ignored, the loop would
			// ask for a second LLM call here and fail (we only scripted
			// one).
			toolCallScript("call-1", "write_file", `{}`),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:   fake,
		Model: "test",
		Tools: []agent.AgentTool{terminateTool("write_file", "wrote ok", true)},
	})

	events, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("run: %v (terminate=true was not honored — extra LLM call attempted)", err)
	}

	// Exactly one LLM call should have been made.
	if len(fake.requests) != 1 {
		t.Errorf("LLM calls: got %d, want 1 (terminate should skip the follow-up call)", len(fake.requests))
	}

	// The FinalMessage on EventRunEnd should be the assistant message
	// that issued the tool call (no follow-up turn exists).
	var endIters int
	var sawEnd bool
	var sawToolEnd bool
	for _, ev := range events {
		switch e := ev.(type) {
		case agent.EventToolEnd:
			if e.ToolCallID == "call-1" && e.Result == "wrote ok" && !e.IsError {
				sawToolEnd = true
			}
		case agent.EventRunEnd:
			sawEnd = true
			endIters = e.Iterations
		}
	}
	if !sawToolEnd {
		t.Error("expected EventToolEnd for call-1 with the original summary")
	}
	if !sawEnd {
		t.Fatal("expected EventRunEnd")
	}
	if endIters != 1 {
		t.Errorf("EventRunEnd.Iterations: got %d, want 1", endIters)
	}
}

// TestTerminate_AnyFalseInBatchPreventsEarlyExit verifies the AND-reduce:
// if even ONE tool in the batch returns Terminate=false, the agent
// continues with another LLM turn.
func TestTerminate_AnyFalseInBatchPreventsEarlyExit(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			// Iteration 1: two tool calls. One terminates, one doesn't.
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "write_file", `{}`},
				struct{ ID, Name, Args string }{"2", "fetch_url", `{}`},
			),
			// Iteration 2: model explains result.
			textOnlyScript("ok the file is written and the url has been fetched"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:   fake,
		Model: "test",
		Tools: []agent.AgentTool{
			terminateTool("write_file", "wrote ok", true),
			terminateTool("fetch_url", "fetched 100 bytes", false),
		},
	})

	_, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Two LLM calls expected (terminate AND-reduce false because one
	// tool didn't opt in).
	if len(fake.requests) != 2 {
		t.Errorf("LLM calls: got %d, want 2 (AND-reduce should require all tools opt in)", len(fake.requests))
	}
}

// TestTerminate_ParallelBatchSameSemantics confirms the AND-reduce
// works the same way under parallel execution.
func TestTerminate_ParallelBatchSameSemantics(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "write_a", `{}`},
				struct{ ID, Name, Args string }{"2", "write_b", `{}`},
				struct{ ID, Name, Args string }{"3", "write_c", `{}`},
			),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		ToolExecution: agent.ToolExecutionParallel,
		Tools: []agent.AgentTool{
			terminateTool("write_a", "ok", true),
			terminateTool("write_b", "ok", true),
			terminateTool("write_c", "ok", true),
		},
	})

	_, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("run: %v (parallel terminate did not exit cleanly)", err)
	}
	if len(fake.requests) != 1 {
		t.Errorf("LLM calls: got %d, want 1 (all-terminate batch should skip follow-up)", len(fake.requests))
	}
}

// TestTerminate_AfterToolCallHookCanForceTerminate verifies that an
// AfterToolCall override with Terminate=true is honored even if the
// underlying handler returned Terminate=false. Useful for "guardrail
// the agent into stopping after a tool ran" without modifying the
// tool itself.
func TestTerminate_AfterToolCallHookCanForceTerminate(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call-1", "any_tool", `{}`),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:   fake,
		Model: "test",
		Tools: []agent.AgentTool{
			// Handler does NOT opt into terminate; hook overrides.
			terminateTool("any_tool", "done", false),
		},
		AfterToolCall: func(_ context.Context, _ agent.RunContext, _ agent.ToolCallInfo, r agent.Result, _ bool) (*agent.Result, error) {
			r.Terminate = true
			return &r, nil
		},
	})

	_, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Errorf("LLM calls: got %d, want 1 (hook override should force terminate)", len(fake.requests))
	}
}

// TestTerminate_HookOverrideWithoutTerminateDropsIt verifies the
// documented "override replaces the result entirely, no deep merge"
// behavior: if the handler returns Terminate=true but the
// AfterToolCall hook returns an override that omits the field, the
// Result.Terminate value drops to false (Go zero-value) and the agent
// continues with another LLM call.
//
// Hooks that want to preserve the handler's Terminate must copy it
// through explicitly (`override.Terminate = result.Terminate`).
func TestTerminate_HookOverrideWithoutTerminateDropsIt(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call-1", "tool_a", `{}`),
			// Iteration 2: model "explains" — needed because override
			// drops Terminate.
			textOnlyScript("explanation after the drop"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:   fake,
		Model: "test",
		Tools: []agent.AgentTool{terminateTool("tool_a", "raw output", true)},
		AfterToolCall: func(_ context.Context, _ agent.RunContext, _ agent.ToolCallInfo, _ agent.Result, _ bool) (*agent.Result, error) {
			// Override that does NOT carry Terminate through.
			return &agent.Result{Summary: "redacted"}, nil
		},
	})
	_, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(fake.requests) != 2 {
		t.Errorf("LLM calls: got %d, want 2 (override-without-Terminate should drop the early exit)", len(fake.requests))
	}
}

// TestTerminate_MixedTextAndToolUseInFinalMessage verifies that when
// Terminate fires, EventRunEnd.FinalMessage is the message that
// issued the tool call — even when it ALSO contains a TextBlock
// before the tool_use (Anthropic does this routinely with "I'll use
// the X tool. <tool_use>"). The consumer reading FinalMessage should
// be able to extract that prefatory text.
func TestTerminate_MixedTextAndToolUseInFinalMessage(t *testing.T) {
	t.Parallel()

	// Custom script: text + tool_use in the same assistant message.
	mixed := []llm.StreamEvent{
		llm.EventMessageStart{Model: "test"},
		llm.EventTextStart{BlockIndex: 0},
		llm.EventTextDelta{BlockIndex: 0, Delta: "I'll save it now."},
		llm.EventTextEnd{BlockIndex: 0},
		llm.EventToolCallStart{BlockIndex: 1, ID: "call-1", Name: "tool_a"},
		llm.EventToolCallEnd{BlockIndex: 1, Arguments: []byte(`{}`)},
		llm.EventMessageEnd{
			StopReason: llm.StopReasonToolUse,
			Usage:      llm.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8},
		},
	}
	fake := &fakeLLM{scripts: [][]llm.StreamEvent{mixed}}
	a, _ := agent.New(agent.Config{
		LLM:   fake,
		Model: "test",
		Tools: []agent.AgentTool{terminateTool("tool_a", "ok", true)},
	})
	events, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Locate the EventRunEnd's FinalMessage and assert both blocks survive.
	var sawEnd bool
	for _, ev := range events {
		end, ok := ev.(agent.EventRunEnd)
		if !ok {
			continue
		}
		sawEnd = true
		var hasText, hasToolCall bool
		var text string
		for _, b := range end.FinalMessage.Content {
			switch bb := b.(type) {
			case llm.TextBlock:
				hasText = true
				text = bb.Text
			case llm.ToolCallBlock:
				hasToolCall = true
			}
		}
		if !hasText || !hasToolCall {
			t.Errorf("FinalMessage on Terminate: hasText=%v hasToolCall=%v; both expected", hasText, hasToolCall)
		}
		if text != "I'll save it now." {
			t.Errorf("FinalMessage text: got %q, want 'I'll save it now.'", text)
		}
	}
	if !sawEnd {
		t.Fatal("missing EventRunEnd")
	}
}

// TestTerminate_InternalErrorResultDoesNotTerminate verifies that
// internal error paths (unknown tool, BeforeToolCall-skip, etc.) leave
// Terminate=false even if other tools in the batch opt in — the model
// needs the chance to react to the internal failure.
func TestTerminate_InternalErrorResultDoesNotTerminate(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			// One tool that opts into terminate + one tool that doesn't
			// exist in the registry. Unknown-tool produces an error
			// result with Terminate=false by construction.
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "write_file", `{}`},
				struct{ ID, Name, Args string }{"2", "nonexistent", `{}`},
			),
			textOnlyScript("recovered from missing tool"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:   fake,
		Model: "test",
		Tools: []agent.AgentTool{terminateTool("write_file", "ok", true)},
	})
	_, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(fake.requests) != 2 {
		t.Errorf("LLM calls: got %d, want 2 (unknown-tool error must NOT silently terminate)", len(fake.requests))
	}
}
