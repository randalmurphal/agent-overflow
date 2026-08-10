package main

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/wake"
)

// `agent-overflow run watch` (D53): the one method that BLOCKS.
//
// Every other method on this surface answers from what is already persisted and
// returns. This one holds the request until the run tree it names moves, which
// is the whole feature: a supervising agent waiting on a wave otherwise polls,
// and polling is what produced 712 status reads and seven hand-rolled monitor
// loops in one campaign — one of which died without saying so.
//
// It is a long poll rather than a subscription because the CLI's transport is
// one scoped HTTP POST per invocation (`internal/transport/httprpc.go`). A
// subscription would mean giving scoped tokens a WebSocket, a replay ring, and
// a per-connection channel filter — a second wire for one verb. The hold is
// bounded (maxWorkflowWatchHold) so that the caller's own HTTP timeout is never
// what ends the call, and so a credential revoked mid-watch — the session that
// minted it ended — is discovered by the next call's 401 rather than by a
// watcher that hangs until someone kills it.

// maxWorkflowWatchHold bounds one blocked call. It sits under the CLI's 30s RPC
// timeout on purpose: the client must be the one still waiting when the server
// answers, never the other way round, or every quiet minute would look like a
// dead backend. It is also the worst case for noticing a revoked token.
const maxWorkflowWatchHold = 25 * time.Second

// WorkflowAgentWatchInput is `run watch`. Cursor is the sequence the caller
// already has: zero means "I have none", which is answered immediately with the
// run's current state so a watch on an already-resting run exits instead of
// blocking on a transition that has already happened.
type WorkflowAgentWatchInput struct {
	ItemID string `json:"itemId"`
	Cursor int64  `json:"cursor,omitempty"`
	// Tree widens the watch to the run and every run it called, transitively.
	// The set is re-resolved on every wake, so a wave started while this call
	// was blocked is watched from its birth transition rather than from the
	// next call.
	Tree bool `json:"tree,omitempty"`
	// WaitMillis is how long the caller is willing to have this call block,
	// clamped to maxWorkflowWatchHold. It exists so `--timeout` is exact: the
	// last poll of a bounded watch waits the remainder and not a second more.
	WaitMillis int64 `json:"waitMillis,omitempty"`
}

// WorkflowAgentTransition is one item-state transition of a watched run. The
// coordinate is the engine's own — the phase and attempt the run was in when it
// moved — so Cause names the attempt a park actually rests on.
type WorkflowAgentTransition struct {
	Seq     int64  `json:"seq"`
	At      int64  `json:"at"`
	ItemID  string `json:"itemId"`
	PhaseID string `json:"phaseId,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	// From is empty for the birth transition of a run that has just started,
	// which is how a `--tree` watcher sees a new wave appear.
	From    string `json:"from,omitempty"`
	To      string `json:"to"`
	Reason  string `json:"reason,omitempty"`
	Cause   string `json:"cause,omitempty"`
	Resting bool   `json:"resting"`
}

// WorkflowAgentWatchRunState is where the watched run is right now, read from
// SQLite rather than derived from the transitions — a watcher that resynced
// after a gap has to be told the truth, not the tail of a ring.
type WorkflowAgentWatchRunState struct {
	ItemID     string `json:"itemId"`
	WorkflowID string `json:"workflowId,omitempty"`
	Goal       string `json:"goal,omitempty"`
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	PhaseID    string `json:"phaseId,omitempty"`
	Resting    bool   `json:"resting"`
	// Repair is the sentence naming the verb that settles this park, composed by
	// the same helper every wake uses (`wake.RepairSentence`) so the two surfaces
	// cannot send a reader to different commands for one reason. Present only
	// once the run is resting, and empty for the reasons that have no one verb.
	Repair string `json:"repair,omitempty"`
}

// WorkflowAgentWatchResult is one long poll's answer.
type WorkflowAgentWatchResult struct {
	ItemID      string                     `json:"itemId"`
	Cursor      int64                      `json:"cursor"`
	Transitions []WorkflowAgentTransition  `json:"transitions"`
	Run         WorkflowAgentWatchRunState `json:"run"`
	// Gap says transitions between the caller's cursor and the oldest retained
	// one were lost — the ring evicted them, or this backend restarted and
	// re-seeded its sequence. It is a resync instruction, exactly as it is on the
	// event wire: the run state above is current, and the cursor to continue from
	// is the one returned.
	Gap bool `json:"gap,omitempty"`
}

// WorkflowAgentWatchRun blocks until a watched run transitions, the caller's
// wait budget expires, or the run is already resting.
//
// LocalOnly: see WorkflowAgentRunStatus. It takes the same grants as reading a
// run's status, because that is what it is — the same fact, delivered when it
// changes instead of when it is asked for.
func (a *App) WorkflowAgentWatchRun(ctx context.Context, input WorkflowAgentWatchInput) (WorkflowAgentWatchResult, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return WorkflowAgentWatchResult{}, err
	}
	item, err := a.scopedRun(scope, input.ItemID, "workflow run watch", true)
	if err != nil {
		return WorkflowAgentWatchResult{}, err
	}
	hold := workflowWatchHold(input.WaitMillis)
	deadline := time.Now().Add(hold)
	// ONE timer for the whole call. The broadcast below is global — any run's
	// transition anywhere in the app wakes every watcher — so a busy backend
	// runs this loop many times inside one hold, and a `time.After` per
	// iteration would leave every one of those timers armed until the deadline
	// it was created for.
	expiry := time.NewTimer(hold)
	defer expiry.Stop()
	for {
		// The wait channel is taken BEFORE the scan, so a transition landing
		// between the two closes a channel this iteration is about to select on
		// rather than one it already missed.
		changed := a.workflowWatch.wait()
		watched, err := a.workflowWatchedRuns(item, input.Tree)
		if err != nil {
			return WorkflowAgentWatchResult{}, err
		}
		transitions, head, gap := a.workflowWatch.since(input.Cursor, func(itemID string) bool {
			return watched[itemID]
		})
		// Every read below is keyed on the AUTHORIZED item, never on the raw
		// request field: `scopedRun` trims the id it checked, so a caller sending
		// "run-1\n" would otherwise pass authorization and then be answered with a
		// bare `sql.ErrNoRows` from a lookup of a run that does not exist.
		current, err := a.workflowWatchRunState(item.ID)
		if err != nil {
			return WorkflowAgentWatchResult{}, err
		}
		if input.Cursor == 0 || gap || len(transitions) > 0 || current.Resting || !time.Now().Before(deadline) {
			// A first call carries no transitions and no gap — the hub answers a
			// caller with no cursor with the head alone — so nothing here needs to
			// special-case it.
			projected, err := a.workflowWatchTransitions(transitions)
			if err != nil {
				return WorkflowAgentWatchResult{}, err
			}
			return WorkflowAgentWatchResult{
				ItemID: item.ID, Cursor: head, Run: current, Gap: gap,
				Transitions: projected,
			}, nil
		}
		select {
		case <-changed:
		case <-expiry.C:
		case <-ctx.Done():
			// The caller hung up (or the app is shutting down). Answering with the
			// current state costs nothing and is never wrong; the client has
			// already stopped listening.
			return WorkflowAgentWatchResult{ItemID: item.ID, Cursor: head, Run: current,
				Transitions: []WorkflowAgentTransition{}}, nil
		}
	}
}

// workflowWatchHold clamps a caller's wait budget. A negative or absent one
// takes the full hold: a watcher that names no budget wants the longest block
// this method offers.
func workflowWatchHold(requested int64) time.Duration {
	if requested <= 0 || time.Duration(requested)*time.Millisecond > maxWorkflowWatchHold {
		return maxWorkflowWatchHold
	}
	return time.Duration(requested) * time.Millisecond
}

// workflowWatchedRuns resolves which runs this watch reports on. A tree watch
// reads the descendants the caller is already entitled to see: `run inspect`
// lists the same children for the same authority, because a wider view of a run
// the caller may already see is not a wider set of runs.
//
// It resolves through the NODE tree read, and that is a requirement rather than
// a preference: this runs again on every wake of a globally-broadcast loop, so
// per tree member it must not drag a frozen workflow snapshot across the SQLite
// boundary NOR make SQLite parse one to compute a phase ordinal nobody here
// reads. Ids are the entire answer.
func (a *App) workflowWatchedRuns(item store.WorkItem, tree bool) (map[string]bool, error) {
	if !tree {
		return map[string]bool{item.ID: true}, nil
	}
	members, err := a.workflowRunTreeNodes(item.ID)
	if err != nil {
		return nil, err
	}
	watched := make(map[string]bool, len(members))
	for _, member := range members {
		watched[member.ID] = true
	}
	return watched, nil
}

// workflowWatchRunState reads the watched run's current coordinate and, once it
// has stopped doing work, the verb that settles it.
func (a *App) workflowWatchRunState(itemID string) (WorkflowAgentWatchRunState, error) {
	summary, err := a.store.GetWorkItemSummary(itemID)
	if err != nil {
		return WorkflowAgentWatchRunState{}, err
	}
	state := WorkflowAgentWatchRunState{
		ItemID: summary.ID, WorkflowID: summary.WorkflowID, Goal: summary.Goal,
		State: summary.State, Reason: summary.Reason, PhaseID: summary.CurrentPhaseID,
		Resting: workflowRunResting(summary.State),
	}
	if !state.Resting {
		return state, nil
	}
	gateDecision, gateLabel := "", ""
	if engine.Reason(summary.Reason) == engine.ReasonGate {
		gateDecision, gateLabel = a.workflowGateDecision(summary.ID)
	}
	state.Repair = wake.RepairSentence(summary.ID, summary.State, summary.Reason, gateDecision, gateLabel)
	return state, nil
}

// workflowWatchTransitions attaches each resting transition's persisted park
// cause. The ring cannot carry it — it is written on the engine's command
// goroutine, which does no reads — so it is resolved here from the exact
// (phase, attempt) coordinate the transition recorded, never from "the run's
// latest attempt", which by now may be a different one.
func (a *App) workflowWatchTransitions(recorded []workflowTransition) ([]WorkflowAgentTransition, error) {
	causes := make(map[string]map[string]string, 1)
	projected := make([]WorkflowAgentTransition, 0, len(recorded))
	for _, entry := range recorded {
		transition := WorkflowAgentTransition{
			Seq: entry.Seq, At: entry.At, ItemID: entry.ItemID,
			PhaseID: entry.PhaseID, Attempt: entry.Attempt,
			From: entry.From, To: entry.To, Reason: entry.Reason,
			Resting: workflowRunResting(entry.To),
		}
		if transition.Resting && entry.PhaseID != "" {
			byAttempt, loaded := causes[entry.ItemID]
			if !loaded {
				var err error
				byAttempt, err = a.workflowParkCauses(entry.ItemID)
				if err != nil {
					return nil, err
				}
				causes[entry.ItemID] = byAttempt
			}
			transition.Cause = byAttempt[workflowAttemptKey(entry.PhaseID, entry.Attempt)]
		}
		projected = append(projected, transition)
	}
	return projected, nil
}

// workflowParkCauses indexes one run's persisted park causes by attempt.
func (a *App) workflowParkCauses(itemID string) (map[string]string, error) {
	rows, err := a.store.ListWorkItemPhaseProvenance(itemID)
	if err != nil {
		return nil, err
	}
	causes := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.ParkCause != "" {
			causes[workflowAttemptKey(row.PhaseID, row.Attempt)] = row.ParkCause
		}
	}
	return causes, nil
}

func workflowAttemptKey(phaseID string, attempt int) string {
	return fmt.Sprintf("%s/%d", phaseID, attempt)
}
