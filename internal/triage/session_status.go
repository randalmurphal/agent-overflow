package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"agent-overflow/internal/provider"
)

// handleSessionStatus is the entry point for EventSessionStatus
// envelopes. Both providers emit a small set of contents: transient
// lifecycle signals ("disconnected", "session_state_changed"), terminal
// process-death ("error"), and anything else falls through to the
// unknown-content log throttle. Retries (Claude `system.api_retry`,
// Codex `error+willRetry`) no longer arrive through this channel —
// they emit EventAPIRetry directly.
//
// "error" is the persistent case: it carries ProcessExitInfo on Meta,
// promotes to a session_died notification + provider:session_died
// frontend event, and synthesizes a truncated EventTurnComplete when a
// turn is open so the working indicator clears even without a wire
// turn-complete. The "disconnected" follow-on the read loop emits
// after "error" is intentionally a no-op — the error handler already
// drained the open turn.
func (r *Router) handleSessionStatus(evt provider.ProviderEvent) error {
	content := strings.TrimSpace(evt.Content)
	switch content {
	case "":
		return nil
	case "disconnected", "session_state_changed":
		return nil
	case "error":
		return r.handleSessionDied(evt)
	default:
		r.logUnknownSessionStatusOnce(content)
		return nil
	}
}

// handleSessionDied promotes a process-exit signal into three loosely-
// coupled UI projections:
//
//  1. Persist a `notification` timeline row tagged
//     `meta.kind = "session_died"` so the chat history records what
//     happened.
//  2. Synthesize a truncated EventTurnComplete when a turn is open so
//     the frontend working indicator clears without waiting for a
//     wire turn-complete that will never arrive.
//  3. Emit a typed `provider:session_died` event so the frontend can
//     light the Reconnect banner.
//
// All three are idempotent: a duplicate session-status error (eg from
// a slow exit followed by stdout drain) replays through the same
// machinery without producing duplicate rows or banners. The
// notification row uses a deterministic id (`session_died:<turnIndex>`)
// so the second persist upserts in place, and the emission is gated on
// "row was new this call" — re-running the same death does not re-fire
// the banner.
func (r *Router) handleSessionDied(evt provider.ProviderEvent) error {
	now := eventTimestampMillis(evt)

	info := decodeProcessExitInfo(evt.Meta)

	turnIndex := r.timelineNotificationTurnIndex(evt.ThreadID)
	deathID := sessionDiedNotificationID(turnIndex)
	wasNew, err := r.persistTimelineNotificationWithID(evt, deathID, sessionDiedNotificationKind, sessionDiedSummary(info))
	if err != nil {
		log.Printf("triage: persist session_died notification: %v", err)
	}

	if _, ok := r.openTurnIndex(evt.ThreadID); ok {
		if err := r.synthesizeTruncatedTurnComplete(evt.ThreadID, now); err != nil {
			log.Printf("triage: synthesize turn-complete on session_died: %v", err)
		}
	}

	if wasNew {
		r.emit("provider:session_died", SessionDiedEvent{
			ThreadID:   evt.ThreadID,
			Reason:     info.Reason,
			ExitCode:   info.ExitCode,
			Signal:     info.Signal,
			OccurredAt: now,
		})
	}
	return nil
}

// sessionDiedNotificationID is the deterministic id pattern for the
// timeline notification row. Keying on the turn the session died
// during means a re-emitted EventSessionStatus{"error"} (slow exit
// followed by a stdout drain, retry of the same death from the read
// loop) upserts in place rather than allocating a fresh
// `notification:N:M` counter id. A subsequent death after reconnect
// targets a fresh turnIndex and gets its own row.
func sessionDiedNotificationID(turnIndex int) string {
	return fmt.Sprintf("session_died:%d", turnIndex)
}

// sessionDiedNotificationKind is the meta.kind discriminator the
// frontend matches on to render a SessionDiedNotification component
// instead of a generic notification row. Kept as a typed constant so
// renames stay in one place.
const sessionDiedNotificationKind = "session_died"

// sessionDiedSummary chooses the one-line summary the timeline row
// renders verbatim (and the banner falls back to). Prefer the most
// specific reason: signal name, exit code, then the raw error message
// from MarshalProcessExitMeta. Empty inputs fall through to a generic
// label so the row is never blank.
func sessionDiedSummary(info provider.ProcessExitInfo) string {
	switch {
	case info.Signal != "":
		return "Provider session terminated by signal " + info.Signal
	case info.ExitCode != 0:
		return "Provider session exited with code " + strconv.Itoa(info.ExitCode)
	case info.Reason != "":
		return info.Reason
	default:
		return "Provider session exited unexpectedly"
	}
}

func decodeProcessExitInfo(raw json.RawMessage) provider.ProcessExitInfo {
	var info provider.ProcessExitInfo
	if len(raw) == 0 {
		return info
	}
	_ = json.Unmarshal(raw, &info)
	return info
}

// unknownSessionStatusCap bounds the throttle set so a long-running
// process that sees a stream of novel session-status strings — a
// provider regression, a wire-format drift, or a fuzzed input — cannot
// grow the map without bound. When the cap is hit the oldest entries
// are dropped wholesale (map reset) which re-admits one extra log line
// per distinct value after a cap rollover. That is acceptable because
// the cap is orders of magnitude higher than the known-good enumeration
// (~6 persistent kinds today).
const unknownSessionStatusCap = 256

func (r *Router) logUnknownSessionStatusOnce(content string) {
	key := strings.TrimSpace(content)
	r.mu.Lock()
	if _, seen := r.unknownSessionStatusLogged[key]; seen {
		r.mu.Unlock()
		return
	}
	if len(r.unknownSessionStatusLogged) >= unknownSessionStatusCap {
		r.unknownSessionStatusLogged = make(map[string]struct{})
	}
	r.unknownSessionStatusLogged[key] = struct{}{}
	r.mu.Unlock()
	log.Printf("triage: unknown session-status content %q — dropping", key)
}
