package app

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
)

// turnSettlingSnapshotFixture is the snapshot both live-state reads give
// while a turn settles, as TestThreadStateWhileTurnSettles captures it. The
// frontend's tests run its hydrators on it (threadLiveActivity.test.ts,
// thread.svelte.test.ts). Regenerate with UPDATE_TURN_SETTLING_SNAPSHOT=1.
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
	rename := func(turn *LiveStateActiveTurn) {
		turn.ThreadID, turn.TurnID, turn.StartedAt = settlingThreadID, settlingRoundID, settlingStarted
	}
	liveState.ThreadID = settlingThreadID
	rename(liveState.ActiveTurn)
	row.ThreadID = settlingThreadID
	rename(row.ActiveTurn)
	encoded, err := json.MarshalIndent(map[string]any{
		"comment": []string{
			"GetThreadLiveState and the thread's ListThreadLiveActivity row while a",
			"turn's end is written, captured by TestThreadStateWhileTurnSettles",
			"(internal/app/app_thread_state_settling_test.go) under fixed names.",
			"Regenerate with UPDATE_TURN_SETTLING_SNAPSHOT=1.",
		},
		"liveState":    liveState,
		"liveActivity": row,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if os.Getenv("UPDATE_TURN_SETTLING_SNAPSHOT") != "" {
		if err := os.WriteFile(turnSettlingSnapshotFixture, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	committed, err := os.ReadFile(turnSettlingSnapshotFixture)
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != string(encoded) {
		t.Fatalf("%s is stale: regenerate it and check the frontend tests agree. Captured:\n%s", turnSettlingSnapshotFixture, encoded)
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

const (
	refuseTurnEnds = `CREATE TRIGGER refuse_turn_settle BEFORE UPDATE OF completed_at ON turns
		WHEN NEW.completed_at IS NOT NULL BEGIN SELECT RAISE(ABORT, 'injected settle failure'); END`
	allowTurnEnds = `DROP TRIGGER refuse_turn_settle`
)

// A completion whose end the store refuses has still ended the turn: the
// thread reads as error at once and a wait on it returns, though its row
// stays open. Once the store takes writes, a send placed on the thread
// writes the end first, with the turn's own outcome, and is placed at the
// boundary rather than queued behind the open row.
func TestThreadStateWhenSettleWriteFails(t *testing.T) {
	q := newQueuedRequests(t)
	tools := q.f.app.threadTools()
	ctx := t.Context()
	execOnStore(t, q.f.dbPath, refuseTurnEnds)
	q.handle(t, "turn 0 complete", provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
	})
	open, found, err := q.f.app.store.GetActiveTurn(q.target.ID)
	if err != nil || !found {
		t.Fatalf("GetActiveTurn = %v, %v; want the refused turn's row still open", found, err)
	}
	wantThreadState(ctx, t, tools, q.target.ID, threadtools.StateError, true)
	report, err := tools.watchThreads(ctx, threadtools.Caller{ThreadID: q.f.caller.ID}, threadtools.StatusCall{
		ThreadIDs: []string{q.target.ID}, WaitSeconds: 30,
	})
	if err != nil || report.WokeOn != q.target.ID || report.TimedOut {
		t.Fatalf("thread_status wait = %+v, %v; want it to return on the thread at once", report, err)
	}
	row, err := q.f.app.store.GetThread(q.target.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if !row.HasFailedTurn {
		t.Fatal("the refused end left the thread row without a failure the sidebar shows")
	}

	execOnStore(t, q.f.dbPath, allowTurnEnds)
	placement, err := q.f.app.resolveUserMessagePlacement(q.target, messageComposer)
	if err != nil {
		t.Fatalf("resolveUserMessagePlacement: %v", err)
	}
	if placement.queue {
		t.Fatal("a send after the store recovered queued behind the turn whose end was refused")
	}
	turn, found, err := q.f.app.store.GetTurn(open.TurnID)
	if err != nil || !found || turn.CompletedAt == nil || turn.StopReason != "end_turn" {
		t.Fatalf("turn row once the store took writes = %+v, %v, %v; want settled end_turn", turn, found, err)
	}
	// The failure stays the thread's until read.
	wantThreadState(ctx, t, tools, q.target.ID, threadtools.StateError, true)
	if _, _, err := q.f.app.store.MarkThreadReadNow(ctx, q.target.ID); err != nil {
		t.Fatalf("MarkThreadReadNow: %v", err)
	}
	wantThreadState(ctx, t, tools, q.target.ID, threadtools.StateIdle, true)
}

// A boot sweep that fails leaves the previous instance's row open: the
// thread reads as error and rests, not as running, and once the store
// takes writes the row is settled by the sweep's rule, as interrupted.
func TestThreadStateWhenTheBootSweepFails(t *testing.T) {
	f := newRequestFixture(t)
	tools := f.app.threadTools()
	ctx := t.Context()
	thread, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := f.app.store.InsertTurn(store.Turn{
		TurnID: "crashed-turn", ThreadID: thread.ID, TurnIndex: 0, StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	execOnStore(t, f.dbPath, refuseTurnEnds)
	if _, err := f.app.triage.RecoverCrashedTurns(); err == nil {
		t.Fatal("the sweep reported no failure")
	}
	wantThreadState(ctx, t, tools, thread.ID, threadtools.StateError, true)
	execOnStore(t, f.dbPath, allowTurnEnds)
	if _, err := f.app.resolveUserMessagePlacement(thread, messageComposer); err != nil {
		t.Fatalf("resolveUserMessagePlacement: %v", err)
	}
	wantThreadState(ctx, t, tools, thread.ID, threadtools.StateInterrupted, true)
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
