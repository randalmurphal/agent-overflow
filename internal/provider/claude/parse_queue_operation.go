package claude

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// Claude records every change to its command queue as a `queue-operation`
// row of the main session file, and --session-mirror streams those rows
// live in `transcript_mirror` batches of that file. They are not
// transcript rows. One of them is the only record that the CLI dropped a
// notification meant to wake a parked background agent (claude-wire.md
// §E6b, dropped wake): its wake router failed to resume the agent, for
// example because the agent's worktree is gone, and removed the
// notification with a drop reason. No `task_started`, `task_updated` or
// level-set frame follows. Captured live on 2.1.284 (2026-10-14,
// fixtures/claude/local_agent_dropped_wake_20261014.ndjson).
type queueOperationEntry struct {
	Type      string          `json:"type"`
	Operation string          `json:"operation"`
	Reason    string          `json:"reason"`
	Content   json.RawMessage `json:"content"`
}

const queueOperationType = "queue-operation"

// queueOperationDropReasons are the reasons the CLI's wake router removes
// an agent's notifications without delivering them: the agent was stopped
// by the user or refused resumption for good (`agent_stopped`), or its
// resume kept failing (`resume_failed`). The other removal reasons
// (`delivered_to_agent`, `absorbed_mid_turn`) delivered the notification.
var queueOperationDropReasons = map[string]bool{
	"agent_stopped": true,
	"resume_failed": true,
}

// queueOperationDeliveredReasons are the removal reasons that delivered
// the notification.
var queueOperationDeliveredReasons = map[string]bool{
	"delivered_to_agent": true,
	"absorbed_mid_turn":  true,
}

// takeQueueOperations removes the `queue-operation` rows from a mirror
// batch, so transcript routing never sees them, and returns the batch
// that remains with one EventAgentWakeDropped per task notification a
// drop removed.
func takeQueueOperations(threadID string, entries []json.RawMessage, now time.Time) ([]json.RawMessage, []provider.ProviderEvent) {
	var kept []json.RawMessage
	var events []provider.ProviderEvent
	for i, entry := range entries {
		if !strings.Contains(string(entry), `"`+queueOperationType+`"`) {
			if kept != nil {
				kept = append(kept, entry)
			}
			continue
		}
		var op queueOperationEntry
		if err := json.Unmarshal(entry, &op); err != nil || op.Type != queueOperationType {
			if kept != nil {
				kept = append(kept, entry)
			}
			continue
		}
		if kept == nil {
			kept = append(make([]json.RawMessage, 0, len(entries)), entries[:i]...)
		}
		events = append(events, wakeDropEvents(threadID, op, now)...)
	}
	if kept == nil {
		return entries, nil
	}
	return kept, events
}

// wakeDropEvents reports the task notifications a drop removed. A removal
// whose reason is unknown is logged: a new reason must be classified here
// before AO can say whether the agent woke.
func wakeDropEvents(threadID string, op queueOperationEntry, now time.Time) []provider.ProviderEvent {
	if op.Operation != "remove" || queueOperationDeliveredReasons[op.Reason] {
		return nil
	}
	if !queueOperationDropReasons[op.Reason] {
		if op.Reason != "" {
			log.Printf("claude: thread %s: queue-operation remove with unknown reason %q", threadID, op.Reason)
		}
		return nil
	}
	var events []provider.ProviderEvent
	for _, fields := range ExtractAllTaskNotificationFields(queueOperationText(op.Content)) {
		taskID := strings.TrimSpace(fields.TaskID)
		if taskID == "" {
			continue
		}
		meta, err := json.Marshal(provider.AgentWakeDroppedMeta{
			TaskID:    taskID,
			ToolUseID: strings.TrimSpace(fields.ToolUseID),
			Reason:    op.Reason,
		})
		if err != nil {
			log.Printf("claude: thread %s: encode dropped wake of task %s: %v", threadID, taskID, err)
			continue
		}
		events = append(events, provider.ProviderEvent{
			Kind:      provider.EventAgentWakeDropped,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: now,
		})
	}
	return events
}

// queueOperationText reads a queued command's value: a string, or content
// blocks whose text is joined.
func queueOperationText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}
