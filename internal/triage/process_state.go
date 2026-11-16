package triage

import "agent-overflow/internal/provider"

// processBound is the thread state that belongs to one provider process.
// MarkThreadActive replaces it whole when it commits the thread to a new
// session, so a field added here is reset with the rest. Every writer of
// it takes the frame's session token and refuses a retired one under the
// r.mu it writes under (frameRetiredLocked).
//
// State that outlives a replacement stays on threadState or threadIdentity:
// the queue and its sends, which the host owns; the open turn and round,
// which the host ends as truncated when it closes the replaced session
// (EndReplacedSessionTurn); streams and prompts, which their own
// settlement or CleanupThread ends; and the thread's identity and locks.
// A start that replaces a live session (stopExistingSessionLocked, such as
// the session-config restart reconcileSessionConfig schedules) reaches
// MarkThreadActive without CleanupThread, so none of that may be reset
// here. Interrupt marks are host-written state on threadIdentity that
// MarkThreadActive clears beside this.
type processBound struct {
	// softClosedTurn is the turn a soft round close settled while its CLI
	// turn's `result` is still to come (softClosedTurnSet).
	// Parent content before the result continues it
	// (reopenSoftClosedTurn); the result and every turn start clear it. Only
	// the dead process's own result or continuation could resume it.
	softClosedTurn    int
	softClosedTurnSet bool

	// settledTurns marks turns whose settlement has run (turns row
	// UPDATE-d, streaming items settled): handleTurnComplete's, or the
	// predecessor settle when the next turn starts. A later
	// EventTurnComplete for a settled turn (the synthetic-truncate-then-
	// real race) is a persistence no-op apart from its late payload, so
	// the turns row isn't re-stamped. Cleared by setOpenTurn (so a
	// re-init can re-settle the same turn). A stale marker would misroute a
	// replacement session's first error result into persistLateTurnPayload.
	// Key = turnIndex. The session has settled a turn when it is
	// non-empty (sessionSettledATurn).
	//
	// This gate operates at LOGICAL-TURN granularity; the frontend-facing
	// `provider:turn_completed` emission is gated per ROUND via
	// currentRound/takeOpenRound below.
	settledTurns map[int]bool
	// lastSettled is how this process's latest turn settlement ended
	// (lastSettledSet), for a Stop marked after it (stop_waiting_send.go).
	// It names a turn of settledTurns, so it resets with it.
	lastSettled    settledTurnOutcome
	lastSettledSet bool

	// effectiveModel is the session-scoped model actually serving the
	// thread after a provider fallback. The durable threads.model remains
	// the user's requested model; this live projection is cleared with the
	// provider session and included in GetThreadLiveState hydration. Its
	// monotonic revision counter lives on threadIdentity — it must
	// survive the session that produced it.
	effectiveModel    string
	effectiveModelSet bool

	// announcedDeath is the id of the session_died notice this thread's
	// session last announced on provider:session_died. It dedupes a
	// replayed death whose notice the store refused, so no saved row can
	// (handleSessionDied). A death announced for the previous process says
	// nothing about this one's.
	announcedDeath string

	// deliveriesAwaitingTurn are queued sends delivered while no turn was
	// open: Claude acks `started` for a held message before the init of the
	// turn it runs in. The next turn start reports them against its index.
	// It waits on the dead process's turn; a replacement's first turn is
	// not it.
	deliveriesAwaitingTurn []SendDelivery

	// pendingWakeupAt is the fire time (epoch ms) of the Claude harness's
	// pending ScheduleWakeup timer. The timer is in-process CLI state with
	// no task lifecycle, so this field and scheduledJobs are the only
	// record that an idle-looking session will resume itself; the
	// idle-session reaper
	// reads it via PendingWakeupAt. A stale deadline would shield a
	// replacement process from the reaper. See session_wakeup.go.
	pendingWakeupAt  int64
	pendingWakeupSet bool
	// scheduledJobs is the provider's latest list of armed scheduled jobs
	// (Claude CronCreate), each with the latest time it can next fire.
	// Same in-process lifetime and same sweeps as pendingWakeupAt; the
	// provider bounds the list. See session_wakeup.go.
	scheduledJobs []provider.ScheduledJob

	// subagentProgress is the latest live progress tick per launch
	// tool_use, keyed by itemID. Live UI state only; the terminal path
	// consumes it into the launch row's meta, and a dead process never
	// delivers that terminal. See subagent_progress.go.
	subagentProgress map[string]provider.SubagentProgressMeta

	// compactingSince is the open compacting window: the epoch-ms
	// timestamp of the frame that opened it. Live-only projection behind
	// `provider:compacting` (compaction_status.go); closed by the explicit
	// close frame, the compact boundary, or turn completion. A replacement
	// process is never mid-compaction.
	compactingSince    int64
	compactingSinceSet bool

	// commandLifecycle correlates Claude's `command_lifecycle` acks back
	// to the AO row and the wire round each stdin user message was queued
	// into, keyed by command_uuid. Send-time carry-over, released on the
	// terminal ack; the acks it waits on can only come from its process.
	// Bounded per thread by
	// maxCommandLifecycleEntriesPerThread. See command_lifecycle.go for
	// why neither value can be recovered lazily.
	commandLifecycle map[string]commandLifecycleEntry

	// workspacePath caches the directory the thread's provider process
	// works in, for path-ref enrichment: every streaming text flush and
	// every prose settle reads it, 10-30 times in a heavy turn. It is read
	// from the thread row at first use, follows the process's own move
	// (followWorkspaceChange). A replacement launches from the thread row's
	// workspace. Every other checkout move restarts the session.
	workspacePath    string
	workspacePathSet bool
}
