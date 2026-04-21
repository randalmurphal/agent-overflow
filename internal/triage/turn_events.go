package triage

import "encoding/json"

// TurnStartedEvent is the frontend-facing payload for provider:turn_started.
// The frontend reacts to this by flipping `pane.activeTurn` on and starting
// the self-ticking working indicator. See
// docs/architecture/turn-lifecycle.md §Frontend state shape for the full
// flow.
type TurnStartedEvent struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	TurnIndex int    `json:"turnIndex"`
	StartedAt int64  `json:"startedAt"`
}

// TurnCompletedEvent is the frontend-facing payload for
// provider:turn_completed. Carries the settled-turn projection the UI
// needs to render the completion divider and the "Worked for Xs · Yk
// tokens" label, plus clear the working indicator. Optional fields are
// populated when the underlying provider surface carries them
// (assistant_message_id, token usage, error message, aborted flag).
type TurnCompletedEvent struct {
	ThreadID           string          `json:"threadId"`
	TurnID             string          `json:"turnId"`
	TurnIndex          int             `json:"turnIndex"`
	StartedAt          int64           `json:"startedAt"`
	CompletedAt        int64           `json:"completedAt"`
	StopReason         string          `json:"stopReason"`
	AssistantMessageID string          `json:"assistantMessageId,omitempty"`
	TokenUsage         json.RawMessage `json:"tokenUsage,omitempty"`
	ErrorMessage       string          `json:"errorMessage,omitempty"`
	Aborted            bool            `json:"aborted,omitempty"`
}

// SubagentNotificationEvent is the frontend-facing payload for
// provider:subagent_notification. Carries the raw Meta from the
// provider adapter's `<subagent_notification>` parse — the frontend
// decides whether to surface a toast, tray entry, or nothing.
// Persistence is deliberately NOT a triage concern for this kind.
type SubagentNotificationEvent struct {
	ThreadID string          `json:"threadId"`
	Meta     json.RawMessage `json:"meta,omitempty"`
}

// turnCompleteMeta is the internal decode shape for
// EventTurnComplete.Meta. Producers (Claude parse_result, Codex
// protocol) build it as a map; we want a typed view here so the
// handler doesn't have to re-decode the same fields three times.
type turnCompleteMeta struct {
	StopReason         string          `json:"stop_reason,omitempty"`
	AssistantMessageID string          `json:"assistant_message_id,omitempty"`
	Usage              json.RawMessage `json:"usage,omitempty"`
	DurationMs         int64           `json:"duration_ms,omitempty"`
	TotalCostUSD       float64         `json:"total_cost_usd,omitempty"`
	Aborted            bool            `json:"aborted,omitempty"`
	Truncated          bool            `json:"truncated,omitempty"`
	TurnStatus         string          `json:"turn_status,omitempty"`
	Error              string          `json:"error,omitempty"`
	// Codex `turn/completed` envelope carries the nested
	// `{turn: {...}}` shape untouched via the adapter's
	// mergeMetaKeys, so a nested assistant message id appears here
	// too. We accept either the flat Claude key or the nested Codex
	// shape.
	LastAssistantMessageID string `json:"lastAssistantMessageId,omitempty"`
	Turn                   struct {
		LastAssistantMessageID string `json:"lastAssistantMessageId,omitempty"`
	} `json:"turn,omitempty"`
}

// decodeTurnCompleteMeta unmarshals the Meta JSON into the typed view
// above. Malformed meta yields a zero-valued struct — the handler
// treats missing fields as "provider didn't tell us" and defaults.
func decodeTurnCompleteMeta(raw json.RawMessage) turnCompleteMeta {
	if len(raw) == 0 {
		return turnCompleteMeta{}
	}
	var m turnCompleteMeta
	if json.Unmarshal(raw, &m) != nil {
		return turnCompleteMeta{}
	}
	return m
}

// canonicalStopReason maps provider-specific stop reasons onto the
// canonical set documented in turn-lifecycle.md: end_turn | max_tokens
// | tool_use | stop_sequence | refusal | error | interrupted. Unknown
// values pass through untouched so a future provider extension isn't
// silently rewritten to "error".
func canonicalStopReason(meta turnCompleteMeta) string {
	// Interruption always wins.
	if meta.Aborted || meta.Truncated || meta.TurnStatus == "interrupted" {
		return "interrupted"
	}
	// Explicit provider-reported reason.
	if r := meta.StopReason; r != "" {
		switch r {
		case "end_turn", "max_tokens", "tool_use", "stop_sequence", "refusal", "error", "interrupted":
			return r
		case "error_during_execution":
			return "error"
		default:
			return r
		}
	}
	// Codex turn_status fallback — completed is the happy path.
	switch meta.TurnStatus {
	case "completed":
		return "end_turn"
	case "failed":
		return "error"
	case "interrupted":
		return "interrupted"
	}
	return ""
}

// resolveAssistantMessageID picks whichever shape the provider used.
// Claude puts it at meta.assistant_message_id; Codex puts the raw
// envelope nested under meta.turn.lastAssistantMessageId, or some
// adapters flatten it to meta.lastAssistantMessageId.
func resolveAssistantMessageID(meta turnCompleteMeta) string {
	if meta.AssistantMessageID != "" {
		return meta.AssistantMessageID
	}
	if meta.LastAssistantMessageID != "" {
		return meta.LastAssistantMessageID
	}
	return meta.Turn.LastAssistantMessageID
}
