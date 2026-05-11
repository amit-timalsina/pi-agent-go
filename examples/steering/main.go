// steering: demonstrates mid-run steering injection from a goroutine.
//
// Scenario: ask the agent to count 1..10 by calling a `count` tool once per
// step. A watcher goroutine intercepts the third call and injects a
// steering message ("only count to 5 then stop") into the agent's
// steering channel. The agent picks up the steering at the next iteration
// boundary and finishes early.
//
// This shows:
//   - Agent.Steer is safe to call from another goroutine while Run() is
//     iterating.
//   - The injected message lands in the transcript at the next LLM call
//     boundary (not mid-call), so the model sees it as a fresh user
//     instruction.
//   - The agent loop respects the injected guidance for the rest of the
//     run.
//
//	export ANTHROPIC_API_KEY=...
//	go run ./examples/steering
package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	llm "github.com/amit-timalsina/pi-llm-go"
	"github.com/amit-timalsina/pi-llm-go/providers/anthropic"
	agent "github.com/amit-timalsina/pi-agent-go"
)

type CountArgs struct {
	N int `json:"n" jsonschema:"description=The current count number."`
}

func main() {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "ANTHROPIC_API_KEY is required")
		os.Exit(2)
	}
	provider, err := anthropic.New(anthropic.Options{APIKey: key})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// callSignal fires each time the count tool is invoked. The watcher
	// goroutine listens for it and injects steering after the Nth call.
	callSignal := make(chan int, 32)

	var mu sync.Mutex
	var lastCount int

	countTool := agent.Typed[CountArgs, int](
		"count",
		"Record the next number in a counting sequence. Pass n=1, n=2, ... in order.",
		func(_ context.Context, in CountArgs) (int, error) {
			mu.Lock()
			lastCount = in.N
			mu.Unlock()
			callSignal <- in.N
			fmt.Fprintf(os.Stderr, "[tool] count(%d)\n", in.N)
			return in.N, nil
		},
		func(n int) string { return fmt.Sprintf("counted: %d", n) },
	)

	a, err := agent.New(agent.Config{
		LLM:          provider,
		Model:        anthropic.ClaudeHaiku4_5,
		SystemPrompt: "You count using the `count` tool. Call it once per step (one tool call per assistant turn). Wait for the result before going to the next number.",
		Tools:        []agent.AgentTool{countTool},
		MaxTokens:    1024,
		// Generous iteration cap so the demo has room to count high before
		// steering kicks in.
		MaxIterations: 25,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Steering watcher: after the 3rd count call, inject a steering
	// message that asks the agent to stop at 5 and report.
	go func() {
		for n := range callSignal {
			if n == 3 {
				steerMsg := llm.Message{
					Role: llm.RoleUser,
					Content: []llm.Block{llm.TextBlock{
						Text: "Change of plan: stop at 5 instead of 10. Once you've counted to 5, summarize what you did.",
					}},
				}
				if err := a.Steer(context.Background(), steerMsg); err != nil {
					fmt.Fprintf(os.Stderr, "[steer] failed: %v\n", err)
				} else {
					fmt.Fprintln(os.Stderr, "[steer] injected: 'stop at 5 and summarize'")
				}
				return
			}
		}
	}()

	prompt := "Count from 1 to 10 by calling the count tool once per step. After you finish, tell me the final number."

	for event, err := range a.Run(context.Background(), prompt) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "\nerror:", err)
			os.Exit(1)
		}
		switch e := event.(type) {
		case agent.EventLLMStream:
			if d, ok := e.Event.(llm.EventTextDelta); ok {
				fmt.Print(d.Delta)
			}
		case agent.EventSteering:
			fmt.Fprintln(os.Stderr, "[event] EventSteering fired -- steering message appended to transcript")
		case agent.EventRunEnd:
			mu.Lock()
			final := lastCount
			mu.Unlock()
			fmt.Fprintf(os.Stderr, "\n[done in %d iterations, last counted=%d]\n", e.Iterations, final)
		}
	}
}
