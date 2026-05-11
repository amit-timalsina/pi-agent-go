// bounded_results: demonstrates Result.Summary + Result.FullPayloadHint
// without any framework abstraction.
//
// Scenario: a `correlation_matrix` tool computes a 50x50 (2500-entry)
// correlation matrix. Naively returning the whole thing as text would
// burn ~30k tokens of context per call. Instead, the tool returns:
//
//   - Summary: top-5 correlations as a one-line list (~200 chars).
//   - FullPayloadHint: an opaque locator (here a filesystem path under
//     /tmp) that observability consumers can surface, and that a separate
//     read_full_matrix tool can read back when the model decides the
//     summary is insufficient.
//
// pi-agent-go does NOT interpret FullPayloadHint or provide a built-in
// fetcher — the caller wires up whatever retrieval tool they want. This
// example registers a tiny read_full_matrix tool that reads the file at
// the hint path. In real consumers the equivalent might be `read_file`,
// `http.GET`, or `db.Query`.
//
// Two prompts demonstrate the pattern:
//
//  1. "What are the top correlations?" — summary suffices.
//
//  2. "What's the correlation between vars 7 and 23?" — model calls
//     read_full_matrix with the hint path to get the full data.
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
	"path/filepath"
	"sort"
	"strings"

	agent "github.com/amit-timalsina/pi-agent-go"
	llm "github.com/amit-timalsina/pi-llm-go"
	"github.com/amit-timalsina/pi-llm-go/providers/anthropic"
)

const numVars = 50

func main() {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "ANTHROPIC_API_KEY is required")
		os.Exit(2)
	}
	provider, _ := anthropic.New(anthropic.Options{APIKey: key})

	a, err := agent.New(agent.Config{
		LLM:   provider,
		Model: anthropic.ClaudeSonnet4_6,
		SystemPrompt: "You are a data-analysis assistant. Use correlation_matrix to compute correlations. " +
			"If a tool result includes a 'full payload at <path>' hint and the summary is insufficient " +
			"for the question, call read_full_matrix with that path to retrieve the full data.",
		Tools:     []agent.AgentTool{buildCorrelationTool(), buildReadMatrixTool()},
		MaxTokens: 1024,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	prompts := []string{
		"Compute the correlation matrix and tell me the top correlations.",
		"Now what's the specific correlation between variable 7 and variable 23? Use read_full_matrix if needed.",
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
				hintMsg := ""
				if e.FullPayloadHint != "" {
					hintMsg = fmt.Sprintf(" [+hint=%s]", e.FullPayloadHint)
				}
				fmt.Fprintf(os.Stderr, "[tool end] %s -> %s%s\n", e.Name, strings.ReplaceAll(preview, "\n", " | "), hintMsg)
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
		"Compute the correlation matrix across all variables. Returns the top-5 correlations inline as the summary; the full matrix is written to a tempfile whose path is surfaced as a FullPayloadHint. Use read_full_matrix with that path when you need values beyond the top 5.",
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

			fullBytes, _ := json.Marshal(entries)
			path := filepath.Join(os.TempDir(), "pi-agent-corr-matrix.json")
			if err := os.WriteFile(path, fullBytes, 0o600); err != nil {
				return agent.Result{}, fmt.Errorf("write full matrix: %w", err)
			}

			var sb strings.Builder
			sb.WriteString("Top 5 correlations (by |r|):\n")
			for _, e := range top5 {
				fmt.Fprintf(&sb, "  vars %d~%d: r=%.3f\n", e.A, e.B, e.V)
			}
			fmt.Fprintf(&sb, "Full matrix of %d pairs at %s — call read_full_matrix with that path for any pair beyond the top 5.\n",
				len(entries), path)

			return agent.Result{
				Summary:         sb.String(),
				FullPayloadHint: path,
			}, nil
		},
	)
}

// buildReadMatrixTool registers a tiny "read full matrix" tool the model
// can call when the summary is insufficient. The path is opaque to
// pi-agent-go — the caller supplies whatever retrieval surface fits.
func buildReadMatrixTool() agent.AgentTool {
	return agent.Raw(
		"read_full_matrix",
		"Read the full correlation matrix at the given path (produced by correlation_matrix). Returns the raw JSON of all pairs.",
		json.RawMessage(`{
            "type": "object",
            "properties": {
                "path": {"type": "string", "description": "Path returned by correlation_matrix as its FullPayloadHint."}
            },
            "required": ["path"],
            "additionalProperties": false
        }`),
		func(_ context.Context, raw json.RawMessage) (agent.Result, error) {
			var args struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return agent.Result{}, fmt.Errorf("read_full_matrix: invalid arguments: %w", err)
			}
			if args.Path == "" {
				return agent.Result{}, fmt.Errorf("read_full_matrix: path is required")
			}
			body, err := os.ReadFile(args.Path)
			if err != nil {
				return agent.Result{}, fmt.Errorf("read_full_matrix: read %s: %w", args.Path, err)
			}
			return agent.Result{Summary: string(body)}, nil
		},
	)
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
