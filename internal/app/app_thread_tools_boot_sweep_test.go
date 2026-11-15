package app

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

// A supervisor trial boots, proves itself and waits for its commit, and a
// rolled-back trial gets its database back and nothing else. The boot sweep
// settles the last run's requests in rows, which the restore undoes; the
// wake it owes can start the caller's session and the scratch thread it
// deletes takes files with it, so both wait for the commit.
func TestAParkedTrialSettlesTheLastRunsRequestsButActsOnlyAfterItsCommit(t *testing.T) {
	a := newBootTestApp(t)
	dbPath := filepath.Join(a.dataDirOverride, "agent-overflow", databaseFileName)
	migratedDatabaseAt(t, dbPath)
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	workspace := t.TempDir()
	if _, err := st.CreateProject(store.Project{ID: defaultTestProjectID, Path: workspace, Name: "Project", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	thread := func(id, mode string) store.Thread {
		return store.Thread{
			ID: id, ProjectID: defaultTestProjectID, Title: id, Provider: string(provider.Claude),
			WorkspacePath: workspace, Model: "claude-opus-4-7", Mode: mode, CreatedAt: now, UpdatedAt: now,
		}
	}
	const caller, scratch, token = "thread-caller", "thread-scratch", "tok-open-ask"
	for _, row := range []store.Thread{thread(caller, threadmode.ModeChat), thread(scratch, threadmode.ModeScratch)} {
		if err := st.CreateThread(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.InsertScratchThread(store.ScratchThread{
		ThreadID: scratch, SourceThreadID: caller, RequestToken: token,
		ReturnMode: threadmode.ModeChat, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// An ask the last run was answering when it stopped.
	if err := st.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: store.ThreadRequestAsk, TargetThreadID: scratch,
		OriginThreadID: caller, Notify: true, State: store.ThreadRequestRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: threadReceiptLocalOwner, SourceThreadID: caller,
		Kind: store.ThreadRequestAsk, TargetThreadID: scratch, State: store.ThreadReceiptRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var started []string
	a.startSessionFn = func(threadID string) error {
		mu.Lock()
		defer mu.Unlock()
		started = append(started, threadID)
		return nil
	}
	sessionsStarted := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), started...)
	}
	progress := newRecordingBootProgress("")
	SetBootProgress(a, progress)
	ParkUnattendedWork(a)
	t.Cleanup(func() {
		if err := a.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	row, found, err := a.store.GetThreadRequest(token)
	if err != nil || !found || row.State != store.ThreadRequestInterrupted {
		t.Fatalf("request before the commit = %+v found=%v err=%v, want it settled interrupted", row, found, err)
	}
	// Long enough for anything the boot left running to act.
	time.Sleep(200 * time.Millisecond)
	if got := sessionsStarted(); len(got) != 0 {
		t.Fatalf("sessions started before the commit: %v", got)
	}
	if _, err := a.store.GetThread(scratch); err != nil {
		t.Fatalf("scratch thread before the commit: %v, want it kept", err)
	}
	if row, _, _ := a.store.GetThreadRequest(token); row.DeliveredAt != 0 {
		t.Fatalf("the wake was delivered before the commit: %+v", row)
	}
	if rows, err := a.store.ListFlushQueueItems(caller); err != nil || len(rows) != 0 {
		t.Fatalf("queued before the commit = %+v, %v; want nothing", rows, err)
	}

	ActivateUnattendedWork(a, ServiceUpdateOutcome{})

	waitUntil(t, 30*time.Second, func() bool {
		_, err := a.store.GetThread(scratch)
		row, _, _ := a.store.GetThreadRequest(token)
		return errors.Is(err, sql.ErrNoRows) && row.DeliveredAt != 0
	})
	if rows, err := a.store.ListFlushQueueItems(caller); err != nil || len(rows) != 1 || rows[0].SendID != threadWakeSendID(token) {
		t.Fatalf("queued after the commit = %+v, %v; want the request's wake", rows, err)
	} else if !strings.Contains(rows[0].Message, threadReceiptRestarted) {
		t.Errorf("wake = %q, want the restart as its cause", rows[0].Message)
	}
	waitUntil(t, 10*time.Second, func() bool {
		for _, id := range sessionsStarted() {
			if id == caller {
				return true
			}
		}
		return false
	})
	if failures := progress.failures(); len(failures) != 0 {
		t.Fatalf("boot failures = %v", failures)
	}
}

// A sweep that cannot settle a request reports it on its own phase, which
// every client shows, rather than in a log line nobody reads.
func TestTheBootRequestSweepReportsWhatItCouldNotSettle(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "mid-thought")
	ack := f.runningRequest(t, "keep going")
	f.execWaiting(t, `CREATE TRIGGER refuse_receipt_settle BEFORE UPDATE OF state ON thread_request_receipts
		BEGIN SELECT RAISE(ABORT, 'injected receipt settle failure'); END`)

	after := f.restart(t)
	progress := newRecordingBootProgress("")
	SetBootProgress(after.app, progress)
	after.app.sweepThreadRequestsAtBoot()

	failures := progress.failures()
	if len(failures) != 1 || !strings.HasPrefix(failures[0], "app.sweep_thread_requests: ") ||
		!strings.Contains(failures[0], ack.Token) || !strings.Contains(failures[0], "injected receipt settle failure") {
		t.Fatalf("failures = %q, want the sweep's, naming the request", failures)
	}
	if row := after.request(t, ack.Token); row.State != store.ThreadRequestRunning {
		t.Fatalf("request = %q, want it left for the next boot with its receipt", row.State)
	}
}

// The half that runs after the gate reports a scratch thread it could not
// delete on its own phase too.
func TestTheBootRequestSweepReportsAScratchThreadItCouldNotDelete(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	scratch := f.forkableThread(t, "boot-scratch")
	if err := f.app.store.InsertScratchThread(store.ScratchThread{
		ThreadID: scratch.ID, SourceThreadID: f.caller.ID, RequestToken: "stale-token",
		ReturnMode: scratchReturnMode(threadmode.ModeChat), CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertScratchThread: %v", err)
	}
	f.execWaiting(t, `CREATE TRIGGER fail_delete_mark BEFORE UPDATE OF deleting ON threads
		WHEN NEW.id = '`+scratch.ID+`' BEGIN SELECT RAISE(ABORT, 'injected mark failure'); END`)

	after := f.restart(t)
	progress := newRecordingBootProgress("")
	SetBootProgress(after.app, progress)
	after.app.sweepThreadRequestsAtBoot()
	after.app.finishThreadRequestSweepAtBoot()

	failures := progress.failures()
	if len(failures) != 1 || !strings.HasPrefix(failures[0], "app.finish_thread_request_sweep: ") ||
		!strings.Contains(failures[0], scratch.ID) || !strings.Contains(failures[0], "injected mark failure") {
		t.Fatalf("failures = %q, want the scratch deletion's, naming the thread", failures)
	}
}
