// terminate_early: demonstrates Result.Terminate for skipping the
// follow-up "model explains what just happened" LLM call when a tool's
// output IS the final answer (file-write, send-message, render-artifact,
// etc.).
//
// Usage:
//
//	export ANTHROPIC_API_KEY=...
//	go run ./examples/terminate_early
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	agent "github.com/amit-timalsina/pi-agent-go"
	"github.com/amit-timalsina/pi-llm-go/providers/anthropic"
)

type WriteFileArgs struct {
	Path    string `json:"path" jsonschema:"description=Filesystem path to write to."`
	Content string `json:"content" jsonschema:"description=Text content to write."`
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

	// A "write_file" tool that opts into Result.Terminate. Once it
	// completes successfully, the agent stops without making the
	// otherwise-inevitable follow-up turn that would just say "I wrote
	// the file."
	writeFile := agent.Typed[WriteFileArgs, string](
		"write_file",
		"Write the given content to the given path. Use this when the user asks to save / write / persist text.",
		func(_ context.Context, in WriteFileArgs) (string, error) {
			// In a real tool, you'd actually write to disk. Here we
			// just confirm the call shape. Return Terminate via the
			// AfterToolCall hook below — the handler signature
			// doesn't carry it on a typed serialize() return, so we
			// promote it post-hoc.
			return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), nil
		},
		func(s string) string { return s },
	)

	cfg := agent.Config{
		LLM:          provider,
		Model:        anthropic.ClaudeHaiku4_5,
		SystemPrompt: "You are a concise file-writing assistant. When asked to save / write text, call the write_file tool exactly once.",
		MaxTokens:    256,
		Tools:        []agent.AgentTool{writeFile},
		// AfterToolCall promotes successful tool results to terminate
		// the run. Three patterns are equivalent for this:
		//
		//   1. Tool handler returns agent.Result{Summary: ..., Terminate: true}
		//      directly. Use this when EVERY successful call should
		//      terminate.
		//   2. AfterToolCall sets Terminate based on per-call policy
		//      (the pattern below). Use when a guardrail above the
		//      tool decides termination.
		//   3. A wrapper tool struct that knows its own terminate
		//      policy and bakes it into the handler.
		AfterToolCall: func(_ context.Context, _ agent.RunContext, _ agent.ToolCallInfo, r agent.Result, isErr bool) (*agent.Result, error) {
			if isErr {
				// Don't terminate on errors — the model needs the
				// chance to recover.
				return nil, nil
			}
			r.Terminate = true
			return &r, nil
		},
	}
	a, err := agent.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	for ev, err := range a.Run(context.Background(), `Save "hello, terminate" to /tmp/demo.txt`) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "run error:", err)
			os.Exit(1)
		}
		switch e := ev.(type) {
		case agent.EventIterationStart:
			fmt.Printf("[iteration %d]\n", e.Iteration)
		case agent.EventToolStart:
			fmt.Printf("  → %s(%s)\n", e.Name, strings.TrimSpace(string(e.Arguments)))
		case agent.EventToolEnd:
			fmt.Printf("  ← %s: %s\n", e.Name, e.Result)
		case agent.EventRunEnd:
			fmt.Printf("[run ended after %d iteration(s)]\n", e.Iterations)
		}
	}
	// With Terminate working, you should see exactly ONE iteration:
	// the LLM call that issued the tool, followed by the tool result,
	// followed by run-end. There is NO second LLM call to "summarize."
}
