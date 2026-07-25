package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/workflow/engine"
	workflowrunner "agent-overflow/internal/workflow/runner"
	"agent-overflow/internal/workflow/wake"
)

// Wake resolution and delivery (spec §5, decision D17).
//
// Every resting transition of a ROOT run — done, failed, cancelled,
// needs-human for any reason — composes one compact message and injects it into
// the run's bound thread through the existing queued-user-message path: the
// provider's next tool boundary when the thread is mid-turn, immediately when
// it is idle. That is precisely what a user sending into a running thread gets,
// which is the point: a wake is not a new delivery mechanism.
//
// A run with no bound thread surfaces through the OS notification and the
// overlay badge instead (§10). The two are not exclusive — a bound run still
// notifies — and a bound thread that has since been deleted degrades to the
// unbound surface with the stale binding cleared, so a wake is never lost.

// maxWakeMessageBytes bounds one composed wake. The composer's own budgets keep
// a normal message far below this; the cap is the backstop that keeps a
// pathological run record from producing a message the queue would refuse.
const maxWakeMessageBytes = 24 * 1024

// afterWorkflowResting runs the wake half of a lifecycle transition. It is
// called from the one app-side item-state listener, so a wake and a
// notification are always decided from the same event.
func (a *App) afterWorkflowResting(item store.WorkItem) {
	if item.OriginThreadID == "" {
		return
	}
	message, err := a.composeWorkflowWake(item, nil)
	if err != nil {
		log.Printf("workflow wake %s: compose: %v", item.ID, err)
		return
	}
	a.deliverWorkflowWake(item, message)
}

// surfaceDescendantPark is the root-side surface for a descendant that parked
// while the root kept waiting (spec §5 amendment, 2026-07-25). Children never
// notify as themselves, so without this a run tree could sit blocked on a
// grandchild's question with nothing on any surface a human or agent watches.
//
// It fires only while the root is still running: once the root has rested for a
// reason of its own, that transition is the surface and this one would be a
// duplicate.
func (a *App) surfaceDescendantPark(child store.WorkItem) {
	root, depth, err := a.workflowRootOf(child)
	if err != nil {
		log.Printf("workflow wake %s: resolve root: %v", child.ID, err)
		return
	}
	if root.ID == child.ID || root.State != string(engine.StateRunning) {
		return
	}
	descendant := &wake.Descendant{
		ItemID: child.ID, WorkflowID: child.WorkflowID,
		State: child.State, Reason: child.Reason, Depth: depth,
	}
	phase, envelope, err := a.workflowRestingPhase(child.ID)
	if err != nil {
		log.Printf("workflow wake %s: read parked phase: %v", child.ID, err)
	} else {
		descendant.PhaseID = phase.PhaseID
		descendant.Detail = workflowEnvelopeDetail(envelope)
	}
	// The root is the unit of attention: it carries the badge, the notification,
	// and the binding, so the descendant's park is announced as the root's.
	digest := WorkflowDigest{
		WhatHappened: fmt.Sprintf("A called run parked %s.", workflowStateText(child.State, child.Reason)),
		WhatItNeeds:  fmt.Sprintf("Resolve called run %s before this run can continue.", child.ID),
	}
	go a.sendWorkflowItemNotification(root, digest)
	if root.OriginThreadID == "" {
		return
	}
	message, err := a.composeWorkflowWake(root, descendant)
	if err != nil {
		log.Printf("workflow wake %s: compose descendant park: %v", root.ID, err)
		return
	}
	a.deliverWorkflowWake(root, message)
}

// workflowRootOf walks call linkage to the tree's root and reports how far below
// it the supplied run sits. Linkage is immutable, so the answer is a stable fact
// about the run rather than live state.
func (a *App) workflowRootOf(item store.WorkItem) (store.WorkItem, int, error) {
	current := item
	for depth := 0; current.ParentItemID != ""; depth++ {
		if depth >= engine.MaxCallDepth {
			return store.WorkItem{}, 0, fmt.Errorf("run %s has more than %d ancestors", item.ID, engine.MaxCallDepth)
		}
		parent, err := a.store.GetWorkItem(current.ParentItemID)
		if err != nil {
			return store.WorkItem{}, 0, fmt.Errorf("load parent %s of %s: %w", current.ParentItemID, current.ID, err)
		}
		current = parent
	}
	return current, item.CallDepth - current.CallDepth, nil
}

// composeWorkflowWake resolves the run record into the composer's flat input.
// Every lookup happens here so the composer stays pure and its format stays
// testable without a store.
func (a *App) composeWorkflowWake(item store.WorkItem, descendant *wake.Descendant) (string, error) {
	input := wake.Input{
		Run: wake.Run{
			ItemID: item.ID, Goal: item.Goal, WorkflowID: item.WorkflowID,
			State: item.State, Reason: item.Reason,
		},
		Descendant: descendant,
	}
	if descendant != nil {
		// The root did not rest: the composer renders it as waiting on the
		// descendant, and the root's own (empty) park reason would be noise.
		input.Run.Reason = ""
	}
	timeline, err := a.store.ListWorkItemPhaseTimeline(item.ID)
	if err != nil {
		return "", fmt.Errorf("list phase timeline: %w", err)
	}
	if descendant == nil {
		if phase, ok := currentWorkflowPhaseTimelineAttempt(timeline); ok {
			input.Run.PhaseID = phase.PhaseID
			input.Run.Detail = workflowEnvelopeDetail(phase.OutputEnvelope)
		}
	}
	outputs, err := workflowNamedOutputs(item.Snapshot, timeline)
	if err != nil {
		return "", fmt.Errorf("read declared outputs: %w", err)
	}
	input.Outputs = wakeOutputs(outputs)
	references, err := a.wakeReferences(item, timeline, descendant)
	if err != nil {
		return "", err
	}
	input.References = references
	message := wake.Compose(input)
	if len(message) > maxWakeMessageBytes {
		return "", fmt.Errorf("composed wake is %d bytes; maximum is %d", len(message), maxWakeMessageBytes)
	}
	return message, nil
}

// wakeOutputs projects the run's declared outputs in a stable order. Values are
// rendered as compact JSON so a structured output stays legible without the
// message carrying an envelope.
func wakeOutputs(outputs map[string]any) []wake.Output {
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	projected := make([]wake.Output, 0, len(names))
	for _, name := range names {
		projected = append(projected, wake.Output{Name: name, Value: renderWakeValue(outputs[name])})
	}
	return projected
}

func renderWakeValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}

// wakeReferences collects the navigable pointers a wake carries: the newest
// narrative of the phase that rested, the run's captured artifacts, and the
// thread of every unit that did not complete. They are pointers, never content
// — an agent that needs the detail opens them.
func (a *App) wakeReferences(
	item store.WorkItem, timeline []store.WorkItemPhaseTimeline, descendant *wake.Descendant,
) ([]wake.Reference, error) {
	var references []wake.Reference
	if descendant != nil {
		references = append(references, wake.Reference{Label: "called run", Value: descendant.ItemID})
		if narrative, ok := a.workflowRestingNarrative(descendant.ItemID); ok {
			references = append(references, wake.Reference{Label: "called run narrative", Value: narrative})
		}
		return references, nil
	}
	if phase, ok := currentWorkflowPhaseTimelineAttempt(timeline); ok {
		if narrative, err := workflowrunner.NarrativePath(
			a.workflowDataRoot(), item.ID, phase.PhaseID, phase.Attempt,
		); err == nil {
			references = append(references, wake.Reference{Label: "narrative", Value: narrative})
		}
		if phase.ThreadID != "" {
			references = append(references, wake.Reference{Label: "phase thread", Value: phase.ThreadID})
		}
	}
	units, err := a.store.ListWorkItemUnits(item.ID)
	if err != nil {
		return nil, fmt.Errorf("list fan-out units: %w", err)
	}
	for _, unit := range units {
		if unit.Kind == store.WorkItemUnitKindJoin || unit.Status != store.WorkItemUnitFailed {
			continue
		}
		value := unit.UnitID
		if unit.ThreadID != "" {
			value += " (thread " + unit.ThreadID + ")"
		}
		references = append(references, wake.Reference{Label: "failed unit", Value: value})
	}
	artifacts, err := listWorkflowArtifacts(a.workflowDataRoot(), item.ID)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	for _, artifact := range artifacts {
		references = append(references, wake.Reference{Label: "artifact", Value: artifact.Path})
	}
	return references, nil
}

func (a *App) workflowRestingNarrative(itemID string) (string, bool) {
	phase, _, err := a.workflowRestingPhase(itemID)
	if err != nil || phase.PhaseID == "" {
		return "", false
	}
	narrative, err := workflowrunner.NarrativePath(a.workflowDataRoot(), itemID, phase.PhaseID, phase.Attempt)
	if err != nil {
		return "", false
	}
	return narrative, true
}

// workflowRestingPhase returns the attempt a run came to rest on plus its
// output envelope.
func (a *App) workflowRestingPhase(itemID string) (store.WorkItemPhaseTimeline, json.RawMessage, error) {
	timeline, err := a.store.ListWorkItemPhaseTimeline(itemID)
	if err != nil {
		return store.WorkItemPhaseTimeline{}, nil, err
	}
	phase, ok := currentWorkflowPhaseTimelineAttempt(timeline)
	if !ok {
		return store.WorkItemPhaseTimeline{}, nil, nil
	}
	return phase, phase.OutputEnvelope, nil
}

func currentWorkflowPhaseTimelineAttempt(phases []store.WorkItemPhaseTimeline) (store.WorkItemPhaseTimeline, bool) {
	for index := len(phases) - 1; index >= 0; index-- {
		if phases[index].Status == "running" {
			return phases[index], true
		}
	}
	if len(phases) == 0 {
		return store.WorkItemPhaseTimeline{}, false
	}
	return phases[len(phases)-1], true
}

// workflowEnvelopeDetail extracts the free text a resting envelope carries: the
// question it asked, or the reason it gave for being stuck. Outputs are already
// reported separately, so nothing else from the envelope reaches the wake.
func workflowEnvelopeDetail(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var envelope struct {
		Question *string `json:"question"`
		Reason   *string `json:"reason"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return ""
	}
	if envelope.Question != nil && strings.TrimSpace(*envelope.Question) != "" {
		return strings.TrimSpace(*envelope.Question)
	}
	if envelope.Reason != nil && strings.TrimSpace(*envelope.Reason) != "" {
		return strings.TrimSpace(*envelope.Reason)
	}
	return ""
}

func workflowStateText(state, reason string) string {
	if reason == "" {
		return state
	}
	return state + " (" + reason + ")"
}

// deliverWorkflowWake injects the composed message into the bound thread. The
// two branches are the same two a human gets: a live session takes the message
// through the flush queue (delivered at the provider's next tool boundary, or
// straight through when nothing is in flight), and a session-less thread takes
// an ordinary send, which lazily starts the session and persists the message as
// a durable row rather than parking it in a queue this process would lose.
func (a *App) deliverWorkflowWake(item store.WorkItem, message string) {
	threadID, ok := a.resolveWakeThread(item)
	if !ok {
		return
	}
	if _, live := a.sessionManager().get(threadID); live {
		if _, err := a.registerQueueItem(threadID, message, SendMessageOptions{}, true); err != nil {
			a.reportWakeFailure(item, threadID, err)
		}
		return
	}
	if _, err := a.sendMessageWithOptions(threadID, message, sendMessageOptions{PreserveDraft: true}); err != nil {
		a.reportWakeFailure(item, threadID, err)
	}
}

// resolveWakeThread validates the binding and reports whether it can carry a
// wake. A binding that no longer resolves is cleared here — loudly — so the run
// converges on the unbound surface instead of retrying a dead thread on every
// future transition.
func (a *App) resolveWakeThread(item store.WorkItem) (string, bool) {
	thread, err := a.store.GetThread(item.OriginThreadID)
	if err != nil {
		a.clearStaleWakeBinding(item, fmt.Sprintf("bound thread %s could not be loaded: %v", item.OriginThreadID, err))
		return "", false
	}
	if thread.Archived {
		a.clearStaleWakeBinding(item, fmt.Sprintf("bound thread %s is archived", thread.ID))
		return "", false
	}
	if err := validWorkflowBindingThread(item, thread); err != nil {
		a.clearStaleWakeBinding(item, err.Error())
		return "", false
	}
	return thread.ID, true
}

func (a *App) clearStaleWakeBinding(item store.WorkItem, reason string) {
	log.Printf("workflow wake %s: %s; falling back to the unbound surface", item.ID, reason)
	if err := a.store.UpdateWorkItemOriginThread(item.ID, ""); err != nil {
		log.Printf("workflow wake %s: clear stale binding: %v", item.ID, err)
	}
	a.emit("workflow:error", engine.ErrorEvent{
		ItemID: item.ID,
		Error:  "this run's bound thread is gone; its results now surface in the workflows overlay",
	})
}

func (a *App) reportWakeFailure(item store.WorkItem, threadID string, cause error) {
	log.Printf("workflow wake %s: deliver to thread %s: %v", item.ID, threadID, cause)
	a.emit("workflow:error", engine.ErrorEvent{
		ItemID: item.ID,
		Error:  "this run's result could not be delivered to its bound thread; open the run in the workflows overlay",
	})
}

// validWorkflowBindingThread is the one rule set both binding and waking apply,
// so a thread that could never be woken can never be bound in the first place.
//
// Workflow-owned threads (phase, unit, studio, triage) are excluded because
// they are driven by the run machinery itself: waking one would inject a user
// turn into a session the engine is steering. Terminal and discussion threads
// are excluded because neither takes an ordinary user message.
func validWorkflowBindingThread(item store.WorkItem, thread store.Thread) error {
	if thread.ProjectID != item.ProjectID {
		return fmt.Errorf("thread %s belongs to project %s, not to this run's project %s",
			thread.ID, thread.ProjectID, item.ProjectID)
	}
	if _, ok := threadmode.ManualSelectionModes[thread.Mode]; !ok {
		return fmt.Errorf("thread %s has mode %q; a run binds a conversation thread (chat, plan, or design)",
			thread.ID, thread.Mode)
	}
	return nil
}
