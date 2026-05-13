// streaming_tool: demonstrates EmitToolDelta for surfacing progress
// from a long-running tool Handler. The model only sees the final
// Result.Summary; observers (this main loop) see each delta as it's
// emitted.
//
// Usage:
//
//	export ANTHROPIC_API_KEY=...
//	go run ./examples/streaming_tool
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	agent "github.com/amit-timalsina/pi-agent-go"
	llm "github.com/amit-timalsina/pi-llm-go"
	"github.com/amit-timalsina/pi-llm-go/providers/anthropic"
)

// CountToArgs is the input schema for the count_to tool.
type CountToArgs struct {
	N int `json:"n" jsonschema:"description=Count from 1 to N inclusive."`
}

func main() {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "ANTHROPIC_API_KEY is required")
		os.Exit(2)
	}
	provider, err := anthropic.New(anthropic.Options{APIKey: apiKey})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// A "long-running" tool that emits one delta per second while
	// counting. Observers see each tick in real time; the model
	// receives only the final summary line.
	countTool := agent.Raw(
		"count_to",
		"Count from 1 to N with a 1s delay between numbers; emit progress.",
		json.RawMessage(`{
			"type":"object",
			"properties":{"n":{"type":"integer","minimum":1,"maximum":10}},
			"required":["n"],
			"additionalProperties":false
		}`),
		func(ctx context.Context, raw json.RawMessage) (agent.Result, error) {
			var in CountToArgs
			if err := json.Unmarshal(raw, &in); err != nil {
				return agent.Result{}, err
			}
			var collected []string
			for i := 1; i <= in.N; i++ {
				select {
				case <-ctx.Done():
					return agent.Result{}, ctx.Err()
				case <-time.After(time.Second):
				}
				line := fmt.Sprintf("counted: %d/%d", i, in.N)
				agent.EmitToolDelta(ctx, line)
				collected = append(collected, line)
			}
			return agent.Result{Summary: strings.Join(collected, "\n")}, nil
		},
	)

	a, err := agent.New(agent.Config{
		LLM:           provider,
		Model:         anthropic.ClaudeHaiku4_5,
		SystemPrompt:  "You are a concise assistant. When asked to count, call the count_to tool.",
		Tools:         []agent.AgentTool{countTool},
		MaxIterations: 4,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	for ev, err := range a.Run(context.Background(), "Count to 3, then tell me you're done.") {
		if err != nil {
			fmt.Fprintln(os.Stderr, "run error:", err)
			os.Exit(1)
		}
		switch e := ev.(type) {
		case agent.EventToolStart:
			fmt.Printf("[tool start] %s\n", e.Name)
		case agent.EventToolDelta:
			fmt.Printf("  [tool delta] %s\n", e.Delta)
		case agent.EventToolEnd:
			fmt.Printf("[tool end]   %s (error=%v)\n", e.Name, e.IsError)
		case agent.EventAssistantMessage:
			for _, b := range e.Message.Content {
				if tb, ok := b.(llm.TextBlock); ok {
					fmt.Printf("[assistant] %s\n", tb.Text)
				}
			}
		case agent.EventRunEnd:
			fmt.Printf("[run end] iterations=%d\n", e.Iterations)
		}
	}
}
