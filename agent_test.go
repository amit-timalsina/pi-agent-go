package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"sync"
	"testing"

	agent "github.com/amit-timalsina/pi-agent-go"
	llm "github.com/amit-timalsina/pi-llm-go"
)

// fakeLLM is a scripted llm.LLM. Each Stream call consumes one script from
// scripts in order; if the agent calls Stream more times than scripts has
// entries, the test fails fast.
type fakeLLM struct {
	mu        sync.Mutex
	scripts   [][]llm.StreamEvent
	scriptErr []error // per-call terminal error; index aligned with scripts
	calls     int
	requests  []llm.Request
	// blockAfterScript: when true, after emitting the scripted events the
	// fake blocks on ctx.Done() and then yields ctx.Err(). Used by tests
	// that need a run to stay in-flight while they make assertions.
	blockAfterScript bool
}

func (f *fakeLLM) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	f.mu.Lock()
	if f.calls >= len(f.scripts) {
		f.mu.Unlock()
		return func(yield func(llm.StreamEvent, error) bool) {
			yield(nil, errors.New("fakeLLM: no more scripts"))
		}
	}
	script := f.scripts[f.calls]
	var termErr error
	if f.calls < len(f.scriptErr) {
		termErr = f.scriptErr[f.calls]
	}
	f.calls++
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	return func(yield func(llm.StreamEvent, error) bool) {
		for _, ev := range script {
			select {
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			default:
			}
			if !yield(ev, nil) {
				return
			}
		}
		if termErr != nil {
			yield(nil, termErr)
			return
		}
		if f.blockAfterScript {
			<-ctx.Done()
			yield(nil, ctx.Err())
		}
	}
}

// textOnlyScript: model emits one text block and ends.
func textOnlyScript(text string) []llm.StreamEvent {
	return []llm.StreamEvent{
		llm.EventMessageStart{Model: "test"},
		llm.EventTextStart{BlockIndex: 0},
		llm.EventTextDelta{BlockIndex: 0, Delta: text},
		llm.EventTextEnd{BlockIndex: 0},
		llm.EventMessageEnd{StopReason: llm.StopReasonEnd, Usage: llm.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}},
	}
}

// toolCallScript: model emits one tool call and ends with stop_reason=tool_use.
func toolCallScript(id, name, args string) []llm.StreamEvent {
	return []llm.StreamEvent{
		llm.EventMessageStart{Model: "test"},
		llm.EventToolCallStart{BlockIndex: 0, ID: id, Name: name},
		llm.EventToolCallDelta{BlockIndex: 0, Delta: args},
		llm.EventToolCallEnd{BlockIndex: 0, Arguments: json.RawMessage(args)},
		llm.EventMessageEnd{StopReason: llm.StopReasonToolUse, Usage: llm.Usage{InputTokens: 8, OutputTokens: 4, TotalTokens: 12}},
	}
}

// echoTool returns whatever input.text was passed.
type echoArgs struct {
	Text string `json:"text"`
}

func echoTool() agent.AgentTool {
	return agent.Typed[echoArgs, string](
		"echo",
		"echo back the text argument",
		func(ctx context.Context, in echoArgs) (string, error) {
			return in.Text, nil
		},
		func(s string) string { return s },
	)
}

func collect(t *testing.T, seq iter.Seq2[agent.AgentEvent, error]) ([]agent.AgentEvent, error) {
	t.Helper()
	var events []agent.AgentEvent
	var lastErr error
	for ev, err := range seq {
		if err != nil {
			lastErr = err
			continue
		}
		events = append(events, ev)
	}
	return events, lastErr
}

func TestRunNoTools(t *testing.T) {
	fake := &fakeLLM{scripts: [][]llm.StreamEvent{textOnlyScript("Hello!")}}
	a, err := agent.New(agent.Config{LLM: fake, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := collect(t, a.Run(context.Background(), "hi"))
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	// Expect: RunStart, IterationStart, several LLMStream, AssistantMessage, RunEnd.
	var sawStart, sawEnd, sawAssist bool
	for _, ev := range events {
		switch e := ev.(type) {
		case agent.EventRunStart:
			sawStart = true
		case agent.EventAssistantMessage:
			sawAssist = true
			if len(e.Message.Content) != 1 {
				t.Errorf("assistant Content len=%d", len(e.Message.Content))
			}
		case agent.EventRunEnd:
			sawEnd = true
			if e.Iterations != 1 {
				t.Errorf("Iterations=%d, want 1", e.Iterations)
			}
		}
	}
	if !sawStart || !sawAssist || !sawEnd {
		t.Errorf("missing lifecycle events: start=%v assist=%v end=%v", sawStart, sawAssist, sawEnd)
	}
	if fake.calls != 1 {
		t.Errorf("expected 1 LLM call, got %d", fake.calls)
	}
}

func TestRunWithToolCall(t *testing.T) {
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "echo", `{"text":"hello world"}`),
			textOnlyScript("done"),
		},
	}
	a, _ := agent.New(agent.Config{LLM: fake, Model: "test", Tools: []agent.AgentTool{echoTool()}})

	events, err := collect(t, a.Run(context.Background(), "use the tool"))
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	var toolStart, toolEnd *agent.AgentEvent
	for i, ev := range events {
		switch ev.(type) {
		case agent.EventToolStart:
			toolStart = &events[i]
		case agent.EventToolEnd:
			toolEnd = &events[i]
		}
	}
	if toolStart == nil || toolEnd == nil {
		t.Fatal("missing tool events")
	}
	if e := (*toolEnd).(agent.EventToolEnd); e.Result != "hello world" || e.IsError {
		t.Errorf("tool result wrong: %+v", e)
	}
	if fake.calls != 2 {
		t.Errorf("expected 2 LLM calls, got %d", fake.calls)
	}

	// The second LLM request should have RoleTool message containing the
	// tool result.
	if len(fake.requests) < 2 {
		t.Fatal("missing second request")
	}
	secondMsgs := fake.requests[1].Messages
	last := secondMsgs[len(secondMsgs)-1]
	if last.Role != llm.RoleTool || len(last.Content) != 1 {
		t.Fatalf("second request last msg wrong: %+v", last)
	}
	if tr, ok := last.Content[0].(llm.ToolResultBlock); !ok || tr.Content != "hello world" {
		t.Errorf("ToolResultBlock content: %+v", last.Content[0])
	}
}

func TestToolErrorBecomesErrorResult(t *testing.T) {
	failingTool := agent.Raw(
		"fail",
		"always fails",
		json.RawMessage(`{"type":"object"}`),
		func(ctx context.Context, args json.RawMessage) (agent.Result, error) {
			return agent.Result{}, errors.New("kaboom")
		},
	)
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "fail", `{}`),
			textOnlyScript("oh no"),
		},
	}
	a, _ := agent.New(agent.Config{LLM: fake, Model: "test", Tools: []agent.AgentTool{failingTool}})
	events, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	var end agent.EventToolEnd
	for _, ev := range events {
		if e, ok := ev.(agent.EventToolEnd); ok {
			end = e
		}
	}
	if !end.IsError || end.Result != "kaboom" {
		t.Errorf("tool end: %+v", end)
	}
}

func TestBeforeToolCallSkip(t *testing.T) {
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "echo", `{"text":"x"}`),
			textOnlyScript("done"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:   fake,
		Model: "test",
		Tools: []agent.AgentTool{echoTool()},
		BeforeToolCall: func(ctx context.Context, rc agent.RunContext, c agent.ToolCallInfo) (bool, string, error) {
			return true, "blocked for test", nil
		},
	})
	events, _ := collect(t, a.Run(context.Background(), "go"))
	for _, ev := range events {
		if e, ok := ev.(agent.EventToolEnd); ok {
			if !e.IsError || e.Result != "blocked for test" {
				t.Errorf("expected blocked error result, got %+v", e)
			}
		}
		if _, ok := ev.(agent.EventToolStart); ok {
			t.Errorf("EventToolStart should not fire when BeforeToolCall skips")
		}
	}
}

func TestAfterToolCallOverride(t *testing.T) {
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "echo", `{"text":"raw"}`),
			textOnlyScript("ok"),
		},
	}
	a, _ := agent.New(agent.Config{
		LLM:   fake,
		Model: "test",
		Tools: []agent.AgentTool{echoTool()},
		AfterToolCall: func(ctx context.Context, rc agent.RunContext, c agent.ToolCallInfo, r agent.Result, isErr bool) (*agent.Result, error) {
			return &agent.Result{Content: "REDACTED"}, nil
		},
	})
	events, _ := collect(t, a.Run(context.Background(), "go"))
	for _, ev := range events {
		if e, ok := ev.(agent.EventToolEnd); ok && e.Result != "REDACTED" {
			t.Errorf("AfterToolCall override not applied: %+v", e)
		}
	}
}

func TestHookErrorAborts(t *testing.T) {
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "echo", `{"text":"x"}`),
		},
	}
	want := errors.New("nope")
	a, _ := agent.New(agent.Config{
		LLM:   fake,
		Model: "test",
		Tools: []agent.AgentTool{echoTool()},
		BeforeToolCall: func(ctx context.Context, rc agent.RunContext, c agent.ToolCallInfo) (bool, string, error) {
			return false, "", want
		},
	})
	_, err := collect(t, a.Run(context.Background(), "go"))
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("want hook error %v, got %v", want, err)
	}
}

func TestSteeringInjectedAtNextIteration(t *testing.T) {
	// First script: tool call; second: text end. The tool handler itself
	// calls Steer synchronously, guaranteeing the steering message is in
	// the channel before iteration 2 starts.
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "trigger", `{}`),
			textOnlyScript("steered ack"),
		},
	}
	steerMsg := llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.Block{llm.TextBlock{Text: "actually do X"}},
	}

	var a *agent.Agent
	triggerTool := agent.Raw(
		"trigger",
		"calls Steer synchronously then returns",
		json.RawMessage(`{"type":"object"}`),
		func(ctx context.Context, _ json.RawMessage) (agent.Result, error) {
			_ = a.Steer(ctx, steerMsg)
			return agent.Result{Content: "fired"}, nil
		},
	)
	a, _ = agent.New(agent.Config{LLM: fake, Model: "test", Tools: []agent.AgentTool{triggerTool}})

	events, err := collect(t, a.Run(context.Background(), "go"))
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	var sawSteering bool
	for _, ev := range events {
		if _, ok := ev.(agent.EventSteering); ok {
			sawSteering = true
		}
	}
	if !sawSteering {
		t.Error("expected EventSteering")
	}
	// Steering message should appear in the second request's messages.
	if len(fake.requests) < 2 {
		t.Fatal("expected 2 requests")
	}
	var foundInSecondReq bool
	for _, m := range fake.requests[1].Messages {
		for _, b := range m.Content {
			if tb, ok := b.(llm.TextBlock); ok && tb.Text == "actually do X" {
				foundInSecondReq = true
			}
		}
	}
	if !foundInSecondReq {
		t.Error("steering message not appended before second LLM call")
	}
}

func TestSteeringDrop(t *testing.T) {
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "trigger", `{}`),
			textOnlyScript("done"),
		},
	}
	var a *agent.Agent
	triggerTool := agent.Raw(
		"trigger",
		"steers, drops",
		json.RawMessage(`{"type":"object"}`),
		func(ctx context.Context, _ json.RawMessage) (agent.Result, error) {
			_ = a.Steer(ctx, llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock{Text: "drop me"}}})
			return agent.Result{Content: "ok"}, nil
		},
	)
	a, _ = agent.New(agent.Config{
		LLM:   fake,
		Model: "test",
		Tools: []agent.AgentTool{triggerTool},
		OnSteering: func(ctx context.Context, rc agent.RunContext, msg llm.Message) (bool, error) {
			return true, nil
		},
	})
	events, _ := collect(t, a.Run(context.Background(), "go"))
	for _, ev := range events {
		if _, ok := ev.(agent.EventSteering); ok {
			t.Error("EventSteering fired despite drop=true")
		}
	}
}

func TestMaxIterationsExhausted(t *testing.T) {
	// Model always returns a tool call -> loop never converges.
	scripts := make([][]llm.StreamEvent, 5)
	for i := range scripts {
		scripts[i] = toolCallScript("call", "echo", `{"text":"x"}`)
	}
	fake := &fakeLLM{scripts: scripts}
	a, _ := agent.New(agent.Config{
		LLM:           fake,
		Model:         "test",
		Tools:         []agent.AgentTool{echoTool()},
		MaxIterations: 3,
	})
	_, err := collect(t, a.Run(context.Background(), "go"))
	if !errors.Is(err, agent.ErrMaxIterations) {
		t.Errorf("want ErrMaxIterations, got %v", err)
	}
	if fake.calls != 3 {
		t.Errorf("expected 3 LLM calls (MaxIterations), got %d", fake.calls)
	}
}

func TestSnapshotReflectsState(t *testing.T) {
	fake := &fakeLLM{scripts: [][]llm.StreamEvent{textOnlyScript("hello")}}
	a, _ := agent.New(agent.Config{LLM: fake, Model: "test"})

	pre := a.Snapshot()
	if pre.IsRunning || pre.Iteration != 0 {
		t.Errorf("pre snapshot wrong: %+v", pre)
	}

	_, _ = collect(t, a.Run(context.Background(), "hi"))

	post := a.Snapshot()
	if post.IsRunning {
		t.Error("post snapshot still IsRunning after run completes")
	}
	if post.RunID == "" || len(post.Messages) < 2 {
		t.Errorf("post snapshot incomplete: %+v", post)
	}
}

func TestResetPanicsIfRunning(t *testing.T) {
	// Keep the run open by making the fake LLM block after its script.
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			{llm.EventMessageStart{Model: "test"}},
		},
		blockAfterScript: true,
	}
	a, _ := agent.New(agent.Config{LLM: fake, Model: "test"})
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		seq := a.Run(ctx, "go")
		for ev := range seq {
			if _, ok := ev.(agent.EventRunStart); ok {
				close(started)
			}
		}
		close(done)
	}()
	<-started
	// Reset while running -> panic.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("Reset should panic while running")
			}
		}()
		a.Reset()
	}()
	cancel()
	<-done
}

func TestAlreadyRunning(t *testing.T) {
	// First run stays in-flight (blockAfterScript). Second attempt should
	// yield ErrAlreadyRunning.
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			{llm.EventMessageStart{Model: "test"}},
		},
		blockAfterScript: true,
	}
	a, _ := agent.New(agent.Config{LLM: fake, Model: "test"})
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		for ev := range a.Run(ctx, "first") {
			if _, ok := ev.(agent.EventRunStart); ok {
				close(started)
			}
		}
		close(done)
	}()
	<-started
	// Second run attempt.
	_, err := collect(t, a.Run(context.Background(), "second"))
	if !errors.Is(err, agent.ErrAlreadyRunning) {
		t.Errorf("want ErrAlreadyRunning, got %v", err)
	}
	cancel()
	<-done
}

func TestNewDuplicateTool(t *testing.T) {
	_, err := agent.New(agent.Config{
		LLM:   &fakeLLM{},
		Model: "test",
		Tools: []agent.AgentTool{echoTool(), echoTool()},
	})
	if err == nil {
		t.Fatal("want duplicate-tool error")
	}
}

func TestTypedToolUnmarshalsArgs(t *testing.T) {
	// Spy: capture args inside the handler.
	var captured echoArgs
	tool := agent.Typed[echoArgs, string](
		"echo",
		"echo",
		func(ctx context.Context, in echoArgs) (string, error) {
			captured = in
			return in.Text, nil
		},
		func(s string) string { return s },
	)
	fake := &fakeLLM{
		scripts: [][]llm.StreamEvent{
			toolCallScript("call_1", "echo", `{"text":"under the hood"}`),
			textOnlyScript("ok"),
		},
	}
	a, _ := agent.New(agent.Config{LLM: fake, Model: "test", Tools: []agent.AgentTool{tool}})
	_, _ = collect(t, a.Run(context.Background(), "go"))
	if captured.Text != "under the hood" {
		t.Errorf("Typed args not unmarshaled: %+v", captured)
	}
}
