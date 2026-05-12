// Package agent is a minimal single-loop agent on top of pi-llm-go.
//
//	a, err := agent.New(agent.Config{
//	    LLM:   llmProvider,
//	    Model: "claude-opus-4-7",
//	    Tools: []agent.AgentTool{calcTool},
//	})
//	for event, err := range a.Run(ctx, "what is 2+2?") {
//	    if err != nil { /* handle */ }
//	    switch e := event.(type) {
//	    case agent.EventLLMStream:
//	        // token-level streaming
//	    case agent.EventToolEnd:
//	        log.Printf("tool %s -> %s", e.Name, e.Result)
//	    }
//	}
//
// The loop runs until the model produces an assistant turn with no tool
// calls, MaxIterations is hit, or an error propagates from the LLM or a
// hook. Tool errors don't abort the run — they're fed back to the model
// as ToolResultBlocks with IsError=true so the model can recover.
//
// One Agent value is NOT safe for concurrent Run calls. Construct one per
// session. Steer and Snapshot are safe to call from other goroutines
// while a Run is in progress.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	llm "github.com/amit-timalsina/pi-llm-go"
)

// Defaults and tunables.
const (
	defaultMaxIterations      = 32
	steeringBufferSize        = 16
	defaultBlockedMessage     = "tool execution blocked by policy"
	defaultUnknownToolMessage = "unknown tool"
)

// Sentinel errors. Use errors.Is to branch on these.
var (
	// ErrMaxIterations: the loop hit Config.MaxIterations without a terminal turn.
	ErrMaxIterations = errors.New("agent: max iterations exhausted")
	// ErrAlreadyRunning: Run was called while another run is in flight.
	ErrAlreadyRunning = errors.New("agent: a run is already in progress")
	// ErrSteeringClosed: Steer was called after the agent was reset or the
	// steering channel was closed.
	ErrSteeringClosed = errors.New("agent: steering channel closed")
	// ErrTransformContext: Config.TransformContext returned an error.
	// The underlying error is wrapped; use errors.Unwrap or errors.As to
	// inspect.
	ErrTransformContext = errors.New("agent: TransformContext failed")
)

// ToolExecutionMode controls whether the tool calls inside one
// assistant turn run one-at-a-time or concurrently. Applies at the
// Config level (default for all tools in the batch) and per-AgentTool
// (an opt-out override for tools that aren't safe to run in parallel).
type ToolExecutionMode int

const (
	// ToolExecutionUnspecified is the zero value. On Config, it falls
	// back to ToolExecutionSequential. On AgentTool, it inherits the
	// effective Config setting.
	ToolExecutionUnspecified ToolExecutionMode = iota

	// ToolExecutionSequential runs tool calls one at a time in source
	// order. Hooks see exactly one in-flight call at any moment. This
	// is the default to preserve v0.2.x behavior.
	ToolExecutionSequential

	// ToolExecutionParallel runs Handler invocations concurrently. The
	// pre-flight phase (BeforeToolCall hook + EventToolStart emission)
	// stays sequential in source order; only the Handler + AfterToolCall
	// hook run in goroutines. The tool_result message and ToolLog
	// entries are reassembled in source order so the wire transcript
	// is stable. EventToolEnd events fire as handlers complete
	// (finish order), not source order — observers that need source
	// ordering can sort by ToolCallID or read from Snapshot().ToolLog.
	//
	// Hook authors are responsible for thread-safety under
	// ToolExecutionParallel: BeforeToolCall + AfterToolCall may be
	// invoked concurrently from multiple goroutines. Protect any
	// shared state externally.
	//
	// If any AgentTool in the batch declares ExecutionMode ==
	// ToolExecutionSequential, the entire batch falls back to
	// sequential execution — a safety valve for tool authors who
	// know their handler can't run beside others.
	ToolExecutionParallel
)

// Config configures a new Agent.
type Config struct {
	// LLM is the provider-agnostic interface from pi-llm-go. Required.
	LLM llm.LLM
	// Model is the provider's model id. Required.
	Model string
	// SystemPrompt is the initial system prompt. Forwarded as
	// llm.Request.System on every iteration unless overridden by
	// Agent.SetSystemPrompt at runtime.
	SystemPrompt string
	// Tools available to the model. Duplicates by Name are rejected at New.
	Tools []AgentTool
	// MaxIterations caps the loop. Defaults to 32 when zero.
	MaxIterations int

	// Optional per-request tunables forwarded to llm.Request on every iteration.
	Temperature *float64
	MaxTokens   int
	Thinking    *llm.ThinkingConfig

	// Optional hooks. nil means "no-op."
	//
	// Concurrency: under ToolExecution = ToolExecutionParallel,
	// AfterToolCall may be invoked concurrently from multiple
	// goroutines (one per in-flight tool call). BeforeToolCall stays
	// sequential in source order even under parallel mode — Mario's
	// design preserves the "hooks see calls in order" invariant for
	// preflight even when execution is concurrent. Hook authors must
	// protect any shared state externally. OnSteering is never called
	// concurrently with itself or with any other hook.
	BeforeToolCall BeforeToolCallHook
	AfterToolCall  AfterToolCallHook
	OnSteering     OnSteeringHook

	// DefaultMaxSummarySize is the per-agent default for Result.Summary
	// length, applied to any AgentTool that doesn't set its own
	// MaxSummarySize. 0 falls back to DefaultMaxSummarySize (32 KiB).
	DefaultMaxSummarySize int

	// ToolExecution selects how tool calls inside one assistant turn
	// run. Defaults to ToolExecutionSequential. See ToolExecutionMode
	// godoc for the full contract.
	ToolExecution ToolExecutionMode

	// TransformContext, when non-nil, is called at the top of every
	// iteration with a copy of the current transcript just before the LLM
	// call. The returned slice is used in place of the original. Use this
	// for context-window management (pruning, summarization) and for
	// late-injecting context that should not be persisted in the durable
	// transcript.
	//
	// Contract: must return a non-nil slice. Returning an error aborts
	// the run and propagates as ErrTransformContext-wrapped. Returning
	// the input unchanged is the no-op fallback.
	//
	// The transcript stored on the Agent is not mutated by this hook;
	// only the slice fed into llm.Request is. Snapshot() continues to
	// return the original transcript.
	//
	// Ordering with SetSystemPrompt: the system prompt is read BEFORE
	// TransformContext runs. If the hook calls Agent.SetSystemPrompt,
	// the new prompt takes effect on iteration N+1, not N. To set both
	// system and messages atomically for the current iteration, do both
	// mutations BEFORE the run reaches buildRequest — e.g. via
	// BeforeToolCall on the prior iteration, or from a separate goroutine
	// before Run starts.
	//
	// Mirrors Mario Zechner's pi-mono `transformContext` (see
	// packages/agent/src/types.ts).
	TransformContext func(ctx context.Context, messages []llm.Message) ([]llm.Message, error)
}

// Agent owns the single-loop conversation state. See package doc.
type Agent struct {
	cfg   Config
	tools map[string]AgentTool

	mu sync.RWMutex
	// systemPrompt is the live system prompt. Initialized from
	// cfg.SystemPrompt at New(); mutated via SetSystemPrompt; consumed
	// at every buildRequest() call. Guarded by mu so a goroutine calling
	// SetSystemPrompt cannot race with the loop reading it.
	systemPrompt string
	messages     []llm.Message
	toolLog      []ToolLogEntry
	lastUsage    llm.Usage
	runID        string
	iteration    int
	running      bool

	steering chan llm.Message
}

// New constructs an Agent. Returns an error if required fields are missing
// or tool names collide.
func New(cfg Config) (*Agent, error) {
	if cfg.LLM == nil {
		return nil, errors.New("agent: Config.LLM is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("agent: Config.Model is required")
	}
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = defaultMaxIterations
	}
	a := &Agent{
		cfg:          cfg,
		systemPrompt: cfg.SystemPrompt,
		tools:        make(map[string]AgentTool, len(cfg.Tools)),
		steering:     make(chan llm.Message, steeringBufferSize),
	}
	for _, t := range cfg.Tools {
		if _, dup := a.tools[t.Name]; dup {
			return nil, fmt.Errorf("agent: duplicate tool name %q", t.Name)
		}
		if t.Handler == nil {
			return nil, fmt.Errorf("agent: tool %q has nil Handler", t.Name)
		}
		a.tools[t.Name] = t
	}
	return a, nil
}

// Run executes one user turn from a plain text prompt.
func (a *Agent) Run(ctx context.Context, prompt string) iter.Seq2[AgentEvent, error] {
	msg := llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.Block{llm.TextBlock{Text: prompt}},
	}
	return a.RunMessage(ctx, msg)
}

// RunMessage executes one user turn from a pre-built user Message. Use
// this when the caller assembles a non-trivial user message (e.g. with
// images once pi-llm-go supports them).
//
// The returned iterator must be fully consumed (range to completion or
// break — breaking signals the loop to stop after the current event).
func (a *Agent) RunMessage(ctx context.Context, userMsg llm.Message) iter.Seq2[AgentEvent, error] {
	return func(yield func(AgentEvent, error) bool) {
		if !a.markRunning() {
			yield(nil, ErrAlreadyRunning)
			return
		}
		defer a.markIdle()

		runID := newRunID()
		a.mu.Lock()
		a.runID = runID
		a.iteration = 0
		a.messages = append(a.messages, userMsg)
		a.mu.Unlock()

		// Decorate ctx with the active RunID so every downstream call
		// (BeforeToolCall, AfterToolCall, OnSteering, TransformContext,
		// tool Handler) can read it via agent.RunIDFromContext. Used for
		// span correlation in observability code that doesn't want to
		// thread the id through tool arguments.
		ctx = WithRunID(ctx, runID)

		if !yield(EventRunStart{RunID: runID}, nil) {
			return
		}

		var lastAssistant llm.Message
		for iteration := 1; iteration <= a.cfg.MaxIterations; iteration++ {
			// Drain steering channel before each iteration. Bounded loop;
			// we drain everything currently buffered, not future arrivals.
			if cont := a.drainSteering(ctx, iteration, yield); !cont {
				return
			}

			a.mu.Lock()
			a.iteration = iteration
			a.mu.Unlock()

			if !yield(EventIterationStart{Iteration: iteration}, nil) {
				return
			}

			// One LLM call per iteration.
			req, err := a.buildRequest(ctx)
			if err != nil {
				yield(nil, err)
				return
			}
			assistantMsg, ok := a.runIteration(ctx, iteration, req, yield)
			if !ok {
				return
			}
			lastAssistant = assistantMsg

			// If the model issued no tool calls, the run is done.
			toolCalls := extractToolCalls(assistantMsg)
			if len(toolCalls) == 0 {
				yield(EventRunEnd{FinalMessage: assistantMsg, Iterations: iteration}, nil)
				return
			}

			// Execute tool calls (sequential at v1), apply hooks, bundle results.
			toolResults, cont := a.executeToolCalls(ctx, iteration, toolCalls, yield)
			if !cont {
				return
			}

			a.mu.Lock()
			a.messages = append(a.messages, llm.Message{
				Role:    llm.RoleTool,
				Content: toolResults,
			})
			a.mu.Unlock()
		}

		// MaxIterations exhausted.
		yield(EventRunEnd{FinalMessage: lastAssistant, Iterations: a.cfg.MaxIterations}, ErrMaxIterations)
	}
}

// Steer injects a user message at the next iteration boundary. Returns
// immediately on success. If the buffered steering channel is full,
// Steer blocks until the loop drains; pass a cancellable ctx if the
// caller wants to abandon.
func (a *Agent) Steer(ctx context.Context, msg llm.Message) error {
	select {
	case a.steering <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetSystemPrompt replaces the system prompt used on subsequent iterations.
// The new prompt takes effect at the next buildRequest() call, which
// happens at the top of every iteration after steering drains. Safe to
// call from any goroutine while Run is in progress; the active LLM call
// completes against the prior prompt.
//
// Pair with Steer to inject a user message at the same iteration
// boundary when the prompt change needs an accompanying nudge to the
// model.
//
// Calling SetSystemPrompt from inside Config.TransformContext does NOT
// affect the current iteration: buildRequest reads the system prompt
// before invoking the hook. The change lands on iteration N+1. To
// change both system and messages atomically for iteration N, perform
// the SetSystemPrompt call before the run reaches that iteration's
// buildRequest (e.g. from BeforeToolCall on iteration N-1).
func (a *Agent) SetSystemPrompt(prompt string) {
	a.mu.Lock()
	a.systemPrompt = prompt
	a.mu.Unlock()
}

// SystemPrompt returns the live system prompt. Returns the current value
// even if SetSystemPrompt was called by another goroutine.
func (a *Agent) SystemPrompt() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.systemPrompt
}

// Snapshot returns an immutable point-in-time view of the agent's state.
// Safe to call from any goroutine while Run is in progress.
func (a *Agent) Snapshot() RunSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := RunSnapshot{
		RunID:        a.runID,
		SystemPrompt: a.systemPrompt,
		Iteration:    a.iteration,
		LastUsage:    a.lastUsage,
		IsRunning:    a.running,
	}
	if len(a.messages) > 0 {
		out.Messages = make([]llm.Message, len(a.messages))
		copy(out.Messages, a.messages)
	}
	if len(a.toolLog) > 0 {
		out.ToolLog = make([]ToolLogEntry, len(a.toolLog))
		copy(out.ToolLog, a.toolLog)
	}
	return out
}

// Reset clears the transcript and tool log. Panics if a run is currently
// in progress — callers must cancel ctx and wait for the iterator to
// drain before resetting.
func (a *Agent) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		panic("agent: Reset called while a run is in progress; cancel ctx and drain the iterator first")
	}
	a.messages = nil
	a.toolLog = nil
	a.lastUsage = llm.Usage{}
	a.runID = ""
	a.iteration = 0
	// Drain any buffered steering messages so a fresh run starts clean.
	for {
		select {
		case <-a.steering:
		default:
			return
		}
	}
}

// markRunning transitions the agent to running state. Returns false if
// another run is already in flight (caller should not start a second).
func (a *Agent) markRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return false
	}
	a.running = true
	return true
}

func (a *Agent) markIdle() {
	a.mu.Lock()
	a.running = false
	a.mu.Unlock()
}

// drainSteering pulls every currently-buffered steering message, applies
// the OnSteering hook (if any), appends survivors to the transcript, and
// emits EventSteering for each. Returns false if the consumer aborted via
// yield=false.
func (a *Agent) drainSteering(
	ctx context.Context,
	iteration int,
	yield func(AgentEvent, error) bool,
) bool {
	for {
		select {
		case sm := <-a.steering:
			rc := a.runContextLocked(iteration)
			if a.cfg.OnSteering != nil {
				drop, err := a.cfg.OnSteering(ctx, rc, sm)
				if err != nil {
					yield(nil, fmt.Errorf("OnSteering: %w", err))
					return false
				}
				if drop {
					continue
				}
			}
			a.mu.Lock()
			a.messages = append(a.messages, sm)
			a.mu.Unlock()
			if !yield(EventSteering{Message: sm}, nil) {
				return false
			}
		default:
			return true
		}
	}
}

// runIteration streams one LLM call, emits per-event events, returns the
// assembled assistant message, and appends it to the transcript. Returns
// (msg, true) on success, (zero, false) on error or consumer abort.
func (a *Agent) runIteration(
	ctx context.Context,
	iteration int,
	req llm.Request,
	yield func(AgentEvent, error) bool,
) (llm.Message, bool) {
	acc := newMessageAccumulator()
	for ev, err := range a.cfg.LLM.Stream(ctx, req) {
		if err != nil {
			yield(nil, err)
			return llm.Message{}, false
		}
		if !yield(EventLLMStream{Iteration: iteration, Event: ev}, nil) {
			return llm.Message{}, false
		}
		acc.apply(ev)
	}
	msg := acc.final()
	a.mu.Lock()
	a.messages = append(a.messages, msg)
	a.lastUsage = msg.Usage
	a.mu.Unlock()
	if !yield(EventAssistantMessage{Iteration: iteration, Message: msg}, nil) {
		return llm.Message{}, false
	}
	return msg, true
}

// executeToolCalls dispatches the batch to sequential or parallel
// execution based on Config.ToolExecution and any per-tool ExecutionMode
// override. Returns the per-call ToolResultBlocks (one entry per tool
// call, in SOURCE order regardless of execution mode) and a continue
// flag.
func (a *Agent) executeToolCalls(
	ctx context.Context,
	iteration int,
	calls []llm.ToolCallBlock,
	yield func(AgentEvent, error) bool,
) ([]llm.Block, bool) {
	if a.shouldRunParallel(calls) {
		return a.executeToolCallsParallel(ctx, iteration, calls, yield)
	}
	return a.executeToolCallsSequential(ctx, iteration, calls, yield)
}

// shouldRunParallel reports whether the batch is eligible for parallel
// execution: the agent must be configured Parallel AND no tool in the
// batch may declare ExecutionMode == Sequential. Matches Mario's
// pi-agent semantics — a single sequential tool drags the whole batch
// down to sequential.
func (a *Agent) shouldRunParallel(calls []llm.ToolCallBlock) bool {
	if a.cfg.ToolExecution != ToolExecutionParallel {
		return false
	}
	if len(calls) <= 1 {
		// One tool call can't benefit from parallel; the sequential
		// path is cheaper (no goroutine + channel).
		return false
	}
	for _, c := range calls {
		if t, ok := a.tools[c.Name]; ok && t.ExecutionMode == ToolExecutionSequential {
			return false
		}
	}
	return true
}

// toolOutcome is the bundle of state we collect for one tool call —
// works for both sequential and parallel paths. resultBlock is what
// gets appended to the wire-level tool_result message; logEntry feeds
// the audit ToolLog; endEvent is the EventToolEnd we yield.
type toolOutcome struct {
	resultBlock llm.ToolResultBlock
	logEntry    ToolLogEntry
	endEvent    EventToolEnd
}

// executeOneToolCall runs the Handler + budget enforcement + AfterToolCall
// hook for a single pre-flighted tool call. Pre-flight (BeforeToolCall +
// EventToolStart emission) is the caller's responsibility — this function
// is invoked once the loop has decided the call should proceed.
//
// Safe to call from multiple goroutines under ToolExecutionParallel:
// reads only immutable closure data (cfg, tools map) and the
// caller-owned preflightResult, and produces a fully-owned
// toolOutcome. The caller serializes writes to a.toolLog and yields
// of the resulting end event.
func (a *Agent) executeOneToolCall(
	ctx context.Context,
	iteration int,
	call llm.ToolCallBlock,
	pre preflightResult,
) (toolOutcome, error) {
	info, rc, tool, toolFound, skip, skipMsg := pre.info, pre.rc, pre.tool, pre.toolFound, pre.skip, pre.skipMsg
	startedAt := time.Now()

	// Effective per-tool budget. Resolved here so the same value
	// applies to the handler's result AND to AfterToolCall overrides
	// — a hook that doubles the summary length without realizing it
	// would otherwise sneak past the budget.
	maxSize := a.cfg.DefaultMaxSummarySize
	if toolFound && tool.MaxSummarySize > 0 {
		maxSize = tool.MaxSummarySize
	}
	if maxSize <= 0 {
		maxSize = DefaultMaxSummarySize
	}

	var result Result
	isError := false

	switch {
	case skip:
		msg := skipMsg
		if msg == "" {
			msg = defaultBlockedMessage
		}
		result = Result{Summary: msg}
		isError = true
	case !toolFound:
		result = Result{Summary: defaultUnknownToolMessage + ": " + info.Name}
		isError = true
	default:
		r, err := tool.Handler(ctx, info.Arguments)
		if err != nil {
			result = Result{Summary: err.Error()}
			isError = true
		} else {
			result = r
		}
	}

	// AfterToolCall hook — may override the result. Returning a hook
	// error aborts the run (caller surfaces the error via yield).
	if a.cfg.AfterToolCall != nil {
		override, err := a.cfg.AfterToolCall(ctx, rc, info, result, isError)
		if err != nil {
			return toolOutcome{}, fmt.Errorf("AfterToolCall: %w", err)
		}
		if override != nil {
			result = *override
		}
	}

	// Budget enforcement. The tool author's bug if violated; we don't
	// abort the run, we replace the result with a clear error so the
	// model sees the violation and the tool author sees it in tests /
	// event logs.
	effective := result.effectiveSummary()
	if len(effective) > maxSize {
		result = Result{
			Summary: fmt.Sprintf("tool %q returned a summary of %d bytes; max is %d. Bug in the tool's summary budgeting; surface large outputs via FullPayloadHint instead.",
				info.Name, len(effective), maxSize),
			FullPayloadHint: result.FullPayloadHint,
		}
		isError = true
		effective = result.Summary
	}

	endedAt := time.Now()
	return toolOutcome{
		resultBlock: llm.ToolResultBlock{
			ToolCallID: call.ID,
			Content:    effective,
			IsError:    isError,
		},
		logEntry: ToolLogEntry{
			Iteration:       iteration,
			ToolCallID:      call.ID,
			Name:            call.Name,
			Arguments:       call.Arguments,
			Result:          effective,
			FullPayloadHint: result.FullPayloadHint,
			IsError:         isError,
			StartedAt:       startedAt,
			EndedAt:         endedAt,
		},
		endEvent: EventToolEnd{
			ToolCallID:      call.ID,
			Name:            call.Name,
			Result:          effective,
			IsError:         isError,
			FullPayloadHint: result.FullPayloadHint,
		},
	}, nil
}

// preflight runs the BeforeToolCall hook (if any), resolves the tool
// from the registry, and returns the inputs executeOneToolCall needs.
// Sequential in both execution modes — Mario's design preserves the
// "hook sees calls in source order" guarantee even under parallel.
type preflightResult struct {
	info      ToolCallInfo
	rc        RunContext
	tool      AgentTool
	toolFound bool
	skip      bool
	skipMsg   string
	emitStart bool // false when the call was skipped or unknown — no Handler will run
}

func (a *Agent) preflight(ctx context.Context, iteration int, call llm.ToolCallBlock) (preflightResult, error) {
	info := ToolCallInfo{
		ToolCallID: call.ID,
		Name:       call.Name,
		Arguments:  call.Arguments,
	}
	rc := a.runContextLocked(iteration)

	skip := false
	skipMsg := ""
	if a.cfg.BeforeToolCall != nil {
		s, er, err := a.cfg.BeforeToolCall(ctx, rc, info)
		if err != nil {
			return preflightResult{}, fmt.Errorf("BeforeToolCall: %w", err)
		}
		skip = s
		skipMsg = er
	}

	tool, toolFound := a.tools[info.Name]
	emitStart := !skip && toolFound
	return preflightResult{
		info:      info,
		rc:        rc,
		tool:      tool,
		toolFound: toolFound,
		skip:      skip,
		skipMsg:   skipMsg,
		emitStart: emitStart,
	}, nil
}

// executeToolCallsSequential runs tool calls one-after-another in
// source order. This is the v0.2.x behavior; it remains the default
// and the fallback when Config.ToolExecution is Sequential or when any
// AgentTool in the batch declared ExecutionMode = Sequential.
func (a *Agent) executeToolCallsSequential(
	ctx context.Context,
	iteration int,
	calls []llm.ToolCallBlock,
	yield func(AgentEvent, error) bool,
) ([]llm.Block, bool) {
	results := make([]llm.Block, 0, len(calls))
	for _, call := range calls {
		pre, err := a.preflight(ctx, iteration, call)
		if err != nil {
			yield(nil, err)
			return nil, false
		}
		if pre.emitStart {
			if !yield(EventToolStart{
				ToolCallID: call.ID,
				Name:       call.Name,
				Arguments:  call.Arguments,
			}, nil) {
				return nil, false
			}
		}
		outcome, err := a.executeOneToolCall(ctx, iteration, call, pre)
		if err != nil {
			yield(nil, err)
			return nil, false
		}
		a.mu.Lock()
		a.toolLog = append(a.toolLog, outcome.logEntry)
		a.mu.Unlock()
		results = append(results, outcome.resultBlock)
		if !yield(outcome.endEvent, nil) {
			return nil, false
		}
	}
	return results, true
}

// executeToolCallsParallel runs Handler + AfterToolCall concurrently
// while keeping pre-flight (BeforeToolCall + EventToolStart) sequential
// in source order. tool_result blocks + ToolLog entries land in source
// order on the wire; EventToolEnd events fire as Handlers complete
// (finish order). Hook authors must protect any shared state across
// concurrent goroutines.
func (a *Agent) executeToolCallsParallel(
	ctx context.Context,
	iteration int,
	calls []llm.ToolCallBlock,
	yield func(AgentEvent, error) bool,
) ([]llm.Block, bool) {
	// Phase 1 (sequential): run BeforeToolCall for each call, emit
	// EventToolStart events, classify each call as immediate (skip /
	// unknown tool — no Handler) or async (Handler to fire in Phase 2).
	type slot struct {
		call      llm.ToolCallBlock
		pre       preflightResult
		immediate *toolOutcome // non-nil when no Handler will fire (skip / unknown)
	}
	slots := make([]slot, len(calls))
	for i, call := range calls {
		pre, err := a.preflight(ctx, iteration, call)
		if err != nil {
			yield(nil, err)
			return nil, false
		}
		s := slot{call: call, pre: pre}
		if pre.emitStart {
			if !yield(EventToolStart{
				ToolCallID: call.ID,
				Name:       call.Name,
				Arguments:  call.Arguments,
			}, nil) {
				return nil, false
			}
		} else {
			// Skip / unknown — produce the outcome synchronously now so
			// Phase 2 can drain it through the same channel ordering as
			// real Handler runs.
			outcome, err := a.executeOneToolCall(ctx, iteration, call, pre)
			if err != nil {
				yield(nil, err)
				return nil, false
			}
			s.immediate = &outcome
		}
		slots[i] = s
	}

	// Phase 2 (parallel): fire Handlers + AfterToolCall in goroutines
	// for the async slots; pipe outcomes (and their source index) over
	// a buffered channel. Buffered to len(calls) so no goroutine ever
	// blocks on send — every slot produces exactly one outcome.
	//
	// We own the CancelFunc directly (not just errgroup's implicit one)
	// so that a consumer-abort path can signal in-flight Handlers via
	// gctx.Done(). errgroup.WithContext only cancels its derived ctx on
	// the FIRST non-nil return from g.Go — not on yield returning false.
	parCtx, parCancel := context.WithCancel(ctx)
	defer parCancel()
	type finished struct {
		idx     int
		outcome toolOutcome
	}
	done := make(chan finished, len(calls))
	g, gctx := errgroup.WithContext(parCtx)
	for i := range slots {
		if slots[i].immediate != nil {
			done <- finished{idx: i, outcome: *slots[i].immediate}
			continue
		}
		s := slots[i]
		g.Go(func() error {
			outcome, err := a.executeOneToolCall(gctx, iteration, s.call, s.pre)
			if err != nil {
				// Returning err cancels gctx; other handlers see the
				// cancellation. Caller surfaces the error after Wait().
				return err
			}
			// Go 1.22+ per-iteration `i` is goroutine-safe — no
			// `i := i` shadow needed.
			done <- finished{idx: i, outcome: outcome}
			return nil
		})
	}

	// Drain finished outcomes in FINISH ORDER while goroutines run.
	// `waitDone` closes once every goroutine has returned (success or
	// error). We hold both signals because g.Wait blocks until all
	// goroutines complete, which lets us detect "all done" without
	// counting outcomes against pending while goroutines are still
	// in-flight.
	var (
		waitErr  error
		waitDone = make(chan struct{})
	)
	go func() {
		waitErr = g.Wait()
		close(waitDone)
	}()

	outcomes := make([]toolOutcome, len(calls))
	pending := len(calls)
DRAIN:
	for pending > 0 {
		select {
		case f := <-done:
			outcomes[f.idx] = f.outcome
			pending--
			if !yield(f.outcome.endEvent, nil) {
				// Consumer aborted. Cancel the context we own so
				// in-flight Handlers honoring ctx.Done() unwind
				// immediately; wait for them to settle so they don't
				// outlive the run, then return.
				parCancel()
				<-waitDone
				return nil, false
			}
		case <-waitDone:
			// All goroutines have returned. Either an error short-
			// circuited the rest (waitErr != nil), or every Handler
			// completed cleanly and the remaining outcomes are buffered
			// in `done`. Switch to a plain drain of the buffered
			// channel so we don't spin on the always-ready closed
			// waitDone.
			if waitErr != nil {
				yield(nil, waitErr)
				return nil, false
			}
			break DRAIN
		}
	}
	// Drain any outcomes still buffered after waitDone fired
	// (Handlers that finished while we were blocked in yield).
	for pending > 0 {
		f := <-done
		outcomes[f.idx] = f.outcome
		pending--
		if !yield(f.outcome.endEvent, nil) {
			parCancel()
			return nil, false
		}
	}

	// Phase 3 (sequential, source order): assemble tool_result blocks
	// and ToolLog entries in SOURCE order, regardless of finish order.
	a.mu.Lock()
	for i := range outcomes {
		a.toolLog = append(a.toolLog, outcomes[i].logEntry)
	}
	a.mu.Unlock()

	results := make([]llm.Block, 0, len(calls))
	for i := range outcomes {
		results = append(results, outcomes[i].resultBlock)
	}
	return results, true
}

// buildRequest snapshots the current transcript into a llm.Request,
// applying Config.TransformContext (if set) at the message-slice
// boundary. The transcript stored on the Agent is not mutated by the
// transform; only the request fed into the LLM is.
func (a *Agent) buildRequest(ctx context.Context) (llm.Request, error) {
	a.mu.RLock()
	tools := make([]llm.Tool, 0, len(a.cfg.Tools))
	for _, t := range a.cfg.Tools {
		tools = append(tools, t.Tool)
	}
	msgs := make([]llm.Message, len(a.messages))
	copy(msgs, a.messages)
	system := a.systemPrompt
	a.mu.RUnlock()

	if a.cfg.TransformContext != nil {
		transformed, err := a.cfg.TransformContext(ctx, msgs)
		if err != nil {
			return llm.Request{}, fmt.Errorf("%w: %w", ErrTransformContext, err)
		}
		if transformed == nil {
			return llm.Request{}, fmt.Errorf("%w: returned nil slice", ErrTransformContext)
		}
		msgs = transformed
	}

	return llm.Request{
		Model:       a.cfg.Model,
		System:      system,
		Messages:    msgs,
		Tools:       tools,
		Temperature: a.cfg.Temperature,
		MaxTokens:   a.cfg.MaxTokens,
		Thinking:    a.cfg.Thinking,
	}, nil
}

// runContextLocked builds a RunContext snapshot for the current run.
// Internal callers hold the appropriate lock or are on the loop goroutine.
func (a *Agent) runContextLocked(iteration int) RunContext {
	a.mu.RLock()
	defer a.mu.RUnlock()
	msgs := make([]llm.Message, len(a.messages))
	copy(msgs, a.messages)
	return RunContext{
		RunID:     a.runID,
		Iteration: iteration,
		Messages:  msgs,
	}
}

func extractToolCalls(msg llm.Message) []llm.ToolCallBlock {
	var calls []llm.ToolCallBlock
	for _, block := range msg.Content {
		if tc, ok := block.(llm.ToolCallBlock); ok {
			calls = append(calls, tc)
		}
	}
	return calls
}

// newRunID generates a sortable, unique run id: "run_<unix-ns-hex>_<8-rand-hex>".
// No external deps; sufficient uniqueness for log correlation.
func newRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("run_%016x_%s", time.Now().UnixNano(), hex.EncodeToString(b[:]))
}

// messageAccumulator folds llm.StreamEvents into a complete assistant
// Message. Internal sibling of llm.Accumulate (which yields snapshots);
// this one only cares about the final message.
type messageAccumulator struct {
	msg              llm.Message
	textBuilders     map[int]*strings.Builder
	thinkBuilders    map[int]*strings.Builder
	toolArgBuilders  map[int]*strings.Builder
	thinkSignatures  map[int]string
	toolCallMetadata map[int]toolCallMeta
}

type toolCallMeta struct {
	id   string
	name string
}

func newMessageAccumulator() *messageAccumulator {
	return &messageAccumulator{
		msg:              llm.Message{Role: llm.RoleAssistant},
		textBuilders:     map[int]*strings.Builder{},
		thinkBuilders:    map[int]*strings.Builder{},
		toolArgBuilders:  map[int]*strings.Builder{},
		thinkSignatures:  map[int]string{},
		toolCallMetadata: map[int]toolCallMeta{},
	}
}

func (a *messageAccumulator) apply(event llm.StreamEvent) {
	switch e := event.(type) {
	case llm.EventMessageStart:
		a.msg.Model = e.Model
	case llm.EventTextStart:
		a.ensureBlock(e.BlockIndex)
		a.textBuilders[e.BlockIndex] = &strings.Builder{}
	case llm.EventTextDelta:
		if b, ok := a.textBuilders[e.BlockIndex]; ok {
			b.WriteString(e.Delta)
		}
	case llm.EventTextEnd:
		if b, ok := a.textBuilders[e.BlockIndex]; ok {
			a.msg.Content[e.BlockIndex] = llm.TextBlock{Text: b.String()}
		}
	case llm.EventThinkingStart:
		a.ensureBlock(e.BlockIndex)
		a.thinkBuilders[e.BlockIndex] = &strings.Builder{}
	case llm.EventThinkingDelta:
		if b, ok := a.thinkBuilders[e.BlockIndex]; ok {
			b.WriteString(e.Delta)
		}
	case llm.EventThinkingEnd:
		if b, ok := a.thinkBuilders[e.BlockIndex]; ok {
			a.msg.Content[e.BlockIndex] = llm.ThinkingBlock{
				Thinking:  b.String(),
				Signature: e.Signature,
			}
		}
	case llm.EventToolCallStart:
		a.ensureBlock(e.BlockIndex)
		a.toolCallMetadata[e.BlockIndex] = toolCallMeta{id: e.ID, name: e.Name}
		a.toolArgBuilders[e.BlockIndex] = &strings.Builder{}
	case llm.EventToolCallDelta:
		if b, ok := a.toolArgBuilders[e.BlockIndex]; ok {
			b.WriteString(e.Delta)
		}
	case llm.EventToolCallEnd:
		meta := a.toolCallMetadata[e.BlockIndex]
		args := e.Arguments
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		a.msg.Content[e.BlockIndex] = llm.ToolCallBlock{
			ID:        meta.id,
			Name:      meta.name,
			Arguments: args,
		}
	case llm.EventMessageEnd:
		a.msg.StopReason = e.StopReason
		a.msg.Usage = e.Usage
	}
}

func (a *messageAccumulator) ensureBlock(idx int) {
	for len(a.msg.Content) <= idx {
		a.msg.Content = append(a.msg.Content, nil)
	}
}

func (a *messageAccumulator) final() llm.Message {
	return a.msg
}
