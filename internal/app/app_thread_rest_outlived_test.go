package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
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
// in, that tell it the user stopped the worker of token, and checks that
// each says the worker is at rest.
func workerStoppedMessages(t *testing.T, a *App, threadID, token string) int {
	t.Helper()
	texts := workerStoppedTexts(t, a, threadID, token)
	for _, text := range texts {
		if !strings.Contains(text, "It is at rest") {
			t.Fatalf("the message = %q, want it to say the worker is at rest", text)
		}
	}
	return len(texts)
}

// workerStoppedTexts is the text of each message threadID holds, queued or
// taken in, that tells it the user stopped the worker of token.
func workerStoppedTexts(t *testing.T, a *App, threadID, token string) []string {
	t.Helper()
	var texts []string
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
		texts = append(texts, text)
	}
	return texts
}

// continueWorker has a person send the worker a message and waits for it to
// work again.
func continueWorker(t *testing.T, a *App, threadID string) {
	t.Helper()
	if _, err := a.SendMessageWithOptions(context.Background(), threadID, "keep going", SendMessageOptions{}); err != nil {
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
// rest and tells nothing more. Once the worker is continued by a person, the
// work is not the spawn's run: its Stop leaves it and tells nobody. A spawn
// reduced to its lineage is the same.
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

			// The Stop brought the worker to rest, which ended the spawn's
			// run: a person continuing it starts a run of the person's, which
			// the spawn's Stop leaves running and tells nobody of.
			f.awaitRunEnded(t, ack.Token)
			continueWorker(t, f.app, ack.ThreadID)
			third, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token)
			if err != nil || third.Outcome != threadAgentEnded {
				t.Fatalf("Stop of the continued worker = %+v, %v; want its spawn's run ended", third, err)
			}
			if rest, err := f.app.workerUnrest(ack.ThreadID); err != nil || rest.unrest != workerRunning {
				t.Fatalf("the continued worker reads %+v, %v; want the person's run going on", rest, err)
			}
			if got := workerStoppedMessages(t, f.app, f.caller.ID, ack.Token); got != 1 {
				t.Fatalf("after a Stop of the continued worker the spawner was told %d times, want once", got)
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

// A Stop that stopped the worker but could not bring all of it to rest tells
// the spawner which parts it could not stop, and does not say it is at rest.
func TestAStopThatLeftPartOfAWorkerRunningNamesItToTheSpawner(t *testing.T) {
	t.Parallel()
	t.Run("here", func(t *testing.T) {
		t.Parallel()
		f := newRequestFixture(t)
		ack := outlivingWorker(t, f, false)
		unpausableRunReportingTo(t, f.app, ack.ThreadID)
		stop, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, ack.Token)
		assertSpawnerToldOfTheUnpausedRun(t, f.app, f.caller.ID, ack.Token, stop, err)
	})
	t.Run("on a paired computer", func(t *testing.T) {
		t.Parallel()
		pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
		destID, _ := pair.dest.backendIdentity()
		ack, err := (threadToolsApp{app: pair.dest}).Reply(t.Context(),
			threadtools.Caller{ThreadID: pair.child, ComputerID: destID, ComputerName: "destination"},
			threadtools.ReplyCall{Token: pair.spawn.Token, Text: "Started; still at it."})
		if err != nil {
			t.Fatalf("Reply there: %v", err)
		}
		// The caller's thread is on the spawner's computer, named by its id there.
		receipt, _, err := pair.dest.store.GetThreadRequestReceipt(pair.spawn.Token)
		if err != nil || ack.SourceComputerID == "" || ack.SourceComputerID != receipt.SourceComputerID {
			t.Fatalf("the reply names the caller's computer %q, want %q (%v)", ack.SourceComputerID, receipt.SourceComputerID, err)
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
		unpausableRunReportingTo(t, pair.dest, pair.child)
		stop, err := pair.source.StopThreadAgentRow(t.Context(), pair.caller.ID, pair.spawn.Token)
		assertSpawnerToldOfTheUnpausedRun(t, pair.source, pair.caller.ID, pair.spawn.Token, stop, err)
	})
}

// unpausableRunReportingTo stores a running run reporting to threadID that
// the engine does not hold, which a pause refuses.
func unpausableRunReportingTo(t *testing.T, a *App, threadID string) {
	t.Helper()
	if err := a.store.CreateWorkItem(store.WorkItem{
		ID: "run-the-engine-lost", ProjectID: defaultTestProjectID, Goal: "lost", WorkflowID: "campaign",
		WorkflowScope: "shared", State: "running", Source: workflowSourceAgent,
		OriginThreadID: threadID, CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
}

// assertSpawnerToldOfTheUnpausedRun checks that stop stopped the worker of
// token but not the run of unpausableRunReportingTo, and that the spawner
// was told once, naming the run and not saying the worker is at rest.
func assertSpawnerToldOfTheUnpausedRun(t *testing.T, a *App, spawnerID, token string, stop ThreadAgentStop, err error) {
	t.Helper()
	if err != nil || stop.Outcome != threadAgentStopped || len(stop.Failures) != 1 ||
		!strings.HasPrefix(stop.Failures[0], "pause its workflow run run-the-engine-lost") {
		t.Fatalf("Stop = %+v, %v; want the worker stopped with the run named", stop, err)
	}
	texts := workerStoppedTexts(t, a, spawnerID, token)
	if len(texts) != 1 {
		t.Fatalf("the spawner was told %d times, want once", len(texts))
	}
	if !strings.Contains(texts[0], stop.Failures[0]) || strings.Contains(texts[0], "It is at rest") ||
		strings.Contains(texts[0], "were paused") {
		t.Fatalf("the message = %q, want it to name %q and not say the worker is at rest", texts[0], stop.Failures[0])
	}
}

// settleSpawnByHand settles the spawn of token as finished in s, its answer
// recorded as having reached its caller inline, so no message wakes the
// caller and no answer on its way keeps it from rest.
func settleSpawnByHand(t *testing.T, s *store.Store, token string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if settled, err := s.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal,
		SettledAt: now,
	}); err != nil || !settled {
		t.Fatalf("settle %s: %v %v", token, settled, err)
	}
	if _, err := s.MarkThreadRequestDelivered(token, store.ThreadWakeInline, now, false); err != nil {
		t.Fatalf("deliver %s: %v", token, err)
	}
}

// awaitAtRest waits until threadID is at rest itself, its own turn ended.
func awaitAtRest(t *testing.T, a *App, threadID string) {
	t.Helper()
	waitUntil(t, 10*time.Second, func() bool {
		rest, err := a.workerUnrest(threadID)
		return err == nil && rest.unrest == ""
	})
}

// assertStopOfTheWorkBelow checks that a person's Stop of the settled spawn
// of token in spawnerID's list, whose thread is at rest and has a worker
// below it that is not, reports it stopped and tells the spawner once, and
// that a repeated Stop finds it all at rest and tells nobody.
func assertStopOfTheWorkBelow(t *testing.T, a *App, spawnerID, token string, nested int) {
	t.Helper()
	stop, err := a.StopThreadAgentRow(t.Context(), spawnerID, token)
	if err != nil || stop.Outcome != threadAgentStopped || stop.Nested != nested || len(stop.Failures) != 0 {
		t.Fatalf("Stop = %+v, %v; want it stopped with the worker below", stop, err)
	}
	if got := workerStoppedMessages(t, a, spawnerID, token); got != 1 {
		t.Fatalf("the spawner was told %d times, want once", got)
	}
	again, err := a.StopThreadAgentRow(t.Context(), spawnerID, token)
	if err != nil || again.Outcome != threadAgentEnded || again.Nested != 0 {
		t.Fatalf("repeated Stop = %+v, %v; want it all found at rest", again, err)
	}
	if got := workerStoppedMessages(t, a, spawnerID, token); got != 1 {
		t.Fatalf("after a repeated Stop the spawner was told %d times, want once", got)
	}
}

// A settled spawn whose thread is at rest but has a worker below it that is
// not is not at rest: a person's Stop of its row stops that worker, reports
// the spawn stopped and tells its spawner once. The worker below may run on
// a paired computer, under a thread here or one there.
func TestAStopOfASettledSpawnStopsItsWorkerBelowAndTellsTheSpawner(t *testing.T) {
	t.Parallel()
	t.Run("here", func(t *testing.T) {
		t.Parallel()
		f := newRequestFixture(t)
		f.mockClaudeHoldingTheTurn(t, "working")
		a, b := f.plainThread(t), f.plainThread(t)
		f.seedAgentRow(t, "a-spawned-b", a.ID, store.ThreadRequestSpawn, b.ID, false)
		settleSpawnByHand(t, f.app.store, "a-spawned-b")
		c, err := f.adapter().Spawn(t.Context(), f.targetIdentity(t, b.ID), threadtools.SpawnCall{Prompt: "keep going"})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		f.awaitRequestState(t, c.Token, store.ThreadRequestRunning)
		settleSpawnByHand(t, f.app.store, c.Token)
		awaitAtRest(t, f.app, b.ID)

		assertStopOfTheWorkBelow(t, f.app, a.ID, "a-spawned-b", 1)
		if _, live := f.app.sessionManager().get(c.ThreadID); live {
			t.Fatal("the worker below kept its session")
		}
	})
	t.Run("below a thread here", func(t *testing.T) {
		t.Parallel()
		pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
		top, err := createTestThread(t, pair.source, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
		if err != nil {
			t.Fatal(err)
		}
		row := store.ThreadRequest{
			Token: "top-spawned-the-caller", CallerThreadID: top.ID, Kind: store.ThreadRequestSpawn,
			TargetThreadID: pair.caller.ID, State: store.ThreadRequestAccepted,
		}
		seedSpawnRun(pair.source, &row)
		if err := pair.source.store.InsertThreadRequest(row); err != nil {
			t.Fatal(err)
		}
		settleSpawnByHand(t, pair.source.store, "top-spawned-the-caller")
		settleSpawnByHand(t, pair.source.store, pair.spawn.Token)
		awaitAtRest(t, pair.source, pair.caller.ID)

		// The worker there and the one it spawned.
		assertStopOfTheWorkBelow(t, pair.source, top.ID, "top-spawned-the-caller", 2)
		if _, live := pair.dest.sessionManager().get(pair.child); live {
			t.Fatal("the worker there kept its session")
		}
	})
	t.Run("below a thread there", func(t *testing.T) {
		t.Parallel()
		pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
		settleSpawnByHand(t, pair.source.store, pair.spawn.Token)
		settleSpawnByHand(t, pair.dest.store, pair.grand)
		if err := pair.dest.StopSession(pair.child); err != nil {
			t.Fatal(err)
		}
		awaitAtRest(t, pair.dest, pair.child)
		grand, _, err := pair.dest.store.GetThreadRequest(pair.grand)
		if err != nil {
			t.Fatal(err)
		}
		waitUntil(t, 10*time.Second, func() bool {
			rest, err := pair.dest.workerUnrest(grand.TargetThreadID)
			return err == nil && rest.unrest == workerRunning
		})

		assertStopOfTheWorkBelow(t, pair.source, pair.caller.ID, pair.spawn.Token, 1)
		if _, live := pair.dest.sessionManager().get(grand.TargetThreadID); live {
			t.Fatal("the worker below there kept its session")
		}
	})
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
