package agent_test

import (
	"context"
	"testing"

	agent "github.com/amit-timalsina/pi-agent-go"
	llm "github.com/amit-timalsina/pi-llm-go"
)

// TestCacheRetention_ForwardedToEveryRequest verifies that
// agent.Config.CacheRetention propagates into llm.Request.CacheRetention
// on EVERY iteration's LLM call. Closes issue #25.
//
// Without this forwarding the dominant cost lever for tool-heavy
// agents (Anthropic prompt caching of system + tools, ~10× input
// rate reduction after the first hit) is unreachable.
func TestCacheRetention_ForwardedToEveryRequest(t *testing.T) {
	t.Parallel()

	// Two iterations: assistant turn -> tool call -> tool result -> assistant turn.
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call-1", "echo", `{"text":"hi"}`),
			textOnlyScript("ok"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:            fake,
		Model:          "test",
		Tools:          []agent.AgentTool{echoTool()},
		CacheRetention: llm.CacheRetentionLong,
	})

	if _, err := collect(t, a.Run(context.Background(), "go")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fake.requests) != 2 {
		t.Fatalf("expected 2 LLM requests, got %d", len(fake.requests))
	}
	for i, req := range fake.requests {
		if req.CacheRetention != llm.CacheRetentionLong {
			t.Errorf("iteration %d: CacheRetention=%q, want %q",
				i+1, req.CacheRetention, llm.CacheRetentionLong)
		}
	}
}

// TestCacheRetention_ZeroValueIsCacheRetentionNone verifies the
// backward-compat default: callers who never set CacheRetention get
// the existing no-cache behavior (CacheRetentionNone == "").
func TestCacheRetention_ZeroValueIsCacheRetentionNone(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{scripts: [][]llm.StreamEvent{textOnlyScript("done")}}
	a, _ := agent.New(agent.Config{
		LLM:   fake,
		Model: "test",
		// CacheRetention deliberately omitted.
	})
	if _, err := collect(t, a.Run(context.Background(), "go")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("expected 1 LLM request, got %d", len(fake.requests))
	}
	if got := fake.requests[0].CacheRetention; got != llm.CacheRetentionNone {
		t.Errorf("CacheRetention=%q, want %q (zero-value backward compat)",
			got, llm.CacheRetentionNone)
	}
}

// TestCacheRetention_ShortAlsoForwarded verifies the short-TTL path
// (5min). Same forwarding, different value — guards against a
// hypothetical regression that only forwards Long.
func TestCacheRetention_ShortAlsoForwarded(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{scripts: [][]llm.StreamEvent{textOnlyScript("done")}}
	a, _ := agent.New(agent.Config{
		LLM:            fake,
		Model:          "test",
		CacheRetention: llm.CacheRetentionShort,
	})
	if _, err := collect(t, a.Run(context.Background(), "go")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fake.requests[0].CacheRetention; got != llm.CacheRetentionShort {
		t.Errorf("CacheRetention=%q, want %q", got, llm.CacheRetentionShort)
	}
}
