package triage

import (
	"encoding/json"
	"fmt"

	"agent-overflow/internal/provider"
)

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

// ActiveTurnSnapshot is the server-owned live projection of the currently
// open frontend wire round. It intentionally mirrors TurnStartedEvent so a
// reconnecting frontend can rebuild the same active-turn registry it would
// have built from the original provider:turn_started push.
type ActiveTurnSnapshot struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	TurnIndex int    `json:"turnIndex"`
	StartedAt int64  `json:"startedAt"`
}

// TurnCompletedEvent is the frontend-facing payload for
// provider:turn_completed. Carries the settled-turn projection the UI
// uses for read-state and trace/debug surfaces, plus clears the working
// indicator. Optional fields are populated when the underlying provider
// surface carries them (assistant_message_id, token usage, error message,
// aborted flag).
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
	// RevertedUserMessage signals that the turn ended because the App
	// layer reverted the most-recent user message (early-interrupt
	// revert). The frontend uses this to suppress the "Interrupted"
	// thread-status pill that would otherwise paint on Aborted: from
	// the user's perspective nothing happened, the message went back
	// into the composer. Always paired with Aborted: true.
	RevertedUserMessage bool `json:"revertedUserMessage,omitempty"`
	CountsAsActivity    bool `json:"countsAsActivity"`
}

// SessionDiedEvent is the frontend-facing payload for
// `provider:session_died`. Emitted when a provider process exits
// abnormally (non-zero code, killed by signal, or read-loop EOF
// without a clean turn boundary). Drives the session-error banner with
// the Reconnect call-to-action — the working indicator is cleared
// separately via the synthesized `provider:turn_completed` and the
// timeline row showing process-exit info is persisted as a
// `notification`-kind item with `meta.kind = "session_died"`. Three
// loosely-coupled observers, three jobs: turn-completed clears the
// working indicator, item upsert persists history, this event lights
// the banner with Reconnect.
type SessionDiedEvent struct {
	ThreadID string `json:"threadId"`
	Reason   string `json:"reason,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
	Signal   string `json:"signal,omitempty"`
	// StderrTail is the provider process's captured last stderr output,
	// pre-sanitized by provider.MarshalProcessExitMeta (single line,
	// hard length cap). It carries the actual failure text for exits
	// with no wire output (bad CLI flag, missing module).
	StderrTail string `json:"stderrTail,omitempty"`
	OccurredAt int64  `json:"occurredAt"`
}

// TodoUpdateEvent is the frontend-facing payload for provider:todo_update.
// Carries the latest live-todo snapshot from either Claude TodoWrite or
// Codex update_plan. Both providers normalise to a shared step shape
// before this point. The frontend renders this in the activity rail and
// intentionally does
// NOT add a row to the chat timeline — todos are session state, not
// history. Persistence is deliberately NOT a triage concern for this
// kind; triage keeps the backend-owned refresh/reconnect snapshot in
// memory while ThreadPane remains only the visible projection.
type TodoUpdateEvent struct {
	ThreadID string     `json:"threadId"`
	Steps    []TodoStep `json:"steps"`
}

// LiveTodoSnapshot is the server-owned copy of the latest live todo panel
// state for a thread. SQLite remains history-only; this in-memory snapshot
// exists so refresh / reconnect sees the same live panel as the original
// frontend process.
type LiveTodoSnapshot struct {
	ThreadID  string     `json:"threadId"`
	Steps     []TodoStep `json:"steps"`
	UpdatedAt int64      `json:"updatedAt"`
}

// TodoStep is one item in a live todo list. Status uses the camelCase
// enum Codex emits on the wire (`pending` | `inProgress` | `completed`).
// Claude TodoWrite's snake_case `in_progress` is normalised to
// `inProgress` upstream in the parser so triage and the frontend see
// one vocabulary. Unknown values pass through; the frontend renders
// them as pending.
//
// `id` and `owner` are populated by the Claude Code 2.1.150+ Task\*
// family path (TaskCreate / TaskUpdate). Legacy TodoWrite + Codex
// update_plan leave them empty. The frontend treats both as
// optional: missing `id` falls back to step-string keying, missing
// or empty `owner` suppresses the badge so the widget matches its
// pre-Task* rendering.
type TodoStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
	ID     string `json:"id,omitempty"`
	Owner  string `json:"owner,omitempty"`
}

// BackgroundTaskStateEvent is the frontend-facing payload for
// provider:background_task_state. Decouples the tray ("process state")
// from the chat sibling row ("agent observation state"). Two states
// today:
//
//   - "exited"  — host signalled process exit (Claude system/task_updated
//     stashed). Tray hides the launch row from this point on; no chat row yet.
//   - "drained" — agent observation event consumed the stash and the
//     tool_completion sibling has been written. Frontend re-queries the
//     tray (the launch is already filtered out by the stash predicate;
//     this event just nudges the UI to refresh).
//
// Pure UI nudge — the SQLite tray query is the source of truth (filters
// on the pending_background_task_terminals table). Frontends that miss
// the event still get correct state on their next mount/query; the
// event just avoids a polling loop in the steady state.
type BackgroundTaskStateEvent struct {
	ThreadID  string `json:"threadId"`
	TaskID    string `json:"taskId"`
	LaunchID  string `json:"launchId,omitempty"`
	State     string `json:"state"`
	UpdatedAt int64  `json:"updatedAt"`
}

// turnCompleteMeta is the internal projection of provider.TurnCompleteMeta.
// EventTurnComplete no longer decodes semantic state from ProviderEvent.Meta;
// producers must populate ProviderEvent.TurnComplete with one of the typed
// provider-neutral payloads.
type turnCompleteMeta struct {
	StopReason         string
	AssistantMessageID string
	Usage              json.RawMessage
	// ModelUsage is the per-model split of Usage (both are per-turn
	// deltas — see provider.WireTurnCompleteMeta). Usage persists on the
	// turns row; ModelUsage appends usage-ledger rows.
	ModelUsage []provider.ModelTokenUsage
	Aborted    bool
	Truncated  bool
	Error      string
}

func turnCompleteMetaFromEvent(evt provider.ProviderEvent) (turnCompleteMeta, error) {
	switch meta := evt.TurnComplete.(type) {
	case *provider.WireTurnCompleteMeta:
		if meta == nil {
			return turnCompleteMeta{}, fmt.Errorf("turn_complete missing typed payload")
		}
		return wireTurnCompleteMeta(*meta), nil
	case *provider.SoftRoundCloseMeta:
		if meta == nil {
			return turnCompleteMeta{}, fmt.Errorf("turn_complete missing typed payload")
		}
		return turnCompleteMeta{
			StopReason:         meta.StopReason,
			AssistantMessageID: meta.AssistantMessageID,
		}, nil
	case *provider.TruncatedTurnCompleteMeta:
		if meta == nil {
			return turnCompleteMeta{}, fmt.Errorf("turn_complete missing typed payload")
		}
		return turnCompleteMeta{
			Truncated: true,
			Error:     meta.ErrorMessage,
		}, nil
	default:
		return turnCompleteMeta{}, fmt.Errorf("turn_complete payload type %T is not supported", evt.TurnComplete)
	}
}

func wireTurnCompleteMeta(meta provider.WireTurnCompleteMeta) turnCompleteMeta {
	var usage json.RawMessage
	if meta.Usage != nil {
		if encoded, err := json.Marshal(meta.Usage); err == nil {
			usage = encoded
		}
	}
	return turnCompleteMeta{
		StopReason:         meta.StopReason,
		AssistantMessageID: meta.AssistantMessageID,
		Usage:              usage,
		ModelUsage:         meta.ModelUsage,
		Aborted:            meta.Aborted,
		Error:              meta.ErrorMessage,
	}
}

// canonicalStopReason maps provider-specific stop reasons onto the
// canonical set documented in turn-lifecycle.md: end_turn | max_tokens
// | tool_use | stop_sequence | refusal | error | interrupted. Unknown
// values pass through untouched so a future provider extension isn't
// silently rewritten to "error".
func canonicalStopReason(meta turnCompleteMeta) string {
	// Interruption always wins.
	if meta.Aborted || meta.Truncated {
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
	return ""
}
