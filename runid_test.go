package agent_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	agent "github.com/amit-timalsina/pi-agent-go"
	llm "github.com/amit-timalsina/pi-llm-go"
)

// TestRunIDFromContext_EmptyOnBareContext pins the zero-value contract:
// a ctx that never touched the agent loop returns "" — no panic, no
// magic, just empty. Callers in tests / library code rely on this.
func TestRunIDFromContext_EmptyOnBareContext(t *testing.T) {
	if got := agent.RunIDFromContext(context.Background()); got != "" {
		t.Errorf("bare ctx: RunIDFromContext=%q, want \"\"", got)
	}
}

// TestWithRunID_EmptyIsNoOp verifies that WithRunID("") returns the
// same ctx — we don't stash empty values.
func TestWithRunID_EmptyIsNoOp(t *testing.T) {
	parent := context.Background()
	derived := agent.WithRunID(parent, "")
	if derived != parent {
		t.Errorf("WithRunID(\"\") must return the input ctx unchanged")
	}
}

// TestWithRunID_RoundTrip pins the basic helper round-trip.
func TestWithRunID_RoundTrip(t *testing.T) {
	ctx := agent.WithRunID(context.Background(), "run_abc123")
	if got := agent.RunIDFromContext(ctx); got != "run_abc123" {
		t.Errorf("round-trip: got=%q, want run_abc123", got)
	}
}

// TestRunIDFromContext_AvailableInToolHandler verifies the agent loop
// decorates the ctx it hands tool Handlers with the active RunID,
// matching what EventRunStart carried.
func TestRunIDFromContext_AvailableInToolHandler(t *testing.T) {
	var observedFromHandler string
	tool := agent.Raw(
		"snitch",
		"records the RunID it sees",
		json.RawMessage(`{"type":"object"}`),
		func(ctx context.Context, _ json.RawMessage) (agent.Result, error) {
			observedFromHandler = agent.RunIDFromContext(ctx)
			return agent.Result{Summary: "ok"}, nil
		},
	)
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "snitch", `{}`),
			textOnlyScript("done"),
		},
	}
	a, _ := agent.New(agent.Config{LLM: fake, Model: "test", Tools: []agent.AgentTool{tool}})

	var runStartID string
	for ev, err := range a.Run(context.Background(), "go") {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if e, ok := ev.(agent.EventRunStart); ok {
			runStartID = e.RunID
		}
	}

	if runStartID == "" {
		t.Fatal("no EventRunStart observed")
	}
	if observedFromHandler == "" {
		t.Fatalf("tool handler saw empty RunID; want %q", runStartID)
	}
	if observedFromHandler != runStartID {
		t.Errorf("tool handler RunID=%q != EventRunStart.RunID=%q", observedFromHandler, runStartID)
	}
}

// TestRunIDFromContext_AvailableInAllHooks verifies the same ctx
// decoration reaches every hook — BeforeToolCall, AfterToolCall,
// OnSteering, TransformContext.
func TestRunIDFromContext_AvailableInAllHooks(t *testing.T) {
	var (
		beforeID    string
		afterID     string
		steeringID  string
		transformID string
	)

	tool := agent.Raw(
		"noop",
		"does nothing",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
			return agent.Result{Summary: "ok"}, nil
		},
	)
	a, _ := agent.New(agent.Config{
		LLM: &fakeLLM{scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "noop", `{}`),
			textOnlyScript("done"),
		}},
		Model: "test",
		Tools: []agent.AgentTool{tool},
		BeforeToolCall: func(ctx context.Context, _ agent.RunContext, _ agent.ToolCallInfo) (bool, string, error) {
			beforeID = agent.RunIDFromContext(ctx)
			return false, "", nil
		},
		AfterToolCall: func(ctx context.Context, _ agent.RunContext, _ agent.ToolCallInfo, _ agent.Result, _ bool) (*agent.Result, error) {
			afterID = agent.RunIDFromContext(ctx)
			return nil, nil
		},
		OnSteering: func(ctx context.Context, _ agent.RunContext, _ llm.Message) (bool, error) {
			steeringID = agent.RunIDFromContext(ctx)
			return false, nil
		},
		TransformContext: func(ctx context.Context, messages []llm.Message) ([]llm.Message, error) {
			if transformID == "" {
				transformID = agent.RunIDFromContext(ctx)
			}
			return messages, nil
		},
	})

	// Steer BEFORE running so the steering channel has a message
	// queued; drainSteering picks it up on iteration 1's pre-flight,
	// which is where OnSteering fires.
	if err := a.Steer(context.Background(), llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.Block{llm.TextBlock{Text: "nudge"}},
	}); err != nil {
		t.Fatalf("Steer: %v", err)
	}

	var runStartID string
	for ev, err := range a.Run(context.Background(), "go") {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if e, ok := ev.(agent.EventRunStart); ok {
			runStartID = e.RunID
		}
	}

	if runStartID == "" {
		t.Fatal("no EventRunStart observed")
	}
	cases := map[string]string{
		"BeforeToolCall":   beforeID,
		"AfterToolCall":    afterID,
		"OnSteering":       steeringID,
		"TransformContext": transformID,
	}
	for hookName, got := range cases {
		if got == "" {
			// OnSteering is the most fragile of the four: it only
			// fires if drainSteering pulls the steering message off
			// the buffered channel before iteration 1 starts. If
			// that pre-flight drain ever moves (e.g. a future
			// refactor sequences steering AFTER the first LLM call),
			// this test silently passes the OnSteering check by
			// never invoking the hook. Spell out the diagnostic so
			// the failure mode is unambiguous.
			t.Errorf("%s saw empty RunID; want %q (was the hook invoked at all? check steering drain ordering if hookName=OnSteering)",
				hookName, runStartID)
		} else if got != runStartID {
			t.Errorf("%s RunID=%q != EventRunStart.RunID=%q", hookName, got, runStartID)
		}
	}
}

// TestRunIDFromContext_AvailableInParallelHandlers verifies the ctx
// decoration survives the errgroup + context.WithCancel chain used
// by executeToolCallsParallel — a regression guard against any
// future refactor that derives the parallel ctx from
// context.Background() instead of the loop's decorated ctx.
func TestRunIDFromContext_AvailableInParallelHandlers(t *testing.T) {
	var (
		mu       sync.Mutex
		observed = map[string]string{} // call_id -> RunID seen
	)
	makeTool := func(name string) agent.AgentTool {
		return agent.Raw(
			name,
			"records the RunID it sees",
			json.RawMessage(`{"type":"object"}`),
			func(ctx context.Context, _ json.RawMessage) (agent.Result, error) {
				mu.Lock()
				observed[name] = agent.RunIDFromContext(ctx)
				mu.Unlock()
				return agent.Result{Summary: "ok"}, nil
			},
		)
	}
	tools := []agent.AgentTool{makeTool("a"), makeTool("b"), makeTool("c")}

	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "a", `{}`},
				struct{ ID, Name, Args string }{"2", "b", `{}`},
				struct{ ID, Name, Args string }{"3", "c", `{}`},
			),
			textOnlyScript("done"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         tools,
		ToolExecution: agent.ToolExecutionParallel,
	})

	var runStartID string
	for ev, err := range a.Run(context.Background(), "go") {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if e, ok := ev.(agent.EventRunStart); ok {
			runStartID = e.RunID
		}
	}

	if runStartID == "" {
		t.Fatal("no EventRunStart observed")
	}
	for _, name := range []string{"a", "b", "c"} {
		got := observed[name]
		if got == "" {
			t.Errorf("parallel handler %q saw empty RunID; want %q (parallel ctx may have lost the WithRunID decoration)", name, runStartID)
		} else if got != runStartID {
			t.Errorf("parallel handler %q RunID=%q != EventRunStart.RunID=%q", name, got, runStartID)
		}
	}
}
