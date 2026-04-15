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

	// New inline events for feature parity.
	EventToolProgress    EventKind = "tool_progress"
	EventCompactBoundary EventKind = "compact_boundary"
	EventRateLimits      EventKind = "rate_limits"
	EventModelRerouted   EventKind = "model_rerouted"
	EventThreadRenamed   EventKind = "thread_renamed"

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
	Replace   bool            `json:"replace,omitempty"` // when true, triage upserts instead of inserting
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
	// Structured approval fields.
	Kind        string              `json:"kind,omitempty"`        // "command"|"file-read"|"file-change"|"user-input"|"permission"
	Questions   []UserInputQuestion `json:"questions,omitempty"`   // populated for user-input kind
	Permissions *PermissionProfile  `json:"permissions,omitempty"` // populated for permission kind
}

// ApprovalResponse is sent back to the provider.
type ApprovalResponse struct {
	RequestID   string             `json:"requestId"`
	Decision    string             `json:"decision"` // "allow", "deny", "allow_session"
	Answers     map[string]string  `json:"answers,omitempty"`     // for user-input responses
	Permissions *PermissionProfile `json:"permissions,omitempty"` // for granted permissions
	Scope       string             `json:"scope,omitempty"`       // "turn"|"session" for permissions
}

// UserInputQuestionOption is a selectable option in a user-input question.
type UserInputQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// UserInputQuestion represents a question in a structured user-input approval.
type UserInputQuestion struct {
	ID          string                    `json:"id"`
	Header      string                    `json:"header"`
	Question    string                    `json:"question"`
	Options     []UserInputQuestionOption `json:"options,omitempty"`
	MultiSelect bool                      `json:"multiSelect,omitempty"`
}

// PermissionProfile describes requested or granted permissions.
type PermissionProfile struct {
	Network    *NetworkPermissions    `json:"network,omitempty"`
	FileSystem *FileSystemPermissions `json:"fileSystem,omitempty"`
}

// NetworkPermissions controls network access.
type NetworkPermissions struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// FileSystemPermissions controls filesystem access.
type FileSystemPermissions struct {
	Read  []string `json:"read,omitempty"`
	Write []string `json:"write,omitempty"`
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

// ContextWindow describes provider context window usage.
type ContextWindow struct {
	UsedTokens     int     `json:"usedTokens"`
	MaxTokens      int     `json:"maxTokens,omitempty"`
	UsedPercentage float64 `json:"usedPercentage,omitempty"`
	TotalProcessed int     `json:"totalProcessed,omitempty"`
}

// RateLimitEntry represents a single rate limit window.
type RateLimitEntry struct {
	LimitID     string  `json:"limitId"`
	LimitName   string  `json:"limitName"`
	UsedPercent float64 `json:"usedPercent"`
	WindowMins  int     `json:"windowMins"`
	ResetsAt    int64   `json:"resetsAt"`
}

// RateLimitsSnapshot is a point-in-time view of all rate limits.
type RateLimitsSnapshot struct {
	Provider  string           `json:"provider"`
	Limits    []RateLimitEntry `json:"limits"`
	UpdatedAt int64            `json:"updatedAt"`
}
