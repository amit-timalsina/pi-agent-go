package agent

import (
	"errors"

	llm "github.com/amit-timalsina/pi-llm-go"
)

// Restore reconstructs an Agent from a prior Snapshot. The returned
// Agent is ready to receive Run / RunMessage calls; the previous
// transcript, ToolLog, system prompt, and last usage are preserved so
// downstream Runs see the prior conversation as context. The next
// call to Run / RunMessage generates a fresh RunID — the snapshot's
// RunID is metadata only (carried in agent state until the next run
// overwrites it).
//
// Restore enables long-running agents to survive process restarts:
// snapshot the state, persist it (callers own the storage layer for
// v0.5.0; see Deferred section in CHANGELOG), then reconstruct with
// Restore on the next process boot. Steering channel contents are NOT
// preserved across restore (the buffered channel is process-local);
// any pending steering must be re-injected post-restore.
//
// Constraints:
//
//   - snap.IsRunning must be false. You cannot resume a run that was
//     mid-flight; snapshots taken while a run is in progress capture
//     a consistent point-in-time view, but the LLM call that was
//     streaming when Snapshot fired is lost.
//   - cfg must satisfy the same constraints as New (LLM, Model
//     required; no duplicate tool names; no nil handlers). Tool
//     registry MAY differ from the original — adding or removing
//     tools is allowed, since tool registration is for the next run's
//     model, not for the historical transcript.
//   - cfg.SystemPrompt is overridden by snap.SystemPrompt when the
//     latter is non-empty (so SetSystemPrompt mutations round-trip
//     correctly). To force the cfg's prompt regardless, blank
//     snap.SystemPrompt before calling Restore.
//
// Note on serialization: RunSnapshot contains `llm.Message` values
// whose `Content []llm.Block` field is an interface slice. The Go
// stdlib `encoding/json` marshals concrete blocks fine but cannot
// unmarshal them back into the interface without a discriminator.
// Native JSON round-trip support is planned for a future pi-llm-go
// release; until then, callers serialize via `gob` (works
// natively) or a custom encoder. See examples/snapshot_resume for an
// in-process round-trip pattern that demonstrates the contract
// without any encoder.
func Restore(cfg Config, snap RunSnapshot) (*Agent, error) {
	if snap.IsRunning {
		return nil, errors.New("agent.Restore: snap.IsRunning is true; cannot resume a run that was mid-flight")
	}
	a, err := New(cfg)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	// SystemPrompt: snap wins when non-empty so SetSystemPrompt
	// mutations on the original agent round-trip. Empty snap.SystemPrompt
	// falls through to cfg.SystemPrompt (already set by New).
	if snap.SystemPrompt != "" {
		a.systemPrompt = snap.SystemPrompt
	}

	// Defensive copies — caller may keep the snap around and mutate
	// its slices; we own our state.
	if len(snap.Messages) > 0 {
		a.messages = append(a.messages[:0:0], snap.Messages...)
	}
	if len(snap.ToolLog) > 0 {
		a.toolLog = append(a.toolLog[:0:0], snap.ToolLog...)
	}
	a.lastUsage = snap.LastUsage
	a.runID = snap.RunID
	a.iteration = snap.Iteration
	return a, nil
}

// Static assertion that llm.Message + ToolLogEntry are the only
// non-trivial state Restore needs to thread through. If a new
// load-bearing field gets added to RunSnapshot, this assertion's
// absence will surface in the diff review.
var _ = llm.Message{}
