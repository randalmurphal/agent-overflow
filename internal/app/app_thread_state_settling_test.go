package app

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/reporoot"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// The snapshots both live-state reads give while a turn settles, as
// TestThreadStateWhileTurnSettles captures them. The frontend's tests run
// its hydrators on them (threadLiveActivity.test.ts, thread.svelte.test.ts).
// Regenerate with UPDATE_LIVE_SNAPSHOT_FIXTURES=1.
const turnSettlingSnapshotFixture = "internal/app/testdata/turn_settling_snapshot.json"

// The fixture's names for what a run makes up.
const (
	settlingThreadID = "settling-thread"
	settlingRoundID  = "settling-round"
	settlingStarted  = 1_000
)

// A turn completing normally reads as running from the provider's
// completion until its row settles, and as idle once it has: never as
// interrupted, and a thread_status wait does not rest on it in between.
// The completion is held at the write of the turn's end (SetTurnSettleGate),
// after triage has taken the round. Both live-state reads report the round
// there; what they report is the fixture the frontend's hydrators run on.
func TestThreadStateWhileTurnSettles(t *testing.T) {
	q := newQueuedRequests(t)
	tools := q.f.app.threadTools()
	ctx := t.Context()

	holding, release := make(chan struct{}), make(chan struct{})
	released := false
	releaseSettle := func() {
		if !released {
			released = true
			close(release)
		}
	}
	t.Cleanup(releaseSettle)
	q.f.app.triage.SetTurnSettleGate(func(threadID string) {
		if threadID == q.target.ID {
			close(holding)
			<-release
		}
	})
	completed := make(chan error, 1)
	go func() {
		evt := provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: q.target.ID, SessionToken: q.session, Timestamp: time.Now(),
			TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
		}
		completed <- q.f.app.triage.Handle(evt)
	}()
	<-holding

	// thread_status, the read a wait on thread ids polls.
	status, err := tools.threadStateNow(ctx, q.target.ID)
	if err != nil {
		t.Fatalf("threadStateNow: %v", err)
	}
	// thread_show and thread_search read the row, then the live state.
	shown := stateAsShown(ctx, t, tools, q.target.ID)
	// A spawn's or a send's wait settles on the thread's rest.
	rest, restErr := q.f.app.threadRest(q.target.ID)
	liveState, err := q.f.app.GetThreadLiveState(q.target.ID)
	if err != nil {
		t.Fatalf("GetThreadLiveState: %v", err)
	}
	activity, err := q.f.app.ListThreadLiveActivity()
	if err != nil {
		t.Fatalf("ListThreadLiveActivity: %v", err)
	}
	releaseSettle()
	if err := <-completed; err != nil {
		t.Fatalf("turn complete: %v", err)
	}
	if status.State != threadtools.StateRunning || status.Resting {
		t.Errorf("thread_status while the turn settles = %q, resting %v; want running, not resting", status.State, status.Resting)
	}
	if shown != threadtools.StateRunning {
		t.Errorf("thread_show while the turn settles = %q, want running", shown)
	}
	if restErr != nil || rest.unrest != workerRunning {
		t.Errorf("rest while the turn settles = %+v (%v), want running", rest, restErr)
	}
	checkTurnSettlingSnapshot(t, q.target.ID, liveState, activity)

	settled, err := tools.threadStateNow(ctx, q.target.ID)
	if err != nil {
		t.Fatalf("threadStateNow after the settle: %v", err)
	}
	if settled.State != threadtools.StateIdle || !settled.Resting {
		t.Errorf("thread_status once the turn settled = %q, resting %v; want idle and resting", settled.State, settled.Resting)
	}
	if after, err := q.f.app.GetThreadLiveState(q.target.ID); err != nil || after.ActiveTurn != nil {
		t.Errorf("GetThreadLiveState once the turn settled = %+v, %v; want no round", after.ActiveTurn, err)
	}
}

// checkTurnSettlingSnapshot compares what the two reads gave in the window
// with the committed fixture, under the fixture's names: both must carry
// the round, so a client that reads either one mid-settle shows the turn
// running.
func checkTurnSettlingSnapshot(t *testing.T, threadID string, liveState ThreadLiveState, activity []ThreadLiveActivity) {
	t.Helper()
	var row *ThreadLiveActivity
	for i := range activity {
		if activity[i].ThreadID == threadID {
			row = &activity[i]
		}
	}
	if liveState.ActiveTurn == nil || row == nil || row.ActiveTurn == nil || *row.ActiveTurn != *liveState.ActiveTurn {
		t.Fatalf("live state round %+v, activity row %+v; want both to carry the settling round", liveState.ActiveTurn, row)
	}
	checkLiveSnapshotFixture(t, turnSettlingSnapshotFixture, []string{
		"GetThreadLiveState and the thread's ListThreadLiveActivity row while a",
		"turn's end is written, captured by TestThreadStateWhileTurnSettles",
		"(internal/app/app_thread_state_settling_test.go) under fixed names.",
		"Regenerate with UPDATE_LIVE_SNAPSHOT_FIXTURES=1.",
	}, liveState, *row)
}

// checkLiveSnapshotFixture compares a thread's two live-state reads with
// the committed fixture, under the fixture's names for the thread and its
// round.
func checkLiveSnapshotFixture(t *testing.T, fixture string, comment []string, liveState ThreadLiveState, row ThreadLiveActivity) {
	t.Helper()
	rename := func(turn *LiveStateActiveTurn) {
		if turn != nil {
			turn.ThreadID, turn.TurnID, turn.StartedAt = settlingThreadID, settlingRoundID, settlingStarted
		}
	}
	liveState.ThreadID = settlingThreadID
	rename(liveState.ActiveTurn)
	row.ThreadID = settlingThreadID
	rename(row.ActiveTurn)
	encoded, err := json.MarshalIndent(map[string]any{
		"comment":      comment,
		"liveState":    liveState,
		"liveActivity": row,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if os.Getenv("UPDATE_LIVE_SNAPSHOT_FIXTURES") != "" {
		if err := os.WriteFile(reporoot.Path(t, fixture), encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	committed, err := os.ReadFile(reporoot.Path(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != string(encoded) {
		t.Fatalf("%s is stale: regenerate it and check the frontend tests agree. Captured:\n%s", fixture, encoded)
	}
}

// An interrupted turn reads as interrupted once its row records it, rests,
// and reads as idle once the user has read the thread.
func TestThreadStateOfInterruptedTurn(t *testing.T) {
	q := newQueuedRequests(t)
	tools := q.f.app.threadTools()
	ctx := t.Context()
	q.handle(t, "turn 0 aborted", provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn", Aborted: true},
	})
	wantThreadState(ctx, t, tools, q.target.ID, threadtools.StateInterrupted, true)
	if _, _, err := q.f.app.store.MarkThreadReadNow(ctx, q.target.ID); err != nil {
		t.Fatalf("MarkThreadReadNow: %v", err)
	}
	wantThreadState(ctx, t, tools, q.target.ID, threadtools.StateIdle, true)
}

// A second completion of a settled turn (a provider's synthetic abort
// after its real result) folds into the row and leaves how it ended.
func TestThreadStateAfterLateCompletion(t *testing.T) {
	q := newQueuedRequests(t)
	tools := q.f.app.threadTools()
	ctx := t.Context()
	q.handle(t, "turn 0 complete", provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
	})
	q.handle(t, "turn 0 late abort", provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{Aborted: true},
	})
	wantThreadState(ctx, t, tools, q.target.ID, threadtools.StateIdle, true)
	turn, found, err := q.f.app.store.GetTurnByThreadIndex(q.target.ID, 0)
	if err != nil || !found {
		t.Fatalf("GetTurnByThreadIndex = found %v, %v", found, err)
	}
	if turn.StopReason != "end_turn" {
		t.Errorf("stop_reason after the late abort = %q, want end_turn", turn.StopReason)
	}
}

// A turn the app crashed in leaves its row open with no provider round:
// it reads as running, not interrupted, until the boot sweep records the
// interruption, and as idle once the user has read the thread.
func TestThreadStateOfCrashLeftTurn(t *testing.T) {
	f := newRequestFixture(t)
	tools := f.app.threadTools()
	ctx := t.Context()
	thread, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// A new thread is read as of its creation, and the sweep ends a turn
	// with no items at its start: unread here, so a turn started in the
	// creation's millisecond is still one the user has not read.
	if _, _, err := f.app.store.MarkThreadUnread(thread.ID); err != nil {
		t.Fatalf("MarkThreadUnread: %v", err)
	}
	if err := f.app.store.InsertTurn(store.Turn{
		TurnID: "crashed-turn", ThreadID: thread.ID, TurnIndex: 0, StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	wantThreadState(ctx, t, tools, thread.ID, threadtools.StateRunning, false)
	if _, err := f.app.triage.RecoverCrashedTurns(); err != nil {
		t.Fatalf("RecoverCrashedTurns: %v", err)
	}
	wantThreadState(ctx, t, tools, thread.ID, threadtools.StateInterrupted, true)
	if _, _, err := f.app.store.MarkThreadReadNow(ctx, thread.ID); err != nil {
		t.Fatalf("MarkThreadReadNow: %v", err)
	}
	wantThreadState(ctx, t, tools, thread.ID, threadtools.StateIdle, true)
}

// A completion whose settle write fails leaves the row open: the thread
// reads as running, and a wait on it does not rest, until the next boot's
// sweep records the turn as interrupted (store/turn_newest.go).
func TestThreadStateWhenSettleWriteFails(t *testing.T) {
	q := newQueuedRequests(t)
	tools := q.f.app.threadTools()
	ctx := t.Context()
	execOnStore(t, q.f.dbPath, `CREATE TRIGGER refuse_turn_settle BEFORE UPDATE OF completed_at ON turns
		WHEN NEW.completed_at IS NOT NULL BEGIN SELECT RAISE(ABORT, 'injected settle failure'); END`)
	q.handle(t, "turn 0 complete", provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
	})
	if q.f.app.triage.LiveStateSnapshotForThread(q.target.ID).ActiveTurn != nil {
		t.Fatal("the completion left its round open")
	}
	wantThreadState(ctx, t, tools, q.target.ID, threadtools.StateRunning, false)
	execOnStore(t, q.f.dbPath, `DROP TRIGGER refuse_turn_settle`)
	if _, err := q.f.app.triage.RecoverCrashedTurns(); err != nil {
		t.Fatalf("RecoverCrashedTurns: %v", err)
	}
	wantThreadState(ctx, t, tools, q.target.ID, threadtools.StateInterrupted, true)
}

// A worker's completion ends its round before the frame that reassesses
// it goes out: a reassessment that runs while the frame is held, the
// liveness drain the frame's tap started, records the worker at rest, not
// running, and nothing later has to correct it.
func TestAWorkerReassessedOnItsCompletionReadsTheRoundEnded(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.settledSpawn(t, "completing-spawn", f.caller.ID)
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: worker.ID + ":t1"}, nil)
	f.awaitWorker(t, f.caller.ID, "completing-spawn", workerRunning)

	held := false
	f.bus.mu.Lock()
	f.bus.tap = func(name eventchan.Channel, data any) {
		f.app.tapThreadAgentRows(name, data)
		if event, ok := data.(triage.TurnCompletedEvent); ok && !held && event.ThreadID == worker.ID {
			held = true
			// The frame's tap has queued the worker's reassessment; it
			// runs to its end before the frame is let go.
			waitAgentEmitsIdle(t, f.app)
		}
	}
	f.bus.mu.Unlock()
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}}, nil)
	if !held {
		t.Fatal("the completion's frame was never held")
	}
	waitAgentEmitsIdle(t, f.app)
	f.app.liveWorkers.mu.Lock()
	recorded, live := f.app.liveWorkers.threads[worker.ID]
	f.app.liveWorkers.mu.Unlock()
	if live {
		t.Fatalf("worker recorded %+v after its completion; want it at rest", recorded)
	}
}

// thread_status reads the state, then the rest: a turn that starts between
// the two reads is running, not a thread at rest.
func TestThreadStatusOfATurnStartedBetweenItsReads(t *testing.T) {
	q := newQueuedRequests(t)
	tools := q.f.app.threadTools()
	ctx := t.Context()
	q.handle(t, "turn 0 complete", provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
	})
	wantThreadState(ctx, t, tools, q.target.ID, threadtools.StateIdle, true)
	q.f.app.threadStateReadForTest = func(threadID string) {
		q.f.app.threadStateReadForTest = nil
		q.handle(t, "turn 1 start", provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: 1})
	}
	status, err := tools.threadStateNow(ctx, q.target.ID)
	if err != nil {
		t.Fatalf("threadStateNow: %v", err)
	}
	if status.State != threadtools.StateRunning || status.Resting {
		t.Fatalf("thread_status across a turn start = %q, resting %v; want running, not resting", status.State, status.Resting)
	}
}

// wantThreadState checks threadID's state as thread_status and thread_show
// report it, and whether a wait on it rests.
func wantThreadState(ctx context.Context, t *testing.T, tools threadToolsApp, threadID, want string, resting bool) {
	t.Helper()
	status, err := tools.threadStateNow(ctx, threadID)
	if err != nil {
		t.Fatalf("threadStateNow: %v", err)
	}
	if status.State != want || status.Resting != resting {
		t.Errorf("thread_status = %q, resting %v; want %q, resting %v", status.State, status.Resting, want, resting)
	}
	if shown := stateAsShown(ctx, t, tools, threadID); shown != want {
		t.Errorf("thread_show = %q, want %q", shown, want)
	}
}

// stateAsShown is the state thread_show reports: the thread's row, then its
// live state, through the App the tools read.
func stateAsShown(ctx context.Context, t *testing.T, tools threadToolsApp, threadID string) string {
	t.Helper()
	thread, err := tools.Thread(ctx, threadID)
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	live, err := tools.LiveState(ctx, threadID)
	if err != nil {
		t.Fatalf("LiveState: %v", err)
	}
	return threadtools.State(thread, live)
}
