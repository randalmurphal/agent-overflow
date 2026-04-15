package provider

import (
	"encoding/json"
	"time"
)

// ProviderKind identifies the provider backend.
type ProviderKind string

const (
	Claude ProviderKind = "claude"
	Codex  ProviderKind = "codex"
)

// EventKind classifies provider events for triage routing.
type EventKind string

const (
	// Inline events — forwarded directly to frontend via EventsEmit.
	EventInit             EventKind = "init"
	EventTextDelta        EventKind = "text_delta"
	EventToolStart        EventKind = "tool_start"
	EventToolComplete     EventKind = "tool_complete"
	EventTurnStart        EventKind = "turn_start"
	EventTurnComplete     EventKind = "turn_complete"
	EventApprovalRequest  EventKind = "approval_request"
	EventApprovalResolved EventKind = "approval_resolved"
	EventSessionStatus    EventKind = "session_status"
	EventTokenUsage       EventKind = "token_usage"
	EventError            EventKind = "error"
	EventBackgroundStart  EventKind = "background_start"
	EventBackgroundDelta  EventKind = "background_delta"
	EventBackgroundComplete EventKind = "background_complete"

	// Heavy events — persisted to SQLite, meta emitted to frontend.
	EventDiff          EventKind = "diff"
	EventCommandOutput EventKind = "command_output"
	EventThinking      EventKind = "thinking"
)

// ItemKind for persisted items in the database.
type ItemKind string

const (
	ItemText              ItemKind = "text"
	ItemToolCall          ItemKind = "tool_call"
	ItemToolResult        ItemKind = "tool_result"
	ItemThinking          ItemKind = "thinking"
	ItemDiff              ItemKind = "diff"
	ItemCommandExecution  ItemKind = "command_execution"
	ItemBackgroundStarted ItemKind = "background_started"
	ItemBackgroundDone    ItemKind = "background_done"
)

// ProviderEvent is the normalized event emitted by both provider protocols.
// The triage layer classifies these and routes them.
type ProviderEvent struct {
	Kind      EventKind       `json:"kind"`
	ThreadID  string          `json:"threadId"`
	TurnID    string          `json:"turnId,omitempty"`
	ItemID    string          `json:"itemId,omitempty"`
	ItemType  string          `json:"itemType,omitempty"`
	Content   string          `json:"content,omitempty"`
	Role      string          `json:"role,omitempty"`
	Meta      json.RawMessage `json:"meta,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
	Raw       json.RawMessage `json:"-"`
}

// ApprovalRequest is sent when a provider needs user permission.
type ApprovalRequest struct {
	RequestID   string          `json:"requestId"`
	ThreadID    string          `json:"threadId"`
	TurnID      string          `json:"turnId,omitempty"`
	ToolName    string          `json:"toolName"`
	Description string          `json:"description"`
	Input       json.RawMessage `json:"input"`
	Title       string          `json:"title"`
}

// ApprovalResponse is sent back to the provider.
type ApprovalResponse struct {
	RequestID string `json:"requestId"`
	Decision  string `json:"decision"` // "allow", "deny", "allow_session"
}

// SessionInfo contains metadata from the provider init/handshake.
type SessionInfo struct {
	SessionID string   `json:"sessionId"`
	Model     string   `json:"model"`
	CWD       string   `json:"cwd"`
	Tools     []string `json:"tools,omitempty"`
	Version   string   `json:"version,omitempty"`
}

// TokenUsage tracks token consumption for display.
type TokenUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens,omitempty"`
	TotalCostUSD             float64 `json:"totalCostUsd,omitempty"`
}
