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
	a.workflowProjectQueuePaused = make(map[string]bool, len(event.Projects))
	for _, project := range event.Projects {
		a.workflowProjectQueuePaused[project.ProjectID] = project.Paused
	}
	a.workflowNotificationsMu.Unlock()
	a.flushWorkflowDrainSummariesIfIdle()
}

func (a *App) flushWorkflowDrainSummariesIfIdle() {
	if a.workflowAutoDispositionPending() {
		return
	}
	a.workflowNotificationsMu.Lock()
	queueActive := a.workflowQueueActive
	projectPaused := make(map[string]bool, len(a.workflowProjectQueuePaused))
	for projectID, paused := range a.workflowProjectQueuePaused {
		projectPaused[projectID] = paused
	}
	projectIDs := make([]string, 0, len(a.workflowNotificationTallies))
	for projectID := range a.workflowNotificationTallies {
		projectIDs = append(projectIDs, projectID)
	}
	a.workflowNotificationsMu.Unlock()
	for _, projectID := range projectIDs {
		states := []string{string(engine.StateRunning)}
		if queueActive && !projectPaused[projectID] {
			states = append(states, string(engine.StateQueued))
		}
		busy, err := a.store.CountProjectWorkItemsInStates(projectID, states...)
		if err != nil {
			log.Printf("workflow notification: count project %s live queue items: %v", projectID, err)
			continue
		}
		if busy == 0 {
			a.flushWorkflowDrainSummary(projectID)
		}
	}
}

func (a *App) flushWorkflowDrainSummary(projectID string) {
	a.workflowNotificationsMu.Lock()
	tally, exists := a.workflowNotificationTallies[projectID]
	delete(a.workflowNotificationTallies, projectID)
	a.workflowNotificationsMu.Unlock()
	if !exists {
		return
	}
	body := workflowDrainSummaryBody(tally)
	if body == "" {
		return
	}
	project, err := a.store.GetProject(projectID)
	if err != nil {
		log.Printf("workflow notification summary %s: load project: %v", projectID, err)
		return
	}
	title := textgen.CapRunesWithEllipsis(project.Name+" workflows", 120)
	go func() {
		if err := a.notifyOS(title, body, notify.Target{Kind: "workflow-triage-agent", ProjectID: projectID}); err != nil {
			log.Printf("workflow notification summary %s: %v", projectID, err)
		}
	}()
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
