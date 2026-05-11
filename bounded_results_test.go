package agent_test

import (
	"context"
	"encoding/json"
	"errors"
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

// --- FullPayloadRef carried through ToolLogEntry + EventToolEnd ---

func TestFullPayloadRefCarriedThroughEvents(t *testing.T) {
	ref := &agent.PayloadRef{Backend: "memory", Key: "k1", Size: 12345, MimeType: "application/json"}
	tool := agent.Raw(
		"refs",
		"returns a payload ref",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
			return agent.Result{Summary: "short summary", FullPayloadRef: ref}, nil
		},
	)
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "refs", `{}`),
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
	if end.FullPayloadRef == nil || end.FullPayloadRef.Key != "k1" {
		t.Errorf("EventToolEnd.FullPayloadRef not carried: %+v", end.FullPayloadRef)
	}

	snap := a.Snapshot()
	if len(snap.ToolLog) == 0 || snap.ToolLog[0].FullPayloadRef == nil ||
		snap.ToolLog[0].FullPayloadRef.Size != 12345 {
		t.Errorf("ToolLogEntry.FullPayloadRef not carried: %+v", snap.ToolLog)
	}
}

// --- EnableFetchToolResult validation ---

func TestNewRejectsEnableFetchWithoutResolver(t *testing.T) {
	_, err := agent.New(agent.Config{
		LLM:                   &fakeLLM{},
		Model:                 "test",
		EnableFetchToolResult: true,
	})
	if err == nil || !strings.Contains(err.Error(), "PayloadResolver") {
		t.Errorf("want PayloadResolver-required error; got %v", err)
	}
}

func TestNewRejectsReservedToolName(t *testing.T) {
	collidingTool := agent.Raw(
		"fetch_tool_result", // reserved when EnableFetchToolResult is on
		"squat",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
			return agent.Result{Summary: ""}, nil
		},
	)
	_, err := agent.New(agent.Config{
		LLM:                   &fakeLLM{},
		Model:                 "test",
		Tools:                 []agent.AgentTool{collidingTool},
		EnableFetchToolResult: true,
		PayloadResolver:       &agent.MemoryPayloadResolver{Payloads: map[string]string{}},
	})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("want reserved-name error; got %v", err)
	}
}

// --- fetch_tool_result end-to-end ---

func TestFetchToolResultResolvesPriorPayload(t *testing.T) {
	// Iter 1: caller's tool emits Summary + FullPayloadRef.
	// Iter 2: model calls fetch_tool_result with call_index=1.
	// Iter 3: model produces final answer.
	resolver := &agent.MemoryPayloadResolver{
		Payloads: map[string]string{"k1": "the full payload body"},
	}
	bigTool := agent.Raw(
		"big",
		"returns ref",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
			return agent.Result{
				Summary:        "summary; ref available",
				FullPayloadRef: &agent.PayloadRef{Backend: "memory", Key: "k1"},
			}, nil
		},
	)
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "big", `{}`),
			toolCallScript("call_2", "fetch_tool_result", `{"call_index":1}`),
			textOnlyScript("final"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:                   fake,
		Model:                 "test",
		Tools:                 []agent.AgentTool{bigTool},
		EnableFetchToolResult: true,
		PayloadResolver:       resolver,
	})
	events, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// The second EventToolEnd is the fetch result.
	var ends []agent.EventToolEnd
	for _, ev := range events {
		if e, ok := ev.(agent.EventToolEnd); ok {
			ends = append(ends, e)
		}
	}
	if len(ends) != 2 {
		t.Fatalf("got %d EventToolEnd, want 2", len(ends))
	}
	if ends[1].Name != "fetch_tool_result" {
		t.Errorf("second tool call should be fetch_tool_result; got %q", ends[1].Name)
	}
	if ends[1].IsError {
		t.Errorf("fetch result was an error: %q", ends[1].Result)
	}
	if ends[1].Result != "the full payload body" {
		t.Errorf("fetch returned %q, want %q", ends[1].Result, "the full payload body")
	}
}

func TestFetchToolResultOutOfRange(t *testing.T) {
	resolver := &agent.MemoryPayloadResolver{Payloads: map[string]string{}}
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "fetch_tool_result", `{"call_index":99}`),
			textOnlyScript("ack"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:                   fake,
		Model:                 "test",
		EnableFetchToolResult: true,
		PayloadResolver:       resolver,
	})
	events, _ := collect(t, a.Run(context.Background(), "go"))
	var end agent.EventToolEnd
	for _, ev := range events {
		if e, ok := ev.(agent.EventToolEnd); ok {
			end = e
		}
	}
	if !end.IsError {
		t.Errorf("out-of-range fetch should be IsError; got %+v", end)
	}
	if !strings.Contains(end.Result, "out of range") {
		t.Errorf("error message should mention out-of-range; got %q", end.Result)
	}
}

func TestFetchToolResultResolverErrorSurfaces(t *testing.T) {
	resolver := &agent.MemoryPayloadResolver{Payloads: map[string]string{}} // empty -> miss
	bigTool := agent.Raw(
		"big",
		"returns ref to a key the resolver won't find",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
			return agent.Result{
				Summary:        "s",
				FullPayloadRef: &agent.PayloadRef{Backend: "memory", Key: "missing"},
			}, nil
		},
	)
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "big", `{}`),
			toolCallScript("call_2", "fetch_tool_result", `{"call_index":1}`),
			textOnlyScript("ack"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:                   fake,
		Model:                 "test",
		Tools:                 []agent.AgentTool{bigTool},
		EnableFetchToolResult: true,
		PayloadResolver:       resolver,
	})
	events, _ := collect(t, a.Run(context.Background(), "go"))
	var ends []agent.EventToolEnd
	for _, ev := range events {
		if e, ok := ev.(agent.EventToolEnd); ok {
			ends = append(ends, e)
		}
	}
	if len(ends) < 2 || !ends[1].IsError {
		t.Fatalf("expected fetch error result; got %+v", ends)
	}
	if !strings.Contains(ends[1].Result, "missing") {
		t.Errorf("expected resolver error in result; got %q", ends[1].Result)
	}
}

// --- MemoryPayloadResolver direct unit tests ---

func TestMemoryPayloadResolverHitMissWrongBackend(t *testing.T) {
	r := &agent.MemoryPayloadResolver{
		Payloads: map[string]string{"k1": "v1"},
	}

	if got, err := r.Resolve(context.Background(), agent.PayloadRef{Backend: "memory", Key: "k1"}, ""); err != nil || got != "v1" {
		t.Errorf("hit: got=%q err=%v", got, err)
	}
	if _, err := r.Resolve(context.Background(), agent.PayloadRef{Backend: "memory", Key: "missing"}, ""); err == nil {
		t.Error("miss: expected error")
	}
	if _, err := r.Resolve(context.Background(), agent.PayloadRef{Backend: "s3", Key: "k1"}, ""); err == nil ||
		!errors.Is(err, err) /* trivially true; just want non-nil */ {
		t.Error("wrong-backend: expected error")
	}

	empty := &agent.MemoryPayloadResolver{}
	if _, err := empty.Resolve(context.Background(), agent.PayloadRef{Key: "k1"}, ""); err == nil {
		t.Error("nil-payloads: expected error")
	}
}
