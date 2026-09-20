package threadtools

// State derives a thread's effective state from its durable columns and
// its live projection. It is the Go half of the frontend's
// resolveEffectiveThreadStatus (frontend/src/lib/utils/threadStatusPill.ts)
// together with getThreadStatus (stores/threadStatuses.svelte.ts), which
// stay the reference: there is no Go enum in the app today, and a thread
// reported by a tool must read as the same state the sidebar shows.
//
// Order, live first:
//
//  1. pending-approval, then awaiting-input. A provider blocked on a
//     person reports that whatever else is true, which is also what ends
//     a wait early.
//  2. running, from an open turn or a send that has not echoed yet.
//  3. the durable fallbacks, in the reference's order: a failed worktree
//     setup names a concrete repair, then a failed turn, then an
//     interrupted one, then an actionable plan.
//
// The frontend's live 'error' and 'interrupted' registries have no Go
// equivalent; the two durable columns below carry the same facts, which is
// what the reference falls back to once the live status is idle.
//
// internal/threadtools/testdata/thread_states.json is the shared table for
// this function (state_test.go) and for the reference's own test
// (frontend/src/lib/utils/threadStatusPill.test.ts), which reads the same
// file. A case added to one side runs on both.
func State(thread Thread, live LiveState) string {
	switch {
	case live.PendingApprovals > 0:
		return StatePendingApproval
	case live.PendingUserInputs > 0:
		return StateAwaitingInput
	case live.ActiveTurn || live.PendingSends:
		return StateRunning
	case thread.WorktreeSetupState == "failed":
		return StateSetupFailed
	case thread.HasFailedTurn:
		return StateError
	case thread.HasIncompleteTurn:
		return StateInterrupted
	case thread.HasActionableProposedPlan:
		return StatePlanReady
	default:
		return StateIdle
	}
}

// Resting reports whether a thread is doing nothing and is not waiting on
// a person. A thread_status wait on a thread id ends when it rests.
func Resting(state string) bool {
	switch state {
	case StateRunning, StatePendingApproval, StateAwaitingInput:
		return false
	default:
		return true
	}
}

// AllStates is the enum a thread_search state filter accepts.
var AllStates = []string{
	StateIdle,
	StateRunning,
	StateAwaitingInput,
	StatePendingApproval,
	StatePlanReady,
	StateError,
	StateSetupFailed,
	StateInterrupted,
}
