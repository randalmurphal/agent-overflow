package wake

import (
	"fmt"
	"strings"

	"agent-overflow/internal/untrustedtext"
)

// The actionable half of a resting wake: what the run is waiting on, and the
// literal call that settles it (D38).

// The resting states and park reasons the closing branches on, mirrored from
// `engine` rather than imported: this package is pure text assembly over a flat
// input and pulling the engine in for a handful of strings would drag the whole
// FSM with it. Each one earns its mirror by changing what the closing says —
// `checkpoint` because the run stopped exactly where it was asked to, and the
// rest because each names a different repair call (or names none, deliberately).
const (
	stateNeedsHuman   = "needs-human"
	stateFailed       = "failed"
	stateDone         = "done"
	stateCancelled    = "cancelled"
	reasonCheckpoint  = "checkpoint"
	reasonUnitFailed  = "unit-failed"
	reasonPaused      = "paused"
	reasonInterrupted = "interrupted"
	reasonGate        = "gate"
	reasonQuestion    = "question"
	reasonStuck       = "stuck"

	reasonRetriesExhausted         = "retries-exhausted"
	reasonProviderRetriesExhausted = "provider-retries-exhausted"
	reasonProviderUsageLimited     = "provider-usage-limited"
	reasonLoopLimitExhausted       = "loop-limit-exhausted"

	// The two decision kinds a `gate` park can rest under, mirrored from
	// def.DecisionHuman / def.DecisionPark for the same reason the reasons
	// above mirror the engine's.
	gateDecisionHuman = "human"
	gateDecisionPark  = "park"
)

// closing states what the run is waiting on. It is the actionable half of the
// message: a terminal run needs nothing, a parked one does not continue until
// somebody resolves it, and a root waiting on a parked descendant is blocked on
// that descendant rather than on itself.
func closing(in Input) string {
	if child := in.Descendant; child != nil {
		if child.Reason == reasonCheckpoint {
			// The lead-in says WHICH run stopped; the fact that the stop was asked
			// for, and the resume that takes the call it skipped, come from the same
			// repair sentence every other surface reads.
			return join(fmt.Sprintf("Called run %s reached the checkpoint and did not start the next one.",
				untrustedtext.Field(child.ItemID)),
				repairSentence(child.ItemID, child.State, child.Reason, "", ""))
		}
		return join(fmt.Sprintf(
			"Run %s cannot continue until called run %s is resolved; act on run %s, not on %s.",
			untrustedtext.Field(in.Run.ItemID), untrustedtext.Field(child.ItemID),
			untrustedtext.Field(child.ItemID), untrustedtext.Field(in.Run.ItemID),
		), repairSentence(child.ItemID, child.State, child.Reason, child.GateDecision, child.GateLabel))
	}
	switch in.Run.State {
	case stateNeedsHuman:
		if in.Run.Reason == reasonCheckpoint {
			// The whole closing, with no "parked and does not continue" preamble:
			// the run stopped exactly where it was asked to, and reporting that as
			// something owing resolution is the fault this branch exists to avoid.
			return repairSentence(in.Run.ItemID, in.Run.State, in.Run.Reason, "", "")
		}
		return join(fmt.Sprintf("Run %s is parked and does not continue until this is resolved.",
			untrustedtext.Field(in.Run.ItemID)),
			repairSentence(in.Run.ItemID, in.Run.State, in.Run.Reason, in.Run.GateDecision, in.Run.GateLabel))
	case stateFailed:
		return join("The run is over.", repairSentence(in.Run.ItemID, in.Run.State, in.Run.Reason, "", ""))
	case stateDone:
		return "The run finished; nothing is waiting on a reply."
	case stateCancelled:
		return "The run was stopped on purpose; nothing is waiting on a reply."
	default:
		return "The run is over; nothing is waiting on a reply."
	}
}

// RepairSentence is repairSentence for the surfaces outside a wake that owe a
// reader the same answer: a resting run's state in run_watch, which reports a
// run coming to rest exactly as a wake does and must not name a different call
// for the same park. Exporting it rather than copying it is what keeps "which
// call settles this" a single definition; the caller resolves the gate kind the
// same way the wake's resolver does.
func RepairSentence(itemID, state, reason, gateDecision, gateLabel string) string {
	return repairSentence(itemID, state, reason, gateDecision, gateLabel)
}

// repairSentence names the exact ao-workflow-tools call that acts on a run
// resting this way, or the fact that no call does. Naming the run without
// naming the call is the gap a cold agent falls into: it knows which run to act
// on, guesses at how, and picks the wrong one. The reasons with no single call
// say so rather than being left out, so silence never reads as "there must be
// one I haven't found".
//
// A `gate` park is two states under one reason, and the call differs:
// gateDecision tells a human: route (approve or reject exists: run_resolve)
// from a park: route (no continuation is declared: resume re-enters the
// phase). An unresolved kind names both rather than guessing, because sending
// a reader to run_resolve for a park: route is exactly the dead end this
// sentence exists to prevent.
func repairSentence(itemID, state, reason, gateDecision, gateLabel string) string {
	if state == stateFailed {
		return controlCall(itemID, "rerun") + " starts its last phase again once the cause is fixed."
	}
	if state != stateNeedsHuman {
		return ""
	}
	switch reason {
	case reasonCheckpoint:
		// The one park that is not a fault, and the one whose sentence IS its
		// closing (both `closing` branches above return it as-is). It lives
		// here rather than only there because run_watch reports a resting run
		// with this function alone: a soft-stop checkpoint is the park a
		// supervising agent produces for ITSELF, and watching it to rest with
		// no call named is exactly the dead end D38 exists to close.
		return "This is the stop that was asked for, not a failure. " +
			resumeCommand(itemID) + " takes the call it skipped, or leave it parked."
	case reasonUnitFailed:
		return fmt.Sprintf(
			"Repair it with %s, or %s for one of the failed units above; a failed join is one of them. %s continues the same attempt instead. None of these re-run a unit that finished; resume with phase_id would, because it starts the phase over.",
			controlCall(itemID, "retry_failed_units"),
			controlCall(itemID, "retry_unit", `unit_id: "<unit-id>"`),
			resumeCommand(itemID))
	case reasonPaused, reasonInterrupted:
		return resumeCommand(itemID) + " returns it to running."
	case reasonGate:
		switch gateDecision {
		case gateDecisionHuman:
			return fmt.Sprintf(
				"Decide it with %s. This is a judgment the workflow routed out of the run, so decide only what you have the standing to decide (a phase session needs the `resolve` grant).",
				resolveCall(itemID, `decision: "approve" or "reject", note: "<text>"`))
		case gateDecisionPark:
			label := ""
			if gateLabel != "" {
				label = fmt.Sprintf(" (%s)", untrustedtext.Field(gateLabel))
			}
			return fmt.Sprintf(
				"This is a park: route%s: it declares no approve or reject, so run_resolve does not apply. Once its cause is addressed, %s re-enters the phase; an approvable park is authored as a human: route.",
				label, resumeCommand(itemID))
		default:
			return fmt.Sprintf(
				"If %s shows the parked attempt's decision as human, decide it with %s; a park: route instead declares no approve or reject, and %s re-enters the phase once its cause is addressed.",
				toolCall("run_status", runArg(itemID)),
				resolveCall(itemID, `decision: "approve" or "reject"`),
				resumeCommand(itemID))
		}
	case reasonQuestion:
		return fmt.Sprintf(
			"Answer it with %s. The answer rides into the phase as feedback, so answer only what you actually know (a phase session needs the `resolve` grant).",
			resolveCall(itemID, `decision: "answer", answer: "<text>"`))
	case reasonStuck:
		return fmt.Sprintf(
			"The phase reported itself stuck rather than failing, so the detail above is its own account of what it needs. Once that is cleared, %s enters the parked phase again as a fresh attempt; if the fix was an edit to the workflow definition or one of its prompt files, %s re-reads them at that entry. A run otherwise renders the definition it froze at start.",
			resumeCommand(itemID), controlCall(itemID, "resume", "refresh_def: true"))
	case reasonProviderRetriesExhausted:
		return fmt.Sprintf(
			"%s continues the parked attempt on the provider session its last turn died in, which is the move once the provider failure is cleared.",
			resumeCommand(itemID))
	case reasonProviderUsageLimited:
		return fmt.Sprintf(
			"%s tries the parked attempt immediately; use it after usage resets or after switching accounts. Recorded limit state never blocks the attempt, so an early resume simply receives another provider refusal if usage is still unavailable.",
			resumeCommand(itemID))
	case reasonLoopLimitExhausted:
		return fmt.Sprintf(
			"The workflow's loop limit is spent. %s naming an EARLIER phase re-enters the loop's target from outside the cycle and refills its bound.",
			controlCall(itemID, "resume", `phase_id: "<phase-id>"`))
	case reasonRetriesExhausted:
		// Compatibility copy for rows written before the two causes received
		// distinct persisted reasons. Their source cannot be reconstructed.
		return fmt.Sprintf(
			"This run predates the separate provider-retry and loop-limit reasons. %s preserves its original behavior and continues the parked attempt. If a workflow loop bound was spent, %s naming an EARLIER phase re-enters the loop's target from outside the cycle and refills its bound.",
			resumeCommand(itemID), controlCall(itemID, "resume", `phase_id: "<phase-id>"`))
	default:
		// Every other reason names its own cause and is repaired by fixing that
		// cause, so there is no one call to print. Inventing a generic resume
		// here would be the guess this function exists to prevent.
		return ""
	}
}

// toolCall renders one ao-workflow-tools call as a reader would make it: the
// tool name and its arguments, named. The run id stays quoted by
// untrustedtext; it is still untrusted run data.
func toolCall(tool string, args ...string) string {
	return "`" + tool + "(" + strings.Join(args, ", ") + ")`"
}

func runArg(itemID string) string { return "run_id: " + untrustedtext.Field(itemID) }

// controlCall renders a run_control action on a run, with any extra
// arguments that action takes.
func controlCall(itemID, action string, extra ...string) string {
	return toolCall("run_control", append([]string{runArg(itemID), `action: "` + action + `"`}, extra...)...)
}

func resolveCall(itemID string, extra ...string) string {
	return toolCall("run_resolve", append([]string{runArg(itemID)}, extra...)...)
}

// resumeCommand renders the literal call rather than the word "resume",
// which a reader has to map onto one of four actions that all sound like
// stopping and starting.
func resumeCommand(itemID string) string {
	return controlCall(itemID, "resume")
}

// inspectCommand renders the drill-down for one attempt, which is where every
// bounded list in a wake tells the reader the rest of the answer lives.
func inspectCommand(itemID, phaseID string, attempt int) string {
	args := []string{runArg(itemID), "phase_id: " + untrustedtext.Field(phaseID)}
	if attempt > 0 {
		args = append(args, fmt.Sprintf("attempt: %d", attempt))
	}
	return toolCall("run_inspect", args...)
}

func join(sentence, next string) string {
	if next == "" {
		return sentence
	}
	return sentence + " " + next
}
