package agent_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	agent "github.com/amit-timalsina/pi-agent-go"
	llm "github.com/amit-timalsina/pi-llm-go"
)

// TestSetSystemPromptTakesEffectOnNextIteration verifies the full
// contract: iteration 1 sees the initial prompt, SetSystemPrompt fired
// between iter 1's LLM call and iter 2's buildRequest takes effect on
// iter 2's wire request.
//
// Determinism note: BeforeToolCall is the only point at which the loop
// guarantees ordering between iter N's LLM call (already completed) and
// iter N+1's buildRequest (yet to fire). Mutating from inside the hook
// is the right way to test this — racing a goroutine against the run
// loop would let the test pass even if SetSystemPrompt were buggy
// (because the write might happen to land before iter 1).
func TestSetSystemPromptTakesEffectOnNextIteration(t *testing.T) {
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "echo", `{"text":"hi"}`), // iter 1
			textOnlyScript("done"),                            // iter 2
		},
	}
	var a *agent.Agent
	a, _ = agent.New(agent.Config{
		LLM:          fake,
		Model:        "test",
		SystemPrompt: "initial-system",
		Tools:        []agent.AgentTool{echoTool()},
		BeforeToolCall: func(_ context.Context, _ agent.RunContext, _ agent.ToolCallInfo) (bool, string, error) {
			// Fires after iter 1's LLM call completes and before iter 2's
			// buildRequest. Mutating here pins the "next iteration sees
			// the update" contract without any cross-goroutine race.
			a.SetSystemPrompt("updated-system")
			return false, "", nil
		},
	})
	if _, err := collect(t, a.Run(context.Background(), "go")); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(fake.requests) != 2 {
		t.Fatalf("expected 2 LLM requests, got %d", len(fake.requests))
	}
	if fake.requests[0].System != "initial-system" {
		t.Errorf("iter 1 System=%q, want %q (pin the initial value, not just the update)", fake.requests[0].System, "initial-system")
	}
	if fake.requests[1].System != "updated-system" {
		t.Errorf("iter 2 System=%q, want %q", fake.requests[1].System, "updated-system")
	}
	if got := a.SystemPrompt(); got != "updated-system" {
		t.Errorf("SystemPrompt()=%q, want updated-system", got)
	}
}

// TestSetSystemPromptInitialValueFromConfig verifies the constructor
// initializes the live system prompt from Config.SystemPrompt and that
// the first iteration sees it on the wire.
func TestSetSystemPromptInitialValueFromConfig(t *testing.T) {
	fake := &fakeLLM{scripts: [][]llm.StreamEvent{textOnlyScript("ok")}}
	a, _ := agent.New(agent.Config{
		LLM:          fake,
		Model:        "test",
		SystemPrompt: "hello-system",
	})

	if got := a.SystemPrompt(); got != "hello-system" {
		t.Errorf("SystemPrompt()=%q, want hello-system", got)
	}
	if _, err := collect(t, a.Run(context.Background(), "go")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.requests[0].System != "hello-system" {
		t.Errorf("Request.System=%q, want hello-system", fake.requests[0].System)
	}
}

// TestSetSystemPromptConcurrentReadAndWrite proves SetSystemPrompt is
// safe to call from another goroutine while a run is in flight. Race
// detector will catch ordering bugs.
func TestSetSystemPromptConcurrentReadAndWrite(t *testing.T) {
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			textOnlyScript("ok"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM: fake, Model: "test", SystemPrompt: "v0",
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			a.SetSystemPrompt(fmt.Sprintf("v%d", i))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = a.SystemPrompt()
			_ = a.Snapshot()
		}
	}()
	wg.Wait()

	if _, err := collect(t, a.Run(context.Background(), "go")); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// TestTransformContextAppliedEveryIteration verifies the hook is called
// at the top of every iteration with the live transcript, and that its
// returned slice (not the original) is used in the request.
func TestTransformContextAppliedEveryIteration(t *testing.T) {
	var calls int
	// observedCounts[i] is the transcript length the hook saw on call i+1.
	// Pins the "transform sees the real growing transcript" contract:
	// iter 1: user (1)
	// iter 2: user, assistant-with-tool-call, tool-result (3)
	var observedCounts []int
	transform := func(_ context.Context, messages []llm.Message) ([]llm.Message, error) {
		calls++
		observedCounts = append(observedCounts, len(messages))
		// Append a synthetic user message that the LLM should see but
		// that should NOT be persisted in the durable transcript.
		out := make([]llm.Message, len(messages), len(messages)+1)
		copy(out, messages)
		out = append(out, llm.Message{
			Role: llm.RoleUser,
			Content: []llm.Block{llm.TextBlock{
				Text: fmt.Sprintf("synthetic-iter-%d", calls),
			}},
		})
		return out, nil
	}

	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "echo", `{"text":"first"}`),
			textOnlyScript("final"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:              fake,
		Model:            "test",
		Tools:            []agent.AgentTool{echoTool()},
		TransformContext: transform,
	})
	if _, err := collect(t, a.Run(context.Background(), "go")); err != nil {
		t.Fatalf("run: %v", err)
	}

	if calls != 2 {
		t.Errorf("TransformContext called %d times, want 2 (once per iteration)", calls)
	}
	wantCounts := []int{1, 3}
	if len(observedCounts) != len(wantCounts) ||
		observedCounts[0] != wantCounts[0] ||
		observedCounts[1] != wantCounts[1] {
		t.Errorf("transcript size per iter = %v, want %v (iter 1: user; iter 2: user+assistant+tool-result)",
			observedCounts, wantCounts)
	}

	// Every request must contain the synthetic message as the LAST entry.
	for i, req := range fake.requests {
		last := req.Messages[len(req.Messages)-1]
		if last.Role != llm.RoleUser {
			t.Errorf("iter %d: last message role=%v, want user", i+1, last.Role)
			continue
		}
		tb, ok := last.Content[0].(llm.TextBlock)
		if !ok || !strings.HasPrefix(tb.Text, "synthetic-iter-") {
			t.Errorf("iter %d: transform-injected block missing; got %+v", i+1, last.Content)
		}
	}

	// The DURABLE transcript stored on the agent must NOT contain the
	// synthetic messages — only the real user prompt, the assistant
	// turn(s), and the tool result.
	snap := a.Snapshot()
	for _, m := range snap.Messages {
		for _, b := range m.Content {
			if tb, ok := b.(llm.TextBlock); ok && strings.HasPrefix(tb.Text, "synthetic-iter-") {
				t.Errorf("durable transcript leaked synthetic block: %+v", tb)
			}
		}
	}
}

// TestTransformContextErrorAbortsRun verifies an error from the hook
// terminates the run and wraps ErrTransformContext.
func TestTransformContextErrorAbortsRun(t *testing.T) {
	sentinel := errors.New("pruner exploded")
	transform := func(_ context.Context, messages []llm.Message) ([]llm.Message, error) {
		return nil, sentinel
	}
	fake := &fakeLLM{scripts: [][]llm.StreamEvent{textOnlyScript("never reached")}}
	a, _ := agent.New(agent.Config{
		LLM:              fake,
		Model:            "test",
		TransformContext: transform,
	})
	_, err := collect(t, a.Run(context.Background(), "go"))
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	if !errors.Is(err, agent.ErrTransformContext) {
		t.Errorf("error %v does not wrap ErrTransformContext", err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error %v does not wrap the underlying sentinel", err)
	}
}

// TestTransformContextNilSliceAbortsRun verifies that the no-nil-slice
// contract is enforced — a hook that returns (nil, nil) is a caller bug
// and should surface, not silently drop the transcript.
func TestTransformContextNilSliceAbortsRun(t *testing.T) {
	transform := func(_ context.Context, _ []llm.Message) ([]llm.Message, error) {
		return nil, nil
	}
	fake := &fakeLLM{scripts: [][]llm.StreamEvent{textOnlyScript("never reached")}}
	a, _ := agent.New(agent.Config{
		LLM: fake, Model: "test", TransformContext: transform,
	})
	_, err := collect(t, a.Run(context.Background(), "go"))
	if !errors.Is(err, agent.ErrTransformContext) {
		t.Errorf("nil-slice contract violation should wrap ErrTransformContext; got %v", err)
	}
}

// TestNoTransformContextLeavesMessagesUntouched verifies the default
// path (no hook) is unchanged from previous behavior.
func TestNoTransformContextLeavesMessagesUntouched(t *testing.T) {
	fake := &fakeLLM{scripts: [][]llm.StreamEvent{textOnlyScript("hi")}}
	a, _ := agent.New(agent.Config{LLM: fake, Model: "test"})
	if _, err := collect(t, a.Run(context.Background(), "go")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(fake.requests[0].Messages) != 1 {
		t.Errorf("expected 1 message (the user prompt); got %d", len(fake.requests[0].Messages))
	}
}
