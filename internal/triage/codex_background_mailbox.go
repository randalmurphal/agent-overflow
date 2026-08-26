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

// codex_background_mailbox.go — the mailbox half of the Codex spawn
// projection: Codex's injected `<subagent_notification>` closure signal
// (a FINAL_ANSWER delivery, which records terminal status on the owning
// launch and synthesizes the transcript completion row once every child in
// that spawn is terminal), the `MESSAGE` progress beat that must never do
// either, the row identity ONE delivery lands on
// (`codexMailboxCompletionID`), and the launch resolution both paths share.
//
// The spawn/launch state machine these writers mutate lives in
// codex_background_subagents.go. Progress deliveries are independent timeline
// activities; they never mutate the historical spawn row.

// observeCodexSubagentNotification handles the detached-child closure
// signal: Codex core injects a <subagent_notification> tag into the
// parent's next user message when a backgrounded child finished with no wait
// outstanding. The projector records terminal status on the owning spawn row
// and only synthesizes the transcript sibling once every child in that spawn is
// terminal. When the provider resolved the path to a parent card, evt.ItemID is
// the authoritative launch id; otherwise we fall back to receiverThreadIDs for
// older unnamed-agent builds where agent_path was the receiver thread id.
func (r *Router) observeCodexSubagentNotification(evt provider.ProviderEvent) error {
	parsed := decodeCodexSubagentSignalMeta(evt.Meta)
	if parsed.AgentPath == "" {
		return nil
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
		childID, ok := codexNotificationChildID(launch.meta, parsed.AgentPath)
		if !ok {
			continue
		}
		// Read before the terminal merge: the delivery belongs to the child
		// turn that is settling now, which is the generation the card already
		// carries. reactivateCodexSpawnChild is what advances it.
		resumeGeneration := decodeCodexChildResumeGenerations(json.RawMessage(launch.item.Meta))[childID]
		if parsed.MailboxDelivery {
			if recorded := strings.TrimSpace(decodeCodexChildTerminalStatuses(json.RawMessage(launch.item.Meta))[childID]); recorded != "" {
				status = recorded
			}
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
		r.emitCodexBackgroundTasksChanged(threadID)
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

// codexMailboxCompletionID is the stable row id for ONE child -> parent mailbox
// delivery on a spawn launch.
//
// It is keyed on the delivery's own CONTENT, never on the provider-supplied
// `delivery_id` alone. A parent turn drains as many deliveries as the child
// sent, and older Codex builds label all of them with the receiving parent
// turn id (corpus: rollout-2026-08-20T16-16-28-01a020d1-* records 686 and 763,
// two distinct FINAL_ANSWERs sharing turn_id 01a020d1-a06b-...): keying on that
// made a child's second answer overwrite its first. Hashing the content keeps
// distinct deliveries distinct AND keeps a genuine retry — the same record seen
// by both the live raw stream and the rollout tail — on one row, without any
// in-memory counter that a restart could reset into a duplicate.
//
// `status` is deliberately not part of the key: the caller may substitute a
// previously recorded terminal status before synthesizing, and the id must not
// move when it does.
//
// resumeGeneration is the one non-content dimension, and it is what keeps a
// child that legitimately answers IDENTICALLY twice on two rows: a
// `followup_task` wakes it for a second turn and it replies "Done." again, byte
// for byte. Pure content hashing collapsed that onto the first row. The
// generation is durable thread state on the launch (`codex_child_resume_
// generations`, advanced by reactivateCodexSpawnChild), never an in-memory
// counter, so both carriers of ONE delivery — the live raw stream and the
// rollout tail — still read the same value and still land on one row.
func codexMailboxCompletionID(launchID string, resumeGeneration int, delivery codexSubagentSignalMeta) string {
	if !delivery.MailboxDelivery {
		return ToolCompletionID(launchID)
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
// Plaintext keeps one bounded line; encrypted delivery still records the beat.
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
