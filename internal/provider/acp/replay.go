package acp

import (
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
	completedTools := make(map[string]bool)
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
	turnHasResponse := false
	openTurn := func() {
		if !turnOpen {
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventTurnStart,
				Timestamp: now,
			})
			turnOpen = true
			turnHasResponse = false
		}
	}
	closeTurn := func() {
		flush()
		if turnOpen {
			events = append(events, provider.ProviderEvent{
				Kind:         provider.EventTurnComplete,
				TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
				Timestamp:    now,
			})
			turnOpen = false
			turnHasResponse = false
		}
	}

	for _, u := range updates {
		switch u.SessionUpdate {
		case "user_message_chunk":
			if turnOpen && turnHasResponse {
				closeTurn()
			}
			openTurn()
			if block, ok := u.decodeContentBlock(); ok && block.Text != "" {
				appendChunk(provider.EventUserText, block.Text)
			}
		case "agent_thought_chunk":
			if textKind == provider.EventUserText {
				flush()
			}
			openTurn()
			turnHasResponse = true
			if block, ok := u.decodeContentBlock(); ok && block.Text != "" {
				appendChunk(provider.EventThinking, block.Text)
			}
		case "agent_message_chunk":
			if textKind == provider.EventUserText {
				flush()
			}
			openTurn()
			turnHasResponse = true
			if block, ok := u.decodeContentBlock(); ok && block.Text != "" {
				appendChunk(provider.EventTextDelta, block.Text)
			}
		case "tool_call":
			flush()
			openTurn()
			turnHasResponse = true
			tc := u.toToolCallUpdate()
			if tc.ToolCallID == "" {
				continue
			}
			norm := flavor.NormalizeToolStart(&tc)
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventToolStart,
				ItemID:    tc.ToolCallID,
				ItemType:  norm.ItemType,
				Meta:      buildToolStartMeta(norm),
				Timestamp: now,
			})
			// In real ACP `session/load` replay, providers like `agy` emit a
			// single `tool_call` frame already carrying its terminal status and
			// rawOutput/content without a separate `tool_call_update`.
			if tc.Status == "completed" || tc.Status == "failed" || tc.Status == "cancelled" {
				doneNorm := flavor.NormalizeToolComplete(&tc, norm)
				events = append(events, buildToolCompleteEvents("", "", tc.ToolCallID, doneNorm, now)...)
				completedTools[tc.ToolCallID] = true
			} else {
				startedTools[tc.ToolCallID] = norm
			}
		case "tool_call_update":
			flush()
			tc := u.toToolCallUpdate()
			if tc.ToolCallID == "" {
				continue
			}
			if tc.Status != "completed" && tc.Status != "failed" && tc.Status != "cancelled" {
				continue
			}
			if completedTools[tc.ToolCallID] {
				continue
			}
			started := startedTools[tc.ToolCallID]
			delete(startedTools, tc.ToolCallID)
			completedTools[tc.ToolCallID] = true
			norm := flavor.NormalizeToolComplete(&tc, started)
			events = append(events, buildToolCompleteEvents("", "", tc.ToolCallID, norm, now)...)
		case "usage_update":
			flush()
		}
	}
	closeTurn()
	return events
}
