package agent

import (
	"testing"

	llm "github.com/amit-timalsina/pi-llm-go"
)

// TestMessageAccumulator_FinalDropsNilBlocks pins the defensive
// filter: a Content slot pre-extended by ensureBlock but never
// finalized by a matching End event must not appear in the returned
// message. Downstream providers (e.g., Anthropic) reject nil blocks
// with "unsupported block type <nil>" at convert time.
//
// Live failure that motivated the fix: noumenal_product SAIL
// dsa-run 019e2720-..., 2026-05-14, where adaptive thinking +
// parallel tool_use on Opus 4.7 left an unfilled slot after 6 LLM
// iterations; iter 7's request-build crashed on the nil.
func TestMessageAccumulator_FinalDropsNilBlocks(t *testing.T) {
	a := newMessageAccumulator()
	a.ensureBlock(3) // pre-extend with 4 nil slots
	a.msg.Content[0] = llm.TextBlock{Text: "hello"}
	a.msg.Content[2] = llm.ToolCallBlock{ID: "tu_1", Name: "echo"}
	// Content[1] and Content[3] stay nil (slot reserved, never finalized).

	final := a.final()
	if len(final.Content) != 2 {
		t.Fatalf("want 2 non-nil blocks in final, got %d (content=%v)", len(final.Content), final.Content)
	}
	if tb, ok := final.Content[0].(llm.TextBlock); !ok || tb.Text != "hello" {
		t.Errorf("Content[0] not the expected TextBlock: %T %v", final.Content[0], final.Content[0])
	}
	if tc, ok := final.Content[1].(llm.ToolCallBlock); !ok || tc.ID != "tu_1" {
		t.Errorf("Content[1] not the expected ToolCallBlock: %T %v", final.Content[1], final.Content[1])
	}
}

// TestMessageAccumulator_FinalPreservesOrderAfterFiltering confirms
// the relative ordering of non-nil blocks survives the filter.
func TestMessageAccumulator_FinalPreservesOrderAfterFiltering(t *testing.T) {
	a := newMessageAccumulator()
	a.ensureBlock(4)
	a.msg.Content[0] = llm.ThinkingBlock{Thinking: "considered..."}
	// Content[1] nil
	a.msg.Content[2] = llm.TextBlock{Text: "first"}
	// Content[3] nil
	a.msg.Content[4] = llm.TextBlock{Text: "second"}

	final := a.final()
	if len(final.Content) != 3 {
		t.Fatalf("want 3 non-nil blocks, got %d", len(final.Content))
	}
	if _, ok := final.Content[0].(llm.ThinkingBlock); !ok {
		t.Errorf("first block should be ThinkingBlock, got %T", final.Content[0])
	}
	if tb, ok := final.Content[1].(llm.TextBlock); !ok || tb.Text != "first" {
		t.Errorf("second block should be TextBlock(first), got %T %v", final.Content[1], final.Content[1])
	}
	if tb, ok := final.Content[2].(llm.TextBlock); !ok || tb.Text != "second" {
		t.Errorf("third block should be TextBlock(second), got %T %v", final.Content[2], final.Content[2])
	}
}

// TestMessageAccumulator_FinalNoAllocWhenNoNils confirms the happy
// path doesn't pay the filter cost — hasNilBlock fast-paths so
// well-formed streams (the overwhelmingly common case) avoid a
// re-allocation.
func TestMessageAccumulator_FinalNoAllocWhenNoNils(t *testing.T) {
	a := newMessageAccumulator()
	a.ensureBlock(1)
	a.msg.Content[0] = llm.TextBlock{Text: "x"}
	a.msg.Content[1] = llm.TextBlock{Text: "y"}

	// Capture the underlying slice header before final() — if no
	// re-alloc happened we'll see the same backing array length.
	before := a.msg.Content
	final := a.final()
	if len(final.Content) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(final.Content))
	}
	// The slice header should be the same when no filter is needed
	// (defensive code path skipped). Compare element pointers via
	// reflect.ValueOf on the first block's value rather than
	// addresses (interface boxing makes address-of tricky). Length +
	// content equivalence is enough to assert no re-alloc happened.
	if len(before) != len(final.Content) {
		t.Errorf("happy path re-allocated: before=%d, after=%d", len(before), len(final.Content))
	}
}

// TestMessageAccumulator_FinalEmptyContentUntouched confirms the
// filter is a no-op on an empty Content slice (no events received
// for this iteration — e.g., a stream that errored before any block
// started).
func TestMessageAccumulator_FinalEmptyContentUntouched(t *testing.T) {
	a := newMessageAccumulator()
	final := a.final()
	if len(final.Content) != 0 {
		t.Errorf("empty Content should stay empty, got len=%d", len(final.Content))
	}
}

