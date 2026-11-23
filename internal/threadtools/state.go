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
//  2. running, from a live round or a send that has not echoed yet.
//  3. running, from the newest turn row still open for a running turn.
//  4. the durable fallbacks, in the reference's order: a failed worktree
//     setup names a concrete repair, then a failed turn, then an
//     interrupted one, then an actionable plan.
//
// Whether a turn runs or has ended, and how, is the newest turn row's
// (store/turn_newest.go): one write moves it from open to settled, so a
// reader of it never sees a turn between the two. The live round holds
// until right before provider:turn_completed, after that write, so the two
// overlap and the absence of a round never makes a thread interrupted. An
// interruption is the row's recorded stop reason.
//
// The frontend's live 'error' and 'interrupted' registries have no Go
// equivalent; the two durable columns below carry the same facts, which is
// what the reference falls back to once the live status is idle. The
// frontend has no HasOpenTurn: its live registry holds a turn until
// provider:turn_completed.
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
	case thread.HasOpenTurn:
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

// Rests reports whether a thread_status row's thread rests: its state is
// a resting one (Resting) and nothing it started still runs (WaitingOn). A
// thread_status wait on a thread id ends when it rests. A computer that
// predates waiting_on reports the state alone.
func Rests(state ThreadState) bool {
	return Resting(state.State) && state.WaitingOn == ""
}

// Resting reports whether a thread's state is doing nothing and is not
// waiting on a person; Rests adds what the thread still waits on.
func Resting(state string) bool {
	switch state {
	case StateRunning, StatePendingApproval, StateAwaitingInput, StateUnavailable, StateUnresolved:
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
