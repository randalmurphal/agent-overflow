package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// The tray lists a thread's 64 soonest reminders and counts the rest.
func TestListThreadWakesBoundsItsReminders(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "guards-reminders")
	base := time.Now().Add(time.Hour).UnixMilli()
	// The bound is the spec's, 64, written out so a change to the constant
	// fails here.
	const shown, total = 64, 70
	// Inserted latest first, so the order listed is the due order's, not
	// the insertion's.
	for i := total - 1; i >= 0; i-- {
		insertTestReminder(t, a, fmt.Sprintf("guards-remind-%03d", i), thread.ID, fmt.Sprintf("note %d", i), base+int64(i)*1000)
	}
	wakes, err := a.ListThreadWakes(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(wakes.Rows) != shown || wakes.MoreReminders != total-shown {
		t.Fatalf("rows = %d, more = %d; want %d and %d", len(wakes.Rows), wakes.MoreReminders, shown, total-shown)
	}
	for i, row := range wakes.Rows {
		if want := fmt.Sprintf("guards-remind-%03d", i); row.ID != want || row.NextAt != base+int64(i)*1000 {
			t.Fatalf("row %d = %s due %d, want %s due %d: the soonest first", i, row.ID, row.NextAt, want, base+int64(i)*1000)
		}
	}
}

// Wake now on a watch runs a script on this computer, so a session without
// threads:autonomy may fire a loop but not a watch.
func TestWakeNowOnAWatchNeedsAutonomy(t *testing.T) {
	t.Parallel()
	a, granted, ungranted := autonomyApp(t)
	thread := threadInMode(t, a, provider.RuntimeApprovalRequired)
	next := time.Now().Add(time.Hour).UnixMilli()
	watch := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "poll", Command: "true", IntervalSeconds: 3600, TimeoutSeconds: 60, NextAt: next})
	loop := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: next})
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })

	wantScopeRefusal(t, a.WakeThreadWakeNow(callFrom(ungranted, false), thread.ID, store.AgentWakeWatch, watch.ID), transport.ScopeThreadsAutonomy)
	if row, _, _ := a.store.GetAgentWake(watch.ID); row.State != store.AgentWakeArmed || row.LastRunAt != 0 {
		t.Fatalf("the refused watch = %+v; want it untouched", row)
	}
	if err := a.WakeThreadWakeNow(callFrom(ungranted, false), thread.ID, store.AgentWakeLoop, loop.ID); err != nil {
		t.Fatalf("a loop without threads:autonomy: %v", err)
	}
	if err := a.WakeThreadWakeNow(callFrom(granted, false), thread.ID, store.AgentWakeWatch, watch.ID); err != nil {
		t.Fatalf("a watch with threads:autonomy: %v", err)
	}
}

// Wake now drives the thread as a send does: a thread whose runtime mode
// needs threads:autonomy refuses a session without it, for a loop too.
func TestWakeNowFollowsTheThreadsRuntimeMode(t *testing.T) {
	t.Parallel()
	a, granted, ungranted := autonomyApp(t)
	thread := threadInMode(t, a, provider.RuntimeFullAccess)
	loop := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })

	wantScopeRefusal(t, a.WakeThreadWakeNow(callFrom(ungranted, false), thread.ID, store.AgentWakeLoop, loop.ID), transport.ScopeThreadsAutonomy)
	if row, _, _ := a.store.GetAgentWake(loop.ID); row.LastRunAt != 0 {
		t.Fatalf("the refused loop ran: %+v", row)
	}
	if err := a.WakeThreadWakeNow(callFrom(granted, false), thread.ID, store.AgentWakeLoop, loop.ID); err != nil {
		t.Fatalf("a session with threads:autonomy: %v", err)
	}
}

// A row is acted on only through its own thread: thread A's tray cannot
// fire, cancel or read thread B's loop or watch.
func TestWakeRowsStayInTheirThread(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	first := noticeTestThread(t, a, "guards-thread-a")
	second := noticeTestThread(t, a, "guards-thread-b")
	next := time.Now().Add(time.Hour).UnixMilli()
	loop := registerTestWake(t, a, store.AgentWake{ThreadID: second.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: next})
	watch := registerTestWake(t, a, store.AgentWake{ThreadID: second.ID, Kind: store.AgentWakeWatch, Label: "poll", Command: "true", IntervalSeconds: 3600, NextAt: next})
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(second.ID) })

	if err := a.WakeThreadWakeNow(context.Background(), first.ID, store.AgentWakeLoop, loop.ID); err == nil || !strings.Contains(err.Error(), "no longer scheduled") {
		t.Fatalf("wake now from another thread: %v, want it refused as not that thread's", err)
	}
	if row, _, _ := a.store.GetAgentWake(loop.ID); row.State != store.AgentWakeArmed || row.LastRunAt != 0 {
		t.Fatalf("thread B's loop after thread A's wake now = %+v; want it untouched", row)
	}
	if _, err := a.GetThreadWakeDetail(first.ID, watch.ID); err == nil {
		t.Fatal("thread A read thread B's watch")
	}
	if err := a.CancelThreadWake(first.ID, store.AgentWakeLoop, loop.ID); err != nil {
		t.Fatalf("cancel from another thread: %v, want nothing to cancel", err)
	}
	if row, _, _ := a.store.GetAgentWake(loop.ID); row.State != store.AgentWakeArmed {
		t.Fatalf("thread B's loop after thread A's cancel = %+v; want it armed", row)
	}
}
