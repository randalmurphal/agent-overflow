package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
)

// An archive cancels the loops and watches the thread had when it
// committed. A thread unarchived before the rest of that archive runs keeps
// what it registered since: a new label, and a long-running watch
// re-registered under the label the archive cancelled, process included.
// Both archive doors behave the same.
func TestAnArchiveLeavesSchedulesRegisteredAfterTheThreadCameBack(t *testing.T) {
	t.Parallel()
	for _, door := range []struct {
		name    string
		archive func(t *testing.T, a *App, threadID string) restingThreadWork
	}{
		{"binding", func(t *testing.T, a *App, threadID string) restingThreadWork {
			work, err := a.archiveThreadLocked(threadID, "")
			if err != nil {
				t.Fatalf("archiveThreadLocked: %v", err)
			}
			return work
		}},
		{"thread_update", func(t *testing.T, a *App, threadID string) restingThreadWork {
			archived := true
			_, work, err := a.applyThreadOrganizePatchLocked(context.Background(), threadID, threadapp.OrganizePatch{Archived: &archived})
			if err != nil || work == nil {
				t.Fatalf("applyThreadOrganizePatchLocked: work %v, %v", work, err)
			}
			return *work
		}},
	} {
		t.Run(door.name, func(t *testing.T) {
			t.Parallel()
			a, _ := newAppForFlushQueueRPC(t)
			a.startSessionFn = func(string) error { return nil }
			thread := noticeTestThread(t, a, "archive-wakes-"+door.name)
			later := time.Now().Add(time.Hour).UnixMilli()
			old := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 3600, NextAt: later})
			tail := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "tail", Command: "sleep 60", LongRunning: true})

			work := door.archive(t, a, thread.ID)
			if _, err := a.UnarchiveThread(thread.ID); err != nil {
				t.Fatalf("UnarchiveThread: %v", err)
			}
			fresh := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "fresh", Note: "n", IntervalSeconds: 3600, NextAt: later})
			again := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "tail", Command: "sleep 60", LongRunning: true})
			if again.ID != tail.ID {
				t.Fatalf("re-registered watch id %s, want the label's row %s", again.ID, tail.ID)
			}
			a.agentWakes.mu.Lock()
			replacement := a.agentWakes.runs[tail.ID]
			a.agentWakes.mu.Unlock()
			if replacement == nil {
				t.Fatal("the re-registered watch has no process")
			}

			if err := a.stopRestingThreadWork(work); err != nil {
				t.Fatalf("stopRestingThreadWork: %v", err)
			}

			for _, want := range []struct {
				id        string
				cancelled bool
			}{{old.ID, true}, {fresh.ID, false}, {tail.ID, false}} {
				row, found, err := a.store.GetAgentWake(want.id)
				if err != nil || !found {
					t.Fatalf("GetAgentWake %s: found %v, %v", want.id, found, err)
				}
				if cancelled := row.State == store.AgentWakeCancelled; cancelled != want.cancelled {
					t.Errorf("%s %q state %s, want cancelled %v", row.Kind, row.Label, row.State, want.cancelled)
				}
			}
			a.agentWakes.mu.Lock()
			current := a.agentWakes.runs[tail.ID]
			a.agentWakes.mu.Unlock()
			if current != replacement || replacement.wasStopped() {
				t.Errorf("the re-registered watch's process was stopped by the earlier archive")
			}
			a.stopAgentWakeRunsForThread(thread.ID)
		})
	}
}

// A cancel that lands while a fire has claimed its watch but not yet
// registered the run still stops that run, whichever door cancels: the
// cancel waits for the registration, so no run outlives it uncancelled.
func TestACancelDuringAFiresAdmissionStopsItsRun(t *testing.T) {
	t.Parallel()
	for _, door := range []struct {
		name   string
		cancel func(a *App, row store.AgentWake) error
	}{
		{"archive", func(a *App, row store.AgentWake) error { return archiveThread(a, row.ThreadID) }},
		{"thread_cancel", func(a *App, row store.AgentWake) error {
			_, _, err := a.cancelAgentWake(row.ThreadID, row.Label, row.Kind)
			return err
		}},
		{"move", func(a *App, row store.AgentWake) error {
			return a.cancelAgentWakesForThread(row.ThreadID, "cancelled: the thread was moved to another computer")
		}},
	} {
		t.Run(door.name, func(t *testing.T) {
			t.Parallel()
			a, _ := newAppForFlushQueueRPC(t)
			a.startSessionFn = func(string) error { return nil }
			thread := noticeTestThread(t, a, "admission-"+door.name)
			row := registerTestWake(t, a, store.AgentWake{
				ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "poll", Command: "true",
				IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli(),
			})
			// Every slot taken: an admitted run waits for one until it is
			// stopped, so it stays registered for the check below.
			slots := a.agentWakeSlots()
			for range cap(slots) {
				slots <- struct{}{}
			}
			defer func() {
				for range cap(slots) {
					<-slots
				}
			}()
			cancelled := make(chan error, 1)
			a.agentWakeClaimedForTest = func(store.AgentWake) {
				go func() { cancelled <- door.cancel(a, row) }()
				// The cancel cannot finish here: it waits for the
				// registration. Give it the time it would take otherwise.
				select {
				case err := <-cancelled:
					cancelled <- err
				case <-time.After(200 * time.Millisecond):
				}
			}
			if err := a.fireAgentWake(row, time.UnixMilli(row.NextAt+1), agentWakeFire{at: row.NextAt + 1}); err != nil {
				t.Fatal("the fire did not start")
			}
			if err := <-cancelled; err != nil {
				t.Fatalf("cancel: %v", err)
			}
			a.agentWakes.mu.Lock()
			run := a.agentWakes.runs[row.ID]
			a.agentWakes.mu.Unlock()
			if run != nil && !run.wasStopped() {
				t.Fatal("the run admitted around the cancel is still running")
			}
			stored, found, err := a.store.GetAgentWake(row.ID)
			if err != nil || !found || stored.State != store.AgentWakeCancelled {
				t.Fatalf("row %+v found %v, %v; want cancelled", stored, found, err)
			}
		})
	}
}

// An archive whose loops and watches could not be cancelled says so, though
// the archive itself landed, and still stops what they had running.
func TestAnArchiveReportsSchedulesItCouldNotCancel(t *testing.T) {
	t.Parallel()
	a, dbPath := newTestAppWithStorePath(t)
	a.startSessionFn = func(string) error { return nil }
	thread := noticeTestThread(t, a, "archive-wakes-refused")
	tail := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "tail", Command: "sleep 60", LongRunning: true})
	a.agentWakes.mu.Lock()
	run := a.agentWakes.runs[tail.ID]
	a.agentWakes.mu.Unlock()
	if run == nil {
		t.Fatal("the watch has no process")
	}
	execOnStore(t, dbPath, `CREATE TRIGGER refuse_wake_cancel BEFORE UPDATE OF state ON agent_wakes
		WHEN NEW.state = 'cancelled' BEGIN SELECT RAISE(ABORT, 'cancel refused'); END`)

	err := archiveThread(a, thread.ID)
	if err == nil || !strings.Contains(err.Error(), "cancel the loops and watches of "+thread.ID) {
		t.Fatalf("ArchiveThread: %v, want the failed cancel reported", err)
	}
	if row, err := a.store.GetThread(thread.ID); err != nil || !row.Archived {
		t.Fatalf("thread archived %v, %v; want the archive kept", row.Archived, err)
	}
	if !run.wasStopped() {
		t.Error("the watch's process was left running")
	}
}
