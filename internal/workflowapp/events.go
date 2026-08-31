package workflowapp

import (
	"bytes"
	"encoding/json"
	"strings"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/workflow/engine"
)

// afterEngineEvent runs the application reactions to engine lifecycle
// events. Rev 2 removed the drain-to-empty coalesced summary along with the
// queue; a run's resting transition is the whole surface — it wakes the thread
// the run was started from (D17) and, when it needs a human, notifies the OS.
//
// The engine emits from its command-loop goroutine, so nothing here may block
// or re-enter the engine. Everything past the classification below runs on the
// per-App serial queues.
func (s *Service) prepareEngineEvent(name eventchan.Channel, payload any) {
	if name != eventchan.WorkflowItemState || s.deps.Digest == nil {
		return
	}
	event, ok := payload.(engine.StateEvent)
	if !ok || event.From == event.To || event.To != engine.StateNeedsHuman && event.To != engine.StateFailed {
		return
	}
	context, err := s.deps.Store.GetWorkItemAttentionContext(event.ItemID)
	if err != nil {
		s.deps.Logf("workflow digest %s: load attention context: %v", event.ItemID, err)
		return
	}
	digest := s.deps.Digest(context.Item, context.PhaseID, context.OutputEnvelope, context.Check)
	encoded, err := json.Marshal(digest)
	if err != nil {
		s.deps.Logf("workflow digest %s: encode template: %v", event.ItemID, err)
		return
	}
	if err := s.deps.Store.UpdateWorkItemDigest(event.ItemID, encoded); err != nil {
		s.deps.Logf("workflow digest %s: persist template: %v", event.ItemID, err)
	}
}

func (s *Service) afterEngineEvent(name eventchan.Channel, payload any) {
	switch name {
	case eventchan.WorkflowItemState:
		event, ok := payload.(engine.StateEvent)
		if !ok {
			return
		}
		// Recorded before the classification below, and without its
		// from == to filter: a `run watch` reports what MOVED, and a takeover
		// re-parking an already-parked run under a new reason is a move a
		// monitor must not be blind to.
		s.recordWorkflowTransition(event)
		if event.From == event.To {
			return
		}
		s.afterWorkflowStateEvent(event)
	case "workflow:gate-notify":
		// A gate took a `notify:` route and the run continued (K1). It is the
		// one surface that reports without a park, so it is decided from its own
		// event rather than from a transition that never happens.
		if event, ok := payload.(engine.NotifyEvent); ok {
			s.afterWorkflowGateNotify(event)
		}
	}
}

// AfterEngineEvent exposes the post-emission half for integration tests and
// host adapters that already emitted through another controlled path.
func (s *Service) AfterEngineEvent(name eventchan.Channel, payload any) {
	s.afterEngineEvent(name, payload)
}

func (s *Service) AfterStateEvent(event engine.StateEvent) {
	s.afterWorkflowStateEvent(event)
}

// recordWorkflowTransition is the App-side event adapter. It timestamps the
// engine's typed transition before handing it to the I/O-free watch ring.
func (s *Service) recordWorkflowTransition(event engine.StateEvent) {
	s.watch.Record(event, s.deps.Now().UnixMilli())
}

func (s *Service) afterWorkflowStateEvent(event engine.StateEvent) {
	// Every transition OUT of a park disarms a self-resume schedule, and this
	// is the one place that is true of: a manual resume, a cancel, a discard, a
	// rerun, and an auto-resume's own resume all pass through here. Parking is
	// the one transition that must not clear — the runner writes the schedule
	// immediately before the park that carries it — which is exactly what
	// `To != needs-human` says.
	if event.To != engine.StateNeedsHuman {
		s.ClearAutoResume(event.ItemID)
	}
	if event.To == engine.StateRunning {
		// Somebody acted on this run — a resolve, an answer, a resume, a retry,
		// a rerun all land here — so whatever its bound thread was last told is
		// spent, and the next wake delivers however familiar it looks. This is
		// the record half of the coalescing rule; the comparison half lives in
		// wakeAlreadyDelivered.
		itemID, from := event.ItemID, event.From
		s.wake.Go(func() { s.clearTreeWorkflowAttentionForTransition(itemID, from) })
		return
	}
	if !restingWorkflowState(event.To) {
		return
	}
	if event.To == engine.StateDone {
		s.queueAutoDisposition(event.ItemID)
	}
	itemID := event.ItemID
	s.wake.Go(func() { s.surfaceRestingWorkflowItem(itemID) })
	// The same transition is what §11's internal-event triggers chain off. It
	// goes to the scheduler on its own queue rather than through the wake, so the
	// two consumers cannot delay each other.
	s.NotifyScheduler(event)
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
func (s *Service) surfaceRestingWorkflowItem(itemID string) UsageAttentionSurface {
	item, err := s.deps.Store.GetWorkItem(itemID)
	if err != nil {
		s.deps.Logf("workflow surface %s: load run: %v", itemID, err)
		return UsageAttentionSurface{}
	}
	if item.ParentItemID != "" {
		// A called run does not surface as itself (§5). A park still needs a
		// human, so it is announced at the root, which is the unit of attention
		// the overlay lists and a notification can be acted on from.
		if engine.State(item.State) == engine.StateNeedsHuman {
			return s.surfaceDescendantPark(item)
		}
		return UsageAttentionSurface{}
	}
	// Every resting transition wakes the bound thread; only the ones that need
	// a human interrupt them through the OS. A usage-limited storm shares one
	// attention generation across all affected runs watched by this thread.
	usage := s.UsageAttentionForRest(item, item)
	return s.surfaceRootRestingWorkflowItem(item, usage)
}

func (s *Service) surfaceRootRestingWorkflowItem(item store.WorkItem, usage UsageAttentionDecision) UsageAttentionSurface {
	if usage.Suppress {
		return usage.Surface
	}
	delivered := s.afterWorkflowResting(item, usage.Claim)
	if engine.State(item.State) == engine.StateNeedsHuman || engine.State(item.State) == engine.StateFailed {
		s.notifyWorkflowItemNeedsHuman(item)
	}
	if usage.Claim != nil && !delivered {
		return UsageAttentionSurface{}
	}
	return usage.Surface
}

func (s *Service) notifyWorkflowItemNeedsHuman(item store.WorkItem) {
	context, err := s.deps.Store.GetWorkItemAttentionContext(item.ID)
	if err != nil {
		s.deps.Logf("workflow notification %s: load attention context: %v", item.ID, err)
		return
	}
	var digest Digest
	upgrade := true
	// An absent digest is the ordinary first-rest case, not a failure: the
	// deterministic template is what the column would have been seeded with.
	// Only content that is present and unreadable is worth a log line.
	if stored := bytes.TrimSpace(context.Item.Digest); len(stored) == 0 {
		if s.deps.Digest != nil {
			digest = s.deps.Digest(context.Item, context.PhaseID, context.OutputEnvelope, context.Check)
		}
	} else if err := json.Unmarshal(stored, &digest); err != nil {
		s.deps.Logf("workflow notification %s: decode template digest: %v", item.ID, err)
		upgrade = false
		if s.deps.Digest != nil {
			digest = s.deps.Digest(context.Item, context.PhaseID, context.OutputEnvelope, context.Check)
		}
	}
	go s.sendWorkflowItemNotification(item, digest)
	// Model upgrades are useful only when a transport-backed app can consume
	// the refresh event. Headless unit constructions retain the deterministic
	// template without spawning an external CLI.
	if upgrade && s.deps.Attention.CanUpgradeDigest != nil && s.deps.Attention.CanUpgradeDigest() {
		s.queueDigestUpgrade(context.Item, digest, append([]byte(nil), context.Item.Digest...))
	}
}

func (s *Service) sendWorkflowItemNotification(item store.WorkItem, digest Digest) {
	title := textgen.CapRunesWithEllipsis(strings.TrimSpace(item.Goal), 120)
	if title == "" {
		title = "Workflow needs attention"
	}
	if s.deps.Attention.Notify == nil {
		return
	}
	if err := s.deps.Attention.Notify(item.ID, title, digest.WhatItNeeds); err != nil {
		s.deps.Logf("workflow notification item %s: %v", item.ID, err)
	}
}
