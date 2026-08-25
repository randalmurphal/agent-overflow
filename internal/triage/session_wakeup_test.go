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
		r.MarkThreadActive(threadID)
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
