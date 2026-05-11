package agent

import (
	"encoding/json"

	llm "github.com/amittimalsina/pi-llm-go"
)

// AgentEvent is the sealed sum type emitted during Agent.Run. Consumers
// type-switch on the concrete value.
//
// Lifecycle (one Run call):
//
//	EventRunStart
//	( EventIterationStart
//	  EventLLMStream*   // wraps every llm.StreamEvent for that LLM call
//	  EventAssistantMessage
//	  ( EventSteering* | EventToolStart EventToolEnd )*
//	)*
//	EventRunEnd
//
// The run terminates when either (a) an assistant turn has no tool calls
// and no steering messages are pending, (b) MaxIterations is hit, or
// (c) an error propagates through the iterator's error half.
type AgentEvent interface {
	isAgentEvent()
}

// EventRunStart is the first event emitted by Run.
type EventRunStart struct {
	RunID string
}

// EventIterationStart marks the start of each iteration (each LLM call).
type EventIterationStart struct {
	Iteration int
}

// EventLLMStream wraps every llm.StreamEvent for the current LLM call,
// so consumers that want token-level streaming can type-switch through
// to the inner event.
type EventLLMStream struct {
	Iteration int
	Event     llm.StreamEvent
}

// EventAssistantMessage is emitted once per iteration with the fully
// assembled assistant Message returned by the LLM.
type EventAssistantMessage struct {
	Iteration int
	Message   llm.Message
}

// EventSteering is emitted when a steering message is dequeued and
// injected into the transcript.
type EventSteering struct {
	Message llm.Message
}

// EventToolStart is emitted just before a tool's Handler runs, after any
// BeforeToolCall hook has cleared.
type EventToolStart struct {
	ToolCallID string
	Name       string
	Arguments  json.RawMessage
}

// EventToolEnd is emitted after a tool finishes executing and any
// AfterToolCall hook has been applied.
type EventToolEnd struct {
	ToolCallID string
	Name       string
	Result     string
	IsError    bool
}

// EventRunEnd is the terminal event for a Run. FinalMessage is the last
// assistant message produced; Iterations is the total LLM call count.
type EventRunEnd struct {
	FinalMessage llm.Message
	Iterations   int
}

func (EventRunStart) isAgentEvent()         {}
func (EventIterationStart) isAgentEvent()   {}
func (EventLLMStream) isAgentEvent()        {}
func (EventAssistantMessage) isAgentEvent() {}
func (EventSteering) isAgentEvent()         {}
func (EventToolStart) isAgentEvent()        {}
func (EventToolEnd) isAgentEvent()          {}
func (EventRunEnd) isAgentEvent()           {}
