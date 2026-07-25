package main

import (
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
// queue; per-run needs-human / failed notifications are the whole surface.
func (a *App) afterWorkflowEngineEvent(name string, payload any) {
	if name != "workflow:item-state" {
		return
	}
	event, ok := payload.(engine.StateEvent)
	if !ok || event.From == event.To {
		return
	}
	a.afterWorkflowStateEvent(event)
}

func (a *App) afterWorkflowStateEvent(event engine.StateEvent) {
	if event.To == engine.StateDone {
		a.queueAutoDisposition(event.ItemID)
		return
	}
	if event.To != engine.StateNeedsHuman && event.To != engine.StateFailed {
		return
	}
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
	// Model upgrades are useful only when a transport-backed app can consume
	// the refresh event. Headless unit constructions retain the deterministic
	// template without spawning an external CLI.
	if upgrade && a.eventBus.Load() != nil && a.osNotifications != nil {
		a.queueWorkflowDigestUpgrade(item, digest, append([]byte(nil), item.Digest...))
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
