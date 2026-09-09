package triage

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// observeCodexSubagentNotification separates legacy terminal notices from V2
// message delivery. V2 receipts never govern the recipient or sender runtime.
func (r *Router) observeCodexSubagentNotification(evt provider.ProviderEvent) error {
	parsed := decodeCodexSubagentSignalMeta(evt.Meta)
	if parsed.AgentPath == "" {
		return nil
	}
	if parsed.MailboxDelivery && evt.ParentToolUseID != "" {
		return r.persistCodexReceivedMessage(evt, parsed)
	}
	if parsed.MailboxDelivery && evt.ItemID == "" {
		return r.persistCodexMailboxProgress(evt, persistedCodexSpawnLaunch{}, parsed)
	}
	if parsed.MailboxDelivery && parsed.Recipient != "" {
		return r.recordCodexMailboxProgress(evt, parsed)
	}
	threadID := evt.ThreadID
	if parsed.isCodexMailboxProgressDelivery() {
		// `Message Type: MESSAGE` is a mid-run progress note, not the child's
		// answer: it must never mark the child terminal or synthesize a
		// completion row. On an encrypted envelope its payload never leaves the
		// ciphertext, so the activity row shows the beat and no body.
		return r.recordCodexMailboxProgress(evt, parsed)
	}
	status := strings.TrimSpace(parsed.Status)
	if status == "" {
		status = "completed"
	}

	launches, err := r.codexSubagentNotificationLaunches(evt, parsed)
	if err != nil {
		return err
	}

	var firstErr error
	for _, launch := range launches {
		launch.item = r.codexAgentRuntimeOrLaunch(launch.item)
		launch.meta = decodeCodexItemMeta(json.RawMessage(launch.item.Meta))
		childID, ok := codexNotificationChildID(launch.meta, parsed.AgentPath)
		if !ok {
			continue
		}
		// Legacy records without a native id use the observed execution generation.
		resumeGeneration := decodeCodexChildResumeGenerations(json.RawMessage(launch.item.Meta))[childID]
		if parsed.MailboxDelivery {
			if recorded := strings.TrimSpace(decodeCodexChildTerminalStatuses(json.RawMessage(launch.item.Meta))[childID]); recorded != "" {
				status = recorded
			}
		}
		// A delivered answer may belong to an earlier turn. Only execution
		// observations may change the current runtime of a reusable child.
		if parsed.MailboxDelivery && (parsed.Recipient != "" || (launch.meta.Runtime != nil && launch.meta.Runtime.TurnID != "")) {
			delivery := evt
			delivery.ItemID = launch.item.ID
			delivery.Content = parsed.Message
			delivery.Meta = subagentStatusToItemStatusMeta("completed")
			if parsed.Recipient == "" {
				delivery.Meta = subagentStatusToItemStatusMeta(status)
			}
			if err := r.synthesizeCodexBackgroundCompletion(delivery, launch.item.ID, codexBackgroundCompletionOptions{completionID: codexMailboxCompletionID(launch.item.ID, resumeGeneration, parsed)}); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		allTerminal, aggregateStatus, err := r.markCodexSpawnChildTerminal(launch.item, launch.meta, childID, status)
		if err != nil {
			log.Printf("triage: codex-background subagent notification mark %s: %v", launch.item.ID, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		r.observeCodexSpawnChildTerminalInMemory(threadID, launch.item.ID, allTerminal)
		if !allTerminal {
			continue
		}
		r.emitBackgroundTasksChangedNudge(threadID)
		evt := provider.ProviderEvent{
			ThreadID:  threadID,
			ItemID:    launch.item.ID,
			Content:   strings.TrimSpace(parsed.Message),
			Meta:      subagentStatusToItemStatusMeta(aggregateStatus),
			Timestamp: time.Now(),
		}
		completionID := ""
		if parsed.MailboxDelivery {
			completionID = codexMailboxCompletionID(launch.item.ID, resumeGeneration, parsed)
		}
		if err := r.synthesizeCodexBackgroundCompletion(evt, launch.item.ID, codexBackgroundCompletionOptions{completionID: completionID}); err != nil {
			log.Printf("triage: codex-background subagent completion %s: %v", launch.item.ID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Native delivery ids do not depend on the sender's execution at receipt time.
// Older records keep their content/generation fallback.
func codexMailboxCompletionID(launchID string, resumeGeneration int, delivery codexSubagentSignalMeta) string {
	if !delivery.MailboxDelivery {
		return ToolCompletionID(launchID)
	}
	if strings.HasPrefix(delivery.DeliveryID, "item:") {
		resumeGeneration = 0
	}
	digest := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s\x00%s\x00%d",
		strings.TrimSpace(delivery.AgentPath),
		strings.TrimSpace(delivery.MessageType),
		strings.TrimSpace(delivery.Message),
		strings.TrimSpace(delivery.DeliveryID),
		resumeGeneration,
	))
	return fmt.Sprintf("complete:%s:delivery:%x", launchID, digest[:8])
}

func codexNotificationChildID(meta codexItemMeta, agentPath string) (string, bool) {
	agentPath = strings.TrimSpace(agentPath)
	if containsString(meta.ReceiverThreadIDs, agentPath) {
		return agentPath, true
	}
	if len(meta.ReceiverThreadIDs) == 1 {
		childID := strings.TrimSpace(meta.ReceiverThreadIDs[0])
		return childID, childID != ""
	}
	return "", false
}

func (r *Router) persistedSubagentNotificationLaunches(
	threadID string,
	launchID string,
	agentPath string,
) ([]persistedCodexSpawnLaunch, error) {
	if launchID != "" {
		launch, found, err := r.findPersistedCodexSpawnLaunch(threadID, launchID, agentPath, false)
		if err != nil || !found {
			return nil, err
		}
		return []persistedCodexSpawnLaunch{launch}, nil
	}
	launches, err := r.listPersistedCodexSpawnLaunches(threadID)
	if err != nil {
		return nil, err
	}
	out := make([]persistedCodexSpawnLaunch, 0, 1)
	for _, launch := range launches {
		if containsString(launch.meta.ReceiverThreadIDs, agentPath) {
			out = append(out, launch)
		}
	}
	return out, nil
}

// recordCodexMailboxProgress lands a child -> parent `MESSAGE` delivery as an
// independent timeline activity: no terminal status and no completion row.
// Metadata keeps a bounded preview; the payload retains the full readable body.
func (r *Router) recordCodexMailboxProgress(evt provider.ProviderEvent, parsed codexSubagentSignalMeta) error {
	launches, err := r.codexSubagentNotificationLaunches(evt, parsed)
	if err != nil || len(launches) == 0 {
		return err
	}
	var firstErr error
	for _, launch := range launches {
		if err := r.persistCodexMailboxProgress(evt, launch, parsed); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// codexSubagentNotificationLaunches resolves the spawn card(s) a mailbox
// delivery belongs to. The provider resolves the canonical agent path to a
// launch id for mailbox deliveries; older unnamed-agent builds only carry the
// receiver thread id, which the roster walk covers.
func (r *Router) codexSubagentNotificationLaunches(
	evt provider.ProviderEvent,
	parsed codexSubagentSignalMeta,
) ([]persistedCodexSpawnLaunch, error) {
	itemID := strings.TrimSpace(evt.ItemID)
	if parsed.MailboxDelivery && itemID != "" {
		launch, found, err := r.findPersistedCodexSpawnLaunchForStatus(evt.ThreadID, itemID, "")
		if err != nil || !found {
			return nil, err
		}
		return []persistedCodexSpawnLaunch{launch}, nil
	}
	return r.persistedSubagentNotificationLaunches(evt.ThreadID, itemID, parsed.AgentPath)
}
