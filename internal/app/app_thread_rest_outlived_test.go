package app

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// outlivingWorker spawns a worker whose turn stays open, has it reply, and
// returns the spawn once it settled with the worker still running. lineage
// reduces the settled spawn to its lineage, as the retention floor does.
func outlivingWorker(t *testing.T, f *requestFixture, lineage bool) threadtools.RequestAck {
	t.Helper()
	f.holdCallerTurn(t)
	f.watchAgentRows()
	f.mockClaudeHoldingTheTurn(t, "working")
	ack := f.runningRequest(t, "deploy it and keep watching")
	if _, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, ack.ThreadID), threadtools.ReplyCall{
		Token: ack.Token, Text: "Deployed; still watching.",
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	f.awaitRequestState(t, ack.Token, store.ThreadRequestReplied)
	if lineage {
		execOnBusyStore(t, f.dbPath, fmt.Sprintf(`UPDATE thread_requests
			SET settled_at = 1, lineage_only = 1, notify = 0, answer = NULL WHERE token = '%s'`, ack.Token))
	}
	waitUntil(t, 10*time.Second, func() bool {
		rest, err := f.app.workerUnrest(ack.ThreadID)
		return err == nil && rest.unrest == workerRunning
	})
	// The pane lists the settled spawn once the live set has its worker.
	waitUntil(t, 10*time.Second, func() bool {
		return slices.Contains(agentRowTokens(f.agentRows(t, f.caller.ID).Rows), ack.Token)
	})
	return ack
}

// workerStoppedMessages counts the messages threadID holds, queued or taken
// in, that tell it the user stopped the worker of token.
func workerStoppedMessages(t *testing.T, a *App, threadID, token string) int {
	t.Helper()
	count := 0
	ids := []string{workerStoppedSendIDRoot + token}
	for n := 1; n <= 8; n++ {
		ids = append(ids, fmt.Sprintf("%s%s:%d", workerStoppedSendIDRoot, token, n))
	}
	for _, id := range ids {
		recorded, found, err := a.findRecordedSend(threadID, id)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			continue
		}
		text := recorded.queued.Message
		if text == "" {
			text = recorded.item.Summary
		}
		if !strings.Contains(text, "The user stopped thread") {
			t.Fatalf("the message = %q, want it to say the user stopped the worker", text)
		}
		count++
	}
	return count
}

// continueWorker has a person send the worker a message and waits for it to
// work again.
func continueWorker(t *testing.T, a *App, threadID string) {
	t.Helper()
	if err := a.SendMessage(threadID, "keep going", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		rest, err := a.workerUnrest(threadID)
		return err == nil && rest.unrest == workerRunning
	})
}

// A person's Stop of a worker that answered its spawn and kept working
// reports the worker stopped and tells its spawner once; the settled spawn
// and its answer stay as they were, and a repeated Stop finds the worker at
// rest and tells nothing more. Once the worker is continued, a Stop that
// stops it again tells the spawner again. A spawn reduced to its lineage is
// the same.
func TestAStopOfAWorkerThatOutlivedItsSpawnTellsTheSpawnerOnce(t *testing.T) {
	t.Parallel()
	for name, lineage := range map[string]bool{"settled": false, "lineage": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			ack := outlivingWorker(t, f, lineage)
			before, _, err := f.app.store.GetThreadRequest(ack.Token)
			if err != nil {
				t.Fatal(err)
			}

			stop, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token)
			if err != nil || stop.Outcome != threadAgentStopped || !stop.Interrupted || len(stop.Failures) != 0 {
				t.Fatalf("Stop = %+v, %v; want the worker stopped mid-turn", stop, err)
			}
			if _, live := f.app.sessionManager().get(ack.ThreadID); live {
				t.Fatal("the worker kept its session")
			}
			if got := workerStoppedMessages(t, f.app, f.caller.ID, ack.Token); got != 1 {
				t.Fatalf("the spawner was told %d times, want once", got)
			}
			after, _, err := f.app.store.GetThreadRequest(ack.Token)
			if err != nil || after.State != before.State || string(after.Answer) != string(before.Answer) {
				t.Fatalf("the spawn = %s %q, %v; want it as it was, %s %q", after.State, after.Answer, err, before.State, before.Answer)
			}

			// A lineage row at rest is no longer listed, so its repeated Stop
			// is refused as one the thread is not waiting on.
			again, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token)
			if (err != nil && !lineage) || (err == nil && again.Outcome != threadAgentEnded) {
				t.Fatalf("repeated Stop = %+v, %v; want the worker found at rest", again, err)
			}
			if got := workerStoppedMessages(t, f.app, f.caller.ID, ack.Token); got != 1 {
				t.Fatalf("after a repeated Stop the spawner was told %d times, want once", got)
			}
			if lineage {
				return
			}

			continueWorker(t, f.app, ack.ThreadID)
			third, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token)
			if err != nil || third.Outcome != threadAgentStopped {
				t.Fatalf("Stop of the continued worker = %+v, %v; want it stopped", third, err)
			}
			if got := workerStoppedMessages(t, f.app, f.caller.ID, ack.Token); got != 2 {
				t.Fatalf("after stopping the continued worker the spawner was told %d times, want twice", got)
			}
		})
	}
}

// Two Stops that both found the worker working are one stopping of it: the
// second, made while the first is about to bring the worker to rest, stops
// it and tells the spawner, and the first then tells nothing more.
func TestStopsRacingOnOneStoppingOfAWorkerTellTheSpawnerOnce(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	ack := outlivingWorker(t, f, false)
	raced := false
	var second ThreadAgentStop
	var secondErr error
	f.app.workerStopObservedForTest = func(token string) {
		if raced || token != ack.Token {
			return
		}
		raced = true
		second, secondErr = f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token)
	}
	first, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token)
	if err != nil || first.Outcome != threadAgentStopped || secondErr != nil || second.Outcome != threadAgentStopped {
		t.Fatalf("Stops = %+v, %v and %+v, %v; want both to report the worker stopped", first, err, second, secondErr)
	}
	if got := workerStoppedMessages(t, f.app, f.caller.ID, ack.Token); got != 1 {
		t.Fatalf("the spawner was told %d times, want once", got)
	}
}

// A worker that outlived its spawn and is stopped through a cascade, or by
// its spawner's own thread_cancel, is stopped and nobody is told.
func TestAWorkerThatOutlivedItsSpawnStoppedOtherwiseTellsNobody(t *testing.T) {
	t.Parallel()
	t.Run("an agent's own cancel", func(t *testing.T) {
		t.Parallel()
		f := newRequestFixture(t)
		ack := outlivingWorker(t, f, false)
		report, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{ThreadID: ack.ThreadID})
		if err != nil || report.Effect != threadtools.EffectThreadStopped {
			t.Fatalf("Cancel = %+v, %v; want the worker stopped", report, err)
		}
		if got := workerStoppedMessages(t, f.app, f.caller.ID, ack.Token); got != 0 {
			t.Fatalf("the spawner was told %d times of its own cancel, want never", got)
		}
	})
	t.Run("a cascade", func(t *testing.T) {
		t.Parallel()
		f := newRequestFixture(t)
		ack := outlivingWorker(t, f, false)
		// The caller is itself a worker; a Stop of its row reaches the
		// worker that outlived its spawn through the cascade.
		top := f.plainThread(t)
		if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
			Token: "spawn-of-the-caller", CallerThreadID: top.ID, Kind: store.ThreadRequestSpawn,
			TargetThreadID: f.caller.ID, State: store.ThreadRequestAccepted,
		}); err != nil {
			t.Fatal(err)
		}
		stop, err := f.app.StopThreadAgentRow(t.Context(), top.ID, "spawn-of-the-caller")
		if err != nil || stop.Nested == 0 || len(stop.Failures) != 0 {
			t.Fatalf("Stop = %+v, %v; want the worker below stopped", stop, err)
		}
		if _, live := f.app.sessionManager().get(ack.ThreadID); live {
			t.Fatal("the worker below kept its session")
		}
		if got := workerStoppedMessages(t, f.app, f.caller.ID, ack.Token); got != 0 {
			t.Fatalf("the caller was told %d times of a cascade, want never", got)
		}
	})
}

// A person's Stop of a worker on a paired computer that answered its spawn
// and kept working there reports it stopped and tells the spawner here once.
func TestAStopOfAPairedWorkerThatOutlivedItsSpawnTellsTheSpawnerOnce(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	destID, _ := pair.dest.backendIdentity()
	if _, err := (threadToolsApp{app: pair.dest}).Reply(t.Context(),
		threadtools.Caller{ThreadID: pair.child, ComputerID: destID, ComputerName: "destination"},
		threadtools.ReplyCall{Token: pair.spawn.Token, Text: "Started; still at it."}); err != nil {
		t.Fatalf("Reply there: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		pair.poll(t)
		row, found, err := pair.source.store.GetThreadRequest(pair.spawn.Token)
		return err == nil && found && row.State == store.ThreadRequestReplied
	})
	waitUntil(t, 10*time.Second, func() bool {
		rest, err := pair.dest.workerUnrest(pair.child)
		return err == nil && rest.unrest == workerRunning
	})

	stop, err := pair.source.StopThreadAgentRow(t.Context(), pair.caller.ID, pair.spawn.Token)
	if err != nil || stop.Outcome != threadAgentStopped || len(stop.Failures) != 0 {
		t.Fatalf("Stop = %+v, %v; want the worker there stopped", stop, err)
	}
	if got := workerStoppedMessages(t, pair.source, pair.caller.ID, pair.spawn.Token); got != 1 {
		t.Fatalf("the spawner here was told %d times, want once", got)
	}
	again, err := pair.source.StopThreadAgentRow(t.Context(), pair.caller.ID, pair.spawn.Token)
	if err != nil || again.Outcome != threadAgentEnded {
		t.Fatalf("repeated Stop = %+v, %v; want the worker there found at rest", again, err)
	}
	if got := workerStoppedMessages(t, pair.source, pair.caller.ID, pair.spawn.Token); got != 1 {
		t.Fatalf("after a repeated Stop the spawner here was told %d times, want once", got)
	}

	continueWorker(t, pair.dest, pair.child)
	third, err := pair.source.StopThreadAgentRow(t.Context(), pair.caller.ID, pair.spawn.Token)
	if err != nil || third.Outcome != threadAgentStopped {
		t.Fatalf("Stop of the continued worker there = %+v, %v; want it stopped", third, err)
	}
	if got := workerStoppedMessages(t, pair.source, pair.caller.ID, pair.spawn.Token); got != 2 {
		t.Fatalf("after stopping the continued worker the spawner here was told %d times, want twice", got)
	}
}
