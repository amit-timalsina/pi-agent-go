// bounded_results: demonstrates Result.Summary + FullPayloadRef with the
// built-in fetch_tool_result meta-tool.
//
// Scenario: a `correlation_matrix` tool computes a 50x50 (2500-entry)
// correlation matrix. Naively returning the whole thing as text would
// burn ~30k tokens of context per call. Instead, the tool returns:
//
//   - Summary: top-5 correlations as a one-line list (~200 chars).
//   - FullPayloadRef: pointer to memory storage holding the full JSON.
//
// The model sees the summary and answers most questions from it. When a
// question requires the full data, the model calls fetch_tool_result
// with the call_index — pi-agent-go's built-in meta-tool resolves the
// payload via the registered MemoryPayloadResolver.
//
// Two prompts demonstrate the pattern:
//
//  1. "What are the top correlations?" — summary suffices.
//
//  2. "What's the correlation between vars 7 and 23?" — requires full payload.
//
//     export ANTHROPIC_API_KEY=...
//     go run ./examples/bounded_results
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"

	agent "github.com/amit-timalsina/pi-agent-go"
	llm "github.com/amit-timalsina/pi-llm-go"
	"github.com/amit-timalsina/pi-llm-go/providers/anthropic"
)

const numVars = 50

// payloadStore is the shared in-memory backing for both the
// correlation_matrix tool (which writes payloads in) and the agent's
// PayloadResolver (which reads them out).
var payloadStore = map[string]string{}

func main() {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "ANTHROPIC_API_KEY is required")
		os.Exit(2)
	}
	provider, _ := anthropic.New(anthropic.Options{APIKey: key})

	corrTool := buildCorrelationTool()

	a, err := agent.New(agent.Config{
		LLM:          provider,
		Model:        anthropic.ClaudeSonnet4_6,
		SystemPrompt: "You are a data-analysis assistant. Use correlation_matrix to compute correlations; reach for fetch_tool_result only when the summary you saw is insufficient for the question.",
		Tools:        []agent.AgentTool{corrTool},
		MaxTokens:    1024,

		// Wire up the built-in fetch_tool_result meta-tool.
		EnableFetchToolResult: true,
		PayloadResolver:       &agent.MemoryPayloadResolver{Payloads: payloadStore},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	prompts := []string{
		"Compute the correlation matrix and tell me the top correlations.",
		"Now what's the specific correlation between variable 7 and variable 23? Use fetch_tool_result if needed.",
	}

	for i, prompt := range prompts {
		fmt.Printf("\n--- iteration %d ---\n", i+1)
		fmt.Printf("user: %s\n", prompt)
		for event, err := range a.Run(context.Background(), prompt) {
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			switch e := event.(type) {
			case agent.EventLLMStream:
				if d, ok := e.Event.(llm.EventTextDelta); ok {
					fmt.Print(d.Delta)
				}
			case agent.EventToolStart:
				fmt.Fprintf(os.Stderr, "\n[tool start] %s args=%s\n", e.Name, string(e.Arguments))
			case agent.EventToolEnd:
				preview := e.Result
				if len(preview) > 200 {
					preview = preview[:200] + "..."
				}
				refMsg := ""
				if e.FullPayloadRef != nil {
					refMsg = fmt.Sprintf(" [+payload ref %s/%s, %d bytes]",
						e.FullPayloadRef.Backend, e.FullPayloadRef.Key, e.FullPayloadRef.Size)
				}
				fmt.Fprintf(os.Stderr, "[tool end] %s -> %s%s\n", e.Name, strings.ReplaceAll(preview, "\n", " | "), refMsg)
			}
		}
		fmt.Println()
	}
}

type corrEntry struct {
	A, B int
	V    float64
}

func buildCorrelationTool() agent.AgentTool {
	return agent.Raw(
		"correlation_matrix",
		"Compute the correlation matrix across all variables. Returns a summary of the top-5 correlations inline; the full matrix is stored via FullPayloadRef and retrievable via fetch_tool_result.",
		json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
			// Deterministic synthetic matrix; seed by var pair so results
			// repeat across runs (good for caching, good for tests).
			rng := rand.New(rand.NewSource(42))
			entries := make([]corrEntry, 0, numVars*(numVars-1)/2)
			for i := 0; i < numVars; i++ {
				for j := i + 1; j < numVars; j++ {
					v := rng.Float64()*2 - 1 // [-1, 1]
					entries = append(entries, corrEntry{A: i, B: j, V: v})
				}
			}

			// Sort by |correlation| desc for the summary.
			sort.Slice(entries, func(i, j int) bool {
				return abs(entries[i].V) > abs(entries[j].V)
			})
			top5 := entries[:5]

			// Build the bounded summary (~200 chars).
			var sb strings.Builder
			sb.WriteString("Top 5 correlations (by |r|):\n")
			for _, e := range top5 {
				fmt.Fprintf(&sb, "  vars %d~%d: r=%.3f\n", e.A, e.B, e.V)
			}
			fmt.Fprintf(&sb, "Full matrix (%d pairs) retrievable via fetch_tool_result.\n", len(entries))
			summary := sb.String()

			// Build the FULL payload — JSON of all pairs.
			fullBytes, _ := json.Marshal(entries)
			key := "corr-matrix-1"
			payloadStore[key] = string(fullBytes)

			return agent.Result{
				Summary: summary,
				FullPayloadRef: &agent.PayloadRef{
					Backend:  "memory",
					Key:      key,
					Size:     int64(len(fullBytes)),
					MimeType: "application/json",
				},
			}, nil
		},
	)
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
