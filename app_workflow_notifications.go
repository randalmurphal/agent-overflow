package main

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/workflow/engine"
)

// afterWorkflowEngineEvent runs the app-side reactions to engine lifecycle
// events. Rev 2 removed the drain-to-empty coalesced summary along with the
// queue; a run's resting transition is the whole surface — it wakes the thread
// the run was started from (D17) and, when it needs a human, notifies the OS.
//
// The engine emits from its command-loop goroutine, so nothing here may block
// or re-enter the engine. Everything past the classification below runs on the
// per-App serial queues.
func (a *App) afterWorkflowEngineEvent(name string, payload any) {
	switch name {
	case "workflow:item-state":
		event, ok := payload.(engine.StateEvent)
		if !ok {
			return
		}
		// Recorded before the classification below, and without its
		// from == to filter: a `run watch` reports what MOVED, and a takeover
		// re-parking an already-parked run under a new reason is a move a
		// monitor must not be blind to.
		a.recordWorkflowTransition(event)
		if event.From == event.To {
			return
		}
		a.afterWorkflowStateEvent(event)
	case "workflow:gate-notify":
		// A gate took a `notify:` route and the run continued (K1). It is the
		// one surface that reports without a park, so it is decided from its own
		// event rather than from a transition that never happens.
		if event, ok := payload.(engine.NotifyEvent); ok {
			a.afterWorkflowGateNotify(event)
		}
	}
}

func (a *App) afterWorkflowStateEvent(event engine.StateEvent) {
	// Every transition OUT of a park disarms a self-resume schedule, and this
	// is the one place that is true of: a manual resume, a cancel, a discard, a
	// rerun, and an auto-resume's own resume all pass through here. Parking is
	// the one transition that must not clear — the runner writes the schedule
	// immediately before the park that carries it — which is exactly what
	// `To != needs-human` says.
	if event.To != engine.StateNeedsHuman {
		a.clearWorkflowAutoResume(event.ItemID)
	}
	if event.To == engine.StateRunning {
		// Somebody acted on this run — a resolve, an answer, a resume, a retry,
		// a rerun all land here — so whatever its bound thread was last told is
		// spent, and the next wake delivers however familiar it looks. This is
		// the record half of the coalescing rule; the comparison half lives in
		// wakeAlreadyDelivered.
		itemID, from := event.ItemID, event.From
		a.workflowWake.Go(func() { a.clearTreeWorkflowAttentionForTransition(itemID, from) })
		return
	}
	if !restingWorkflowState(event.To) {
		return
	}
	if event.To == engine.StateDone {
		a.queueAutoDisposition(event.ItemID)
	}
	itemID := event.ItemID
	a.workflowWake.Go(func() { a.surfaceRestingWorkflowItem(itemID) })
	// The same transition is what §11's internal-event triggers chain off. It
	// goes to the scheduler on its own queue rather than through the wake, so the
	// two consumers cannot delay each other.
	a.notifyWorkflowScheduler(event)
}

// restingWorkflowState reports the transitions a run comes to rest on. `running`
// is the only state that is not one: it is the state a run passes through.
func restingWorkflowState(state engine.State) bool {
	switch state {
	case engine.StateNeedsHuman, engine.StateDone, engine.StateFailed, engine.StateCancelled:
		return true
	default:
		return false
	}
}

// surfaceRestingWorkflowItem is the single decision point for "a run rested —
// who is told, and how". Keeping the wake and the notification on one path is
// what makes them consistent: a run cannot notify without waking its bound
// thread, and a called run cannot do either as itself. Its return value is used
// only by restart recovery to confirm that a replacement usage alert was
// accepted by the delivery path or suppressed by a concurrent claim.
func (a *App) surfaceRestingWorkflowItem(itemID string) workflowUsageAttentionSurface {
	item, err := a.store.GetWorkItem(itemID)
	if err != nil {
		log.Printf("workflow surface %s: load run: %v", itemID, err)
		return workflowUsageAttentionSurface{}
	}
	if item.ParentItemID != "" {
		// A called run does not surface as itself (§5). A park still needs a
		// human, so it is announced at the root, which is the unit of attention
		// the overlay lists and a notification can be acted on from.
		if engine.State(item.State) == engine.StateNeedsHuman {
			return a.surfaceDescendantPark(item)
		}
		return workflowUsageAttentionSurface{}
	}
	// Every resting transition wakes the bound thread; only the ones that need
	// a human interrupt them through the OS. A usage-limited storm shares one
	// attention generation across all affected runs watched by this thread.
	usage := a.workflowUsageAttentionForRest(item, item)
	return a.surfaceRootRestingWorkflowItem(item, usage)
}

func (a *App) surfaceRootRestingWorkflowItem(item store.WorkItem, usage workflowUsageAttentionDecision) workflowUsageAttentionSurface {
	if usage.Suppress {
		return usage.Surface
	}
	delivered := a.afterWorkflowResting(item, usage.Claim)
	if engine.State(item.State) == engine.StateNeedsHuman || engine.State(item.State) == engine.StateFailed {
		a.notifyWorkflowItemNeedsHuman(item)
	}
	if usage.Claim != nil && !delivered {
		return workflowUsageAttentionSurface{}
	}
	return usage.Surface
}

func (a *App) notifyWorkflowItemNeedsHuman(item store.WorkItem) {
	context, err := a.store.GetWorkItemAttentionContext(item.ID)
	if err != nil {
		log.Printf("workflow notification %s: load attention context: %v", item.ID, err)
		return
	}
	var digest WorkflowDigest
	upgrade := true
	// An absent digest is the ordinary first-rest case, not a failure: the
	// deterministic template is what the column would have been seeded with.
	// Only content that is present and unreadable is worth a log line.
	if stored := bytes.TrimSpace(context.Item.Digest); len(stored) == 0 {
		digest = workflowTemplateDigest(
			context.Item, context.PhaseID, context.OutputEnvelope, context.Check,
		)
	} else if err := json.Unmarshal(stored, &digest); err != nil {
		log.Printf("workflow notification %s: decode template digest: %v", item.ID, err)
		upgrade = false
		digest = workflowTemplateDigest(
			context.Item, context.PhaseID, context.OutputEnvelope, context.Check,
		)
	}
	go a.sendWorkflowItemNotification(item, digest)
	// Model upgrades are useful only when a transport-backed app can consume
	// the refresh event. Headless unit constructions retain the deterministic
	// template without spawning an external CLI.
	if upgrade && a.eventBus.Load() != nil && a.osNotifications != nil {
		a.queueWorkflowDigestUpgrade(context.Item, digest, append([]byte(nil), context.Item.Digest...))
	}
}

func (a *App) sendWorkflowItemNotification(item store.WorkItem, digest WorkflowDigest) {
	title := textgen.CapRunesWithEllipsis(strings.TrimSpace(item.Goal), 120)
	if title == "" {
		title = "Workflow needs attention"
	}
	if err := a.notifyOS(title, digest.WhatItNeeds, notify.Target{Kind: "workflow-item", WorkItemID: item.ID}); err != nil {
		log.Printf("workflow notification item %s: %v", item.ID, err)
	}
}
