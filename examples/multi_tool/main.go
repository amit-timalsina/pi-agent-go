// multi_tool: agent with three typed tools chained across iterations.
//
// Shows:
//   - Registering multiple Typed[I, O] tools with different input / output
//     types.
//   - The agent picking the right tool per sub-task automatically.
//   - Multi-iteration tool sequences (the agent calls -> sees result ->
//     calls again).
//   - Snapshot() + ToolLog at the end to surface an audit trail.
//
//	export ANTHROPIC_API_KEY=...
//	go run ./examples/multi_tool
package main

import (
	"context"
	"fmt"
	"math"
	"os"

	llm "github.com/amit-timalsina/pi-llm-go"
	"github.com/amit-timalsina/pi-llm-go/providers/anthropic"
	agent "github.com/amit-timalsina/pi-agent-go"
)

// --- Tool 1: currency conversion (with stub rates) ---

type ConvertArgs struct {
	Amount float64 `json:"amount" jsonschema:"description=The amount to convert."`
	From   string  `json:"from" jsonschema:"description=Source currency code (USD, EUR, GBP, INR, JPY)."`
	To     string  `json:"to" jsonschema:"description=Target currency code."`
}

type ConvertResult struct {
	Amount float64 `json:"amount"`
	Code   string  `json:"code"`
}

// rates is a snapshot of FX rates against USD. Not real-time -- this is a
// demo. The agent doesn't know; it just trusts what the tool returns.
var rates = map[string]float64{
	"USD": 1.0,
	"EUR": 0.92,
	"GBP": 0.78,
	"INR": 83.0,
	"JPY": 150.0,
}

func convertCurrency(_ context.Context, in ConvertArgs) (ConvertResult, error) {
	fromRate, okF := rates[in.From]
	toRate, okT := rates[in.To]
	if !okF || !okT {
		return ConvertResult{}, fmt.Errorf("unknown currency: from=%q to=%q (supported: USD, EUR, GBP, INR, JPY)", in.From, in.To)
	}
	usd := in.Amount / fromRate
	out := usd * toRate
	return ConvertResult{Amount: out, Code: in.To}, nil
}

// --- Tool 2: compound interest ---

type InterestArgs struct {
	Principal    float64 `json:"principal" jsonschema:"description=Initial principal amount."`
	AnnualRate   float64 `json:"annual_rate" jsonschema:"description=Annual interest rate as a fraction (e.g. 0.05 for 5%)."`
	Years        float64 `json:"years" jsonschema:"description=Number of years."`
	CompoundsPer int     `json:"compounds_per_year,omitempty" jsonschema:"description=Compounding periods per year. Defaults to 1 (annual) if omitted."`
}

func compoundInterest(_ context.Context, in InterestArgs) (float64, error) {
	n := in.CompoundsPer
	if n == 0 {
		n = 1
	}
	final := in.Principal * math.Pow(1+in.AnnualRate/float64(n), float64(n)*in.Years)
	return final, nil
}

// --- Tool 3: comparison ---

type CompareArgs struct {
	A     float64 `json:"a"`
	B     float64 `json:"b"`
	Units string  `json:"units,omitempty" jsonschema:"description=Optional unit string used only for the human-readable answer."`
}

type CompareResult struct {
	Verdict    string  `json:"verdict"`    // "a > b" | "a == b" | "a < b"
	Difference float64 `json:"difference"` // abs(a - b)
}

func compareValues(_ context.Context, in CompareArgs) (CompareResult, error) {
	diff := math.Abs(in.A - in.B)
	verdict := "a == b"
	switch {
	case in.A > in.B:
		verdict = "a > b"
	case in.A < in.B:
		verdict = "a < b"
	}
	return CompareResult{Verdict: verdict, Difference: diff}, nil
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

	tools := []agent.AgentTool{
		agent.Typed[ConvertArgs, ConvertResult](
			"convert_currency",
			"Convert an amount between currencies (USD, EUR, GBP, INR, JPY).",
			convertCurrency,
			func(r ConvertResult) string { return fmt.Sprintf("%.2f %s", r.Amount, r.Code) },
		),
		agent.Typed[InterestArgs, float64](
			"compound_interest",
			"Compute the final value of a principal under compound interest over a number of years.",
			compoundInterest,
			func(v float64) string { return fmt.Sprintf("%.2f", v) },
		),
		agent.Typed[CompareArgs, CompareResult](
			"compare",
			"Compare two numeric values and report which is larger and the difference.",
			compareValues,
			func(r CompareResult) string {
				return fmt.Sprintf("%s (|a-b|=%.4f)", r.Verdict, r.Difference)
			},
		),
	}

	a, err := agent.New(agent.Config{
		LLM:          provider,
		Model:        anthropic.ClaudeSonnet4_6,
		SystemPrompt: "You are a numerate financial assistant. Use the provided tools rather than computing by hand. Be concise.",
		Tools:        tools,
		MaxTokens:    1024,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	prompt := "I have 1000 USD. My friend has 800 EUR. " +
		"Convert both to USD using the conversion tool, then compute what each grows to at 5%/year (annual compounding) after 10 years using the interest tool, " +
		"then compare the two future values. Tell me who ends up wealthier and by how much in USD."

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
		case agent.EventToolEnd:
			fmt.Fprintf(os.Stderr, "[tool] %s -> %s\n", e.Name, e.Result)
		case agent.EventRunEnd:
			fmt.Fprintf(os.Stderr, "\n[done in %d iterations]\n", e.Iterations)
		}
	}

	// Surface the audit trail via Snapshot() — this is what an operator
	// dashboard or post-mortem investigator would inspect.
	snap := a.Snapshot()
	fmt.Fprintf(os.Stderr, "\n=== audit trail (%d tool invocations) ===\n", len(snap.ToolLog))
	for i, e := range snap.ToolLog {
		latency := e.EndedAt.Sub(e.StartedAt)
		fmt.Fprintf(os.Stderr, "%d. iter=%d %s(%s) -> %s [err=%v latency=%s]\n",
			i+1, e.Iteration, e.Name, string(e.Arguments), e.Result, e.IsError, latency)
	}
	fmt.Fprintf(os.Stderr, "tokens last LLM call: in=%d out=%d total=%d\n",
		snap.LastUsage.InputTokens, snap.LastUsage.OutputTokens, snap.LastUsage.TotalTokens)
}
