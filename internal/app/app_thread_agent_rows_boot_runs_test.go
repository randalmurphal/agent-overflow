package app

// While the boot has not recovered what its threads wait on, no rest is
// read, so the run of a rested worker is not yet recorded ended. A message
// that starts work on that worker waits for the recovery, then ends the
// earlier run before its own work: it never merges into a run that ended
// (app_thread_agent_rows_runs.go, endRunsBeforeWork).

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// restedWorkerAtBoot is a worker the fixture's caller spawned in placement,
// that made two tool calls for 60 tokens and came to rest with its spawn
// settled, while the boot's recovery of rest bars every rest read: the
// spawn's run is still recorded open.
func (f *requestFixture) restedWorkerAtBoot(t *testing.T, token, placement string) store.Thread {
	t.Helper()
	worker := f.plainThread(t)
	f.acceptAgentRequest(t, token, f.caller.ID, store.ThreadRequestSpawn, worker.ID)
	if _, err := f.app.store.SetThreadPlacement([]string{worker.ID}, placement); err != nil {
		t.Fatal(err)
	}
	f.app.barRestAtBoot()
	workerTurn(t, f.app, worker.ID, "t0", 2, 60)
	f.settleAgentRequest(t, token)
	if _, err := f.app.store.SetThreadRequestNotify(token, false); err != nil {
		t.Fatal(err)
	}
	f.app.threadRequestsWG.Wait()
	if row := f.request(t, token); !row.RunOpen() {
		t.Fatalf("spawn %+v; the test needs its run unrecorded while the boot recovers", row)
	}
	return worker
}

// stillWaiting fails when done delivers within a short window: the
// message is expected to wait for the boot's recovery.
func stillWaiting(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("the message went ahead (%v) before the boot recovered what its threads wait on", err)
	case <-time.After(500 * time.Millisecond):
	}
}

// A person's message to a rested worker while the boot recovers waits for
// the recovery and ends the spawn's run first: the spawn keeps its own
// figures. On an owned worker the message is a nudge, whose run is a row of
// its own; on a worker with a sidebar row it is nothing of the spawner's.
func TestAMessageDuringTheBootsRecoveryEndsTheEarlierRunFirst(t *testing.T) {
	t.Parallel()
	for _, placement := range []string{store.PlacementOwned, store.PlacementStandalone} {
		t.Run(placement, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.watchAgentRows()
			f.holdCallerStart()
			gate := filepath.Join(t.TempDir(), "person-turn")
			installMockClaudeTurns(t, f.app, [][]string{runTurnLines("boot-person", "Checked the docs.", gate)})
			worker := f.restedWorkerAtBoot(t, "boot-spawn", placement)

			done := make(chan error, 1)
			go func() { done <- f.app.SendMessage(t.Context(), worker.ID, "also check the docs", nil) }()
			stillWaiting(t, done)
			f.app.recoverRestAtBoot()
			if err := within(t, "the message", done); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			release(t, gate)
			waitUntil(t, 10*time.Second, func() bool {
				rest, err := f.app.workerUnrest(worker.ID)
				return err == nil && rest.unrest == "" && !f.app.liveWorker(worker.ID)
			})
			f.app.threadRequestsWG.Wait()
			if spawn := f.awaitRunEnded(t, "boot-spawn"); spawn.RunTotal.ToolCalls != 2 || spawn.RunTotal.Tokens != 60 {
				t.Fatalf("spawn's run %+v; want its own two calls and 60 tokens, not the message's", spawn.RunTotal)
			}
			tokens := f.pastTokens(t, f.caller.ID)
			if placement == store.PlacementStandalone {
				if len(tokens) != 1 {
					t.Fatalf("Past %q; want the spawn's run alone", tokens)
				}
				return
			}
			var nudge ThreadAgentRow
			waitUntil(t, 10*time.Second, func() bool {
				f.app.threadRequestsWG.Wait()
				for _, token := range f.pastTokens(t, f.caller.ID) {
					if row, ok := f.pastRow(t, f.caller.ID, token); ok && row.Kind == threadAgentKindNudge {
						nudge = row
						return row.Tokens == 60
					}
				}
				return false
			})
			if nudge.ToolCalls != 1 {
				t.Fatalf("nudge's Past row %+v; want the message's one call", nudge)
			}
		})
	}
}

// The spawner's own send to its rested worker while the boot recovers
// waits for the recovery and starts a run of its own, never joining the
// spawn's, which ended.
func TestASpawnersSendDuringTheBootsRecoveryStartsARunOfItsOwn(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerStart()
	installMockClaudeTurns(t, f.app, [][]string{heldTurnLines("boot-send", "On it.")})
	worker := f.restedWorkerAtBoot(t, "boot-send-spawn", store.PlacementOwned)

	type sent struct {
		ack threadtools.RequestAck
		err error
	}
	done := make(chan sent, 1)
	go func() {
		ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: worker.ID, Message: "check the changelog"})
		done <- sent{ack, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("the send went ahead (%+v) before the boot recovered what its threads wait on", got)
	case <-time.After(500 * time.Millisecond):
	}
	f.app.recoverRestAtBoot()
	var got sent
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the send never went ahead after the recovery")
	}
	if got.err != nil {
		t.Fatalf("Send: %v", got.err)
	}
	if row := f.request(t, got.ack.Token); row.RunToken != got.ack.Token {
		t.Fatalf("the send %+v is in run %q; want a run of its own", row, row.RunToken)
	}
	if spawn := f.request(t, "boot-send-spawn"); !spawn.RunEnded() || spawn.RunTotal.ToolCalls != 2 {
		t.Fatalf("spawn %+v; want its run ended with its own figures", spawn)
	}
}

// A person's message waiting for the boot's recovery fails, once the wait
// is out, with an error the person can act on, and nothing is sent or
// recorded.
func TestAMessageWaitingTooLongForTheBootsRecoveryFails(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.app.restRecoveryWaitOverride = 200 * time.Millisecond
	worker := f.restedWorkerAtBoot(t, "waiting-spawn", store.PlacementOwned)
	if err := f.app.SendMessage(t.Context(), worker.ID, "also check the docs", nil); !errors.Is(err, errRestRecovering) {
		t.Fatalf("SendMessage = %v; want %v", err, errRestRecovering)
	}
	items, err := f.app.store.ListItems(worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind == "user_text" {
			t.Fatalf("the failed message left %+v in the worker", item)
		}
	}
	if open := f.openRows(t, f.caller.ID); len(open) != 0 {
		t.Fatalf("open rows %+v; want no run for a message that was not sent", open)
	}
}

// A thread's send waiting for the boot's recovery ends with its call: it
// fails with the call's context, and no request is recorded.
func TestASendWaitingForTheBootsRecoveryEndsWithItsCall(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.restedWorkerAtBoot(t, "cancelled-spawn", store.PlacementOwned)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := f.adapter().Send(ctx, f.callerIdentity(), threadtools.SendCall{ThreadID: worker.ID, Message: "check the changelog"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send = %v; want the call's deadline", err)
	}
	if requests, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 10, 0); err != nil || len(requests) != 1 {
		t.Fatalf("requests %+v, %v; want the spawn alone", requests, err)
	}
}
