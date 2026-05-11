package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agent "github.com/amit-timalsina/pi-agent-go"
	llm "github.com/amit-timalsina/pi-llm-go"
)

// --- Content alias backward-compat ---

func TestResultContentAliasFallsBackWhenSummaryEmpty(t *testing.T) {
	// A handler that returns Result{Content: "..."} (deprecated path)
	// should produce the same end-to-end behavior as Result{Summary: "..."}.
	contentTool := agent.Raw(
		"legacy",
		"returns via Content alias",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
			return agent.Result{Content: "from legacy alias"}, nil
		},
	)
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "legacy", `{}`),
			textOnlyScript("ack"),
		},
	}
	a, _ := agent.New(agent.Config{LLM: fake, Model: "test", Tools: []agent.AgentTool{contentTool}})
	events, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var endResult string
	for _, ev := range events {
		if e, ok := ev.(agent.EventToolEnd); ok {
			endResult = e.Result
		}
	}
	if endResult != "from legacy alias" {
		t.Errorf("Content alias didn't fall through; got %q", endResult)
	}
}

func TestResultSummaryWinsOverContent(t *testing.T) {
	// When both are set, Summary wins (forward-compat: callers can opt
	// in to the new name without removing Content first).
	tool := agent.Raw(
		"both",
		"sets both",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
			return agent.Result{Summary: "from Summary", Content: "from Content"}, nil
		},
	)
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "both", `{}`),
			textOnlyScript("ack"),
		},
	}
	a, _ := agent.New(agent.Config{LLM: fake, Model: "test", Tools: []agent.AgentTool{tool}})
	events, _ := collect(t, a.Run(context.Background(), "go"))
	for _, ev := range events {
		if e, ok := ev.(agent.EventToolEnd); ok && e.Result != "from Summary" {
			t.Errorf("Summary did not win; got %q", e.Result)
		}
	}
}

// --- Budget enforcement ---

func TestBudgetEnforcementOversizedSummary(t *testing.T) {
	oversized := strings.Repeat("x", 100)
	tool := agent.Raw(
		"oversize",
		"returns more than its budget",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
			return agent.Result{Summary: oversized}, nil
		},
	)
	tool.MaxSummarySize = 50 // generous would be 100; 50 forces violation
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "oversize", `{}`),
			textOnlyScript("ack"),
		},
	}
	a, _ := agent.New(agent.Config{LLM: fake, Model: "test", Tools: []agent.AgentTool{tool}})
	events, _ := collect(t, a.Run(context.Background(), "go"))

	var end agent.EventToolEnd
	for _, ev := range events {
		if e, ok := ev.(agent.EventToolEnd); ok {
			end = e
		}
	}
	if !end.IsError {
		t.Error("budget violation should produce IsError=true tool result")
	}
	if !strings.Contains(end.Result, "100 bytes") || !strings.Contains(end.Result, "max is 50") {
		t.Errorf("error result should describe the violation; got %q", end.Result)
	}
}

func TestBudgetUsesDefaultMaxSummarySizeWhenPerToolZero(t *testing.T) {
	// Per-tool MaxSummarySize == 0 falls back to Config.DefaultMaxSummarySize.
	tool := agent.Raw(
		"big",
		"returns 200 bytes",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
			return agent.Result{Summary: strings.Repeat("x", 200)}, nil
		},
	)
	// tool.MaxSummarySize stays 0
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "big", `{}`),
			textOnlyScript("ack"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:                   fake,
		Model:                 "test",
		Tools:                 []agent.AgentTool{tool},
		DefaultMaxSummarySize: 100, // per-agent override; 200-byte summary exceeds
	})
	events, _ := collect(t, a.Run(context.Background(), "go"))
	for _, ev := range events {
		if e, ok := ev.(agent.EventToolEnd); ok {
			if !e.IsError {
				t.Errorf("expected budget violation; got Result=%q", e.Result)
			}
		}
	}
}

// --- FullPayloadHint carried through ToolLogEntry + EventToolEnd ---

func TestFullPayloadHintCarriedThroughEvents(t *testing.T) {
	const hint = "/tmp/full-payload-xyz.json"
	tool := agent.Raw(
		"hints",
		"returns a payload hint",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
			return agent.Result{Summary: "short summary", FullPayloadHint: hint}, nil
		},
	)
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "hints", `{}`),
			textOnlyScript("ack"),
		},
	}
	a, _ := agent.New(agent.Config{LLM: fake, Model: "test", Tools: []agent.AgentTool{tool}})
	events, _ := collect(t, a.Run(context.Background(), "go"))

	var end agent.EventToolEnd
	for _, ev := range events {
		if e, ok := ev.(agent.EventToolEnd); ok {
			end = e
		}
	}
	if end.FullPayloadHint != hint {
		t.Errorf("EventToolEnd.FullPayloadHint=%q, want %q", end.FullPayloadHint, hint)
	}

	snap := a.Snapshot()
	if len(snap.ToolLog) == 0 || snap.ToolLog[0].FullPayloadHint != hint {
		t.Errorf("ToolLogEntry.FullPayloadHint not carried: %+v", snap.ToolLog)
	}
}

func TestBudgetViolationPreservesFullPayloadHint(t *testing.T) {
	// When the loop rewrites Result to surface a budget-violation error,
	// the FullPayloadHint should survive — observability consumers still
	// want to see where the (rejected) full payload lives.
	const hint = "/tmp/oversize.bin"
	tool := agent.Raw(
		"oversize",
		"summary too big",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
			return agent.Result{
				Summary:         strings.Repeat("x", 200),
				FullPayloadHint: hint,
			}, nil
		},
	)
	tool.MaxSummarySize = 50
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "oversize", `{}`),
			textOnlyScript("ack"),
		},
	}
	a, _ := agent.New(agent.Config{LLM: fake, Model: "test", Tools: []agent.AgentTool{tool}})
	events, _ := collect(t, a.Run(context.Background(), "go"))

	var end agent.EventToolEnd
	for _, ev := range events {
		if e, ok := ev.(agent.EventToolEnd); ok {
			end = e
		}
	}
	if !end.IsError {
		t.Fatal("expected budget violation marked as error")
	}
	if end.FullPayloadHint != hint {
		t.Errorf("FullPayloadHint lost after budget rewrite: got %q, want %q", end.FullPayloadHint, hint)
	}
}
