package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

func wakeupEvent(t *testing.T, threadID string, scheduledForUnixMs int64) provider.ProviderEvent {
	t.Helper()
	meta, err := json.Marshal(provider.SessionWakeupMeta{ScheduledForUnixMs: scheduledForUnixMs})
	if err != nil {
		t.Fatalf("marshal wakeup meta: %v", err)
	}
	return provider.ProviderEvent{
		Kind:      provider.EventSessionWakeup,
		ThreadID:  threadID,
		Meta:      meta,
		Timestamp: time.Now(),
	}
}

// TestSessionWakeupTransitions covers the call sequences, not just the
// values: schedule sets, re-schedule replaces (the harness keeps one
// wakeup per loop), stop clears, and schedule-after-stop sets again.
func TestSessionWakeupTransitions(t *testing.T) {
	r := NewRouter(nil, func(eventchan.Channel, any) {})
	const threadID = "thread-wakeup"
	fireAt := time.Now().Add(25 * time.Minute).UnixMilli()

	if _, ok := r.PendingWakeupAt(threadID); ok {
		t.Fatal("fresh router must report no pending wakeup")
	}

	if err := r.Handle(wakeupEvent(t, threadID, fireAt)); err != nil {
		t.Fatalf("handle schedule: %v", err)
	}
	at, ok := r.PendingWakeupAt(threadID)
	if !ok || at.UnixMilli() != fireAt {
		t.Fatalf("PendingWakeupAt after schedule: got (%v, %v), want (%d, true)", at.UnixMilli(), ok, fireAt)
	}

	// Re-schedule replaces.
	replaced := fireAt + int64(30*time.Minute/time.Millisecond)
	if err := r.Handle(wakeupEvent(t, threadID, replaced)); err != nil {
		t.Fatalf("handle re-schedule: %v", err)
	}
	if at, ok := r.PendingWakeupAt(threadID); !ok || at.UnixMilli() != replaced {
		t.Fatalf("PendingWakeupAt after re-schedule: got (%v, %v), want (%d, true)", at.UnixMilli(), ok, replaced)
	}

	// Stop (ScheduledForUnixMs 0) clears.
	if err := r.Handle(wakeupEvent(t, threadID, 0)); err != nil {
		t.Fatalf("handle stop: %v", err)
	}
	if _, ok := r.PendingWakeupAt(threadID); ok {
		t.Fatal("stop ack must clear the pending wakeup")
	}

	// Schedule after stop sets again.
	if err := r.Handle(wakeupEvent(t, threadID, fireAt)); err != nil {
		t.Fatalf("handle schedule after stop: %v", err)
	}
	if _, ok := r.PendingWakeupAt(threadID); !ok {
		t.Fatal("schedule after stop must set the pending wakeup again")
	}
}

// TestSessionWakeupClearedBySessionLifecycle pins the two lifecycle
// sweeps: the wakeup timer is in-process CLI state, so both a session
// teardown (CleanupThread) and a replacement-session commit
// (MarkThreadActive, the repair-restart path that skips CleanupThread)
// must drop the record — a stale future fire time would shield a fresh
// process that holds no timer.
func TestSessionWakeupClearedBySessionLifecycle(t *testing.T) {
	fireAt := time.Now().Add(45 * time.Minute).UnixMilli()

	t.Run("CleanupThread", func(t *testing.T) {
		r := NewRouter(nil, func(eventchan.Channel, any) {})
		const threadID = "thread-wakeup-cleanup"
		if err := r.Handle(wakeupEvent(t, threadID, fireAt)); err != nil {
			t.Fatalf("handle schedule: %v", err)
		}
		r.CleanupThread(threadID)
		if _, ok := r.PendingWakeupAt(threadID); ok {
			t.Fatal("CleanupThread must clear the pending wakeup")
		}
	})

	t.Run("MarkThreadActive", func(t *testing.T) {
		r := NewRouter(nil, func(eventchan.Channel, any) {})
		const threadID = "thread-wakeup-restart"
		if err := r.Handle(wakeupEvent(t, threadID, fireAt)); err != nil {
			t.Fatalf("handle schedule: %v", err)
		}
		r.MarkThreadActive(threadID, "")
		if _, ok := r.PendingWakeupAt(threadID); ok {
			t.Fatal("MarkThreadActive must clear the previous process's pending wakeup")
		}
	})
}

// TestPendingWakeupAtNilSafety mirrors HasPendingWork's nil contract.
func TestPendingWakeupAtNilSafety(t *testing.T) {
	var r *Router
	if _, ok := r.PendingWakeupAt("thread"); ok {
		t.Fatal("nil router must report no pending wakeup")
	}
	live := NewRouter(nil, func(eventchan.Channel, any) {})
	if _, ok := live.PendingWakeupAt(""); ok {
		t.Fatal("empty thread id must report no pending wakeup")
	}
}

func scheduledJobsEvent(t *testing.T, threadID string, jobs ...provider.ScheduledJob) provider.ProviderEvent {
	t.Helper()
	meta, err := json.Marshal(provider.ScheduledJobsMeta{Jobs: jobs})
	if err != nil {
		t.Fatalf("marshal scheduled jobs: %v", err)
	}
	return provider.ProviderEvent{Kind: provider.EventScheduledJobs, ThreadID: threadID, Meta: meta, Timestamp: time.Now()}
}

// The armed-job list replaces itself on every report, and the thread's
// wake deadline is the latest of the ScheduleWakeup time and every job's
// next-fire deadline, so neither record can hide the other.
func TestScheduledJobsExtendThePendingWakeup(t *testing.T) {
	r := NewRouter(nil, func(eventchan.Channel, any) {})
	const threadID = "thread-cron"
	wakeAt := time.Now().Add(10 * time.Minute).UnixMilli()
	jobLate := time.Now().Add(40 * time.Minute).UnixMilli()
	jobEarly := time.Now().Add(5 * time.Minute).UnixMilli()

	if err := r.Handle(scheduledJobsEvent(t, threadID,
		provider.ScheduledJob{ID: "a", Cron: "* * * * *", Recurring: true, FireByUnixMs: jobEarly},
		provider.ScheduledJob{ID: "b", Cron: "0 * * * *", Recurring: true, FireByUnixMs: jobLate},
	)); err != nil {
		t.Fatalf("handle jobs: %v", err)
	}
	if at, ok := r.PendingWakeupAt(threadID); !ok || at.UnixMilli() != jobLate {
		t.Fatalf("PendingWakeupAt = (%v, %v), want the latest job deadline %d", at.UnixMilli(), ok, jobLate)
	}
	if err := r.Handle(wakeupEvent(t, threadID, wakeAt)); err != nil {
		t.Fatalf("handle wakeup: %v", err)
	}
	if at, _ := r.PendingWakeupAt(threadID); at.UnixMilli() != jobLate {
		t.Fatalf("a nearer ScheduleWakeup hid a later job deadline: %d", at.UnixMilli())
	}
	// A replacement list drops the later job.
	if err := r.Handle(scheduledJobsEvent(t, threadID,
		provider.ScheduledJob{ID: "a", Cron: "* * * * *", Recurring: true, FireByUnixMs: jobEarly})); err != nil {
		t.Fatalf("handle replacement: %v", err)
	}
	if at, _ := r.PendingWakeupAt(threadID); at.UnixMilli() != wakeAt {
		t.Fatalf("PendingWakeupAt = %d after the list shrank, want the wakeup %d", at.UnixMilli(), wakeAt)
	}
	// An empty list clears the jobs; the wakeup stays its own record.
	if err := r.Handle(scheduledJobsEvent(t, threadID)); err != nil {
		t.Fatalf("handle empty: %v", err)
	}
	if err := r.Handle(wakeupEvent(t, threadID, 0)); err != nil {
		t.Fatalf("handle stop: %v", err)
	}
	if _, ok := r.PendingWakeupAt(threadID); ok {
		t.Fatal("an empty job list and a stopped wakeup left a deadline")
	}
}

// Armed jobs and the wakeup timer live in the CLI process, so every end
// of that process drops both: the host's close (CleanupThread), a
// replacement start (MarkThreadActive) and the process's own exit
// (ClearProcessSchedule).
func TestScheduledJobsClearedBySessionLifecycle(t *testing.T) {
	job := provider.ScheduledJob{ID: "a", Cron: "0 9 * * 1", FireByUnixMs: time.Now().Add(6 * 24 * time.Hour).UnixMilli()}
	for name, sweep := range map[string]func(*Router, string){
		"CleanupThread":        func(r *Router, id string) { r.CleanupThread(id) },
		"MarkThreadActive":     func(r *Router, id string) { r.MarkThreadActive(id, "") },
		"ClearProcessSchedule": func(r *Router, id string) { r.ClearProcessSchedule(id) },
	} {
		t.Run(name, func(t *testing.T) {
			r := NewRouter(nil, func(eventchan.Channel, any) {})
			const threadID = "thread-cron-sweep"
			if err := r.Handle(scheduledJobsEvent(t, threadID, job)); err != nil {
				t.Fatalf("handle jobs: %v", err)
			}
			if err := r.Handle(wakeupEvent(t, threadID, time.Now().Add(30*time.Minute).UnixMilli())); err != nil {
				t.Fatalf("handle wakeup: %v", err)
			}
			sweep(r, threadID)
			if _, ok := r.PendingWakeupAt(threadID); ok {
				t.Fatalf("%s left the previous process's wakeup or jobs armed", name)
			}
			if r.AnyUnfinishedWork(time.Minute) {
				t.Fatalf("%s left unfinished work behind", name)
			}
		})
	}
}

// Recurring jobs keep the idle reaper away but never hold a transfer or
// an update: both end the session and its jobs either way. A one-shot
// job and the ScheduleWakeup timer are turns the session still owes.
func TestRecurringJobsProtectOnlyAgainstTheReaper(t *testing.T) {
	r := NewRouter(nil, func(eventchan.Channel, any) {})
	const threadID = "thread-cron-recurring"
	recurringAt := time.Now().Add(50 * time.Minute).UnixMilli()
	oneShotAt := time.Now().Add(20 * time.Minute).UnixMilli()
	recurring := provider.ScheduledJob{ID: "loop", Cron: "*/30 * * * *", Recurring: true, FireByUnixMs: recurringAt}
	if err := r.Handle(scheduledJobsEvent(t, threadID, recurring)); err != nil {
		t.Fatalf("handle jobs: %v", err)
	}
	if at, ok := r.PendingWakeupAt(threadID); !ok || at.UnixMilli() != recurringAt {
		t.Fatalf("reaper deadline = (%d, %v), want the recurring job's %d", at.UnixMilli(), ok, recurringAt)
	}
	if at, ok := r.PendingScheduledWorkAt(threadID); ok {
		t.Fatalf("a recurring job counted as scheduled work until %d", at.UnixMilli())
	}
	if r.AnyUnfinishedWork(time.Minute) {
		t.Fatal("a recurring job blocked updates")
	}
	if got := r.RecurringScheduledJobs(threadID); len(got) != 1 || got[0] != recurring {
		t.Fatalf("RecurringScheduledJobs = %+v, want the armed loop", got)
	}

	oneShot := provider.ScheduledJob{ID: "once", Cron: "0 9 * * 1", FireByUnixMs: oneShotAt}
	if err := r.Handle(scheduledJobsEvent(t, threadID, recurring, oneShot)); err != nil {
		t.Fatalf("handle jobs: %v", err)
	}
	if at, ok := r.PendingScheduledWorkAt(threadID); !ok || at.UnixMilli() != oneShotAt {
		t.Fatalf("scheduled work = (%d, %v), want the one-shot's %d", at.UnixMilli(), ok, oneShotAt)
	}
	if !r.AnyUnfinishedWork(time.Minute) {
		t.Fatal("a pending one-shot job did not block updates")
	}
	if got := r.RecurringScheduledJobs(threadID); len(got) != 1 || got[0].ID != "loop" {
		t.Fatalf("RecurringScheduledJobs = %+v, want only the loop", got)
	}
}
