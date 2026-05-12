// snapshot_resume: demonstrates the Snapshot → Restore pattern.
//
//	export ANTHROPIC_API_KEY=...
//	go run ./examples/snapshot_resume
//
// The example:
//
//  1. Creates an agent, runs one turn ("what's the capital of France?").
//  2. Takes a Snapshot — what you'd persist across a process restart.
//  3. Reconstructs the agent via Restore with a FRESH provider
//     instance. (Real consumers would serialize the snapshot and
//     reload it from disk between these two steps; here we keep
//     the snap in memory to focus on the contract.)
//  4. Runs a second turn that references the first ("what's its
//     population?") — proving the LLM sees the full prior transcript.
//
// Persistence layer: RunSnapshot is NOT yet JSON-friendly because
// llm.Message.Content is a []llm.Block interface slice that the
// stdlib json package can't unmarshal back into without a
// discriminator. For v0.5.0, callers serialize via `gob` (stdlib,
// works on interface types when concrete types are registered) or
// implement a custom encoder. A future pi-llm-go release will add
// native JSON support on llm.Message.
package main

import (
	"context"
	"fmt"
	"iter"
	"os"

	agent "github.com/amit-timalsina/pi-agent-go"
	llm "github.com/amit-timalsina/pi-llm-go"
	"github.com/amit-timalsina/pi-llm-go/providers/anthropic"
)

func main() {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "ANTHROPIC_API_KEY required")
		os.Exit(2)
	}

	// === Phase 1: original agent runs one turn ===
	provider1, _ := anthropic.New(anthropic.Options{APIKey: key})
	original, err := agent.New(agent.Config{
		LLM:          provider1,
		Model:        anthropic.ClaudeSonnet4_6,
		SystemPrompt: "You are a concise geography tutor. Answer in one short sentence.",
		MaxTokens:    256,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent.New:", err)
		os.Exit(1)
	}
	fmt.Println("--- turn 1 (original agent) ---")
	printRun(original.Run(context.Background(), "What's the capital of France?"))

	// === Phase 2: snapshot + simulated process restart ===
	snap := original.Snapshot()
	fmt.Printf("\n[snapshot taken: run_id=%s, iteration=%d, messages=%d]\n",
		snap.RunID, snap.Iteration, len(snap.Messages))
	fmt.Println("[--- imagine the process crashes and restarts here ---]")

	// === Phase 3: reconstruct from the snapshot ===
	// New provider instance — Restore doesn't reuse the prior one.
	provider2, _ := anthropic.New(anthropic.Options{APIKey: key})
	restored, err := agent.Restore(agent.Config{
		LLM:          provider2,
		Model:        anthropic.ClaudeSonnet4_6,
		SystemPrompt: "(this will be overridden by snap.SystemPrompt)",
		MaxTokens:    256,
	}, snap)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent.Restore:", err)
		os.Exit(1)
	}
	fmt.Printf("[restored: system_prompt=%q, transcript_len=%d]\n",
		restored.SystemPrompt(), len(restored.Snapshot().Messages))

	// === Phase 4: continue the conversation ===
	fmt.Println("\n--- turn 2 (restored agent, references prior turn) ---")
	printRun(restored.Run(context.Background(), "What's its population? Answer with just the number."))
}

func printRun(events iter.Seq2[agent.AgentEvent, error]) {
	for ev, err := range events {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		switch e := ev.(type) {
		case agent.EventLLMStream:
			if d, ok := e.Event.(llm.EventTextDelta); ok {
				fmt.Print(d.Delta)
			}
		case agent.EventRunEnd:
			fmt.Println()
		}
	}
}
