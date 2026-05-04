package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
// bypasses approvals entirely. Provider packages own the wire-level mapping.
// Orthogonal to InteractionMode ("plan" / "design" / "discussion"), which
// shapes *what* the agent does, not how much friction is in the way.
type RuntimeMode string

const (
	// RuntimeApprovalRequired prompts the user for every tool use.
	RuntimeApprovalRequired RuntimeMode = "approval-required"

	// RuntimeAutoAcceptEdits auto-approves file edits inside the workspace
	// but still prompts for shell commands and other escalation.
	RuntimeAutoAcceptEdits RuntimeMode = "auto-accept-edits"

	// RuntimeFullAccess bypasses approvals entirely. This is the
	// agent-overflow default — chosen deliberately over safer defaults
	// because our target user is an agent operator who explicitly wants
	// frictionless runs and will opt in to the stricter tiers on a
	// per-thread basis.
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

// EventKind classifies provider events for triage routing.
type EventKind string

const (
	// Inline events — forwarded directly to frontend via EventsEmit.
	EventInit              EventKind = "init"
	EventTextDelta         EventKind = "text_delta"
	EventToolStart         EventKind = "tool_start"
	EventToolComplete      EventKind = "tool_complete"
	EventTurnStart         EventKind = "turn_start"
	EventTurnComplete      EventKind = "turn_complete"
	EventApprovalRequest   EventKind = "approval_request"
	EventApprovalResolved  EventKind = "approval_resolved"
	EventUserInputRequest  EventKind = "user_input_request"
	EventUserInputResolved EventKind = "user_input_resolved"
	EventSessionStatus     EventKind = "session_status"
	EventTokenUsage        EventKind = "token_usage"
	EventError             EventKind = "error"
	EventTodoUpdate        EventKind = "todo_update"
	EventNotification      EventKind = "notification"

	// EventAPIRetry surfaces transient-retry envelopes from both providers
	// (Claude `system.api_retry` and Codex `error+willRetry:true`). Triage
	// renders them as inline timeline rows (kind `api_retry`) hiding the
	// first three attempts to mirror Claude Code's interactive UI; the row
	// flips to completed on the next forward-progress event for the thread.
	// There is no resolution-counterpart wire event from either SDK; the
	// row is the historical record of the retry attempt itself, not a
	// banner needing later clearing.
	EventAPIRetry EventKind = "api_retry"

	// Inline/system events that do not render as timeline rows.
	EventCompactBoundary   EventKind = "compact_boundary"
	EventRateLimits        EventKind = "rate_limits"
	EventModelRerouted     EventKind = "model_rerouted"
	EventThreadRenamed     EventKind = "thread_renamed"
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
	EventBackgroundTaskTerminal EventKind = "background_task_terminal"

	// EventBackgroundTaskNotification surfaces Claude `system/task_notification`
	// as a non-lifecycle notification row. It may carry a durable output_file
	// path that triage reads into SQLite for later expansion, but it must never
	// mark a task complete or failed by itself.
	EventBackgroundTaskNotification EventKind = "background_task_notification"

	// EventSubagentNotification surfaces Codex's `<subagent_notification>`
	// tag detections (session.go parser → triage handler →
	// provider:subagent_notification on the frontend event bus). Emitted
	// when a detached spawned child has produced a terminal state and
	// Codex injected the notification into the parent's next user turn.
	EventSubagentNotification EventKind = "subagent_notification"

	// EventTerminalInteraction surfaces Codex's
	// `TerminalInteractionNotification` — the wire signal when the model
	// calls `write_stdin` against a backgrounded unified-exec PTY. An
	// empty `Stdin` is the "polling" variant (agent asked to wait
	// without sending input); a non-empty value carries the keystrokes
	// Codex forwarded. Triage persists a lightweight
	// `terminal_interaction` row for the empty case so the timeline can
	// render "Waited for background terminal" inline, mirroring Codex's
	// own TUI (chatwidget.rs:618). The non-empty case persists an
	// "Interacted with background terminal" marker while redacting the
	// stdin bytes from durable item metadata.
	EventTerminalInteraction EventKind = "terminal_interaction"

	// EventUserText is the wire-confirmation envelope for an AO-initiated
	// user message. The triage router uses it to correlate the wire-side
	// echo of a user prompt back to the in-memory pending-send registered
	// by the send path. Phase A introduces only the constant + dispatch
	// stub; emission sites land in later phases.
	EventUserText EventKind = "user_text"

	// Heavy events — persisted to SQLite, meta emitted to frontend.
	EventDiff          EventKind = "diff"
	EventCommandOutput EventKind = "command_output"
	EventThinking      EventKind = "thinking"
	EventProposedPlan  EventKind = "proposed_plan"
)

const (
	TerminalWaitResultRunning = "running"
	TerminalWaitResultExited  = "exited"
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
	EventUserInputRequest,
	EventUserInputResolved,
	EventSessionStatus,
	EventTokenUsage,
	EventError,
	EventTodoUpdate,
	EventNotification,
	EventAPIRetry,
	EventCompactBoundary,
	EventRateLimits,
	EventModelRerouted,
	EventThreadRenamed,
	EventContentBlockStart,
	EventContentBlockStop,
	EventBackgroundTaskTerminal,
	EventBackgroundTaskNotification,
	EventSubagentNotification,
	EventTerminalInteraction,
	EventUserText,
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
	ItemNotification   ItemKind = "notification"
	// ItemAPIRetry is the live-updating retry indicator. Deterministic
	// per-turn id (retry:N where N is turnIndex) so subsequent api_retry
	// events upsert in place. Hidden until the 4th attempt to mirror
	// Claude Code's SystemAPIErrorMessage hidden-until-attempt-4 behavior.
	ItemAPIRetry ItemKind = "api_retry"
	// ItemAPIError is a retry-exhausted assistant API error. Distinguished
	// from ItemError so the renderer can branch on the assistant.error
	// enum value (rate_limit, authentication_failed, billing_error, etc.)
	// for kind-specific actionable copy. Generic ItemError stays for
	// non-API errors and Codex provider errors.
	ItemAPIError ItemKind = "api_error"
	// ItemTerminalInteraction is the minimal "Waited for background
	// terminal" marker persisted when the Codex model polled a
	// backgrounded PTY via an empty-stdin `write_stdin` call. No
	// payload — the kind alone carries the semantic; meta carries
	// process_id for debugging.
	ItemTerminalInteraction ItemKind = "terminal_interaction"
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
	Kind        string             `json:"kind,omitempty"`        // "command"|"file-read"|"file-change"|"permission"|"mcp-elicitation"
	Permissions *PermissionProfile `json:"permissions,omitempty"` // populated for permission kind
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
	Action    string           `json:"action"` // "request" | "resolve" | "fail"
	ThreadID  string           `json:"threadId,omitempty"`
	Request   *ApprovalRequest `json:"request,omitempty"`
	RequestID string           `json:"requestId,omitempty"`
	Decision  string           `json:"decision,omitempty"` // approved|declined|amended|timeout|lost|failed
	Detail    string           `json:"detail,omitempty"`
}

// UserInputRequest is sent when a provider needs structured user input.
// It is deliberately separate from ApprovalRequest: user-input prompts are
// answer collection, not permission grants, and the frontend renders them
// through a different composer flow.
type UserInputRequest struct {
	RequestID string              `json:"requestId"`
	ThreadID  string              `json:"threadId"`
	TurnID    string              `json:"turnId,omitempty"`
	ToolUseID string              `json:"toolUseId,omitempty"`
	ToolName  string              `json:"toolName"`
	Title     string              `json:"title"`
	Questions []UserInputQuestion `json:"questions"`
}

// UserInputEvent is the frontend-facing channel payload for structured
// user-input prompt changes.
type UserInputEvent struct {
	Action    string            `json:"action"` // "request" | "resolve" | "fail"
	ThreadID  string            `json:"threadId,omitempty"`
	Request   *UserInputRequest `json:"request,omitempty"`
	RequestID string            `json:"requestId,omitempty"`
	Decision  string            `json:"decision,omitempty"` // answered|declined|timeout|lost|failed
	Detail    string            `json:"detail,omitempty"`
}

// UsageEvent is the frontend-facing channel payload for the context-window
// meter. `usage` updates the ring; `reset` clears it after compaction;
// `rate_limits` carries a rate-limits snapshot folded onto the same channel
// for future UI, but does not change the context ring.
type UsageEvent struct {
	Action                string              `json:"action"` // "usage" | "reset" | "rate_limits"
	ThreadID              string              `json:"threadId"`
	UsedTokens            int                 `json:"usedTokens,omitempty"`
	MaxTokens             int                 `json:"maxTokens,omitempty"`
	ContextPercent        float64             `json:"contextPercent,omitempty"`
	AutoCompactPercent    int                 `json:"autoCompactPercent,omitempty"`
	AutoCompactTokenLimit int                 `json:"autoCompactTokenLimit,omitempty"`
	RateLimits            *RateLimitsSnapshot `json:"rateLimits,omitempty"`
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
	RequestID   string                 `json:"requestId"`
	Decision    string                 `json:"decision"`              // Codex-native: "accept", "acceptForSession", "decline", "cancel"
	Permissions *PermissionProfile     `json:"permissions,omitempty"` // for granted permissions
	Scope       string                 `json:"scope,omitempty"`       // "turn"|"session" for permissions
	Elicitation *ElicitationResolution `json:"elicitation,omitempty"` // for MCP elicitation responses
	// UpdatedInput replaces the original tool input when an approval is granted.
	// Only meaningful for allow decisions; ignored on deny. Opaque JSON — the
	// shape mirrors the tool's input schema, which is provider-specific.
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
	// UpdatedPermissions mirrors the Claude SDK's `updatedPermissions` field on
	// allow decisions: a JSON array of PermissionUpdate objects used to broaden
	// or narrow the session's permission scope. Ignored on deny.
	UpdatedPermissions json.RawMessage `json:"updatedPermissions,omitempty"`
}

// UserInputResponse is sent back to the provider for a structured user-input
// request.
type UserInputResponse struct {
	RequestID string                     `json:"requestId"`
	Decision  string                     `json:"decision"` // accept; empty is treated as accept
	Answers   map[string]UserInputAnswer `json:"answers,omitempty"`
}

// ErrStaleInteractiveRequest marks provider errors for approval/user-input
// callbacks that no longer have a live provider request behind them.
var ErrStaleInteractiveRequest = errors.New("provider: stale interactive request")

// ErrInvalidUserInputDecision marks a user-input response that uses approval
// decision vocabulary. Structured user input is answer collection; turn
// cancellation goes through Interrupt, not a user-input JSON-RPC result.
var ErrInvalidUserInputDecision = errors.New("provider: invalid user-input decision")

func NormalizeUserInputDecision(decision string) (string, error) {
	switch decision {
	case "", "accept", "allow":
		return "accept", nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidUserInputDecision, decision)
	}
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
// Preview, when non-empty, is markdown-rendered alongside the option list as
// a side-by-side mockup/code comparison aid. Preview is single-select only;
// Claude Code's tool spec ignores it on multi-select questions.
type UserInputQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Preview     string `json:"preview,omitempty"`
}

// UserInputQuestion represents a question in a structured user-input request.
type UserInputQuestion struct {
	ID          string                    `json:"id"`
	Header      string                    `json:"header"`
	Question    string                    `json:"question"`
	Options     []UserInputQuestionOption `json:"options,omitempty"`
	MultiSelect bool                      `json:"multiSelect,omitempty"`
}

func NormalizeUserInputQuestions(questions []UserInputQuestion) []UserInputQuestion {
	normalized := make([]UserInputQuestion, 0, len(questions))
	seen := make(map[string]int, len(questions))
	for i, question := range questions {
		question.ID = normalizeUserInputQuestionID(question, i, seen)
		if strings.TrimSpace(question.Header) == "" {
			question.Header = fmt.Sprintf("Question %d", i+1)
		}
		normalized = append(normalized, question)
	}
	return normalized
}

func normalizeUserInputQuestionID(question UserInputQuestion, index int, seen map[string]int) string {
	candidates := []string{question.ID, question.Header, question.Question, fmt.Sprintf("q-%d", index)}
	id := fmt.Sprintf("q-%d", index)
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" || isReservedUserInputQuestionID(trimmed) {
			continue
		}
		id = trimmed
		break
	}
	count := seen[id]
	seen[id] = count + 1
	if count == 0 {
		return id
	}
	return fmt.Sprintf("%s-%d", id, count+1)
}

func isReservedUserInputQuestionID(id string) bool {
	switch id {
	case "__proto__", "prototype", "constructor":
		return true
	default:
		return false
	}
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

// TokenUsage tracks turn token/cost accounting.
type TokenUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens,omitempty"`
	TotalCostUSD             float64 `json:"totalCostUsd,omitempty"`
}

// ContextWindow describes provider context window usage.
type ContextWindow struct {
	UsedTokens            int     `json:"usedTokens"`
	MaxTokens             int     `json:"maxTokens,omitempty"`
	UsedPercentage        float64 `json:"usedPercentage,omitempty"`
	AutoCompactPercent    int     `json:"autoCompactPercent,omitempty"`
	AutoCompactTokenLimit int     `json:"autoCompactTokenLimit,omitempty"`
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
