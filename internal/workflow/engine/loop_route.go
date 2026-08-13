package engine

import (
	"fmt"

	"agent-overflow/internal/workflow/def"
)

// What a loop route may change about the round it creates.
//
// A loop edge used to say one thing: which phase to re-enter. Two knobs make the
// re-entry itself authorable, and both exist because a live campaign's
// review/fix loops ping-ponged without them.
//
//   - `session: continue` runs the new attempt as the next turn of the target
//     phase's OWN previous session, so a fix round remembers what it just tried
//     instead of re-deriving it from the rejection note. `fresh` stays the
//     default, and deliberately so: a review phase re-entered warm is an
//     adjudicator that remembers arguing for its last verdict, which is the
//     anchoring the loop exists to break.
//   - `prompt:` renders a different body for the one attempt the route creates,
//     so "round three asks a narrower question" is authorable on the edge rather
//     than branched on inside the phase.
//
// Both are ROUTE-scoped. Nothing here is sticky: an attempt that loops again
// through a route declaring neither runs cold on the phase's own body, because
// the only thing that sets either is the traversal of a route that declares it.

// loopContinuationNote rides the feedback of an attempt whose route asked to
// continue the phase's session and could not. The run is not stopped by it — a
// degraded continuation is still the round the loop wanted — so the fact has to
// be stated where the turn and the human can both see it.
const loopContinuationNote = "this round was authored to continue the previous attempt's session, but that session is no longer available, so it starts cold: treat the feedback above as the whole record of what the last round tried"

// applyLoopRoute stamps a loop decision's per-round knobs onto the item before
// its next phase entry. It runs AFTER the teardown that persisted the deciding
// attempt (so `session: continue` can see that attempt's row) and after the
// feedback is composed (so a degraded continuation can say so in it).
//
// A decision that is not a loop leaves everything alone: the knobs are refused
// on other route kinds by validation, and enforced here as well, because a
// frozen snapshot is decoded and never re-validated.
func (e *Engine) applyLoopRoute(item *runtimeItem, decision def.RouteDecision, sourcePhaseID string) {
	if decision.Kind != def.DecisionLoop {
		return
	}
	route, ok := loopRoute(item.workflow, sourcePhaseID, decision.RouteIndex)
	if !ok || route.Loop == "" {
		// Either the gate that produced this decision is not in the snapshot — a
		// corrupt record, not an authoring mistake — or the route index names a
		// route that is not a loop at all, which is what a HUMAN gate's reject
		// carries: `resolveHumanGate` synthesizes a loop decision whose index
		// points at the `human:` route it came from. Neither knob is authorable
		// there, and reading one off that route would apply a declaration
		// validation refuses. Both cases re-enter cold and unoverridden, which is
		// what every loop did before these knobs existed.
		return
	}
	if route.Prompt != "" {
		item.nextPromptRoute = &PromptRoute{PhaseID: sourcePhaseID, RouteIndex: decision.RouteIndex}
	}
	// The DECISION decides, not the route: `decisionForRoute` sets Session only
	// for the kinds a loop route can produce, so a mode declared on a route kind
	// that never continues anything cannot be honoured here even from a snapshot
	// frozen before validation refused it — the same enforcement `notify:` gets.
	if decision.Session != def.SessionContinue {
		return
	}
	if target, found := findPhase(item.workflow, decision.Target); !found || !consumesPriorSession(target) {
		// A `shape: call` target runs no turn of its own, so there is no session
		// for it to continue and nothing in its entry would consume the id. The
		// declaration is refused statically, but a frozen snapshot is decoded and
		// never re-validated, so it is refused HERE too — before the field is set,
		// because an id nothing consumes is an id the phase after the call would.
		e.logEvent(LogEvent{
			Event: LogEventLoopSession, ItemID: item.item.ID, ProjectID: item.item.ProjectID,
			PhaseID: decision.Target,
			Message: fmt.Sprintf(
				"loop route %s:%d asked to continue phase %q's session, which starts no session of its own; the round runs cold",
				sourcePhaseID, decision.RouteIndex, decision.Target),
		})
		return
	}
	threadID, why := e.loopContinuationThread(item, decision.Target)
	if threadID == "" {
		item.feedback = appendFeedbackNote(item.feedback, loopContinuationNote)
		e.logEvent(LogEvent{
			Event: LogEventLoopSession, ItemID: item.item.ID, ProjectID: item.item.ProjectID,
			PhaseID: decision.Target,
			Message: fmt.Sprintf(
				"loop route %s:%d asked to continue phase %q's session and starts a fresh one instead: %s",
				sourcePhaseID, decision.RouteIndex, decision.Target, why),
		})
		return
	}
	// The same field an Answer continuation and a resume-in-place set: one
	// same-session mechanism, so a warm loop round is the same thing to the
	// runner as every other continuation, and the shared thread id on the two
	// attempt rows is the durable evidence that it happened.
	item.priorThreadID = threadID
}

// loopContinuationThread resolves the session a `session: continue` re-entry
// continues: the newest attempt of the TARGET phase that ran on a thread which
// still exists. The second result says why there is none, for the log line.
//
// It cannot fail, and that is deliberate. Every reason a session is unavailable
// — a deleted thread, a crash, a phase that has never run, a store read that
// would not answer — leaves exactly one sane move: run the round cold, the way
// every loop ran before this knob existed. Returning an error instead would put
// the failure in a caller that has already torn down the deciding attempt and
// has nowhere to put it but a park, which turns an unavailable optimisation into
// an outage. The degradation is recorded on the attempt and in the log rather
// than swallowed.
func (e *Engine) loopContinuationThread(item *runtimeItem, targetPhaseID string) (string, string) {
	phases, err := e.store.ListWorkItemPhases(item.item.ID)
	if err != nil {
		return "", fmt.Sprintf("this run's attempts could not be read (%v)", err)
	}
	best := ""
	bestAttempt := 0
	for _, phase := range phases {
		if phase.PhaseID != targetPhaseID || phase.ThreadID == "" || phase.Attempt < bestAttempt {
			continue
		}
		best = phase.ThreadID
		bestAttempt = phase.Attempt
	}
	if best == "" {
		return "", "no previous attempt of that phase holds a session"
	}
	exists, err := e.store.ThreadCanResume(best)
	if err != nil {
		return "", fmt.Sprintf("thread %s could not be resolved (%v)", best, err)
	}
	if !exists {
		return "", fmt.Sprintf("thread %s no longer exists", best)
	}
	return best, ""
}

// loopRoute resolves one gate route out of the frozen snapshot by the coordinate
// a decision (or a persisted PromptRoute) carries.
func loopRoute(workflow def.Workflow, phaseID string, routeIndex int) (def.Route, bool) {
	phase, ok := findPhase(workflow, phaseID)
	if !ok {
		return def.Route{}, false
	}
	if routeIndex < 0 || routeIndex >= len(phase.Gate.Routes) {
		return def.Route{}, false
	}
	return phase.Gate.Routes[routeIndex], true
}

// consumePromptRoute decides whether an armed prompt-route coordinate belongs to
// the entry being made, and it is the one place that question is asked.
//
// An arming is made by a loop decision for exactly ONE entry: the entry into
// that route's own loop TARGET. Checking the target rather than trusting the
// field is what makes the override phase-scoped by construction — a coordinate
// that survived a park, a crash, or a gate the operator resolved into some other
// phase is inert, instead of a narrower question asked of a phase that was never
// asked it. A route whose override an edit removed is inert for the same reason:
// `promptBody` would fall back to the phase's body anyway, and refusing here
// keeps the attempt's persisted input from claiming a coordinate it did not run.
func consumePromptRoute(workflow def.Workflow, armed *PromptRoute, phaseID string) *PromptRoute {
	if armed == nil {
		return nil
	}
	route, ok := loopRoute(workflow, armed.PhaseID, armed.RouteIndex)
	if !ok || route.Prompt == "" || route.Loop != phaseID {
		return nil
	}
	return armed
}

// promptBody resolves what an attempt renders: the loop route's inlined override
// when the entry carries one, and the phase's own authored body otherwise. It is
// the single decision point, which is why RunRequest carries the resolved body
// on its Phase rather than the two sources for a consumer to choose between.
//
// A reference that no longer resolves — a snapshot re-frozen by `--refresh-def`
// whose edit removed the route — falls back to the phase's body rather than
// failing the attempt: the round still has a prompt, and the narrowing it lost
// is a definition the operator deliberately replaced.
func promptBody(workflow def.Workflow, phase def.Phase, ref *PromptRoute) string {
	if ref == nil {
		return phase.Prompt
	}
	route, ok := loopRoute(workflow, ref.PhaseID, ref.RouteIndex)
	if !ok || route.Prompt == "" {
		return phase.Prompt
	}
	return route.Prompt
}

// appendFeedbackNote adds one engine sentence to an attempt's feedback,
// allocating the feedback when the route composed none. It is the same append
// `noteDefinitionRefresh` does; both exist because the next turn's only channel
// for "your instructions are not what you think" is the feedback block.
func appendFeedbackNote(feedback *Feedback, note string) *Feedback {
	if feedback == nil {
		feedback = &Feedback{}
	}
	if feedback.Note != "" {
		feedback.Note += "\n"
	}
	feedback.Note += note
	return feedback
}
