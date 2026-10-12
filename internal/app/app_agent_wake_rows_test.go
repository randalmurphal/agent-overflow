package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
)

// wakeChanges lists the thread:wakes_changed frames naming threadID.
func wakeChanges(rec *emitRecorder, threadID string) []ThreadWakeChange {
	var out []ThreadWakeChange
	for _, call := range rec.snapshot() {
		if call.Channel != eventchan.ThreadWakesChanged.String() {
			continue
		}
		if change, ok := call.Data.(ThreadWakeChange); ok && change.ThreadID == threadID {
			out = append(out, change)
		}
	}
	return out
}

// wakeRowsChanged counts the thread:wakes_changed frames naming threadID.
func wakeRowsChanged(rec *emitRecorder, threadID string) int {
	return len(wakeChanges(rec, threadID))
}

func registerTestWake(t *testing.T, a *App, row store.AgentWake) store.AgentWake {
	t.Helper()
	stored, err := a.registerAgentWake(row)
	if err != nil {
		t.Fatalf("register %s %q: %v", row.Kind, row.Label, err)
	}
	return stored
}

func insertTestReminder(t *testing.T, a *App, token, threadID, note string, due int64) {
	t.Helper()
	if err := a.store.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: threadID, Kind: store.ThreadRequestRemind, DueAt: due,
		Answer: []byte(note), AnswerKind: store.ThreadAnswerNote, Notify: true, State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatalf("insert reminder: %v", err)
	}
}

// The tray lists a thread's live loops and watches by label, then its
// reminders soonest first, each reminder named by its note's first line.
func TestListThreadWakesListsLoopsWatchesAndReminders(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "rows-list")
	other := noticeTestThread(t, a, "rows-other")
	later := time.Now().Add(time.Hour).UnixMilli()
	registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "poll", Command: "true", IntervalSeconds: 3600, TimeoutSeconds: 600, NextAt: later})
	registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "check", IntervalSeconds: 3600, NextAt: later})
	registerTestWake(t, a, store.AgentWake{ThreadID: other.ID, Kind: store.AgentWakeLoop, Label: "elsewhere", Note: "n", IntervalSeconds: 3600, NextAt: later})
	insertTestReminder(t, a, "rows-remind-late", thread.ID, "deploy check\nsecond line", later+2000)
	insertTestReminder(t, a, "rows-remind-soon", thread.ID, "  CI run  ", later+1000)

	wakes, err := a.ListThreadWakes(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, row := range wakes.Rows {
		got = append(got, row.Kind+":"+row.Label)
	}
	if want := "loop:board watch:poll reminder:CI run reminder:deploy check"; strings.Join(got, " ") != want {
		t.Fatalf("rows = %q, want %q", strings.Join(got, " "), want)
	}
	if wakes.MoreReminders != 0 || wakes.Rows[2].ID != "rows-remind-soon" || wakes.Rows[2].NextAt != later+1000 {
		t.Fatalf("reminder row = %+v, more = %d", wakes.Rows[2], wakes.MoreReminders)
	}
}

// An expanded watch shows its command, the directory it runs in and its
// stderr tail; a loop runs no command.
func TestThreadWakeDetailResolvesTheWatchDirectory(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "rows-detail")
	next := time.Now().Add(time.Hour).UnixMilli()
	plain := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "plain", Command: "sh poll.sh", IntervalSeconds: 60, NextAt: next})
	sub := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "sub", Command: "true", Cwd: "scripts", IntervalSeconds: 60, NextAt: next})
	loop := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 60, NextAt: next})
	if _, err := a.store.NoteAgentWakeStderr(plain.ID, "poll: checked\n"); err != nil {
		t.Fatal(err)
	}
	detail, err := a.GetThreadWakeDetail(thread.ID, plain.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Command != "sh poll.sh" || detail.Cwd != thread.WorkspacePath || detail.Stderr != "poll: checked\n" {
		t.Fatalf("detail = %+v", detail)
	}
	if detail, err := a.GetThreadWakeDetail(thread.ID, sub.ID); err != nil || detail.Cwd != filepath.Join(thread.WorkspacePath, "scripts") {
		t.Fatalf("relative cwd detail = %+v, %v", detail, err)
	}
	if _, err := a.GetThreadWakeDetail(thread.ID, loop.ID); err == nil {
		t.Fatal("a loop's detail was served")
	}
	if _, err := a.GetThreadWakeDetail("rows-elsewhere", plain.ID); err == nil {
		t.Fatal("another thread read the watch's detail")
	}
}

// Wake now is refused with the reason for a row that cannot fire now, and
// for a thread that is archived.
func TestWakeNowRefusesWhatCannotFire(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	ctx := context.Background()
	thread := noticeTestThread(t, a, "rows-refuse")
	next := time.Now().Add(time.Hour).UnixMilli()
	loop := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 60, NextAt: next})
	long := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "tail", Command: "true", LongRunning: true})
	a.stopAgentWakeRunsForThread(thread.ID)

	cases := []struct {
		name, kind, id, want string
	}{
		{"another thread's row", store.AgentWakeLoop, "missing", "no longer scheduled"},
		{"the wrong kind", store.AgentWakeWatch, loop.ID, `the loop "board" is not a watch`},
		{"a long-running watch", store.AgentWakeWatch, long.ID, "has no fire to bring forward"},
	}
	for _, tc := range cases {
		if err := a.WakeThreadWakeNow(ctx, thread.ID, tc.kind, tc.id); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}

	if claimed, err := a.store.ClaimAgentWake(loop.ID, time.Now().UnixMilli(), next, 0); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if err := a.WakeThreadWakeNow(ctx, thread.ID, store.AgentWakeLoop, loop.ID); err == nil || !strings.Contains(err.Error(), `the loop "board" is already running`) {
		t.Errorf("a running loop: %v", err)
	}
	if _, _, err := a.cancelAgentWake(thread.ID, "board", store.AgentWakeLoop); err != nil {
		t.Fatal(err)
	}
	if err := a.WakeThreadWakeNow(ctx, thread.ID, store.AgentWakeLoop, loop.ID); err == nil || !strings.Contains(err.Error(), `the loop "board" was cancelled`) {
		t.Errorf("a cancelled loop: %v", err)
	}

	insertTestReminder(t, a, "rows-refuse-remind", thread.ID, "later", next)
	if _, _, err := a.store.ArchiveThread(thread.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.WakeThreadWakeNow(ctx, thread.ID, threadWakeKindReminder, "rows-refuse-remind"); err == nil || !strings.Contains(err.Error(), "archived") {
		t.Errorf("an archived thread's reminder: %v", err)
	}
	if row, found, err := a.store.GetThreadRequest("rows-refuse-remind"); err != nil || !found || row.State != store.ThreadRequestAccepted {
		t.Fatalf("the refused reminder = %+v found=%v err=%v; want it still waiting", row, found, err)
	}
}

// Wake now on an armed watch is the sweep's fire: the row is claimed, runs
// and records its outcome and stderr, its next fire is unchanged, and every
// step tells the tray.
func TestWakeNowFiresAWatchThroughTheSweep(t *testing.T) {
	t.Parallel()
	a, rec := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "rows-fire")
	home := t.TempDir()
	if _, _, err := a.store.PromoteStandingAgent(thread.ID, home, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	next := time.Now().Add(time.Hour).UnixMilli()
	watch := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "quiet",
		Command: "echo 'quiet: nothing new' >&2", IntervalSeconds: 3600, TimeoutSeconds: 60, NextAt: next})
	before := wakeRowsChanged(rec, thread.ID)

	if err := a.WakeThreadWakeNow(context.Background(), thread.ID, store.AgentWakeWatch, watch.ID); err != nil {
		t.Fatalf("wake now: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var row store.AgentWake
	for {
		var err error
		row, _, err = a.store.GetAgentWake(watch.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.State == store.AgentWakeArmed && row.LastRunAt != 0 && row.LastOutcome != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run did not finish: %+v", row)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.HasPrefix(row.LastOutcome, agentWakeOutcomeNoOutput) || row.LastStderr != "quiet: nothing new\n" || row.NextAt != next {
		t.Fatalf("row after wake now = %+v; want a quiet run, its stderr kept and the schedule unchanged", row)
	}
	// Claim, stderr and finish.
	changes := wakeChanges(rec, thread.ID)[before:]
	if len(changes) < 3 || changes[0].Row == nil || changes[0].Row.State != store.AgentWakeRunning {
		t.Fatalf("thread:wakes_changed frames = %+v, want the claim, the stderr and the finish, the claim running", changes)
	}
	if last := changes[len(changes)-1].Row; last == nil || last.State != store.AgentWakeArmed || last.LastOutcome != row.LastOutcome || last.NextAt != next {
		t.Fatalf("the last frame's row = %+v, want the finished row", last)
	}
}

// Cancel from the tray is thread_cancel's cancel: a loop by its id, a
// reminder by its token, and a row already gone is not an error. Each change
// tells the tray.
func TestCancelThreadWakeCancelsLikeTheTool(t *testing.T) {
	t.Parallel()
	a, rec := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "rows-cancel")
	next := time.Now().Add(time.Hour).UnixMilli()
	loop := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 60, NextAt: next})
	insertTestReminder(t, a, "rows-cancel-remind", thread.ID, "later", next)
	before := wakeRowsChanged(rec, thread.ID)

	if err := a.CancelThreadWake("rows-elsewhere", store.AgentWakeLoop, loop.ID); err != nil {
		t.Fatalf("another thread's cancel: %v, want nothing to cancel", err)
	}
	if row, _, _ := a.store.GetAgentWake(loop.ID); row.State != store.AgentWakeArmed {
		t.Fatalf("another thread cancelled the loop: %+v", row)
	}
	if err := a.CancelThreadWake(thread.ID, store.AgentWakeLoop, loop.ID); err != nil {
		t.Fatal(err)
	}
	if row, _, _ := a.store.GetAgentWake(loop.ID); row.State != store.AgentWakeCancelled || row.LastOutcome != agentWakeOutcomeCancelled {
		t.Fatalf("loop after cancel = %+v", row)
	}
	if err := a.CancelThreadWake(thread.ID, store.AgentWakeLoop, loop.ID); err != nil {
		t.Fatalf("a second cancel: %v", err)
	}
	if err := a.CancelThreadWake(thread.ID, threadWakeKindReminder, "rows-cancel-remind"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := a.store.GetThreadRequest("rows-cancel-remind"); err != nil || found {
		t.Fatalf("reminder after cancel: found=%v err=%v", found, err)
	}
	changes := wakeChanges(rec, thread.ID)[before:]
	if len(changes) != 2 || changes[0] != (ThreadWakeChange{ThreadID: thread.ID, ID: loop.ID, Removed: true}) ||
		changes[1] != (ThreadWakeChange{ThreadID: thread.ID, ID: "rows-cancel-remind", Removed: true}) {
		t.Fatalf("thread:wakes_changed frames = %+v, want the loop and the reminder removed", changes)
	}
	if wakes, err := a.ListThreadWakes(thread.ID); err != nil || len(wakes.Rows) != 0 {
		t.Fatalf("rows after the cancels = %+v, %v", wakes, err)
	}
}
