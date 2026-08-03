package provider

import (
	"encoding/json"
	"time"
)

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

	// EventTaskCreate and EventTaskUpdate are per-task CRUD events
	// emitted by the Claude Code 2.1.150+ Task* parser path. Triage
	// accumulates them into a per-thread task mirror and projects each
	// mutation as an EventTodoUpdate snapshot for the activity rail.
	// The pair replaces the parser-side snapshot that previously lived
	// on the per-Session Parser — this placement survives session
	// resume because triage's Router outlives any individual Parser.
	EventTaskCreate   EventKind = "task_create"
	EventTaskUpdate   EventKind = "task_update"
	EventNotification EventKind = "notification"

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
	EventCompactBoundary EventKind = "compact_boundary"
	EventRateLimits      EventKind = "rate_limits"
	EventModelRerouted   EventKind = "model_rerouted"
	// EventModelFallback reports a provider safety/classifier fallback where
	// the configured model remains the user's preference but the live session
	// is now running another model. Unlike EventModelRerouted, triage must not
	// overwrite threads.model; it persists the warning and projects the
	// effective model as session-scoped live state.
	EventModelFallback     EventKind = "model_fallback"
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

	// EventSessionWakeup is the Claude-only signal that the CLI harness
	// holds (or dropped) an in-process wakeup timer for the session: the
	// ScheduleWakeup tool's ack carries the absolute fire time, and the
	// harness later injects the stored prompt as a fresh user turn when
	// it fires. There is NO task lifecycle behind it — no task_started,
	// no task_updated terminal — so the pending timer is invisible to
	// the background-tool-call machinery. Triage records the fire time
	// per thread and the idle-session reaper refuses to close a session
	// whose wakeup is still in the future: closing the process would
	// silently kill the timer. A meta with ScheduledForUnixMs <= 0
	// clears the pending state (the `{stop:true}` ack).
	EventSessionWakeup EventKind = "session_wakeup"

	// EventSubagentNotification surfaces Codex's `<subagent_notification>`
	// tag detections (session.go parser → triage handler →
	// provider:subagent_notification on the frontend event bus). Emitted
	// when a detached spawned child has produced a terminal state and
	// Codex injected the notification into the parent's next user turn.
	EventSubagentNotification EventKind = "subagent_notification"

	// EventSubagentStatus is an internal Codex child-thread lifecycle signal.
	// It marks spawned child work inactive for live background-task projection.
	// Parent transcript completion is owned by wait_agent completions or
	// Codex's injected <subagent_notification> fragments.
	EventSubagentStatus EventKind = "subagent_status"

	// EventCodexExecResult is a Codex-only internal signal derived from the
	// raw `exec_command` function-call output. It records whether the model
	// was told the process exited during the initial wait or yielded with a
	// resumable session id. Triage uses it only to enrich live process state;
	// typed item/completed owns command history.
	EventCodexExecResult EventKind = "codex_exec_result"

	// EventTerminalInteraction surfaces Codex's
	// `TerminalInteractionNotification` — the wire signal when the model
	// calls `write_stdin` against a backgrounded unified-exec PTY. An
	// empty `Stdin` is the "polling" variant (agent asked to wait
	// without sending input); a non-empty value carries the keystrokes
	// Codex forwarded. Triage persists/reuses a lightweight
	// `terminal_interaction` row for the empty case so the timeline can show
	// the live wait and later flip it to the same "waited" history marker
	// Codex's own TUI flushes from its wait streak. The non-empty case first
	// flushes any active wait for that process, then persists an "Interacted
	// with background terminal" marker while redacting the stdin bytes from
	// durable item metadata.
	EventTerminalInteraction EventKind = "terminal_interaction"

	// EventUserText is the wire-confirmation envelope for an AO-initiated
	// user message. Provider parsers and triage use it to correlate the
	// wire-side echo of a user prompt back to the in-memory pending-send
	// registered by the send path.
	EventUserText EventKind = "user_text"

	// EventCommandLifecycle is Claude's delivery ack for a user message
	// AO wrote to stdin, keyed by the uuid AO stamped on the envelope
	// (`command_uuid`). It reports queued → started → completed, or
	// cancelled for a message the CLI will never deliver. Live UI state
	// only — the durable record of the message is its user_text row, and
	// the wire echo remains the row's confirmation signal. See
	// docs/references/claude-wire.md §command_lifecycle.
	EventCommandLifecycle EventKind = "command_lifecycle"

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
	EventUserInputRequest,
	EventUserInputResolved,
	EventSessionStatus,
	EventTokenUsage,
	EventError,
	EventTodoUpdate,
	EventTaskCreate,
	EventTaskUpdate,
	EventNotification,
	EventAPIRetry,
	EventCompactBoundary,
	EventRateLimits,
	EventModelRerouted,
	EventModelFallback,
	EventThreadRenamed,
	EventContentBlockStart,
	EventContentBlockStop,
	EventBackgroundTaskTerminal,
	EventBackgroundTaskNotification,
	EventSessionWakeup,
	EventSubagentNotification,
	EventSubagentStatus,
	EventCodexExecResult,
	EventTerminalInteraction,
	EventUserText,
	EventCommandLifecycle,
	EventDiff,
	EventCommandOutput,
	EventThinking,
	EventProposedPlan,
}

// ProviderEvent is the normalized event emitted by both provider protocols.
// The triage layer classifies these and routes them.
type ProviderEvent struct {
	Kind      EventKind `json:"kind"`
	ThreadID  string    `json:"threadId"`
	TurnID    string    `json:"turnId,omitempty"`
	TurnIndex int       `json:"turnIndex,omitempty"`
	ItemID    string    `json:"itemId,omitempty"`
	ItemType  string    `json:"itemType,omitempty"`
	Content   string    `json:"content,omitempty"`
	// ContentPresent distinguishes authoritative empty content from absent
	// content on completion-style events.
	ContentPresent bool            `json:"contentPresent,omitempty"`
	Role           string          `json:"role,omitempty"`
	Meta           json.RawMessage `json:"meta,omitempty"`
	// StructuredOutput carries the provider's structured payload on
	// EventTurnComplete. Nil means the turn produced no usable payload.
	StructuredOutput json.RawMessage `json:"structuredOutput,omitempty"`
	Timestamp        time.Time       `json:"timestamp"`
	Replace          bool            `json:"replace,omitempty"` // when true, triage upserts instead of inserting
	// ParentToolUseID links a subagent-emitted event to its parent Task-tool
	// use. Claude surfaces this on assistant messages when the message is
	// produced inside a Task (Agent) tool call. Empty for top-level events.
	ParentToolUseID string           `json:"parentToolUseId,omitempty"`
	Raw             json.RawMessage  `json:"-"`
	TurnComplete    TurnCompleteMeta `json:"-"`
}

// CompactionReasoningScope is the reserved ParentToolUseID the claudetui
// provider stamps on the compaction summarizer's live thinking deltas. The
// summarizer's /v1/messages turn is otherwise suppressed (it must not surface as
// a phantom agent turn), but its reasoning streams live so the frontend can show
// a "compact" tail above the "Context compacted" divider. Triage keys off this
// sentinel to route those deltas to a top-level `compaction_reasoning` streaming
// row instead of nesting them under an Agent card — the shared parser is
// untouched (it just sees a parented thinking stream). The bracketed form can't
// collide with a real Claude tool_use id (uuid / `toolu_…`).
const CompactionReasoningScope = "__ao_compaction_reasoning__"

// TurnCompleteMeta is the typed payload for EventTurnComplete. Turn
// completion has several semantic sources (provider wire result, soft
// round-close, synthetic truncation); keeping those as distinct Go types
// prevents new producers from smuggling another ad hoc JSON shape through
// ProviderEvent.Meta.
type TurnCompleteMeta interface {
	isTurnCompleteMeta()
}

// WireTurnCompleteMeta represents a provider-reported turn boundary. It
// carries the durable payload triage may persist or forward to the frontend.
//
// Usage is the turn's aggregate per-turn delta (see TokenUsage docs);
// ModelUsage is the same delta split per model where the provider can
// attribute it. When both are set, Usage equals the sum of ModelUsage —
// triage persists Usage on the turn row and ModelUsage as usage-ledger
// rows.
type WireTurnCompleteMeta struct {
	StopReason         string
	AssistantMessageID string
	Usage              *TokenUsage
	ModelUsage         []ModelTokenUsage
	Aborted            bool
	ErrorMessage       string
	// FastMode is the provider's fast-mode report for the turn that just
	// ended. Nil when the envelope carried no fast-mode keys — see
	// FastModeStatus for why absence is silence, not "off". Live state,
	// not turn accounting: triage forwards it to the frontend and does
	// not persist it.
	FastMode *FastModeStatus
}

func (*WireTurnCompleteMeta) isTurnCompleteMeta() {}

// SoftRoundCloseMeta represents Claude's message_delta stop_reason path:
// the parent model has stopped emitting for this wire round, but the trailing
// result envelope may still arrive later with cumulative usage.
type SoftRoundCloseMeta struct {
	StopReason         string
	AssistantMessageID string
}

func (*SoftRoundCloseMeta) isTurnCompleteMeta() {}

// TruncatedTurnCompleteMeta represents an app/triage-synthesized close when
// the provider did not produce a clean wire turn boundary.
type TruncatedTurnCompleteMeta struct {
	ErrorMessage string
	Synthetic    bool
}

func (*TruncatedTurnCompleteMeta) isTurnCompleteMeta() {}

// SessionWakeupMeta is the typed payload for EventSessionWakeup.
// ScheduledForUnixMs is the absolute wall-clock fire time (epoch ms, the
// provider clock — same host as ours) from the ScheduleWakeup ack's
// `scheduledFor`; a value <= 0 clears the thread's pending-wakeup state
// (the `{stop:true}` ack reports `scheduledFor: 0, stopped: true`).
type SessionWakeupMeta struct {
	ScheduledForUnixMs int64 `json:"scheduledForUnixMs"`
}

// CommandLifecycleState is one of the four delivery states Claude's
// `command_lifecycle` frames report for a stdin user message. The set is
// closed by the parser: an unrecognised state is dropped rather than
// forwarded, so no consumer has to carry an "unknown" branch.
type CommandLifecycleState string

const (
	// CommandQueued — the CLI accepted the envelope and holds it. Emitted
	// immediately on write, before the message reaches the model.
	CommandQueued CommandLifecycleState = "queued"
	// CommandStarted — the message reached the model. Arriving BEFORE the
	// running turn's `result` means it was drained mid-turn and redirected
	// that turn; after means it began a fresh turn.
	CommandStarted CommandLifecycleState = "started"
	// CommandCompleted — the turn the message drove has finished.
	CommandCompleted CommandLifecycleState = "completed"
	// CommandCancelled — the message will NEVER be delivered.
	CommandCancelled CommandLifecycleState = "cancelled"
)

// CommandLifecycleMeta is the typed payload for EventCommandLifecycle.
// CommandUUID is the client-minted uuid AO put on the outbound envelope's
// top-level `uuid` field — the same value that lands on the user_text
// row's `provider_item_id` meta.
type CommandLifecycleMeta struct {
	CommandUUID string                `json:"commandUuid"`
	State       CommandLifecycleState `json:"state"`
}

// TaskCreateMeta is the typed payload for EventTaskCreate.
// Triage decodes it from ProviderEvent.Meta via json.Unmarshal.
type TaskCreateMeta struct {
	TaskID  string `json:"taskId"`
	Subject string `json:"subject"`
}

// TaskUpdateMeta is the typed payload for EventTaskUpdate.
// Empty Status/Subject/Owner means "no change" — triage applies
// partial mutation with the same precedence the parser's mutateTask
// used. Deleted is a separate boolean so the partial-update rule
// stays orthogonal to the terminal delete signal.
type TaskUpdateMeta struct {
	TaskID  string `json:"taskId"`
	Status  string `json:"status,omitempty"`
	Subject string `json:"subject,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}
