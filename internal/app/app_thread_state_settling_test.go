package app

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
)

// A turn completing normally reads as running from the provider's
// completion until its row settles, and as idle once it has: never as
// interrupted, and a thread_status wait does not rest on it in between.
// The settle is held at the store: a second connection takes SQLite's
// write lock before the completion arrives, so the completion's writes
// wait inside it after triage has handed the round out.
func TestThreadStateWhileTurnSettles(t *testing.T) {
	q := newQueuedRequests(t)
	tools := q.f.app.threadTools()
	ctx := t.Context()

	release := holdStoreWrites(t, q.f.dbPath)
	completed := make(chan error, 1)
	go func() {
		evt := provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: q.target.ID, SessionToken: q.session, Timestamp: time.Now(),
			TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
		}
		completed <- q.f.app.triage.Handle(evt)
	}()
	deadline := time.Now().Add(4 * time.Second)
	for q.f.app.triage.LiveStateSnapshotForThread(q.target.ID).ActiveTurn != nil {
		if time.Now().After(deadline) {
			release()
			t.Fatal("the completion never took the round")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-completed:
		release()
		t.Fatalf("the completion finished while the store was held: %v", err)
	default:
	}

	// thread_status, the read a wait on thread ids polls.
	status, err := tools.threadStateNow(ctx, q.target.ID)
	if err != nil {
		release()
		t.Fatalf("threadStateNow: %v", err)
	}
	// thread_show and thread_search read the row, then the live state.
	shown := stateAsShown(ctx, t, tools, q.target.ID)
	// A spawn's or a send's wait settles on the thread's rest.
	rest, restErr := q.f.app.threadRest(q.target.ID)
	release()
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

	settled, err := tools.threadStateNow(ctx, q.target.ID)
	if err != nil {
		t.Fatalf("threadStateNow after the settle: %v", err)
	}
	if settled.State != threadtools.StateIdle || !settled.Resting {
		t.Errorf("thread_status once the turn settled = %q, resting %v; want idle and resting", settled.State, settled.Resting)
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

// A completion whose settle write fails leaves the row open: the thread
// reads as running, and a wait on it does not rest, until the next boot's
// sweep records the turn (store/turn_newest.go). The failure is the
// thread's error row, which the sidebar shows as Failed once the round has
// ended and which outranks the sweep's interruption until it is read.
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
	row, err := q.f.app.store.GetThread(q.target.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if !row.HasFailedTurn {
		t.Fatal("the failed settle left the thread row without a failure the sidebar shows")
	}
	execOnStore(t, q.f.dbPath, `DROP TRIGGER refuse_turn_settle`)
	if _, err := q.f.app.triage.RecoverCrashedTurns(); err != nil {
		t.Fatalf("RecoverCrashedTurns: %v", err)
	}
	wantThreadState(ctx, t, tools, q.target.ID, threadtools.StateError, true)
	if _, _, err := q.f.app.store.MarkThreadReadNow(ctx, q.target.ID); err != nil {
		t.Fatalf("MarkThreadReadNow: %v", err)
	}
	wantThreadState(ctx, t, tools, q.target.ID, threadtools.StateIdle, true)
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

// holdStoreWrites takes SQLite's write lock on dbPath from a connection of
// its own; every store write waits on it (busy_timeout) until the returned
// release runs. Reads go on: the database is in WAL mode.
func holdStoreWrites(t *testing.T, dbPath string) (release func()) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `PRAGMA busy_timeout = 2000`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		if _, err := conn.ExecContext(context.Background(), `ROLLBACK`); err != nil {
			t.Errorf("release the write lock: %v", err)
		}
		if err := conn.Close(); err != nil {
			t.Errorf("close the lock connection: %v", err)
		}
		if err := raw.Close(); err != nil {
			t.Errorf("close the lock database: %v", err)
		}
	}
	t.Cleanup(release)
	return release
}
