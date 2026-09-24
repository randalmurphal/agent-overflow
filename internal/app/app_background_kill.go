package app

import (
	"encoding/json"
	"fmt"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// A Claude `control_request{interrupt}` kills every live async agent the
// session holds, whichever turn launched it, together with the background
// shells each agent owns (claude-wire.md §Background task ownership). The
// two Stop RPCs, InterruptTurn and InterruptAndRevertIfClean, therefore
// refuse while such agents are live until the caller confirms, and name the
// agents so the person can see what the stop would cost. The refusal comes
// before the call has any effect.
//
// Agent thread requests, their cancels and workflow takeovers interrupt
// through the same code ungated: none of them is a person's Stop, and the
// caller that started the turn is the one stopping it.

// BackgroundKillAgent is one live background agent a provider interrupt on
// its thread would kill.
type BackgroundKillAgent struct {
	// LaunchItemID is the launch row the agent's run state is served on: the
	// Agent call, or the resume carrier running a resumed round.
	LaunchItemID string `json:"launchItemId"`
	// Description is the task line the agent is named by.
	Description string `json:"description"`
	// RunState is "running", or "parked" for an agent that reported and
	// waits on background commands it started (claude-wire.md §E6b).
	RunState string `json:"runState"`
	// TranscriptRootID is the row whose subtree holds the agent's
	// transcript, the id that opens its agent pane (agentScopeRootId).
	TranscriptRootID string `json:"transcriptRootId"`
}

// interruptKillsAgent reports whether a Claude interrupt kills an agent in
// the given served run state. A running agent dies, and so does a parked one
// with the shells it waits on; neither ever wakes.
func interruptKillsAgent(runState string) bool {
	switch runState {
	case triage.AgentRunRunning, triage.AgentRunParked:
		return true
	}
	return false
}

// RunningBackgroundAgents lists the live background agents a Stop on this
// thread would kill, as a refused Stop reports them. Empty for a thread
// whose provider's interrupt leaves its agents running.
//
//ao:scope threads:read
func (a *App) RunningBackgroundAgents(threadID string) ([]BackgroundKillAgent, error) {
	if err := a.store.CheckForkReady(threadID); err != nil {
		return nil, err
	}
	return a.runningBackgroundAgents(threadID)
}

// runningBackgroundAgents reads the answer from the store: the tray's live
// list with the park model's run states, never a transcript.
func (a *App) runningBackgroundAgents(threadID string) ([]BackgroundKillAgent, error) {
	agents := []BackgroundKillAgent{}
	if a.triage == nil {
		return agents, nil
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, fmt.Errorf("running background agents: load thread: %w", err)
	}
	if thread.Provider != string(provider.Claude) {
		return agents, nil
	}
	cutoff := time.Now().UnixMilli() - backgroundTaskRetentionMillis
	items, err := a.store.ListLiveBackgroundTasks(threadID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("running background agents: list live background tasks: %w", err)
	}
	items, err = a.triage.DecorateAgentRunStates(threadID, items)
	if err != nil {
		return nil, fmt.Errorf("running background agents: %w", err)
	}
	for _, item := range items {
		if item.CompletionOf != "" || !triage.IsSubagentTranscriptLaunch(item) {
			continue
		}
		state := triage.AgentRunState(item)
		if !interruptKillsAgent(state) {
			continue
		}
		rootID, err := a.triage.TranscriptRootID(threadID, item)
		if err != nil {
			return nil, fmt.Errorf("running background agents: transcript root of %s: %w", item.ID, err)
		}
		description := triage.AgentLaunchDescription(item)
		if description == "" {
			description = item.Summary
		}
		agents = append(agents, BackgroundKillAgent{
			LaunchItemID:     item.ID,
			Description:      description,
			RunState:         state,
			TranscriptRootID: rootID,
		})
	}
	return agents, nil
}

// refuseBackgroundKill returns a backgroundKillRefusal when interrupting
// sess would kill live background agents on threadID. Only a Claude session
// is refused: its interrupt is the one that kills async agents.
func (a *App) refuseBackgroundKill(threadID string, sess session) error {
	if sess.Claude == nil {
		return nil
	}
	agents, err := a.runningBackgroundAgents(threadID)
	if err != nil {
		return fmt.Errorf("interrupt %s: %w", threadID, err)
	}
	if len(agents) == 0 {
		return nil
	}
	return newBackgroundKillRefusal(agents)
}

// backgroundKillRefusal is the public background_agents_running error. The
// transport sends Agents as the frame's backgroundAgents field.
type backgroundKillRefusal struct {
	Agents  []BackgroundKillAgent
	payload json.RawMessage
	public  error
}

func newBackgroundKillRefusal(agents []BackgroundKillAgent) error {
	payload, err := json.Marshal(agents)
	if err != nil {
		return fmt.Errorf("encode background agents: %w", err)
	}
	message := "Stopping now would also stop 1 background agent. Confirm to stop it."
	if len(agents) != 1 {
		message = fmt.Sprintf("Stopping now would also stop %d background agents. Confirm to stop them.", len(agents))
	}
	return &backgroundKillRefusal{
		Agents:  agents,
		payload: payload,
		public:  errorsx.Public(transport.ErrCodeBackgroundAgentsRunning, message, nil),
	}
}

func (e *backgroundKillRefusal) Error() string { return e.public.Error() }

func (e *backgroundKillRefusal) Unwrap() error { return e.public }

// RefusedBackgroundAgents is the frame's backgroundAgents payload.
func (e *backgroundKillRefusal) RefusedBackgroundAgents() json.RawMessage { return e.payload }
