package triage

import (
	"encoding/json"
	"fmt"
	"log"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

// CompactingStateEvent is the payload of `provider:compacting`, the live
// per-thread flag that the provider is summarizing this thread's context
// right now — a window that can run for minutes with no other wire
// traffic (2-3 minutes is typical for a full-window compaction).
//
// Live session state, never history (root CLAUDE.md principle 2): the
// timeline's durable record of a compaction is the `compaction` divider
// row the boundary event persists; this flag only drives the activity
// label while the work is in flight. Unlike `provider:fast_mode`,
// absence here is not "unknown" — triage owns the window and closes it
// itself, so the reconnect snapshot (`LiveStateSnapshot`) carries it: a
// frontend refreshed mid-compaction would otherwise show a stale
// "Working" label for the rest of a multi-minute silent window.
type CompactingStateEvent struct {
	ThreadID string `json:"threadId"`
	Active   bool   `json:"active"`
	// SinceUnixMs anchors the "how long has this been running" reading on
	// the frame that OPENED the window; keep-alive repeats do not move it.
	// Zero on Active=false frames.
	SinceUnixMs int64 `json:"sinceUnixMs,omitempty"`
}

// handleCompactionStatus projects EventCompactionStatus onto the live
// compacting flag. Open frames are idempotent (Claude re-emits
// `status:"compacting"` as a 30s keep-alive on remote-bridged sessions;
// the repeat must not restart the window or spam the frontend). Close
// frames route through clearCompacting, which emits only when the flag
// was actually set.
//
// A failed close carries the wire's error string; it is logged rather
// than surfaced because each caller already has its own user-facing
// channel — a manual /compact failure comes back as the command's own
// result row, and both CLIs retry auto-compaction on the next turn by
// design.
func (r *Router) handleCompactionStatus(evt provider.ProviderEvent) error {
	var meta provider.CompactionStatusMeta
	if len(evt.Meta) > 0 {
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			return fmt.Errorf("triage: decode compaction status meta for thread %s: %w", evt.ThreadID, err)
		}
	}
	if !meta.Active {
		if meta.Result == "failed" {
			log.Printf("triage: compaction failed for thread %s: %s", evt.ThreadID, meta.ErrorMessage)
		}
		r.clearCompacting(evt.ThreadID)
		return nil
	}

	r.mu.Lock()
	st := r.state(evt.ThreadID)
	since, already := st.compactingSince, st.compactingSinceSet
	if !already {
		since = evt.Timestamp.UnixMilli()
		st.compactingSince = since
		st.compactingSinceSet = true
	}
	r.mu.Unlock()
	if already {
		return nil
	}
	r.emit(eventchan.ProviderCompacting, CompactingStateEvent{
		ThreadID:    evt.ThreadID,
		Active:      true,
		SinceUnixMs: since,
	})
	return nil
}

// clearCompacting closes the thread's compacting window, emitting the
// inactive frame only when a window was open. Beyond the explicit close
// frame, two defensive callers keep the flag from sticking: the
// compact-boundary handler (both providers' success signal — Claude
// also sends the explicit close ~20ms earlier, so that one no-ops) and
// turn completion (a FAILED Codex compaction abandons its item without
// completing it, so the turn boundary is its only reliable close).
// cleanupThread and MarkThreadActive delete the map entry without
// emitting — the frontend drops its copy on the same session-teardown
// path that drops fast-mode state.
func (r *Router) clearCompacting(threadID string) {
	if threadID == "" {
		return
	}
	r.mu.Lock()
	active := false
	if st := r.threadStateIfPresent(threadID); st != nil {
		active = st.compactingSinceSet
		st.compactingSince, st.compactingSinceSet = 0, false
	}
	r.mu.Unlock()
	if active {
		r.emit(eventchan.ProviderCompacting, CompactingStateEvent{ThreadID: threadID, Active: false})
	}
}
