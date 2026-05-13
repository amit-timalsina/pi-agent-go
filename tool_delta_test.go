package agent_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	agent "github.com/amit-timalsina/pi-agent-go"
	llm "github.com/amit-timalsina/pi-llm-go"
)

// progressTool emits a fixed number of EmitToolDelta calls before
// returning a Result.Summary that does NOT include the delta text —
// the agent should surface deltas only to observers, not to the model.
func progressTool(deltas []string, summary string) agent.AgentTool {
	return agent.Raw("progress", "emits incremental deltas",
		json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		func(ctx context.Context, _ json.RawMessage) (agent.Result, error) {
			for _, d := range deltas {
				agent.EmitToolDelta(ctx, d)
			}
			return agent.Result{Summary: summary}, nil
		})
}

func TestEmitToolDelta_SequentialOrderBetweenStartAndEnd(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call-1", "progress", `{}`),
			textOnlyScript("done"),
		},
	}
	tool := progressTool([]string{"step 1", "step 2", "step 3"}, "ok")
	a, err := agent.New(agent.Config{LLM: fake, Model: "test", Tools: []agent.AgentTool{tool}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	events, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Find the indices of start/deltas/end for call-1.
	startIdx, endIdx := -1, -1
	var deltas []agent.EventToolDelta
	for i, ev := range events {
		switch v := ev.(type) {
		case agent.EventToolStart:
			if v.ToolCallID == "call-1" {
				startIdx = i
			}
		case agent.EventToolDelta:
			if v.ToolCallID == "call-1" {
				deltas = append(deltas, v)
			}
		case agent.EventToolEnd:
			if v.ToolCallID == "call-1" {
				endIdx = i
			}
		}
	}
	if startIdx < 0 || endIdx < 0 {
		t.Fatalf("missing start/end events; got events: %#v", events)
	}
	if len(deltas) != 3 {
		t.Errorf("delta count: got %d, want 3", len(deltas))
	}
	for _, d := range deltas {
		if d.Name != "progress" {
			t.Errorf("delta Name: got %q, want %q", d.Name, "progress")
		}
	}
	if got := []string{deltas[0].Delta, deltas[1].Delta, deltas[2].Delta}; !equalStrSlice(got, []string{"step 1", "step 2", "step 3"}) {
		t.Errorf("delta order: got %v, want [step 1 step 2 step 3]", got)
	}

	// All deltas must sit between start and end indices.
	for i, ev := range events {
		if _, ok := ev.(agent.EventToolDelta); ok {
			if i < startIdx || i > endIdx {
				t.Errorf("EventToolDelta at index %d falls outside [%d, %d]", i, startIdx, endIdx)
			}
		}
	}

	// And the model must NOT have seen the delta text — only the
	// final Summary. The second LLM call's request contains the
	// tool_result; assert it carries "ok", not "step 1" etc.
	if len(fake.requests) < 2 {
		t.Fatalf("expected 2 LLM requests, got %d", len(fake.requests))
	}
	secondReq := fake.requests[1]
	for _, m := range secondReq.Messages {
		for _, b := range m.Content {
			if tr, ok := b.(llm.ToolResultBlock); ok && tr.ToolCallID == "call-1" {
				if tr.Content != "ok" {
					t.Errorf("tool_result content: got %q, want %q", tr.Content, "ok")
				}
				for _, want := range []string{"step 1", "step 2", "step 3"} {
					if tr.Content == want {
						t.Errorf("tool_result leaked delta %q", want)
					}
				}
			}
		}
	}
}

func TestEmitToolDelta_OutsideHandlerReturnsFalse(t *testing.T) {
	t.Parallel()

	// Calling EmitToolDelta on a bare context (no run in progress) must
	// return false and be a no-op. Callers can use the return as a
	// "skip work if no observer wired" hint.
	if agent.EmitToolDelta(context.Background(), "x") {
		t.Error("EmitToolDelta on bare ctx returned true; want false")
	}
}

func TestEmitToolDelta_ParallelDeltasAllSurface(t *testing.T) {
	t.Parallel()

	// Two parallel tool calls, each emitting 3 deltas. All 6 must
	// arrive; per-call ordering must be preserved; cross-call
	// interleaving is non-deterministic.
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			twoToolCallsScript("p-1", "p-2"),
			textOnlyScript("done"),
		},
	}
	tool := agent.Raw("progress", "emits deltas",
		json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		func(ctx context.Context, _ json.RawMessage) (agent.Result, error) {
			// Use the tool call ID to differentiate the two concurrent
			// invocations. ID isn't in the args, so retrieve via
			// RunIDFromContext won't work; just send three uniform
			// deltas — the observer differentiates by ToolCallID.
			for i := 0; i < 3; i++ {
				agent.EmitToolDelta(ctx, "tick")
			}
			return agent.Result{Summary: "done"}, nil
		})
	a, err := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         []agent.AgentTool{tool},
		ToolExecution: agent.ToolExecutionParallel,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	events, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	byID := map[string]int{}
	for _, ev := range events {
		if d, ok := ev.(agent.EventToolDelta); ok {
			byID[d.ToolCallID]++
		}
	}
	if byID["p-1"] != 3 {
		t.Errorf("p-1 delta count: got %d, want 3", byID["p-1"])
	}
	if byID["p-2"] != 3 {
		t.Errorf("p-2 delta count: got %d, want 3", byID["p-2"])
	}
}

// TestEmitToolDelta_DropOnOverflowDoesNotBlockHandler is a smoke test
// that a handler emitting more deltas than the buffer can hold does
// not deadlock. The overflow-drop behavior is documented as
// best-effort; we don't assert exact counts.
func TestEmitToolDelta_DropOnOverflowDoesNotBlockHandler(t *testing.T) {
	t.Parallel()

	const burst = 10_000 // far beyond the channel's buffer of 32 (2*16)

	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			twoToolCallsScript("b-1", "b-2"),
			textOnlyScript("done"),
		},
	}

	var wg sync.WaitGroup
	wg.Add(2) // two parallel tool calls scripted below
	tool := agent.Raw("progress", "spam",
		json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		func(ctx context.Context, _ json.RawMessage) (agent.Result, error) {
			defer wg.Done()
			for i := 0; i < burst; i++ {
				agent.EmitToolDelta(ctx, "x")
			}
			return agent.Result{Summary: "done"}, nil
		})

	a, err := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         []agent.AgentTool{tool},
		ToolExecution: agent.ToolExecutionParallel,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	events, err := collect(t, a.Run(context.Background(), "spam"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Handlers all completed — the test would hang if a handler
	// blocked on the deltas channel.
	wg.Wait()
	// Smoke check that AT LEAST some deltas survived: a future bug
	// that drops 100% of deltas (e.g. mistakenly always falling into
	// the default branch of the non-blocking send) would otherwise
	// pass this test.
	var deltaCount int
	for _, ev := range events {
		if _, ok := ev.(agent.EventToolDelta); ok {
			deltaCount++
		}
	}
	if deltaCount == 0 {
		t.Error("expected at least some deltas to survive overflow; got 0")
	}
}

// twoToolCallsScript: one assistant message with two parallel tool calls.
func twoToolCallsScript(id1, id2 string) []llm.StreamEvent {
	return []llm.StreamEvent{
		llm.EventMessageStart{Model: "test"},
		llm.EventToolCallStart{BlockIndex: 0, ID: id1, Name: "progress"},
		llm.EventToolCallEnd{BlockIndex: 0, Arguments: json.RawMessage(`{}`)},
		llm.EventToolCallStart{BlockIndex: 1, ID: id2, Name: "progress"},
		llm.EventToolCallEnd{BlockIndex: 1, Arguments: json.RawMessage(`{}`)},
		llm.EventMessageEnd{StopReason: llm.StopReasonToolUse, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}},
	}
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
