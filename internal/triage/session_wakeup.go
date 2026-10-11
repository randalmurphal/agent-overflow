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
// time and the armed-job list (handleScheduledJobs) are the only state
// anywhere that says "this idle-looking session is scheduled to resume
// itself." The idle-session reaper consults
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

// handleScheduledJobs replaces the thread's list of armed scheduled jobs
// (Claude CronCreate, claude-wire.md §E11). A job, like a ScheduleWakeup
// timer, lives only in the CLI process and fires as a turn the CLI starts
// itself, so killing an idle-looking session would silently cancel it.
// The provider sends the FULL list whenever it changes, each job with the
// latest time it can next fire: a create, a delete, a CronList and every
// fire (which moves a recurring job's deadline forward and releases a
// fired one-shot). An empty list clears the record; the session sweeps
// that clear pendingWakeupAt clear this too.
func (r *Router) handleScheduledJobs(evt provider.ProviderEvent) error {
	var meta provider.ScheduledJobsMeta
	if len(evt.Meta) > 0 {
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			return fmt.Errorf("triage: decode scheduled jobs meta for thread %s: %w", evt.ThreadID, err)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(meta.Jobs) == 0 {
		if st := r.threadStateIfPresent(evt.ThreadID); st != nil {
			st.scheduledJobs = nil
		}
		return nil
	}
	r.state(evt.ThreadID).scheduledJobs = meta.Jobs
	return nil
}

// wakeDeadline is the latest time the session is known to resume itself:
// its ScheduleWakeup fire time or any armed job's next-fire deadline. A
// past value is returned as-is; the caller applies its own clock and grace.
func (st *threadState) wakeDeadline() (int64, bool) {
	at, ok := st.pendingWakeupAt, st.pendingWakeupSet
	for _, job := range st.scheduledJobs {
		if !ok || job.FireByUnixMs > at {
			at, ok = job.FireByUnixMs, true
		}
	}
	return at, ok
}

// PendingWakeupAt returns the latest time the thread's session is
// scheduled to resume itself: a harness-wakeup fire time or an armed
// scheduled job's next-fire deadline (wakeDeadline). It performs no clock
// comparison: the caller decides what "still pending" means against its
// own notion of now (the reaper uses its injectable clock plus a grace
// window), so an already-elapsed time is returned as-is with ok=true.
// Nil-safe like HasPendingWork (test fixtures, partial init).
func (r *Router) PendingWakeupAt(threadID string) (time.Time, bool) {
	threadID = strings.TrimSpace(threadID)
	if r == nil || threadID == "" {
		return time.Time{}, false
	}
	r.mu.Lock()
	st := r.threadStateIfPresent(threadID)
	var at int64
	var ok bool
	if st != nil {
		at, ok = st.wakeDeadline()
	}
	r.mu.Unlock()
	if !ok {
		return time.Time{}, false
	}
	return time.UnixMilli(at), true
}
