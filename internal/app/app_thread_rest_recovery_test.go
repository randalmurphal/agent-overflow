package app

// The boot's recovery of rest is a retried phase of the request sweep: a
// source it cannot read is read again by a later pass, and the bar on
// settling at rest lifts once every read succeeded.

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// failRestSource makes one recovered source's read fail until the returned
// func restores it.
func (f *requestFixture) failRestSource(t *testing.T) (restore func()) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+f.dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`ALTER TABLE remote_watches RENAME TO remote_watches_unreadable`); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if _, err := db.Exec(`ALTER TABLE remote_watches_unreadable RENAME TO remote_watches`); err != nil {
			t.Fatal(err)
		}
	}
}

// A recovery whose read of one source fails keeps the bar: a held request
// whose thread is at rest stays held, while an unrelated worker's error and
// another's reply settle as ever. A later pass of the sweep reads it, the
// bar lifts, and the held request settles with no restart.
func TestAFailedRecoveryReadIsRetriedByTheSweep(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	held := f.heldOnALoop(t)

	f.app.barRestAtBoot()
	restore := f.failRestSource(t)
	f.app.recoverRestAtBoot()
	if !f.app.restRecoveryPending() {
		t.Fatal("the failed read left nothing to retry")
	}
	wakes, err := f.app.store.ListAgentWakes(held.ThreadID)
	if err != nil || len(wakes) != 1 {
		t.Fatalf("wakes %+v, %v", wakes, err)
	}
	if err := f.app.CancelThreadWake(held.ThreadID, store.AgentWakeLoop, wakes[0].ID); err != nil {
		t.Fatalf("CancelThreadWake: %v", err)
	}
	f.app.threadRequestsWG.Wait()
	f.app.settleRestedReceipts()
	if row := f.request(t, held.Token); row.State != store.ThreadRequestRunning {
		t.Fatalf("request %+v settled at rest with the recovery incomplete", row)
	}

	installMockClaudeTurns(t, f.app, [][]string{{mockClaudeInitLine,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["the build broke"]}`}})
	failed := f.spawnWorker(t, "build it")
	if row := f.awaitRequestState(t, failed.Token, store.ThreadRequestErrored); !strings.Contains(string(row.Answer), "the build broke") {
		t.Fatalf("errored %+v; want the worker's error", row)
	}
	gate := filepath.Join(t.TempDir(), "reply")
	installMockClaudeTurns(t, f.app, [][]string{gatedTurn(gate, "Working on it.")})
	replied := f.spawnWorker(t, "answer it")
	f.awaitTurnRunning(t, replied.Token)
	if _, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, replied.ThreadID), threadtools.ReplyCall{Token: replied.Token, Text: "Here it is."}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if row := f.awaitRequestState(t, replied.Token, store.ThreadRequestReplied); string(row.Answer) != "Here it is." {
		t.Fatalf("replied %+v", row)
	}
	release(t, gate)

	restore()
	f.app.retryRestRecovery()
	if f.app.restRecoveryPending() {
		t.Fatal("the retried read left the recovery pending")
	}
	if row := f.awaitRequestState(t, held.Token, store.ThreadRequestFinished); string(row.Answer) != "Watching the board." {
		t.Fatalf("settled %+v; want finished with the worker's message", row)
	}
}

// The sweep's passes are what retry: the boot's recovery fails, the sweep
// sleeps no longer than the retry while it is pending, and a later pass
// lifts the bar once the source reads.
func TestTheSweepRetriesARecoveryReadSoon(t *testing.T) {
	t.Parallel()
	// A restarted app, whose life ends with the test, so its sweep does.
	f := newRequestFixture(t).restart(t)
	f.app.barRestAtBoot()
	restore := f.failRestSource(t)
	f.app.startThreadRequestSweeps()
	waitUntil(t, 10*time.Second, f.app.restRecoveryPending)
	if delay := f.app.threadRequestSweepDelay(time.Now()); delay > restRecoveryRetry {
		t.Fatalf("the sweep sleeps %v with the recovery pending; want at most %v", delay, restRecoveryRetry)
	}
	restore()
	waitUntil(t, 3*restRecoveryRetry, func() bool { return !f.app.restRecoveryPending() })
	f.app.threadRequests.mu.Lock()
	barred := f.app.threadRequests.rest.barred
	f.app.threadRequests.mu.Unlock()
	if barred {
		t.Fatal("the bar stayed with the recovery done")
	}
}
