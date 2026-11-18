package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// moveAway records threadID as moved off app's computer to another one.
func moveAway(t *testing.T, app *App, threadID string) {
	t.Helper()
	transfer, err := app.store.CreateThreadTransfer(store.ThreadTransfer{
		ID: entityid.New(), ThreadID: threadID, PeerBackendID: entityid.New(), Kind: "move", Direction: "outgoing",
		ActivationHash: strings.Repeat("a", 64), PrivateState: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"prepared", "committed"} {
		if _, err := app.store.AdvanceThreadTransfer(transfer.ID, phase, strings.Repeat("b", 64)); err != nil {
			t.Fatalf("advance the move to %s: %v", phase, err)
		}
	}
}

// TestAThreadThatLeftDuringAWaitIsReportedBesideItsSiblings: of three
// threads on the other computer, one is deleted and one moved away after
// the calling computer resolved them and before that computer serves the
// wait. Each is reported as what happened to it, their resting sibling as
// it stands, and the computer is not reported as one that did not answer.
func TestAThreadThatLeftDuringAWaitIsReportedBesideItsSiblings(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	var faults *peerFaults
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		wire, served := serveFaultyApp(t, app)
		faults = served
		return wire
	})
	deleted := reachThread(t, pair.dest, "dddddddd-0000-4000-8000-000000000001", "Deleted")
	moved := reachThread(t, pair.dest, "dddddddd-0000-4000-8000-000000000002", "Moved")
	resting := reachThread(t, pair.dest, "dddddddd-0000-4000-8000-000000000003", "Resting")
	faults.beforeWatch = func() {
		if err := pair.dest.store.DeleteThread(deleted.ID); err != nil {
			t.Errorf("delete the thread: %v", err)
		}
		moveAway(t, pair.dest, moved.ID)
	}

	result := pair.call(t, "thread_status", `{"thread_ids":["`+deleted.ID+`","`+moved.ID+`","`+resting.ID+`"],"wait_seconds":30}`)
	if result["errors"] != nil {
		t.Fatalf("errors = %v, want none: the computer answered", result["errors"])
	}
	threads, _ := result["threads"].([]any)
	if len(threads) != 3 {
		t.Fatalf("threads = %v, want all three", result["threads"])
	}
	for index, want := range []struct{ id, says string }{{deleted.ID, "No thread or assistant matches"}, {moved.ID, "moved to"}} {
		row, _ := threads[index].(map[string]any)
		message, _ := row["error"].(string)
		if row["thread_id"] != want.id || row["state"] != threadtools.StateUnavailable || row["error_code"] != threadtools.CodeNotFound ||
			row["computer_id"] != pair.computer || !strings.Contains(message, want.says) {
			t.Fatalf("row %d = %v, want %s gone from the other computer, saying %q", index, row, want.id, want.says)
		}
	}
	kept, _ := threads[2].(map[string]any)
	if kept["thread_id"] != resting.ID || kept["state"] != threadtools.StateIdle || kept["resting"] != true {
		t.Fatalf("the sibling's row = %v, want it resting as it stands", kept)
	}

	// The adapter reads each thread on its own the same way when they go
	// after its wait began.
	report, err := pair.dest.threadToolsAdapter().RequestStates(t.Context(), pair.callerIdentity(),
		threadtools.StatusCall{ThreadIDs: []string{deleted.ID, moved.ID, resting.ID}})
	if err != nil {
		t.Fatalf("a read of threads that left and their sibling = %v, want every row", err)
	}
	if len(report.Threads) != 3 || report.Threads[0].ErrorCode != threadtools.CodeNotFound ||
		!strings.Contains(report.Threads[1].Error, "moved to") || report.Threads[2].State != threadtools.StateIdle {
		t.Fatalf("rows = %+v, want the deleted and moved threads gone and their sibling idle", report.Threads)
	}
	// A thread that left will not rest here, so a wait on it ends at once.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	report, err = pair.dest.threadToolsAdapter().RequestStates(ctx, pair.callerIdentity(),
		threadtools.StatusCall{ThreadIDs: []string{deleted.ID}, WaitSeconds: 30})
	if err != nil || report.WokeOn != deleted.ID || ctx.Err() != nil {
		t.Fatalf("a wait on a deleted thread = %+v, %v; want it ended by that thread", report, err)
	}
}
