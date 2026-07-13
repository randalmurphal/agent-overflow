package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/workflow/engine"
)

type workflowNotificationTally struct {
	Finished int
	NeedsYou int
	Failed   int
}

func (a *App) afterWorkflowEngineEvent(name string, payload any) {
	switch name {
	case "workflow:item-state":
		event, ok := payload.(engine.StateEvent)
		if !ok || event.From == event.To {
			return
		}
		a.afterWorkflowStateEvent(event)
	case "workflow:queue-state":
		event, ok := payload.(engine.QueueEvent)
		if ok {
			a.afterWorkflowQueueEvent(event)
		}
	}
}

func (a *App) afterWorkflowStateEvent(event engine.StateEvent) {
	if event.To == engine.StateNeedsHuman || event.To == engine.StateFailed {
		context, err := a.store.GetWorkItemAttentionContext(event.ItemID)
		if err != nil {
			log.Printf("workflow notification %s: load attention context: %v", event.ItemID, err)
			return
		}
		item := context.Item
		var digest WorkflowDigest
		upgrade := true
		if err := json.Unmarshal(item.Digest, &digest); err != nil {
			log.Printf("workflow notification %s: decode template digest: %v", event.ItemID, err)
			upgrade = false
			digest = workflowTemplateDigest(
				item, context.PhaseID, context.OutputEnvelope, context.Check,
			)
		}
		go a.sendWorkflowItemNotification(item, digest)
		// Model upgrades are useful only when a transport-backed app can
		// consume the refresh event. Headless unit constructions retain the
		// deterministic template without spawning an external CLI.
		if upgrade && a.eventBus.Load() != nil && a.osNotifications != nil {
			a.queueWorkflowDigestUpgrade(item, digest, append([]byte(nil), item.Digest...))
		}
	}
	if event.To == engine.StateDone {
		a.queueAutoDisposition(event.ItemID)
		return
	}

	a.recordWorkflowNotificationOutcome(event.ProjectID, event.To)
	if event.To == engine.StateCancelled || event.To == engine.StateNeedsHuman || event.To == engine.StateFailed {
		a.flushWorkflowDrainSummariesIfIdle()
	}
}

func (a *App) recordWorkflowNotificationOutcome(projectID string, state engine.State) {
	a.workflowNotificationsMu.Lock()
	defer a.workflowNotificationsMu.Unlock()
	if a.workflowNotificationTallies == nil {
		a.workflowNotificationTallies = make(map[string]workflowNotificationTally)
	}
	tally := a.workflowNotificationTallies[projectID]
	switch state {
	case engine.StateDone:
		tally.Finished++
	case engine.StateNeedsHuman:
		tally.NeedsYou++
	case engine.StateFailed:
		tally.Failed++
	}
	a.workflowNotificationTallies[projectID] = tally
}

func (a *App) afterWorkflowQueueEvent(event engine.QueueEvent) {
	a.workflowNotificationsMu.Lock()
	wasActive := a.workflowQueueActive
	a.workflowQueueActive = event.Active
	if event.Active && !wasActive {
		a.workflowNotificationTallies = make(map[string]workflowNotificationTally)
	}
	a.workflowNotificationsMu.Unlock()
	if !event.Active && wasActive {
		pending, err := a.store.CountWorkItemsInStates(string(engine.StateQueued))
		if err != nil {
			log.Printf("workflow notification: count pending items on pause: %v", err)
			return
		}
		running, err := a.store.CountWorkItemsInStates(string(engine.StateRunning))
		if err != nil {
			log.Printf("workflow notification: count running items on pause: %v", err)
			return
		}
		if pending > 0 && running == 0 && !a.workflowAutoDispositionPending() {
			a.flushWorkflowDrainSummaries()
		}
	}
}

func (a *App) flushWorkflowDrainSummariesIfIdle() {
	if a.workflowAutoDispositionPending() {
		return
	}
	a.workflowNotificationsMu.Lock()
	queueActive := a.workflowQueueActive
	a.workflowNotificationsMu.Unlock()
	states := []string{string(engine.StateRunning)}
	if queueActive {
		states = append(states, string(engine.StateQueued))
	}
	busy, err := a.store.CountWorkItemsInStates(states...)
	if err != nil {
		log.Printf("workflow notification: count live queue items: %v", err)
		return
	}
	if busy == 0 {
		a.flushWorkflowDrainSummaries()
	}
}

func (a *App) flushWorkflowDrainSummaries() {
	a.workflowNotificationsMu.Lock()
	tallies := a.workflowNotificationTallies
	a.workflowNotificationTallies = make(map[string]workflowNotificationTally)
	a.workflowNotificationsMu.Unlock()
	for projectID, tally := range tallies {
		body := workflowDrainSummaryBody(tally)
		if body == "" {
			continue
		}
		project, err := a.store.GetProject(projectID)
		if err != nil {
			log.Printf("workflow notification summary %s: load project: %v", projectID, err)
			continue
		}
		title := textgen.CapRunesWithEllipsis(project.Name+" workflows", 120)
		go func(projectID, title, body string) {
			if err := a.notifyOS(title, body, notify.Target{Kind: "workflow-triage-agent", ProjectID: projectID}); err != nil {
				log.Printf("workflow notification summary %s: %v", projectID, err)
			}
		}(projectID, title, body)
	}
}

func workflowDrainSummaryBody(tally workflowNotificationTally) string {
	segments := make([]string, 0, 3)
	if tally.Finished > 0 {
		segments = append(segments, fmt.Sprintf("%d finished", tally.Finished))
	}
	if tally.NeedsYou > 0 {
		segments = append(segments, fmt.Sprintf("%d need you", tally.NeedsYou))
	}
	if tally.Failed > 0 {
		segments = append(segments, fmt.Sprintf("%d failed", tally.Failed))
	}
	return strings.Join(segments, " · ")
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
