package app

import (
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// A change frame carries the row as it stands: registered and pending
// reminders whole, a cancelled row or a fired reminder as removed, and a
// thread-wide cancel as one removal per row.
func TestWakeChangesCarryTheRow(t *testing.T) {
	t.Parallel()
	a, rec := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "changes-row")
	next := time.Now().Add(time.Hour).UnixMilli()

	loop := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: next})
	changes := wakeChanges(rec, thread.ID)
	if len(changes) != 1 || changes[0].Row == nil || changes[0].ID != loop.ID ||
		changes[0].Row.Kind != store.AgentWakeLoop || changes[0].Row.Label != "board" || changes[0].Row.NextAt != next || changes[0].Removed {
		t.Fatalf("register frames = %+v", changes)
	}

	insertTestReminder(t, a, "changes-remind", thread.ID, "deploy check\nmore", next)
	a.emitThreadReminderChanged(thread.ID, "changes-remind")
	if got := wakeChanges(rec, thread.ID)[1]; got.Row == nil || got.Row.Kind != threadWakeKindReminder || got.Row.Label != "deploy check" || got.Row.NextAt != next {
		t.Fatalf("reminder frame = %+v", got)
	}
	reminder, _, err := a.store.GetThreadRequest("changes-remind")
	if err != nil {
		t.Fatal(err)
	}
	a.settleThreadReminder(reminder, time.Now())
	if got := wakeChanges(rec, thread.ID)[2]; got != (ThreadWakeChange{ThreadID: thread.ID, ID: "changes-remind", Removed: true}) {
		t.Fatalf("fired reminder frame = %+v", got)
	}

	watch := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "poll", Command: "true", IntervalSeconds: 3600, NextAt: next})
	before := len(wakeChanges(rec, thread.ID))
	a.cancelAgentWakesForThread(thread.ID, "cancelled: archived")
	removed := map[string]bool{}
	for _, change := range wakeChanges(rec, thread.ID)[before:] {
		if !change.Removed || change.Row != nil {
			t.Fatalf("thread-wide cancel frame = %+v", change)
		}
		removed[change.ID] = true
	}
	if len(removed) != 2 || !removed[loop.ID] || !removed[watch.ID] {
		t.Fatalf("removed = %v, want the loop and the watch", removed)
	}
}

// The sidebar's read lists every standing agent's live loops and watches
// by thread, and nothing of a thread that is not one.
func TestListStandingAgentWakesListsAgentsOnly(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	first := noticeTestThread(t, a, "changes-agent-a")
	second := noticeTestThread(t, a, "changes-agent-b")
	plain := noticeTestThread(t, a, "changes-plain")
	now := time.Now()
	for _, id := range []string{first.ID, second.ID} {
		if _, _, err := a.store.PromoteStandingAgent(id, t.TempDir(), now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	next := now.Add(time.Hour).UnixMilli()
	registerTestWake(t, a, store.AgentWake{ThreadID: first.ID, Kind: store.AgentWakeLoop, Label: "z-board", Note: "n", IntervalSeconds: 3600, NextAt: next})
	registerTestWake(t, a, store.AgentWake{ThreadID: first.ID, Kind: store.AgentWakeWatch, Label: "a-poll", Command: "true", IntervalSeconds: 3600, NextAt: next})
	registerTestWake(t, a, store.AgentWake{ThreadID: second.ID, Kind: store.AgentWakeLoop, Label: "digest", Note: "n", IntervalSeconds: 3600, NextAt: next})
	registerTestWake(t, a, store.AgentWake{ThreadID: plain.ID, Kind: store.AgentWakeLoop, Label: "plain", Note: "n", IntervalSeconds: 3600, NextAt: next})
	if _, _, err := a.cancelAgentWake(second.ID, "digest", store.AgentWakeLoop); err != nil {
		t.Fatal(err)
	}
	registerTestWake(t, a, store.AgentWake{ThreadID: second.ID, Kind: store.AgentWakeLoop, Label: "standup", Note: "n", IntervalSeconds: 3600, NextAt: next})

	agents, err := a.ListStandingAgentWakes()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, agent := range agents {
		for _, row := range agent.Rows {
			got[agent.ThreadID] = append(got[agent.ThreadID], row.Kind+":"+row.Label)
		}
	}
	if len(agents) != 2 || len(got) != 2 ||
		len(got[first.ID]) != 2 || got[first.ID][0] != "watch:a-poll" || got[first.ID][1] != "loop:z-board" ||
		len(got[second.ID]) != 1 || got[second.ID][0] != "loop:standup" {
		t.Fatalf("agents = %v", got)
	}
}
