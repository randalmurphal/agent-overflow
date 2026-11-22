package acp

import (
	"encoding/json"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// ReplayUpdatesToEvents translates ACP `session/load` replay updates into
// normalized provider.ProviderEvent items suitable for session import.
func ReplayUpdatesToEvents(updates []SessionUpdate, flavor Flavor) []provider.ProviderEvent {
	if flavor == nil {
		flavor = DefaultFlavor{}
	}
	var events []provider.ProviderEvent
	var textKind provider.EventKind
	var textBuf strings.Builder
	startedTools := make(map[string]NormalizedToolStart)
	now := time.Now()
	flush := func() {
		if textBuf.Len() == 0 {
			return
		}
		role := "assistant"
		if textKind == provider.EventUserText {
			role = "user"
		}
		events = append(events, provider.ProviderEvent{
			Kind:      textKind,
			Role:      role,
			Content:   textBuf.String(),
			Timestamp: now,
		})
		textBuf.Reset()
		textKind = ""
	}
	appendChunk := func(kind provider.EventKind, chunk string) {
		if chunk == "" {
			return
		}
		if textKind != "" && textKind != kind {
			flush()
		}
		textKind = kind
		textBuf.WriteString(chunk)
	}

	turnOpen := false
	openTurn := func() {
		if !turnOpen {
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventTurnStart,
				Timestamp: now,
			})
			turnOpen = true
		}
	}
	closeTurn := func() {
		flush()
		if turnOpen {
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventTurnComplete,
				Timestamp: now,
			})
			turnOpen = false
		}
	}

	for _, u := range updates {
		switch u.SessionUpdate {
		case "user_message_chunk":
			if turnOpen {
				closeTurn()
			}
			if block, ok := u.decodeContentBlock(); ok && block.Text != "" {
				appendChunk(provider.EventUserText, block.Text)
			}
		case "agent_thought_chunk":
			if textKind == provider.EventUserText {
				flush()
			}
			openTurn()
			if block, ok := u.decodeContentBlock(); ok && block.Text != "" {
				appendChunk(provider.EventThinking, block.Text)
			}
		case "agent_message_chunk":
			if textKind == provider.EventUserText {
				flush()
			}
			openTurn()
			if block, ok := u.decodeContentBlock(); ok && block.Text != "" {
				appendChunk(provider.EventTextDelta, block.Text)
			}
		case "tool_call":
			flush()
			openTurn()
			tc := u.toToolCallUpdate()
			if tc.ToolCallID == "" {
				continue
			}
			norm := flavor.NormalizeToolStart(&tc)
			startedTools[tc.ToolCallID] = norm
			metaFields := map[string]any{"toolName": norm.ToolName}
			if len(norm.Input) > 0 {
				metaFields["input"] = norm.Input
			}
			if norm.Description != "" {
				metaFields["description"] = norm.Description
			}
			for k, v := range norm.ExtraMeta {
				metaFields[k] = v
			}
			meta, _ := json.Marshal(metaFields)
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventToolStart,
				ItemID:    tc.ToolCallID,
				ItemType:  norm.ItemType,
				Meta:      meta,
				Timestamp: now,
			})
		case "tool_call_update":
			flush()
			tc := u.toToolCallUpdate()
			if tc.ToolCallID == "" {
				continue
			}
			if tc.Status != "completed" && tc.Status != "failed" && tc.Status != "cancelled" {
				continue
			}
			started := startedTools[tc.ToolCallID]
			delete(startedTools, tc.ToolCallID)
			norm := flavor.NormalizeToolComplete(&tc, started)
			if norm.DiffPatch != "" {
				diffMeta, _ := json.Marshal(map[string]any{
					"filePath": norm.DiffFilePath,
					"toolName": norm.ToolName,
				})
				events = append(events, provider.ProviderEvent{
					Kind:      provider.EventDiff,
					ItemID:    tc.ToolCallID,
					ItemType:  "file_change",
					Content:   norm.DiffPatch,
					Meta:      diffMeta,
					Replace:   true,
					Timestamp: now,
				})
			}
			completeMeta := map[string]any{
				"toolName": norm.ToolName,
				"is_error": norm.IsError,
			}
			for k, v := range norm.ExtraMeta {
				completeMeta[k] = v
			}
			meta, _ := json.Marshal(completeMeta)
			events = append(events, provider.ProviderEvent{
				Kind:           provider.EventToolComplete,
				ItemID:         tc.ToolCallID,
				ItemType:       norm.ItemType,
				Content:        norm.Content,
				ContentPresent: norm.Content != "",
				Meta:           meta,
				Timestamp:      now,
			})
		case "usage_update":
			flush()
		}
	}
	closeTurn()
	return events
}
