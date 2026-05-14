package codex

import (
	"encoding/json"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// classifyTurnNotification handles `turn/*` methods.
func classifyTurnNotification(threadID, method string, params json.RawMessage, now time.Time) ([]provider.ProviderEvent, bool) {
	switch method {
	case "turn/started":
		turnID := readNestedString(params, "turn", "id")
		return []provider.ProviderEvent{{
			Kind:      provider.EventTurnStart,
			ThreadID:  threadID,
			TurnID:    turnID,
			Timestamp: now,
		}}, true

	case "turn/completed":
		return classifyTurnCompleted(threadID, params, now), true

	case "turn/diff/updated":
		// Codex uses this as an aggregate turn-level diff snapshot. The
		// transcript edit row is the structured fileChange item; this snapshot is
		// only available to fill summary-only expanded diff content.
		diff := readNestedString(params, "diff")
		if strings.TrimSpace(diff) == "" {
			return nil, true
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventDiff,
			ThreadID:  threadID,
			TurnID:    firstNonEmptyString(readNestedString(params, "turnId"), readNestedString(params, "turn", "id")),
			Content:   diff,
			Meta:      mergeMetaKeys(params, map[string]any{"upgrade_only": true, "source": "turn/diff/updated"}),
			Timestamp: now,
			Replace:   true,
		}}, true

	case "turn/plan/updated":
		return []provider.ProviderEvent{{
			Kind:      provider.EventTodoUpdate,
			ThreadID:  threadID,
			Content:   "Updated Todos",
			Meta:      mergeMetaKeys(params, map[string]any{"kind": "todo_update", "title": "Updated Todos"}),
			Timestamp: now,
		}}, true
	}
	return nil, false
}

// classifyTurnCompleted breaks out the turn/completed shape. Codex's
// context-window signal is thread/tokenUsage/updated; turn/completed is only
// the lifecycle boundary plus any terminal error state.
func classifyTurnCompleted(threadID string, params json.RawMessage, now time.Time) []provider.ProviderEvent {
	wire := decodeTurnCompletedParams(params)
	turnID := wire.Turn.ID
	status := wire.Turn.Status
	errorMsg := wire.Turn.Error.Message
	stopReason := ""
	aborted := false
	switch status {
	case "completed":
		stopReason = "end_turn"
	case "failed":
		stopReason = "error"
	case "interrupted":
		stopReason = "interrupted"
		aborted = true
	}

	var events []provider.ProviderEvent

	if status == "failed" && errorMsg != "" {
		events = append(events, provider.ProviderEvent{
			Kind: provider.EventError, ThreadID: threadID, TurnID: turnID, Content: errorMsg, Timestamp: now,
		})
	}

	events = append(events, provider.ProviderEvent{
		Kind:     provider.EventTurnComplete,
		ThreadID: threadID,
		TurnID:   turnID,
		TurnComplete: &provider.WireTurnCompleteMeta{
			StopReason:   stopReason,
			Aborted:      aborted,
			ErrorMessage: errorMsg,
		},
		Timestamp: now,
	})
	return events
}

type turnCompletedParams struct {
	Turn struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"turn"`
}

func decodeTurnCompletedParams(params json.RawMessage) turnCompletedParams {
	var decoded turnCompletedParams
	_ = json.Unmarshal(params, &decoded)
	return decoded
}
