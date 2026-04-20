package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

func (r *Router) handleDiff(evt provider.ProviderEvent) error {
	if itemID := eventItemID(evt); itemID != "" {
		item, found, err := r.store.GetThreadItem(evt.ThreadID, itemID)
		if err != nil {
			return fmt.Errorf("diff get item %s: %w", itemID, err)
		}
		if !found {
			item, err = r.newToolCallItem(evt.ThreadID, itemID, "file_change", buildSummary("diff", buildPayloadMeta("diff", evt)), statusCompleted, eventTimestampMillis(evt))
			if err != nil {
				return fmt.Errorf("diff create tool_call %s: %w", itemID, err)
			}
		}
		return r.attachPayloadToItem(item, evt, "diff", item.Summary, evt.Replace)
	}

	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("diff turn index: %w", err)
	}
	upgraded, err := r.upgradeSummaryOnlyToolResults(evt.ThreadID, turnIndex, evt.Content)
	if err != nil {
		return err
	}
	if upgraded {
		return nil
	}

	item, found, err := r.findLatestToolCall(evt.ThreadID, "file_change", "edit", "write", "multiedit", "multi_edit")
	if err != nil {
		return fmt.Errorf("diff resolve target: %w", err)
	}
	if !found {
		item, err = r.newToolCallItem(
			evt.ThreadID,
			fmt.Sprintf("diff:%d", turnIndex),
			"file_change",
			buildSummary("diff", buildPayloadMeta("diff", evt)),
			statusCompleted,
			eventTimestampMillis(evt),
		)
		if err != nil {
			return fmt.Errorf("diff fallback tool_call: %w", err)
		}
	}
	return r.attachPayloadToItem(item, evt, "diff", item.Summary, evt.Replace)
}

func (r *Router) handleCommandOutput(evt provider.ProviderEvent) error {
	itemID := eventItemID(evt)
	if itemID != "" {
		item, found, err := r.store.GetThreadItem(evt.ThreadID, itemID)
		if err != nil {
			return fmt.Errorf("command output get item %s: %w", itemID, err)
		}
		if !found {
			item, err = r.newToolCallItem(
				evt.ThreadID,
				itemID,
				"command_execution",
				buildSummary("command_output", buildPayloadMeta("command_output", evt)),
				statusRunning,
				eventTimestampMillis(evt),
			)
			if err != nil {
				return fmt.Errorf("command output create tool_call %s: %w", itemID, err)
			}
		}
		return r.attachPayloadToItem(item, evt, "command_output", item.Summary, false)
	}

	item, found, err := r.findLatestToolCall(evt.ThreadID, "command_execution", "bash")
	if err != nil {
		return fmt.Errorf("command output resolve target: %w", err)
	}
	if !found {
		turnIndex, terr := r.currentTurnIndex(evt.ThreadID)
		if terr != nil {
			return fmt.Errorf("command output turn index: %w", terr)
		}
		item, err = r.newToolCallItem(
			evt.ThreadID,
			fmt.Sprintf("command-output:%d", turnIndex),
			"command_execution",
			buildSummary("command_output", buildPayloadMeta("command_output", evt)),
			statusRunning,
			eventTimestampMillis(evt),
		)
		if err != nil {
			return fmt.Errorf("command output fallback tool_call: %w", err)
		}
	}
	return r.attachPayloadToItem(item, evt, "command_output", item.Summary, false)
}

func (r *Router) handleProposedPlan(evt provider.ProviderEvent) error {
	now := eventTimestampMillis(evt)
	metaJSON := buildPayloadMeta("proposed_plan", evt)
	summary := buildSummary("proposed_plan", metaJSON)
	itemID := eventItemID(evt)
	if itemID == "" {
		turnIndex, err := r.currentTurnIndex(evt.ThreadID)
		if err != nil {
			return fmt.Errorf("plan turn index: %w", err)
		}
		itemID = fmt.Sprintf("plan:%d", turnIndex)
	}

	item, found, err := r.store.GetThreadItem(evt.ThreadID, itemID)
	if err != nil {
		return fmt.Errorf("plan get item %s: %w", itemID, err)
	}
	if !found {
		item, err = r.newToolCallItem(evt.ThreadID, itemID, "plan", summary, statusCompleted, now)
		if err != nil {
			return fmt.Errorf("plan create tool_call %s: %w", itemID, err)
		}
	} else {
		item.Kind = itemKindToolCall
		item.Role = "assistant"
		item.Status = statusCompleted
		item.ToolName = "plan"
		item.Summary = summary
		item.UpdatedAt = now
	}

	return r.attachPayloadToItem(item, evt, "proposed_plan", item.Summary, true)
}

func eventTimestampMillis(evt provider.ProviderEvent) int64 {
	now := evt.Timestamp.UnixMilli()
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	return now
}

func eventItemID(evt provider.ProviderEvent) string {
	if evt.ItemID != "" {
		return strings.TrimSpace(evt.ItemID)
	}
	return strings.TrimSpace(metaNestedString(evt.Meta, "item", "id"))
}

func metaNestedString(raw json.RawMessage, path ...string) string {
	if len(raw) == 0 || len(path) == 0 {
		return ""
	}

	var current any
	if err := json.Unmarshal(raw, &current); err != nil {
		return ""
	}
	for _, segment := range path {
		obj, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		next, ok := obj[segment]
		if !ok {
			return ""
		}
		current = next
	}
	value, _ := current.(string)
	return value
}

func (r *Router) newToolCallItem(
	threadID, itemID, toolName, summary, status string,
	now int64,
) (store.Item, error) {
	turnIndex, err := r.currentTurnIndex(threadID)
	if err != nil {
		return store.Item{}, err
	}
	if summary == "" {
		summary = toolName
	}
	item := store.Item{
		ID:        itemID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      itemKindToolCall,
		Role:      "assistant",
		Status:    status,
		Summary:   summary,
		ToolName:  toolName,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if decision := r.peekApprovalDecision(threadID, itemID); decision != "" {
		item.Decision = decision
	}
	return item, nil
}

// findLatestToolCall returns the most-recent tool_call row in the
// current turn whose tool_name matches one of toolNames
// (case-insensitive). Delegates to store.LatestToolCallByName so the
// filter runs in SQLite — previous implementations pulled every turn
// item into Go and scanned in reverse, which was O(turn_items) on
// a path called once per fallback diff/command-output event.
func (r *Router) findLatestToolCall(threadID string, toolNames ...string) (store.Item, bool, error) {
	turnIndex, err := r.currentTurnIndex(threadID)
	if err != nil {
		return store.Item{}, false, err
	}
	normalized := make([]string, 0, len(toolNames))
	for _, name := range toolNames {
		trimmed := strings.TrimSpace(strings.ToLower(name))
		if trimmed != "" {
			normalized = append(normalized, trimmed)
		}
	}
	return r.store.LatestToolCallByName(threadID, turnIndex, normalized)
}

func (r *Router) attachPayloadToItem(
	item store.Item,
	evt provider.ProviderEvent,
	payloadKind string,
	summary string,
	replace bool,
) error {
	now := eventTimestampMillis(evt)
	payloadID := item.PayloadID
	data := []byte(evt.Content)
	linked := payloadID != "" && item.PayloadKind == payloadKind

	// Append-only hot path: when the item already owns a payload of the
	// same kind and the caller isn't replacing the blob wholesale, we
	// append the delta inside SQLite and update meta + summary without
	// ever reading the prior data into Go memory. The former path —
	// GetPayloadData → append(existing, data...) → write full blob —
	// was O(N^2) in cumulative payload size. Meta is derived from the
	// DELTA alone: command_output's preview has always reflected the
	// latest chunk (forge parity), and diff meta carries file-level
	// counters that are already cumulative via the prior read — for
	// diff the caller passes replace=true anyway, so we stay on the
	// full-write branch below.
	if linked && !replace {
		metaEvt := evt
		metaEvt.Content = string(data)
		metaJSON := buildPayloadMeta(payloadKind, metaEvt)
		if err := r.store.AppendPayloadData(payloadID, data, metaJSON, now); err != nil {
			return fmt.Errorf("append %s payload %s: %w", payloadKind, payloadID, err)
		}
		item.PayloadID = payloadID
		if summary != "" {
			item.Summary = summary
		}
		item.UpdatedAt = now
		if item.CreatedAt == 0 {
			item.CreatedAt = now
		}
		return r.persistItem(item, nil)
	}

	if !linked {
		payloadID = uuid.New().String()
	}

	metaEvt := evt
	metaEvt.Content = string(data)
	metaJSON := buildPayloadMeta(payloadKind, metaEvt)
	payload := store.Payload{
		ID:        payloadID,
		Kind:      payloadKind,
		Meta:      metaJSON,
		Data:      data,
		CreatedAt: now,
	}
	item.PayloadID = payloadID
	if summary != "" {
		item.Summary = summary
	}
	item.UpdatedAt = now
	if item.CreatedAt == 0 {
		item.CreatedAt = now
	}
	return r.persistItem(item, &payload)
}

func buildPayloadMeta(payloadKind string, evt provider.ProviderEvent) string {
	switch payloadKind {
	case "diff":
		dm := ExtractDiffMeta(evt.Content)
		data, err := json.Marshal(dm)
		if err != nil {
			log.Printf("triage: marshal diff meta: %v", err)
			return "{}"
		}
		return string(data)
	case "command_output":
		cm := ExtractCommandOutputMeta(evt.Content, "", 0)
		if evt.Meta != nil {
			var parsed struct {
				Command  string `json:"command"`
				ExitCode int    `json:"exitCode"`
			}
			if json.Unmarshal(evt.Meta, &parsed) == nil {
				cm = ExtractCommandOutputMeta(evt.Content, parsed.Command, parsed.ExitCode)
			}
		}
		data, err := json.Marshal(cm)
		if err != nil {
			log.Printf("triage: marshal command output meta: %v", err)
			return "{}"
		}
		return string(data)
	case "thinking":
		tm := ExtractThinkingMeta(evt.Content)
		tm.Signature = metaNestedString(evt.Meta, "signature")
		data, err := json.Marshal(tm)
		if err != nil {
			log.Printf("triage: marshal thinking meta: %v", err)
			return "{}"
		}
		return string(data)
	case "proposed_plan":
		pm := ExtractProposedPlanMeta(evt.Content)
		data, err := json.Marshal(pm)
		if err != nil {
			log.Printf("triage: marshal proposed plan meta: %v", err)
			return "{}"
		}
		return string(data)
	default:
		return "{}"
	}
}

// buildSummary creates a short human-readable summary from meta.
func buildSummary(payloadKind, metaJSON string) string {
	switch payloadKind {
	case "diff":
		var dm DiffMeta
		if json.Unmarshal([]byte(metaJSON), &dm) == nil {
			return fmt.Sprintf("%s: +%d/-%d %s", dm.ChangeKind, dm.Insertions, dm.Deletions, dm.FilePath)
		}
	case "command_output":
		var cm CommandOutputMeta
		if json.Unmarshal([]byte(metaJSON), &cm) == nil {
			return fmt.Sprintf("$ %s (exit %d, %d lines)", cm.Command, cm.ExitCode, cm.LineCount)
		}
	case "thinking":
		var tm ThinkingMeta
		if json.Unmarshal([]byte(metaJSON), &tm) == nil {
			return tm.Preview
		}
	case "proposed_plan":
		var pm ProposedPlanMeta
		if json.Unmarshal([]byte(metaJSON), &pm) == nil && pm.Title != "" {
			return pm.Title
		}
	}
	return ""
}
