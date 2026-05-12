package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/amit-timalsina/pi-agent-go"
	llm "github.com/amit-timalsina/pi-llm-go"
)

// --- Test scaffolding ---

// multiToolCallScript emits an assistant turn with N tool calls in one
// message. Each call gets its own BlockIndex per the streaming contract.
func multiToolCallScript(calls ...struct{ ID, Name, Args string }) []llm.StreamEvent {
	out := []llm.StreamEvent{llm.EventMessageStart{Model: "test"}}
	for i, c := range calls {
		out = append(out,
			llm.EventToolCallStart{BlockIndex: i, ID: c.ID, Name: c.Name},
			llm.EventToolCallDelta{BlockIndex: i, Delta: c.Args},
			llm.EventToolCallEnd{BlockIndex: i, Arguments: json.RawMessage(c.Args)},
		)
	}
	out = append(out, llm.EventMessageEnd{
		StopReason: llm.StopReasonToolUse,
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})
	return out
}

// sleepyTool returns an AgentTool whose handler sleeps for the given
// duration before returning the given summary. Suitable for proving
// concurrency: in parallel mode, N tools each sleeping D should
// complete in ~D, not ~N*D.
func sleepyTool(name string, dur time.Duration, summary string) agent.AgentTool {
	return agent.Raw(
		name,
		"sleeps and returns",
		json.RawMessage(`{"type":"object"}`),
		func(ctx context.Context, _ json.RawMessage) (agent.Result, error) {
			select {
			case <-time.After(dur):
				return agent.Result{Summary: summary}, nil
			case <-ctx.Done():
				return agent.Result{}, ctx.Err()
			}
		},
	)
}

// barrierTool: handler waits on a shared *sync.WaitGroup. Used to prove
// that N tool handlers are simultaneously inside their Handler body —
// stronger than just "they finished fast." If all N never converge on
// the barrier, the WaitGroup wait will time out under the surrounding
// test ctx and the handler returns an error.
func barrierTool(name string, wg *sync.WaitGroup, summary string) agent.AgentTool {
	return agent.Raw(
		name,
		"waits on the barrier and returns",
		json.RawMessage(`{"type":"object"}`),
		func(ctx context.Context, _ json.RawMessage) (agent.Result, error) {
			wg.Done()
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
				return agent.Result{Summary: summary}, nil
			case <-ctx.Done():
				return agent.Result{}, ctx.Err()
			}
		},
	)
}

// --- Tests ---

// TestParallel_ActuallyRunsConcurrently uses a sync.WaitGroup barrier
// to prove all three Handlers are in-flight at the same wall-clock
// moment. Without parallel exec they would deadlock on the barrier.
func TestParallel_ActuallyRunsConcurrently(t *testing.T) {
	const n = 3
	var wg sync.WaitGroup
	wg.Add(n)

	tools := []agent.AgentTool{
		barrierTool("a", &wg, "ra"),
		barrierTool("b", &wg, "rb"),
		barrierTool("c", &wg, "rc"),
	}
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "a", `{}`},
				struct{ ID, Name, Args string }{"2", "b", `{}`},
				struct{ ID, Name, Args string }{"3", "c", `{}`},
			),
			textOnlyScript("done"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         tools,
		ToolExecution: agent.ToolExecutionParallel,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := collect(t, a.Run(ctx, "go"))
	if err != nil {
		t.Fatalf("run: %v (barrier did not converge — handlers ran sequentially?)", err)
	}
}

// TestParallel_SourceOrderPreservedInResults verifies tool_result blocks
// (in the durable transcript) land in SOURCE order regardless of finish
// order. Handler 3 finishes first; handler 1 finishes last. Wire order
// must still be [1, 2, 3].
func TestParallel_SourceOrderPreservedInResults(t *testing.T) {
	tools := []agent.AgentTool{
		sleepyTool("a", 80*time.Millisecond, "ra"), // slowest
		sleepyTool("b", 40*time.Millisecond, "rb"),
		sleepyTool("c", 1*time.Millisecond, "rc"), // fastest
	}
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "a", `{}`},
				struct{ ID, Name, Args string }{"2", "b", `{}`},
				struct{ ID, Name, Args string }{"3", "c", `{}`},
			),
			textOnlyScript("done"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         tools,
		ToolExecution: agent.ToolExecutionParallel,
	})
	if _, err := collect(t, a.Run(context.Background(), "go")); err != nil {
		t.Fatalf("run: %v", err)
	}

	// The transcript after the run holds the user message, the assistant
	// tool-call message, and the tool-result message. We assert that
	// tool-result message's blocks are in source order.
	snap := a.Snapshot()
	var resultBlocks []llm.ToolResultBlock
	for _, m := range snap.Messages {
		if m.Role != llm.RoleTool {
			continue
		}
		for _, b := range m.Content {
			if tr, ok := b.(llm.ToolResultBlock); ok {
				resultBlocks = append(resultBlocks, tr)
			}
		}
	}
	if len(resultBlocks) != 3 {
		t.Fatalf("got %d tool_result blocks, want 3", len(resultBlocks))
	}
	wantIDs := []string{"1", "2", "3"}
	for i, b := range resultBlocks {
		if b.ToolCallID != wantIDs[i] {
			t.Errorf("tool_result[%d].ToolCallID=%q, want %q (source order lost?)", i, b.ToolCallID, wantIDs[i])
		}
	}
}

// TestParallel_ToolLogInSourceOrder verifies Snapshot().ToolLog entries
// are in source order even when handlers finished in a different order.
func TestParallel_ToolLogInSourceOrder(t *testing.T) {
	tools := []agent.AgentTool{
		sleepyTool("a", 60*time.Millisecond, "ra"),
		sleepyTool("b", 1*time.Millisecond, "rb"),
	}
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "a", `{}`},
				struct{ ID, Name, Args string }{"2", "b", `{}`},
			),
			textOnlyScript("done"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         tools,
		ToolExecution: agent.ToolExecutionParallel,
	})
	if _, err := collect(t, a.Run(context.Background(), "go")); err != nil {
		t.Fatalf("run: %v", err)
	}
	snap := a.Snapshot()
	if len(snap.ToolLog) != 2 {
		t.Fatalf("ToolLog len=%d, want 2", len(snap.ToolLog))
	}
	if snap.ToolLog[0].ToolCallID != "1" || snap.ToolLog[1].ToolCallID != "2" {
		t.Errorf("ToolLog out of source order: got IDs %q, %q",
			snap.ToolLog[0].ToolCallID, snap.ToolLog[1].ToolCallID)
	}
}

// TestParallel_EndEventsInFinishOrder verifies EventToolEnd events fire
// in finish order, not source order — the documented Mario-aligned
// behavior. Handler "c" finishes first; its EventToolEnd should arrive
// first.
func TestParallel_EndEventsInFinishOrder(t *testing.T) {
	tools := []agent.AgentTool{
		sleepyTool("a", 80*time.Millisecond, "ra"),
		sleepyTool("b", 40*time.Millisecond, "rb"),
		sleepyTool("c", 1*time.Millisecond, "rc"),
	}
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "a", `{}`},
				struct{ ID, Name, Args string }{"2", "b", `{}`},
				struct{ ID, Name, Args string }{"3", "c", `{}`},
			),
			textOnlyScript("done"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         tools,
		ToolExecution: agent.ToolExecutionParallel,
	})
	events, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var endOrder []string
	for _, ev := range events {
		if e, ok := ev.(agent.EventToolEnd); ok {
			endOrder = append(endOrder, e.ToolCallID)
		}
	}
	want := []string{"3", "2", "1"} // finish order: fastest first
	if len(endOrder) != 3 {
		t.Fatalf("EventToolEnd count=%d, want 3", len(endOrder))
	}
	for i, id := range endOrder {
		if id != want[i] {
			t.Errorf("EventToolEnd[%d].ToolCallID=%q, want %q (finish order broken)", i, id, want[i])
		}
	}
}

// TestParallel_PerToolSequentialOptOutDragsBatchSequential proves that
// declaring AgentTool.ExecutionMode == Sequential on any single tool
// in the batch forces the entire batch to sequential execution. We
// detect by counting concurrent in-flight handlers via a peak counter;
// in sequential mode the peak must be 1.
func TestParallel_PerToolSequentialOptOutDragsBatchSequential(t *testing.T) {
	var (
		inFlight int32
		peak     int32
	)
	// Signature dictated by agent.Raw; the (always-nil) error return
	// trips unparam in tests but matching the public API is the right
	// move here.
	//nolint:unparam
	track := func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
		now := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if now <= p {
				break
			}
			if atomic.CompareAndSwapInt32(&peak, p, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return agent.Result{Summary: "ok"}, nil
	}

	rawTool := func(name string, mode agent.ToolExecutionMode) agent.AgentTool {
		t := agent.Raw(name, "ok", json.RawMessage(`{"type":"object"}`), track)
		t.ExecutionMode = mode
		return t
	}

	tools := []agent.AgentTool{
		rawTool("safe", agent.ToolExecutionUnspecified),
		rawTool("unsafe", agent.ToolExecutionSequential), // drags batch
		rawTool("safe2", agent.ToolExecutionUnspecified),
	}
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "safe", `{}`},
				struct{ ID, Name, Args string }{"2", "unsafe", `{}`},
				struct{ ID, Name, Args string }{"3", "safe2", `{}`},
			),
			textOnlyScript("done"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         tools,
		ToolExecution: agent.ToolExecutionParallel,
	})
	if _, err := collect(t, a.Run(context.Background(), "go")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := atomic.LoadInt32(&peak); got != 1 {
		t.Errorf("peak concurrent handlers=%d, want 1 (Sequential opt-out should have dragged the batch)", got)
	}
}

// TestParallel_BeforeToolCallHookFiresInSourceOrder verifies the
// pre-flight phase stays sequential even under parallel execution.
// Records the call order via a mutex-protected slice.
func TestParallel_BeforeToolCallHookFiresInSourceOrder(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	tools := []agent.AgentTool{
		sleepyTool("a", 30*time.Millisecond, "ra"),
		sleepyTool("b", 30*time.Millisecond, "rb"),
		sleepyTool("c", 30*time.Millisecond, "rc"),
	}
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "a", `{}`},
				struct{ ID, Name, Args string }{"2", "b", `{}`},
				struct{ ID, Name, Args string }{"3", "c", `{}`},
			),
			textOnlyScript("done"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         tools,
		ToolExecution: agent.ToolExecutionParallel,
		BeforeToolCall: func(_ context.Context, _ agent.RunContext, info agent.ToolCallInfo) (bool, string, error) {
			mu.Lock()
			order = append(order, info.ToolCallID)
			mu.Unlock()
			return false, "", nil
		},
	})
	if _, err := collect(t, a.Run(context.Background(), "go")); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{"1", "2", "3"}
	if len(order) != 3 {
		t.Fatalf("BeforeToolCall fired %d times, want 3", len(order))
	}
	for i, id := range order {
		if id != want[i] {
			t.Errorf("BeforeToolCall order[%d]=%q, want %q", i, id, want[i])
		}
	}
}

// TestParallel_AfterToolCallHookCalledConcurrently verifies that
// AfterToolCall fires inside the parallel goroutine. We assert the
// peak concurrent count > 1.
func TestParallel_AfterToolCallHookCalledConcurrently(t *testing.T) {
	var (
		inFlight int32
		peak     int32
	)
	tools := []agent.AgentTool{
		sleepyTool("a", 1*time.Millisecond, "ra"),
		sleepyTool("b", 1*time.Millisecond, "rb"),
		sleepyTool("c", 1*time.Millisecond, "rc"),
	}
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "a", `{}`},
				struct{ ID, Name, Args string }{"2", "b", `{}`},
				struct{ ID, Name, Args string }{"3", "c", `{}`},
			),
			textOnlyScript("done"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         tools,
		ToolExecution: agent.ToolExecutionParallel,
		AfterToolCall: func(_ context.Context, _ agent.RunContext, _ agent.ToolCallInfo, _ agent.Result, _ bool) (*agent.Result, error) {
			now := atomic.AddInt32(&inFlight, 1)
			for {
				p := atomic.LoadInt32(&peak)
				if now <= p {
					break
				}
				if atomic.CompareAndSwapInt32(&peak, p, now) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			return nil, nil
		},
	})
	if _, err := collect(t, a.Run(context.Background(), "go")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := atomic.LoadInt32(&peak); got < 2 {
		t.Errorf("peak AfterToolCall concurrent=%d, want >= 2 (parallel should overlap hooks)", got)
	}
}

// TestParallel_HandlerErrorBecomesIsErrorResultDoesNotAbortRun verifies
// that a non-nil error returned from one parallel handler is converted
// into a IsError=true ToolResultBlock just like the sequential path,
// and the run continues to the next iteration.
func TestParallel_HandlerErrorBecomesIsErrorResultDoesNotAbortRun(t *testing.T) {
	failTool := agent.Raw("fail", "boom",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
			return agent.Result{}, errors.New("intentional failure")
		},
	)
	okTool := sleepyTool("ok", 5*time.Millisecond, "ok-result")

	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "fail", `{}`},
				struct{ ID, Name, Args string }{"2", "ok", `{}`},
			),
			textOnlyScript("recovered"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         []agent.AgentTool{failTool, okTool},
		ToolExecution: agent.ToolExecutionParallel,
	})
	events, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("run aborted unexpectedly: %v", err)
	}

	var ends []agent.EventToolEnd
	for _, ev := range events {
		if e, ok := ev.(agent.EventToolEnd); ok {
			ends = append(ends, e)
		}
	}
	if len(ends) != 2 {
		t.Fatalf("got %d EventToolEnd, want 2", len(ends))
	}
	sort.Slice(ends, func(i, j int) bool { return ends[i].ToolCallID < ends[j].ToolCallID })
	if !ends[0].IsError || ends[0].Result != "intentional failure" {
		t.Errorf("fail tool: IsError=%v Result=%q, want IsError=true with the error string", ends[0].IsError, ends[0].Result)
	}
	if ends[1].IsError || ends[1].Result != "ok-result" {
		t.Errorf("ok tool: IsError=%v Result=%q, want IsError=false with ok-result", ends[1].IsError, ends[1].Result)
	}
}

// TestParallel_AfterToolCallHookErrorAbortsRun verifies that a non-nil
// error from AfterToolCall (hook error, NOT a handler error) DOES
// terminate the run. errgroup cancels gctx; other handlers see the
// cancellation and bail.
func TestParallel_AfterToolCallHookErrorAbortsRun(t *testing.T) {
	tools := []agent.AgentTool{
		sleepyTool("a", 1*time.Millisecond, "ra"),
		sleepyTool("b", 100*time.Millisecond, "rb"), // slower; should be cancelled
	}
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "a", `{}`},
				struct{ ID, Name, Args string }{"2", "b", `{}`},
			),
			textOnlyScript("never reached"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         tools,
		ToolExecution: agent.ToolExecutionParallel,
		AfterToolCall: func(_ context.Context, _ agent.RunContext, info agent.ToolCallInfo, _ agent.Result, _ bool) (*agent.Result, error) {
			if info.ToolCallID == "1" {
				return nil, errors.New("hook explosion")
			}
			return nil, nil
		},
	})
	_, err := collect(t, a.Run(context.Background(), "go"))
	if err == nil {
		t.Fatal("expected hook error to abort the run; got nil")
	}
	if !errorMessageContains(err, "hook explosion") {
		t.Errorf("error=%v, want it to wrap 'hook explosion'", err)
	}
}

// TestParallel_SingleCallBatchStaysSequential verifies the
// single-call fast path: even with Config.ToolExecution=Parallel, a
// batch of one tool call should not spin up a goroutine + channel.
// We can't easily observe "no goroutine" from a test; instead we
// confirm correctness — a single-tool turn behaves identically to
// sequential mode.
func TestParallel_SingleCallBatchStaysSequential(t *testing.T) {
	tool := sleepyTool("solo", 1*time.Millisecond, "solo-result")
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			multiToolCallScript(struct{ ID, Name, Args string }{"1", "solo", `{}`}),
			textOnlyScript("done"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         []agent.AgentTool{tool},
		ToolExecution: agent.ToolExecutionParallel,
	})
	events, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var ends []agent.EventToolEnd
	for _, ev := range events {
		if e, ok := ev.(agent.EventToolEnd); ok {
			ends = append(ends, e)
		}
	}
	if len(ends) != 1 || ends[0].Result != "solo-result" {
		t.Errorf("single-call batch result wrong: %+v", ends)
	}
}

// TestParallel_DefaultIsSequential verifies that ToolExecutionUnspecified
// (zero value) means sequential — preserving v0.2.x behavior for
// callers who upgrade without setting Config.ToolExecution.
func TestParallel_DefaultIsSequential(t *testing.T) {
	var (
		inFlight int32
		peak     int32
	)
	//nolint:unparam // signature dictated by agent.Raw
	track := func(_ context.Context, _ json.RawMessage) (agent.Result, error) {
		now := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if now <= p {
				break
			}
			if atomic.CompareAndSwapInt32(&peak, p, now) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return agent.Result{Summary: "ok"}, nil
	}
	rawTool := func(name string) agent.AgentTool {
		return agent.Raw(name, "ok", json.RawMessage(`{"type":"object"}`), track)
	}
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "a", `{}`},
				struct{ ID, Name, Args string }{"2", "b", `{}`},
				struct{ ID, Name, Args string }{"3", "c", `{}`},
			),
			textOnlyScript("done"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:   fake,
		Model: "test",
		Tools: []agent.AgentTool{rawTool("a"), rawTool("b"), rawTool("c")},
		// ToolExecution left unspecified (zero value).
	})
	if _, err := collect(t, a.Run(context.Background(), "go")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := atomic.LoadInt32(&peak); got != 1 {
		t.Errorf("default ToolExecution: peak concurrent=%d, want 1 (sequential should be the default)", got)
	}
}

// TestParallel_OuterCtxCancellationUnwindsHandlers verifies that
// cancelling the ctx passed to Run propagates into in-flight Handler
// goroutines under parallel mode. Without proper ctx propagation,
// slow handlers would keep running after the consumer cancelled.
func TestParallel_OuterCtxCancellationUnwindsHandlers(t *testing.T) {
	// Three slow handlers; we cancel after they've started.
	tools := []agent.AgentTool{
		sleepyTool("a", 500*time.Millisecond, "ra"),
		sleepyTool("b", 500*time.Millisecond, "rb"),
		sleepyTool("c", 500*time.Millisecond, "rc"),
	}
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "a", `{}`},
				struct{ ID, Name, Args string }{"2", "b", `{}`},
				struct{ ID, Name, Args string }{"3", "c", `{}`},
			),
			textOnlyScript("never reached"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         tools,
		ToolExecution: agent.ToolExecutionParallel,
	})

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after the run starts — handlers should observe
	// ctx.Done() via sleepyTool's select and bail with ctx.Err().
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	_, err := collect(t, a.Run(ctx, "go"))
	elapsed := time.Since(started)

	if err == nil {
		t.Errorf("expected run to terminate with an error after ctx cancel; got nil")
	}
	// Sanity: total wall time should be a fraction of 500ms (not 3x).
	// Generous bound to avoid CI flakes; the test mainly proves that we
	// don't wait the full 500ms of any handler after cancellation.
	if elapsed > 400*time.Millisecond {
		t.Errorf("run took %v after ctx cancel; want < 400ms (handlers did not unwind)", elapsed)
	}
}

// TestParallel_ConsumerAbortCancelsInFlightHandlers verifies the
// must-fix-1 contract: when the iter consumer breaks out of the range
// mid-batch (yield returns false), the loop cancels its internal ctx
// so in-flight handlers honoring ctx.Done() bail out promptly. Without
// the explicit context.WithCancel inside executeToolCallsParallel,
// this test would block until the slow handler completed naturally.
func TestParallel_ConsumerAbortCancelsInFlightHandlers(t *testing.T) {
	tools := []agent.AgentTool{
		sleepyTool("a", 1*time.Millisecond, "ra"),   // finishes first
		sleepyTool("b", 500*time.Millisecond, "rb"), // slow; should be cancelled
		sleepyTool("c", 500*time.Millisecond, "rc"), // slow; should be cancelled
	}
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			multiToolCallScript(
				struct{ ID, Name, Args string }{"1", "a", `{}`},
				struct{ ID, Name, Args string }{"2", "b", `{}`},
				struct{ ID, Name, Args string }{"3", "c", `{}`},
			),
			textOnlyScript("never reached"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         tools,
		ToolExecution: agent.ToolExecutionParallel,
	})

	// Consume events; break out of the iter as soon as we see the
	// first EventToolEnd (which will be the fastest handler "a").
	started := time.Now()
	for ev, err := range a.Run(context.Background(), "go") {
		if err != nil {
			t.Fatalf("unexpected error before abort: %v", err)
		}
		if _, ok := ev.(agent.EventToolEnd); ok {
			break // simulate consumer abort
		}
	}
	elapsed := time.Since(started)

	// If the abort properly cancelled the in-flight handlers, total
	// elapsed should be well under the 500ms of the slowest handler.
	if elapsed > 300*time.Millisecond {
		t.Errorf("consumer abort took %v to return; want < 300ms (in-flight handlers did not unwind)", elapsed)
	}
}

// errorMessageContains is a small helper that doesn't depend on
// strings.Contains being imported in this test file already.
func errorMessageContains(err error, s string) bool {
	if err == nil {
		return false
	}
	return fmt.Sprintf("%v", err) != "" && containsSubstr(err.Error(), s)
}

func containsSubstr(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
