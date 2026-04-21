package provider

import (
	"encoding/json"
	"fmt"
	"time"
)

// ProviderKind identifies the provider backend.
type ProviderKind string

const (
	Claude ProviderKind = "claude"
	Codex  ProviderKind = "codex"
)

// RuntimeMode is the three-tier approval axis (mirrors t3-code). It controls
// whether the agent prompts for tool use, auto-approves file edits, or
// bypasses approvals entirely. Orthogonal to InteractionMode ("plan" /
// "design" / "discussion"), which shapes *what* the agent does, not how
// much friction is in the way.
type RuntimeMode string

const (
	// RuntimeApprovalRequired prompts the user for every tool use. Safest
	// default; maps to Claude permission mode "default" and Codex approval
	// policy "untrusted" with a read-only sandbox.
	RuntimeApprovalRequired RuntimeMode = "approval-required"

	// RuntimeAutoAcceptEdits auto-approves file edits inside the workspace
	// but still prompts for shell commands and other escalation. Maps to
	// Claude "acceptEdits" and Codex "on-request" with a workspace-write
	// sandbox.
	RuntimeAutoAcceptEdits RuntimeMode = "auto-accept-edits"

	// RuntimeFullAccess bypasses approvals entirely. Maps to Claude
	// "bypassPermissions" and Codex "never" with a danger-full-access
	// sandbox. This is the agent-overflow default — chosen deliberately
	// over safer defaults because our target user is an agent operator
	// who explicitly wants frictionless runs and will opt in to the
	// stricter tiers on a per-thread basis.
	RuntimeFullAccess RuntimeMode = "full-access"
)

// AllRuntimeModes is the canonical list. CHECK constraints, frontend
// pickers, and migration fallbacks all reference it — keep in sync with the
// const block above.
var AllRuntimeModes = []RuntimeMode{
	RuntimeApprovalRequired,
	RuntimeAutoAcceptEdits,
	RuntimeFullAccess,
}

// DefaultRuntimeMode is the seed value for new threads and for the global
// settings default. Intentionally frictionless — see RuntimeFullAccess.
const DefaultRuntimeMode = RuntimeFullAccess

// NormalizeRuntimeMode returns the input if it's a known mode; otherwise
// falls back to DefaultRuntimeMode. Callers pass arbitrary strings coming
// from the wire or an older DB row; this is the chokepoint that keeps
// unknown values out of the session-config mapping.
func NormalizeRuntimeMode(mode string) RuntimeMode {
	switch RuntimeMode(mode) {
	case RuntimeApprovalRequired, RuntimeAutoAcceptEdits, RuntimeFullAccess:
		return RuntimeMode(mode)
	default:
		return DefaultRuntimeMode
	}
}

// ClaudePermissionFlags maps a RuntimeMode to the raw CLI flag sequence the
// Claude CLI expects. Returning a []string (rather than a single string) lets
// us represent boolean-style flags that take no value, most notably
// `--dangerously-skip-permissions` for full-access.
//
//   - RuntimeApprovalRequired: no flags (CLI's built-in default prompts for
//     each tool use).
//   - RuntimeAutoAcceptEdits:  `--permission-mode acceptEdits`.
//   - RuntimeFullAccess:       `--dangerously-skip-permissions` (canonical
//     name of the bypass flag — `--permission-mode bypassPermissions` is an
//     alias but the skip flag is what the current CLI docs surface).
func ClaudePermissionFlags(mode RuntimeMode) []string {
	switch mode {
	case RuntimeAutoAcceptEdits:
		return []string{"--permission-mode", "acceptEdits"}
	case RuntimeFullAccess:
		return []string{"--dangerously-skip-permissions"}
	case RuntimeApprovalRequired:
		fallthrough
	default:
		// Claude CLI without a permission flag is its built-in default
		// mode — prompts for every tool. Returning nil signals the caller
		// to pass no permission flag.
		return nil
	}
}

// CodexApprovalPolicy maps a RuntimeMode to Codex's approval_policy field.
func CodexApprovalPolicy(mode RuntimeMode) string {
	switch mode {
	case RuntimeAutoAcceptEdits:
		return "on-request"
	case RuntimeFullAccess:
		return "never"
	case RuntimeApprovalRequired:
		fallthrough
	default:
		return "untrusted"
	}
}

// CodexSandbox maps a RuntimeMode to Codex's sandbox_mode field.
func CodexSandbox(mode RuntimeMode) string {
	switch mode {
	case RuntimeAutoAcceptEdits:
		return "workspace-write"
	case RuntimeFullAccess:
		return "danger-full-access"
	case RuntimeApprovalRequired:
		fallthrough
	default:
		return "read-only"
	}
}

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

	// Inline/system events that do not render as timeline rows.
	EventCompactBoundary EventKind = "compact_boundary"
	EventRateLimits      EventKind = "rate_limits"
	EventModelRerouted   EventKind = "model_rerouted"
	EventThreadRenamed   EventKind = "thread_renamed"
	EventContentBlockStart EventKind = "content_block_start"
	EventContentBlockStop  EventKind = "content_block_stop"

	// EventBackgroundTaskTerminal is the additive Claude-only task-lifecycle
	// terminal signal, emitted when `system/task_updated` lands with a
	// terminal `patch.status` OR when a TaskOutput `tool_use_result.task`
	// carries a terminal status. It NEVER replaces the tool-lifecycle
	// `EventToolComplete` for the original backgrounded tool_use_id —
	// it layers a richer sibling `tool_completion` row on top. See
	// docs/architecture/turn-lifecycle.md §Task lifecycle and
	// docs/architecture/invariants.md invariant 20.
	EventBackgroundTaskTerminal EventKind = "tool.background_task_terminal"

	// EventSubagentNotification is a reserved Codex-only event kind for
	// future `<subagent_notification>` surfacing. Not currently emitted
	// by any parser — reserved so the frontend can opt into handling it
	// ahead of the emission being wired. See
	// docs/archive/turn-lifecycle-refactor-plan.md WT-codex-parser.
	EventSubagentNotification EventKind = "subagent_notification"

	// Heavy events — persisted to SQLite, meta emitted to frontend.
	EventDiff          EventKind = "diff"
	EventCommandOutput EventKind = "command_output"
	EventThinking      EventKind = "thinking"
	EventProposedPlan  EventKind = "proposed_plan"
)

// AllEventKinds is the canonical list of EventKind values. Triage and the
// frontend router MUST handle every entry here; the exhaustiveness tests
// enforce this. Keep in sync with the const block above — a new kind that is
// not listed here (or any listed kind without a handler case) is the kind of
// silent drop this slice exists to prevent.
var AllEventKinds = []EventKind{
	EventInit,
	EventTextDelta,
	EventToolStart,
	EventToolComplete,
	EventTurnStart,
	EventTurnComplete,
	EventApprovalRequest,
	EventApprovalResolved,
	EventSessionStatus,
	EventTokenUsage,
	EventError,
	EventCompactBoundary,
	EventRateLimits,
	EventModelRerouted,
	EventThreadRenamed,
	EventContentBlockStart,
	EventContentBlockStop,
	EventBackgroundTaskTerminal,
	EventSubagentNotification,
	EventDiff,
	EventCommandOutput,
	EventThinking,
	EventProposedPlan,
}

// ItemKind for persisted items in the database.
type ItemKind string

const (
	ItemUserText       ItemKind = "user_text"
	ItemAssistantText  ItemKind = "assistant_text"
	ItemThinking       ItemKind = "thinking"
	ItemToolCall       ItemKind = "tool_call"
	ItemToolCompletion ItemKind = "tool_completion"
	ItemError          ItemKind = "error"
	ItemCompaction     ItemKind = "compaction"
)

// ProviderEvent is the normalized event emitted by both provider protocols.
// The triage layer classifies these and routes them.
type ProviderEvent struct {
	Kind      EventKind       `json:"kind"`
	ThreadID  string          `json:"threadId"`
	TurnID    string          `json:"turnId,omitempty"`
	TurnIndex int             `json:"turnIndex,omitempty"`
	ItemID    string          `json:"itemId,omitempty"`
	ItemType  string          `json:"itemType,omitempty"`
	Content   string          `json:"content,omitempty"`
	Role      string          `json:"role,omitempty"`
	Meta      json.RawMessage `json:"meta,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
	Replace   bool            `json:"replace,omitempty"` // when true, triage upserts instead of inserting
	// ParentToolUseID links a subagent-emitted event to its parent Task-tool
	// use. Claude surfaces this on assistant messages when the message is
	// produced inside a Task (Agent) tool call. Empty for top-level events.
	ParentToolUseID string          `json:"parentToolUseId,omitempty"`
	Raw             json.RawMessage `json:"-"`
}

// ApprovalRequest is sent when a provider needs user permission.
type ApprovalRequest struct {
	RequestID   string          `json:"requestId"`
	ThreadID    string          `json:"threadId"`
	TurnID      string          `json:"turnId,omitempty"`
	ToolUseID   string          `json:"toolUseId,omitempty"`
	ToolName    string          `json:"toolName"`
	Description string          `json:"description"`
	Input       json.RawMessage `json:"input"`
	Title       string          `json:"title"`
	// Structured approval fields.
	Kind        string              `json:"kind,omitempty"`        // "command"|"file-read"|"file-change"|"user-input"|"permission"|"mcp-elicitation"
	Questions   []UserInputQuestion `json:"questions,omitempty"`   // populated for user-input kind
	Permissions *PermissionProfile  `json:"permissions,omitempty"` // populated for permission kind
	// Elicitation is populated for the mcp-elicitation kind. Carries the high-
	// level mode discriminator and the shape the frontend needs to render the
	// dialog. The schema for form mode is passed through as raw JSON — the
	// frontend owns its interpretation so this package doesn't have to mirror
	// the full Codex elicitation schema taxonomy in Go types.
	Elicitation *ElicitationRequest `json:"elicitation,omitempty"`
	// PermissionSuggestions carries the Claude SDK's optional `permission_suggestions`
	// array from the CanUseTool control_request. The payload is a JSON array of
	// PermissionUpdate objects; the shape is provider-specific so it flows
	// through the pipeline as opaque JSON for the frontend to interpret.
	PermissionSuggestions json.RawMessage `json:"permissionSuggestions,omitempty"`
}

// ApprovalEvent is the frontend-facing channel payload for approval overlay
// changes. The request ID remains the provider-native identifier because the
// response binding routes back through it unchanged.
type ApprovalEvent struct {
	Action    string           `json:"action"` // "request" | "resolve"
	ThreadID  string           `json:"threadId,omitempty"`
	Request   *ApprovalRequest `json:"request,omitempty"`
	RequestID string           `json:"requestId,omitempty"`
	Decision  string           `json:"decision,omitempty"` // approved|declined|amended|timeout|lost
}

// UsageEvent is the frontend-facing channel payload for the context-window
// meter. `usage` updates the ring; `reset` clears it after compaction;
// `rate_limits` carries a rate-limits snapshot folded onto the same channel
// (the meter popover surfaces this alongside token usage per the chat-rewrite
// spec — see docs/architecture/chat-rewrite.md "Channels").
type UsageEvent struct {
	Action         string              `json:"action"` // "usage" | "reset" | "rate_limits"
	ThreadID       string              `json:"threadId"`
	UsedTokens     int                 `json:"usedTokens,omitempty"`
	MaxTokens      int                 `json:"maxTokens,omitempty"`
	ContextPercent float64             `json:"contextPercent,omitempty"`
	RateLimits     *RateLimitsSnapshot `json:"rateLimits,omitempty"`
}

// ProviderStatusEventKind enumerates the persistent provider-status banner
// states defined by docs/architecture/chat-rewrite.md (Channels section).
// The frontend banner dispatches on these values; anything outside the set
// MUST be dropped by the emitter rather than silently rendered.
type ProviderStatusEventKind string

const (
	ProviderStatusBinaryMissing       ProviderStatusEventKind = "binary_missing"
	ProviderStatusUnauthenticated     ProviderStatusEventKind = "unauthenticated"
	ProviderStatusVersionIncompatible ProviderStatusEventKind = "version_incompatible"
	ProviderStatusRateLimitedRetrying ProviderStatusEventKind = "rate_limited_retrying"
	// ProviderStatusTransientRetry covers retrying against a non-rate-limit
	// cause: 5xx, invalid_request, server_error, etc. The banner copy is
	// warning-styled and carries the upstream reason in Message so the
	// user can distinguish "provider is busy" from "credentials expired".
	ProviderStatusTransientRetry ProviderStatusEventKind = "transient_retry"
	ProviderStatusOK             ProviderStatusEventKind = "ok"
)

// ProviderStatusEvent is the frontend-facing channel payload for the
// persistent provider banner. Emitted on `provider:status`. The spec keeps
// this struct deliberately minimal (Kind + Message); Provider and ThreadID
// are added so a multi-provider UI can scope the banner to the right pane
// without round-tripping back to a binding.
type ProviderStatusEvent struct {
	Kind     ProviderStatusEventKind `json:"kind"`
	Message  string                  `json:"message,omitempty"`
	Provider string                  `json:"provider,omitempty"`
	ThreadID string                  `json:"threadId,omitempty"`
}

// ElicitationRequest is the frontend-facing shape for an MCP elicitation
// request, extracted from the raw provider payload. Only one of
// (RequestedSchema) or (URL + ElicitationID) is populated depending on Mode.
// Wire contract lives at:
// /Users/randy/repos/codex-source/codex-rs/app-server-protocol/schema/typescript/v2/McpServerElicitationRequestParams.ts
type ElicitationRequest struct {
	Mode       string `json:"mode"`                 // "form" or "url"
	Message    string `json:"message"`              // human-readable prompt shown to the user
	ServerName string `json:"serverName,omitempty"` // name of the MCP server issuing the request

	// Form mode only.
	RequestedSchema json.RawMessage `json:"requestedSchema,omitempty"`

	// URL mode only.
	URL           string `json:"url,omitempty"`
	ElicitationID string `json:"elicitationId,omitempty"`
}

// ElicitationResolution carries the MCP elicitation response fields.
type ElicitationResolution struct {
	Action  string          `json:"action"`
	Content json.RawMessage `json:"content,omitempty"`
	Meta    json.RawMessage `json:"meta,omitempty"`
}

// ApprovalResponse is sent back to the provider.
type ApprovalResponse struct {
	RequestID   string                     `json:"requestId"`
	Decision    string                     `json:"decision"`              // Codex-native: "accept", "acceptForSession", "decline", "cancel"
	Answers     map[string]UserInputAnswer `json:"answers,omitempty"`     // for user-input responses
	Permissions *PermissionProfile         `json:"permissions,omitempty"` // for granted permissions
	Scope       string                     `json:"scope,omitempty"`       // "turn"|"session" for permissions
	Elicitation *ElicitationResolution     `json:"elicitation,omitempty"` // for MCP elicitation responses
	// UpdatedInput replaces the original tool input when an approval is granted.
	// Only meaningful for allow decisions; ignored on deny. Opaque JSON — the
	// shape mirrors the tool's input schema, which is provider-specific.
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
	// UpdatedPermissions mirrors the Claude SDK's `updatedPermissions` field on
	// allow decisions: a JSON array of PermissionUpdate objects used to broaden
	// or narrow the session's permission scope. Ignored on deny.
	UpdatedPermissions json.RawMessage `json:"updatedPermissions,omitempty"`
}

// NormalizeApprovalDecision converts transport-specific approval response
// values ("allow", "accept", "decline", "deny", etc.) into the persisted item
// decision enum used by the chat rewrite.
func NormalizeApprovalDecision(resp ApprovalResponse) string {
	switch resp.Decision {
	case "allow", "allow_session", "accept", "acceptForSession":
		if len(resp.UpdatedInput) > 0 || len(resp.UpdatedPermissions) > 0 {
			return "amended"
		}
		return "approved"
	case "deny", "decline", "cancel":
		return "declined"
	default:
		return ""
	}
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

// UserInputAnswer stores one or more selected answers for a question.
// It marshals as a string for single-select answers and a string array for
// multi-select answers to match the frontend/Forge contract.
type UserInputAnswer []string

// SingleUserInputAnswer constructs a single-answer response value.
func SingleUserInputAnswer(value string) UserInputAnswer {
	return UserInputAnswer{value}
}

// MarshalJSON emits a bare string for single answers and an array for
// multi-select answers.
func (a UserInputAnswer) MarshalJSON() ([]byte, error) {
	if len(a) == 1 {
		return json.Marshal(a[0])
	}
	return json.Marshal([]string(a))
}

// UnmarshalJSON accepts either a single string or an array of strings.
func (a *UserInputAnswer) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*a = UserInputAnswer{single}
		return nil
	}

	var multiple []string
	if err := json.Unmarshal(data, &multiple); err == nil {
		*a = UserInputAnswer(multiple)
		return nil
	}

	return fmt.Errorf("user input answer must be a string or []string")
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
	// SlashCommands lists the user-configurable slash commands Claude surfaces
	// via `system.init` (from `.claude/commands/` and built-ins). Claude-only —
	// Codex's init payload has no equivalent and leaves this nil.
	SlashCommands []string `json:"slashCommands,omitempty"`
}

// AccountInfo describes the authenticated Claude account surfaced through
// the `system/init` message. Subscription type and token source fields are
// populated only when the CLI includes them; older CLI versions or
// unauthenticated invocations may leave them empty.
type AccountInfo struct {
	SubscriptionType string `json:"subscriptionType,omitempty"`
	TokenSource      string `json:"tokenSource,omitempty"`
	APIProvider      string `json:"apiProvider,omitempty"`
	Model            string `json:"model,omitempty"`
	Version          string `json:"version,omitempty"`
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
