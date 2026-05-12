package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agent "github.com/amit-timalsina/pi-agent-go"
	llm "github.com/amit-timalsina/pi-llm-go"
)

// TestRestore_BasicRoundTrip verifies the core contract: snapshot →
// Restore → the new Agent's Snapshot matches the original on the
// load-bearing fields (messages, tool log, system prompt, usage,
// runID).
func TestRestore_BasicRoundTrip(t *testing.T) {
	fake := &fakeLLM{scripts: [][]llm.StreamEvent{textOnlyScript("Hello!")}}
	original, _ := agent.New(agent.Config{
		LLM:          fake,
		Model:        "test",
		SystemPrompt: "you are helpful",
	})
	if _, err := collect(t, original.Run(context.Background(), "hi")); err != nil {
		t.Fatalf("run: %v", err)
	}
	snap := original.Snapshot()

	restored, err := agent.Restore(agent.Config{
		LLM:          &fakeLLM{}, // fresh; restore doesn't reuse the prior provider state
		Model:        "test",
		SystemPrompt: "this will be overridden by snap.SystemPrompt",
	}, snap)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got := restored.Snapshot()

	if got.RunID != snap.RunID {
		t.Errorf("restored RunID=%q, want %q", got.RunID, snap.RunID)
	}
	if got.SystemPrompt != snap.SystemPrompt {
		t.Errorf("restored SystemPrompt=%q, want %q", got.SystemPrompt, snap.SystemPrompt)
	}
	if got.Iteration != snap.Iteration {
		t.Errorf("restored Iteration=%d, want %d", got.Iteration, snap.Iteration)
	}
	if len(got.Messages) != len(snap.Messages) {
		t.Errorf("restored Messages len=%d, want %d", len(got.Messages), len(snap.Messages))
	}
	if got.LastUsage != snap.LastUsage {
		t.Errorf("restored LastUsage=%+v, want %+v", got.LastUsage, snap.LastUsage)
	}
	if got.IsRunning {
		t.Errorf("restored agent should not be running")
	}
}

// TestRestore_RejectsRunning verifies you can't restore an agent that
// was captured mid-run. The snap.IsRunning flag is set by Snapshot()
// when called concurrently with an in-flight Run; restoring such a
// snapshot is undefined and we error out cleanly.
func TestRestore_RejectsRunning(t *testing.T) {
	snap := agent.RunSnapshot{
		RunID:     "run_abc",
		IsRunning: true,
	}
	_, err := agent.Restore(agent.Config{LLM: &fakeLLM{}, Model: "test"}, snap)
	if err == nil {
		t.Fatal("expected error when snap.IsRunning=true; got nil")
	}
	if !strings.Contains(err.Error(), "mid-flight") {
		t.Errorf("error %q should mention mid-flight", err.Error())
	}
}

// TestRestore_PreservesSystemPromptMutation verifies that
// SetSystemPrompt mutations on the original agent round-trip through
// snapshot + restore (the snap's SystemPrompt wins over cfg's).
func TestRestore_PreservesSystemPromptMutation(t *testing.T) {
	fake := &fakeLLM{scripts: [][]llm.StreamEvent{textOnlyScript("ok")}}
	original, _ := agent.New(agent.Config{
		LLM:          fake,
		Model:        "test",
		SystemPrompt: "original-prompt",
	})
	original.SetSystemPrompt("mutated-prompt")
	if _, err := collect(t, original.Run(context.Background(), "hi")); err != nil {
		t.Fatalf("run: %v", err)
	}
	snap := original.Snapshot()

	restored, err := agent.Restore(agent.Config{
		LLM:          &fakeLLM{},
		Model:        "test",
		SystemPrompt: "different-cfg-prompt",
	}, snap)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := restored.SystemPrompt(); got != "mutated-prompt" {
		t.Errorf("restored SystemPrompt=%q, want mutated-prompt (snap wins over cfg)", got)
	}
}

// TestRestore_EmptyPromptFallsThroughToConfig verifies the fallback
// path: an empty snap.SystemPrompt means "use cfg.SystemPrompt"
// instead of clobbering with an empty string.
func TestRestore_EmptyPromptFallsThroughToConfig(t *testing.T) {
	snap := agent.RunSnapshot{
		RunID:        "run_abc",
		SystemPrompt: "",
	}
	restored, err := agent.Restore(agent.Config{
		LLM:          &fakeLLM{},
		Model:        "test",
		SystemPrompt: "config-prompt",
	}, snap)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := restored.SystemPrompt(); got != "config-prompt" {
		t.Errorf("restored SystemPrompt=%q, want config-prompt (empty snap should fall through)", got)
	}
}

// TestRestore_PreservesToolLog verifies ToolLog round-trips intact.
func TestRestore_PreservesToolLog(t *testing.T) {
	echo := agent.Raw(
		"echo", "echo",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
			return agent.Result{Summary: "echoed"}, nil
		},
	)
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "echo", `{}`),
			textOnlyScript("done"),
		},
	}
	original, _ := agent.New(agent.Config{LLM: fake, Model: "test", Tools: []agent.AgentTool{echo}})
	if _, err := collect(t, original.Run(context.Background(), "go")); err != nil {
		t.Fatalf("run: %v", err)
	}
	snap := original.Snapshot()

	restored, err := agent.Restore(agent.Config{
		LLM:   &fakeLLM{},
		Model: "test",
		Tools: []agent.AgentTool{echo},
	}, snap)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	rsnap := restored.Snapshot()
	if len(rsnap.ToolLog) != len(snap.ToolLog) {
		t.Fatalf("restored ToolLog len=%d, want %d", len(rsnap.ToolLog), len(snap.ToolLog))
	}
	if len(rsnap.ToolLog) == 0 {
		t.Fatal("expected at least one ToolLog entry")
	}
	if rsnap.ToolLog[0].ToolCallID != snap.ToolLog[0].ToolCallID {
		t.Errorf("restored ToolLog[0].ToolCallID=%q, want %q", rsnap.ToolLog[0].ToolCallID, snap.ToolLog[0].ToolCallID)
	}
	if rsnap.ToolLog[0].Result != snap.ToolLog[0].Result {
		t.Errorf("restored ToolLog[0].Result=%q, want %q", rsnap.ToolLog[0].Result, snap.ToolLog[0].Result)
	}
}

// TestRestore_NextRunGetsFreshRunID verifies the snap.RunID is just
// metadata; the next Run generates a brand-new RunID.
func TestRestore_NextRunGetsFreshRunID(t *testing.T) {
	fake := &fakeLLM{scripts: [][]llm.StreamEvent{textOnlyScript("first")}}
	original, _ := agent.New(agent.Config{LLM: fake, Model: "test"})
	if _, err := collect(t, original.Run(context.Background(), "hi")); err != nil {
		t.Fatalf("original run: %v", err)
	}
	snap := original.Snapshot()
	priorRunID := snap.RunID

	restored, err := agent.Restore(agent.Config{
		LLM:   &fakeLLM{scripts: [][]llm.StreamEvent{textOnlyScript("second")}},
		Model: "test",
	}, snap)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	var newRunStartID string
	for ev, err := range restored.Run(context.Background(), "follow up") {
		if err != nil {
			t.Fatalf("restored run: %v", err)
		}
		if e, ok := ev.(agent.EventRunStart); ok {
			newRunStartID = e.RunID
		}
	}
	if newRunStartID == "" {
		t.Fatal("no EventRunStart from restored run")
	}
	if newRunStartID == priorRunID {
		t.Errorf("restored run reused prior RunID %q; want a fresh one", priorRunID)
	}
}

// TestRestore_ContinuesConversation verifies the next LLM call after
// restore sees the full prior transcript as context (the model gets
// the original user turn + assistant reply + the new follow-up).
func TestRestore_ContinuesConversation(t *testing.T) {
	fake1 := &fakeLLM{scripts: [][]llm.StreamEvent{textOnlyScript("first answer")}}
	original, _ := agent.New(agent.Config{LLM: fake1, Model: "test"})
	if _, err := collect(t, original.Run(context.Background(), "first question")); err != nil {
		t.Fatalf("original run: %v", err)
	}
	snap := original.Snapshot()

	// Restore + new fake that captures every request.
	fake2 := &fakeLLM{scripts: [][]llm.StreamEvent{textOnlyScript("second answer")}}
	restored, err := agent.Restore(agent.Config{LLM: fake2, Model: "test"}, snap)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := collect(t, restored.Run(context.Background(), "follow-up")); err != nil {
		t.Fatalf("restored run: %v", err)
	}

	if len(fake2.requests) != 1 {
		t.Fatalf("fake2 got %d requests, want 1", len(fake2.requests))
	}
	msgs := fake2.requests[0].Messages
	// Expected wire transcript: original user, original assistant,
	// follow-up user — 3 messages.
	if len(msgs) != 3 {
		t.Fatalf("restored run sent %d messages, want 3 (continuation should include prior transcript)", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser {
		t.Errorf("msgs[0].Role=%v, want user (original question)", msgs[0].Role)
	}
	if msgs[1].Role != llm.RoleAssistant {
		t.Errorf("msgs[1].Role=%v, want assistant (original answer)", msgs[1].Role)
	}
	if msgs[2].Role != llm.RoleUser {
		t.Errorf("msgs[2].Role=%v, want user (follow-up)", msgs[2].Role)
	}
}

// TestRestore_ConfigValidationStillFires verifies that Restore goes
// through New first, so cfg-level validation (missing LLM, missing
// Model, duplicate tool names) still produces clear errors.
func TestRestore_ConfigValidationStillFires(t *testing.T) {
	snap := agent.RunSnapshot{RunID: "run_abc"}

	if _, err := agent.Restore(agent.Config{Model: "test"}, snap); err == nil {
		t.Error("expected error when Config.LLM is nil")
	}
	if _, err := agent.Restore(agent.Config{LLM: &fakeLLM{}}, snap); err == nil {
		t.Error("expected error when Config.Model is empty")
	}
}

// TestRestore_ZeroSnapEquivalentToNew verifies that restoring with an
// empty (zero-value) snap is equivalent to calling New — useful for
// callers that always go through Restore and pass a zero snap when
// starting fresh.
func TestRestore_ZeroSnapEquivalentToNew(t *testing.T) {
	a, err := agent.Restore(agent.Config{LLM: &fakeLLM{}, Model: "test", SystemPrompt: "fresh"}, agent.RunSnapshot{})
	if err != nil {
		t.Fatalf("Restore zero snap: %v", err)
	}
	snap := a.Snapshot()
	if len(snap.Messages) != 0 {
		t.Errorf("zero-snap restore produced %d messages; want 0", len(snap.Messages))
	}
	if snap.SystemPrompt != "fresh" {
		t.Errorf("zero-snap restore SystemPrompt=%q; want fresh (cfg should pass through)", snap.SystemPrompt)
	}
}

// TestRestore_DefensiveCopySliceMutationDoesntLeakBack verifies that
// mutating the original snapshot's slices after Restore doesn't
// affect the restored agent's state. Restore takes defensive copies.
func TestRestore_DefensiveCopySliceMutationDoesntLeakBack(t *testing.T) {
	fake := &fakeLLM{scripts: [][]llm.StreamEvent{textOnlyScript("hi")}}
	original, _ := agent.New(agent.Config{LLM: fake, Model: "test"})
	if _, err := collect(t, original.Run(context.Background(), "first")); err != nil {
		t.Fatalf("run: %v", err)
	}
	snap := original.Snapshot()

	restored, err := agent.Restore(agent.Config{LLM: &fakeLLM{}, Model: "test"}, snap)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Mutate the snap's slices after Restore.
	if len(snap.Messages) > 0 {
		snap.Messages[0] = llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock{Text: "TAMPERED"}}}
	}

	rsnap := restored.Snapshot()
	if len(rsnap.Messages) == 0 {
		t.Fatal("restored agent lost its messages")
	}
	first := rsnap.Messages[0]
	if len(first.Content) > 0 {
		if tb, ok := first.Content[0].(llm.TextBlock); ok && tb.Text == "TAMPERED" {
			t.Errorf("Restore did not take a defensive copy — caller mutation leaked into agent state")
		}
	}
}
