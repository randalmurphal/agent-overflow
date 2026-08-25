package triage

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// handleSessionWakeup records (or clears) the thread's pending harness
// wakeup. The Claude harness holds a ScheduleWakeup timer purely
// in-process — no task lifecycle, no wire traffic until it fires and the
// stored prompt arrives as a fresh user turn — so this per-thread fire
// time is the only state anywhere that says "this idle-looking session
// is scheduled to resume itself." The idle-session reaper consults
// PendingWakeupAt before closing a session; killing the process would
// silently kill the timer.
//
// Call-sequence coverage (not just value coverage): a schedule replaces
// any prior pending fire time (the harness keeps one wakeup per loop —
// re-scheduling supersedes), a `{stop:true}` ack arrives as
// ScheduledForUnixMs <= 0 and deletes the entry, and both cleanupThread
// and MarkThreadActive drop it because a wakeup timer never survives its
// CLI process — neither a torn-down session nor a replacement process
// retains it. Entries for wakeups that already fired read as elapsed at
// the caller's comparison and are swept at session end.
func (r *Router) handleSessionWakeup(evt provider.ProviderEvent) error {
	var meta provider.SessionWakeupMeta
	if len(evt.Meta) > 0 {
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			return fmt.Errorf("triage: decode session wakeup meta for thread %s: %w", evt.ThreadID, err)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if meta.ScheduledForUnixMs <= 0 {
		if st := r.threadStateIfPresent(evt.ThreadID); st != nil {
			st.pendingWakeupAt, st.pendingWakeupSet = 0, false
		}
		return nil
	}
	st := r.state(evt.ThreadID)
	st.pendingWakeupAt = meta.ScheduledForUnixMs
	st.pendingWakeupSet = true
	return nil
}

// PendingWakeupAt returns the thread's recorded harness-wakeup fire
// time. It performs no clock comparison — the caller decides what
// "still pending" means against its own notion of now (the reaper uses
// its injectable clock plus a grace window), so an already-elapsed fire
// time is returned as-is with ok=true. Nil-safe like HasPendingWork
// (test fixtures, partial init).
func (r *Router) PendingWakeupAt(threadID string) (time.Time, bool) {
	threadID = strings.TrimSpace(threadID)
	if r == nil || threadID == "" {
		return time.Time{}, false
	}
	r.mu.Lock()
	st := r.threadStateIfPresent(threadID)
	var at int64
	ok := st != nil && st.pendingWakeupSet
	if ok {
		at = st.pendingWakeupAt
	}
	r.mu.Unlock()
	if !ok {
		return time.Time{}, false
	}
	return time.UnixMilli(at), true
}
