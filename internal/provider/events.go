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

	// EventCompactionStatus is the live signal that the provider is
	// summarizing the thread's context right now — a window that can run
	// for minutes with no other wire traffic. Claude emits it from
	// `system/status` (`status:"compacting"` opens, a `compact_result`
	// frame closes, success or failure alike); Codex opens it from
	// `item/started` for a `contextCompaction` item, whose completion
	// already arrives as EventCompactBoundary. Both manual and
	// auto-triggered compaction emit it. Triage keeps it as live-only
	// per-thread state (`provider:compacting`) — session state, never
	// history — with the boundary, turn completion, and thread cleanup
	// as defensive clears because a failed Codex compaction never
	// completes its item. Meta is CompactionStatusMeta.
	EventCompactionStatus EventKind = "compaction_status"

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

	// EventSubagentProgress is the provider-neutral live-progress tick for
	// ONE running subagent: the launch tool_use (ItemID) it belongs to, plus
	// the counters a compact agent card shows while the agent runs (tool
	// count, token spend, elapsed, current activity line). Claude emits it
	// from `system/task_progress` (local_agent tasks only — never a forked
	// skill); Codex from a child thread's `thread/tokenUsage/updated`,
	// scoped to the spawn_agent tool_use and kept OFF the parent's own
	// context meter. Live UI state, never a timeline row: triage holds the
	// latest tick per launch in memory and persists only the FINAL numbers
	// onto the launch row at its terminal. Meta is SubagentProgressMeta.
	EventSubagentProgress EventKind = "subagent_progress"

	// EventSubagentBackgrounded is the wire-typed signal that a FOREGROUND
	// task was moved to the background mid-flight (Claude
	// `system/task_updated` with the non-terminal `patch.is_backgrounded`,
	// the reply to Ctrl+B or AO's `background_tasks` control_request). An
	// agent launched async never emits it. It is the only statement that
	// ordinary sidechain forwarding stopped at this point. New Claude
	// sessions continue the transcript through `transcript_mirror`; triage
	// stamps the launch row with the cut timestamp as detached-state
	// provenance. Meta is SubagentBackgroundedMeta.
	EventSubagentBackgrounded EventKind = "subagent_backgrounded"

	// EventBackgroundTasksChanged is Claude's LEVEL signal for the set of
	// live background tasks (`system/background_tasks_changed`): the whole
	// set, REPLACE semantics, emitted on every membership change. Foreground
	// agents and forked skills are not in it. Consumers swap their set for
	// the payload rather than pairing start/stop edges, so a missed bookend
	// cannot wedge a stale running indicator. Meta is
	// BackgroundTasksChangedMeta.
	EventBackgroundTasksChanged EventKind = "background_tasks_changed"

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

	// EventCommandsChanged is Claude's spontaneous `system/commands_changed`
	// push: the CLI re-announces the FULL command list after mid-session
	// skill discovery or a `reload_plugins`. Its contract is REPLACE, not
	// merge — a command that dropped off the list is gone. Live session
	// state; nothing persists.
	EventCommandsChanged EventKind = "commands_changed"

	// EventCommandResult carries the output of a slash command the provider
	// CLI executed itself — no API call, no model output. Claude delivers it
	// as an `assistant` envelope whose `message.model` is the CLI's
	// `<synthetic>` sentinel (see claude-wire.md §"Slash commands"); the
	// parser routes it here so it can never render as an assistant bubble.
	// Triage persists it as a `command_result` row.
	EventCommandResult EventKind = "command_result"

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
	EventCompactionStatus,
	EventSubagentNotification,
	EventSubagentStatus,
	EventCodexExecResult,
	EventSubagentProgress,
	EventSubagentBackgrounded,
	EventBackgroundTasksChanged,
	EventTerminalInteraction,
	EventUserText,
	EventCommandLifecycle,
	EventCommandsChanged,
	EventCommandResult,
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
	// Failure is the provider adapter's normalized error disposition. Raw wire
	// fields remain in Meta/Raw for rendering and forensics; control-flow
	// consumers must use this typed value rather than decoding provider JSON.
	Failure *FailureMeta `json:"-"`
}

// MetaTranscriptMirroredKey marks a subagent launch whose missing sidechain
// is already being projected from the provider's live transcript mirror.
// Terminal recovery may retain an on-disk fallback for older sessions but
// must not parse the same transcript again when this marker is present.
const MetaTranscriptMirroredKey = "transcript_mirrored"

// MetaTranscriptSnapshotKey marks content reconstructed from a complete
// transcript row rather than received as provider stream deltas. Consumers
// must project it at its current state instead of replaying a fake typing or
// thinking stream after the transcript has already advanced beyond it.
const MetaTranscriptSnapshotKey = "transcript_snapshot"

// MetaSubagentLaunchKey marks a tool start whose input creates a new agent.
// Provider adapters know this from their typed wire shape; triage must not
// maintain a tool-name list to recover it.
const MetaSubagentLaunchKey = "subagent_launch"

// MetaSubagentOpeningPromptKey marks the first user-role row in one
// subagent's transcript. Its stable row identity is the launch scope, not the
// transcript uuid: the launch input lets the live path render the prompt
// before the child produces output, while the later transcript row supplies
// the provider uuid without moving that row.
const MetaSubagentOpeningPromptKey = "subagent_opening_prompt"

// MetaSubagentPromptProvisionalKey marks an opening prompt copied from the
// launch input before its transcript row arrives. The first scoped user echo
// replaces this snapshot in place and clears the marker.
const MetaSubagentPromptProvisionalKey = "subagent_prompt_provisional"

// MetaSubagentResumePromptKey marks the user-role row that opens a RESUMED
// round of one agent (Claude §E6): the `prompt` the rebind
// `system/task_started` carries, which is the resuming tool's own message
// text. It is not the agent's opening prompt — that row belongs to the
// original launch — so the two are marked apart, but both are launch-scoped
// prompt rows whose provider uuid arrives later from the transcript.
const MetaSubagentResumePromptKey = "subagent_resume_prompt"

// MetaResumeCarrierIDKey names the resume CARRIER a resume prompt row was
// minted for. The row itself is written under the agent's transcript ROOT
// (where every sidechain row of every round stays parented), so the carrier
// id is the only record of which round the prompt opened.
const MetaResumeCarrierIDKey = "resume_carrier_id"

// MetaTranscriptRootIDKey marks a resume CARRIER row with the id of the
// launch that owns the agent's transcript. Only a carrier carries it, which
// makes it the one structural test for "this row is a lifecycle row, not a
// transcript root".
const MetaTranscriptRootIDKey = "transcript_root_id"

// SubagentOpeningPromptItemID is shared by live triage and session import so
// refresh converges on the prompt row created when the agent launched.
func SubagentOpeningPromptItemID(scope string) string {
	return "user:subagent-prompt:" + scope
}

type FailureClass string

const (
	FailureTransient           FailureClass = "transient"
	FailureTransientAfterRetry FailureClass = "transient-after-retry"
	FailureFatal               FailureClass = "fatal"
)

type FailureReason string

const (
	// FailureReasonUsageLimit means the provider refused the turn because an
	// account usage allowance is exhausted. Reset windows remain advisory:
	// consumers must not infer narrower model/bucket availability from them.
	FailureReasonUsageLimit FailureReason = "usage-limit"
)

type FailureBoundary uint8

const (
	// FailureBoundaryUnspecified is used when the event kind itself supplies the
	// boundary, such as EventAPIRetry and EventTurnComplete.
	FailureBoundaryUnspecified FailureBoundary = iota
	// FailureBoundaryTurn means this error is informational until the provider's
	// authoritative EventTurnComplete arrives.
	FailureBoundaryTurn
	// FailureBoundaryEvent means this error event itself closes the turn.
	FailureBoundaryEvent
)

type FailureScope uint8

const (
	FailureScopeEventTurn FailureScope = iota
	// FailureScopeParentTurn marks the unusual provider event that carries a
	// child linkage but actually reports failure of the parent turn consuming
	// that child. Claude Task assistant errors have this shape.
	FailureScopeParentTurn
)

// FailureMeta is the provider adapter's complete failure classification. One
// boundary enum deliberately replaces independent wait/terminal flags: an
// error cannot both close a turn and promise a later authoritative close.
type FailureMeta struct {
	Class    FailureClass
	Boundary FailureBoundary
	Reason   FailureReason
	Scope    FailureScope
	// Code is the provider's stable typed error name, normalized by the adapter
	// for diagnostics. Control flow must use Class, Boundary, Reason, and Scope.
	Code string
}

func (f FailureMeta) WaitsForTurnComplete() bool { return f.Boundary == FailureBoundaryTurn }

func (f FailureMeta) EndsTurn() bool { return f.Boundary == FailureBoundaryEvent }

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

// CompactionStatusMeta is the typed payload for EventCompactionStatus.
// Active=true opens the thread's compacting window; Active=false closes
// it. Result and ErrorMessage are only populated on a close, and only
// when the wire reported them — Claude's `compact_result` frame carries
// `"success"`/`"failed"` plus a `compact_error` string on failure, while
// Codex's close (the completed item) says nothing beyond that it
// finished. Triage logs a failure's error; nothing persists.
type CompactionStatusMeta struct {
	Active       bool   `json:"active"`
	Result       string `json:"result,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

// CommandLifecycleState is one of the five delivery states Claude's
// `command_lifecycle` frames report for a stdin user message. The set is
// closed by the parser: an unrecognised state is dropped rather than
// forwarded, so no consumer has to carry an "unknown" branch.
//
// The wire carries NO reason field — the schema is
// `{type, command_uuid, state, uuid, session_id}` (verified 2.1.237) —
// so a consumer that wants to explain a terminal state has only the state
// itself to go on.
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
	// CommandCancelled — the message will NEVER be delivered. Claude emits
	// it for a removal (cancel_async_message, an interrupt sweeping the
	// queue, a pending cancel landing just before dispatch) AND for a
	// message consumed into a turn that then aborted or died on a hard
	// failure. Cancelled-over-completed is deliberate on the CLI's side:
	// dup-over-loss for exactly-once resenders.
	CommandCancelled CommandLifecycleState = "cancelled"
	// CommandDiscarded — the SESSION ended (end_session) with the message
	// still queued, so it was never delivered and never will be. Added by
	// Claude Code 2.1.224+ (verified in the 2.1.237 schema alongside the
	// four above); older CLIs report the same situation as `cancelled` or
	// as nothing at all.
	//
	// Terminal and undelivered, exactly like CommandCancelled — every
	// consumer treats the two the same way, and they are separate states
	// only so the CAUSE survives to the timeline: "the session ended" is
	// actionable in a way "cancelled" is not.
	CommandDiscarded CommandLifecycleState = "discarded"
)

// CommandLifecycleMeta is the typed payload for EventCommandLifecycle.
// CommandUUID is the client-minted uuid AO put on the outbound envelope's
// top-level `uuid` field — the same value that lands on the user_text
// row's `provider_item_id` meta.
type CommandLifecycleMeta struct {
	CommandUUID string                `json:"commandUuid"`
	State       CommandLifecycleState `json:"state"`
	// Origin names the producer of a message this app did not send.
	// Empty — the overwhelmingly common case — means AO issued the uuid
	// itself. The only value today is claude.PeerTurnOrigin
	// ("peer-session"): another Claude session on this machine addressed
	// this one through the cross-session inbox, and the CLI minted the
	// uuid for the injected user row. Absence is the safe reading, so a
	// provider that cannot tell leaves it empty.
	Origin string `json:"origin,omitempty"`
}

// CommandsChangedMeta is the typed payload for EventCommandsChanged: the
// provider's FULL replacement list. An empty Commands slice is a real answer
// (every command went away) and consumers must apply it as such — the wire
// contract is replace, never merge.
type CommandsChangedMeta struct {
	Commands []SlashCommand `json:"commands"`
}

// CommandResultMeta is the typed payload for EventCommandResult.
type CommandResultMeta struct {
	// CommandUUID is the client-minted uuid of the stdin user message whose
	// slash command produced this output, correlated by the parser from the
	// surrounding command_lifecycle started/completed pair. Empty when the
	// CLI emitted no lifecycle ack for the command (older CLIs) — consumers
	// must treat absence as "uncorrelated", never as an error. The app's
	// live-config reconciler matches it against the uuids it stamped on
	// /effort and /fast applies to confirm them.
	CommandUUID string `json:"commandUuid,omitempty"`
	// Suppressed marks output that must NOT become a timeline row: a command
	// Agent Overflow issued for its own bookkeeping, or one whose only output
	// is a confirmation of state AO already renders in its own UI. The
	// decision is made at SEND time and correlated back through CommandUUID —
	// never by reading this text — so a provider that emits no lifecycle
	// bracket simply never sets it.
	//
	// It is a ROW decision only. The event is still delivered, because the
	// app-layer live-config reconciler is what settles an /effort or /fast
	// apply from exactly this output.
	Suppressed bool `json:"suppressed,omitempty"`
	// AgentResult identifies a provider-executed command answer that came
	// back from a forked agent. The row stays a command_result because the
	// parent model did not author it, but this typed source lets the timeline
	// render the answer as readable Markdown with honest attribution.
	AgentResult *CommandAgentResultMeta `json:"agentResult,omitempty"`
}

// CommandAgentResultMeta is the durable source identity for a forked
// command's final answer. LaunchID links the answer to the activity card
// without parenting the row beneath it: the answer must remain top-level so
// collapsing the activity cannot hide the result.
type CommandAgentResultMeta struct {
	LaunchID   string `json:"launchId"`
	SourceKind string `json:"sourceKind"`
	SourceName string `json:"sourceName"`
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

// SubagentProgressMeta is the typed payload for EventSubagentProgress.
// Every counter is cumulative for the agent's whole run, never a delta;
// a provider that cannot report a field leaves it zero / empty and the
// consumer keeps whatever it already had for that field (Codex reports
// only TotalTokens).
type SubagentProgressMeta struct {
	// TaskID is the provider's own id for the running agent (Claude
	// task_id; Codex child thread id). Empty when the provider has none.
	TaskID string `json:"taskId,omitempty"`
	// ToolUses is the number of tool calls the agent has made so far.
	ToolUses int `json:"toolUses,omitempty"`
	// TotalTokens is the agent's own token spend so far — every token it
	// caused to be processed, counted ONCE. Never folded into the parent
	// thread's meter. Re-sending a cached prompt is not spend: a figure
	// that counts it grows with round count instead of with work, which
	// is what made a real 42-minute Codex child report 4.5M against
	// 210k of actual spend.
	//
	// The two providers reach it differently, because only one of them
	// gives a choice. Codex ships cumulative breakdowns, so
	// childAgentTokenSpend sums fresh input + cache writes + all output
	// and the result is MONOTONIC. Claude ships one pre-summed
	// `{total_tokens}` with no breakdown, and 2.1.237 builds it as LATEST
	// input plus all output — so Claude's dips when a subagent compacts
	// and cannot be made to do otherwise. The two agree until a
	// compaction (summing each round's fresh input is how the current
	// context got its size); after one they diverge, Claude low.
	//
	// Consumers must therefore NOT assume this only grows. See
	// triage.mergeSubagentProgress, which takes the newest value here
	// while the genuinely monotonic counters take the max.
	TotalTokens int64 `json:"totalTokens,omitempty"`
	// DurationMs is the agent's wall-clock run time so far.
	DurationMs int64 `json:"durationMs,omitempty"`
	// Activity is the agent's CURRENT activity line (Claude: the
	// task_progress `description`, which is NOT the launch description).
	Activity string `json:"activity,omitempty"`
	// LastToolName is the name of the tool the agent used most recently.
	LastToolName string `json:"lastToolName,omitempty"`
	// AgentType is the provider's agent/subagent type name when it
	// reports one (Claude `subagent_type`).
	AgentType string `json:"agentType,omitempty"`
	// Summary is an optional provider-written progress summary.
	Summary string `json:"summary,omitempty"`
}

// SubagentBackgroundedMeta is the typed payload for
// EventSubagentBackgrounded. ItemID on the event is the launch tool_use.
type SubagentBackgroundedMeta struct {
	TaskID string `json:"taskId,omitempty"`
}

// BackgroundTaskRef is one member of the live background-task set.
type BackgroundTaskRef struct {
	TaskID string `json:"taskId"`
	// ToolUseID is the launch tool_use the task belongs to when the
	// parser can resolve it (task_id ↔ tool_use_id map); empty otherwise.
	ToolUseID   string `json:"toolUseId,omitempty"`
	TaskType    string `json:"taskType,omitempty"`
	Description string `json:"description,omitempty"`
}

// BackgroundTasksChangedMeta is the typed payload for
// EventBackgroundTasksChanged: the provider's FULL replacement set. An
// empty Tasks slice is a real answer (nothing is running in the
// background) and consumers must apply it as such.
type BackgroundTasksChangedMeta struct {
	Tasks []BackgroundTaskRef `json:"tasks"`
}
