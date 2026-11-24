package app

// An owned worker is its spawner's sub-agent always
// (docs/specs/standing-agents.md, "Owned workers"): a message the person or
// another thread sends it is a nudge, whose work is one run of its spawner's
// row, counted on its own, holding the spawner, its answer kept in the
// worker. A worker with a sidebar row continued by someone else is no row of
// its spawner's. The rule follows the placement when each thing happens.

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"

	"github.com/google/uuid"
)

// restedWorker is a worker the fixture's caller spawned in placement, that
// made two tool calls for 60 tokens in its spawn's run and came to rest
// with its answer taken, the spawn's run ended.
func (f *requestFixture) restedWorker(t *testing.T, token, placement string) store.Thread {
	t.Helper()
	worker := f.plainThread(t)
	f.restAsWorker(t, token, worker, placement)
	return worker
}

// restAsWorker makes worker restedWorker's worker, spawned as token.
func (f *requestFixture) restAsWorker(t *testing.T, token string, worker store.Thread, placement string) {
	t.Helper()
	f.acceptAgentRequest(t, token, f.caller.ID, store.ThreadRequestSpawn, worker.ID)
	if _, err := f.app.store.SetThreadPlacement([]string{worker.ID}, placement); err != nil {
		t.Fatal(err)
	}
	workerTurn(t, f.app, worker.ID, "t0", 2, 60)
	f.settleAgentRequest(t, token)
	if _, err := f.app.store.SetThreadRequestNotify(token, false); err != nil {
		t.Fatal(err)
	}
	if ended := f.awaitRunEnded(t, token); ended.RunTotal.ToolCalls != 2 || ended.RunTotal.Tokens != 60 {
		t.Fatalf("spawn's run %+v; want its two calls and 60 tokens", ended.RunTotal)
	}
}

// nudge has by send the worker a message.
func (f *requestFixture) nudge(t *testing.T, by string, worker store.Thread) {
	t.Helper()
	switch by {
	case "the person":
		if _, err := f.app.SendMessageWithOptions(t.Context(), worker.ID, "also check the docs", pageSendOptions(uuid.NewString())); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
	case "another thread's send":
		other := f.plainThread(t)
		if _, err := f.adapter().Send(t.Context(), threadtools.Caller{ThreadID: other.ID, Title: other.Title}, threadtools.SendCall{
			ThreadID: worker.ID, Message: "also check the docs",
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	default:
		t.Fatalf("no nudge by %q", by)
	}
}

// A person's message or another thread's send to an owned worker at rest is
// a nudge: its run is the one row the spawner lists for the worker, counting
// only what the worker did since, and the spawner reads as having open work
// below it. When the worker rests again nothing reaches the spawner: no
// answer is queued and no session is started for it. Past keeps the spawn's
// run and the nudge's run, one row each.
func TestANudgedOwnedWorkerIsOneRunOfItsSpawner(t *testing.T) {
	t.Parallel()
	for _, by := range []string{"the person", "another thread's send"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.watchAgentRows()
			f.holdCallerStart()
			gate := filepath.Join(t.TempDir(), "nudged-turn")
			installMockClaudeTurns(t, f.app, [][]string{runTurnLines("nudged", "Checked the docs.", gate)})
			worker := f.restedWorker(t, "nudged-spawn", store.PlacementOwned)

			f.nudge(t, by, worker)
			var open []ThreadAgentRow
			waitUntil(t, 10*time.Second, func() bool {
				f.app.threadRequestsWG.Wait()
				open = f.openRows(t, f.caller.ID)
				return len(open) == 1 && open[0].TurnOpen
			})
			rows := f.agentRows(t, f.caller.ID).Rows
			row := open[0]
			if len(rows) != 1 || row.Kind != "nudge" || row.ThreadID != worker.ID || row.Token == "nudged-spawn" {
				t.Fatalf("rows %+v; want one nudge row for the worker", rows)
			}
			if row.Tokens != 0 || row.ToolCalls > 1 || row.Placement != store.PlacementOwned {
				t.Fatalf("nudge row %+v; want only the run since the nudge counted", row)
			}
			if got := f.workerSummary(t, f.caller.ID); got.Open != 1 {
				t.Fatalf("summary %+v; want the nudged worker counted", got)
			}
			if rest, err := f.app.threadRest(f.caller.ID); err != nil || rest.unrest != workerWaiting {
				t.Fatalf("the spawner reads %+v, %v; want it waiting on the worker it started", rest, err)
			}
			nudge := row.Token

			release(t, gate)
			var past ThreadAgentRow
			waitUntil(t, 10*time.Second, func() bool {
				f.app.threadRequestsWG.Wait()
				var ok bool
				past, ok = f.pastRow(t, f.caller.ID, nudge)
				return ok && past.Tokens == 60
			})
			if past.Kind != "nudge" || past.ToolCalls != 1 || past.TurnOpen || past.Answer != "" {
				t.Fatalf("nudge's Past row %+v; want its run's call and tokens and no answer", past)
			}
			if spawn, ok := f.pastRow(t, f.caller.ID, "nudged-spawn"); !ok || spawn.ToolCalls != 2 || spawn.Tokens != 60 {
				t.Fatalf("spawn's Past row %+v (%v); want its own run's figures", spawn, ok)
			}
			if tokens := f.pastTokens(t, f.caller.ID); len(tokens) != 2 {
				t.Fatalf("Past %q; want the spawn's run and the nudge's", tokens)
			}
			if open := f.openRows(t, f.caller.ID); len(open) != 0 {
				t.Fatalf("open rows %+v after the worker rested", open)
			}
			if queued := durableQueueRows(t, f.app, f.caller.ID); len(queued) != 0 {
				t.Fatalf("the spawner holds %+v; want nothing delivered for a nudge", queued)
			}
			if _, live := f.app.sessionManager().get(f.caller.ID); live {
				t.Fatal("the spawner was started for a nudge's end")
			}
			if requests, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 20, 0); err != nil || len(requests) != 1 {
				t.Fatalf("thread_status lists %+v, %v; want the spawn alone, the nudge not being the spawner's request", requests, err)
			}
		})
	}
}

// An owned worker spawned before runs were kept, nudged after its spawn
// settled: the spawn stays closed with none of the worker's figures, and the
// nudge is a run of its own, the spawner's one open row for the worker.
func TestANudgeOfAWorkerSpawnedBeforeRunsIsARunOfItsOwn(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerStart()
	installMockClaudeTurns(t, f.app, [][]string{heldTurnLines("old-nudged", "On it.")})
	worker := f.plainThread(t)
	seedSpawnBeforeRuns(t, f.app, "old-nudged-spawn", f.caller.ID, worker.ID)
	workerTurn(t, f.app, worker.ID, "t0", 2, 60)
	settleSpawnBeforeRuns(t, f.app, "old-nudged-spawn")
	if _, err := f.app.store.SetThreadPlacement([]string{worker.ID}, store.PlacementOwned); err != nil {
		t.Fatal(err)
	}
	f.nudge(t, "the person", worker)
	var open []ThreadAgentRow
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		open = f.openRows(t, f.caller.ID)
		return len(open) == 1 && open[0].Worker == workerRunning
	})
	if open[0].Kind != threadAgentKindNudge || open[0].Token == "old-nudged-spawn" || open[0].Tokens != 0 {
		t.Fatalf("open rows %+v; want the nudge's run alone, counted from the nudge", open)
	}
	if spawn, ok := f.pastRow(t, f.caller.ID, "old-nudged-spawn"); !ok || spawn.ToolCalls != 0 || spawn.Tokens != 0 || spawn.TurnOpen {
		t.Fatalf("spawn's Past row %+v (%v); want it closed with no figures", spawn, ok)
	}
}

// A worker with a sidebar row that someone else continues after its
// spawner's run is no row of its spawner's and holds nothing of it.
func TestANudgedSidebarWorkerIsNoRowOfItsSpawner(t *testing.T) {
	t.Parallel()
	for _, placement := range []string{store.PlacementStandalone, store.PlacementGrouped} {
		t.Run(placement, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.watchAgentRows()
			installMockClaudeTurns(t, f.app, [][]string{heldTurnLines("continued", "On it.")})
			worker := f.restedWorker(t, "sidebar-spawn", store.PlacementStandalone)
			if placement == store.PlacementGrouped {
				group, err := f.app.store.CreateThreadGroup(worker.ProjectID, "Sweep")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.app.store.SetThreadGroup([]string{worker.ID}, group.ID); err != nil {
					t.Fatal(err)
				}
			}
			f.nudge(t, "the person", worker)
			waitUntil(t, 10*time.Second, func() bool {
				rest, err := f.app.workerUnrest(worker.ID)
				return err == nil && rest.unrest == workerRunning
			})
			f.app.threadRequestsWG.Wait()
			if rows := f.agentRows(t, f.caller.ID).Rows; slices.ContainsFunc(rows, func(row ThreadAgentRow) bool { return row.open() }) {
				t.Fatalf("rows %+v; want no open row for a run the spawner did not start", rows)
			}
			if rest, err := f.app.threadRest(f.caller.ID); err != nil || rest.unrest != "" {
				t.Fatalf("the spawner reads %+v, %v; want it at rest", rest, err)
			}
		})
	}
}

// A nudge while its spawner's run is open is part of that run: no run of
// its own is recorded and the spawn's row stays the worker's one row.
func TestANudgeDuringItsSpawnersRunJoinsThatRun(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	installMockClaudeTurns(t, f.app, [][]string{heldTurnLines("joined", "On it.")})
	worker := f.plainThread(t)
	f.acceptAgentRequest(t, "open-spawn", f.caller.ID, store.ThreadRequestSpawn, worker.ID)
	if _, err := f.app.store.SetThreadPlacement([]string{worker.ID}, store.PlacementOwned); err != nil {
		t.Fatal(err)
	}
	f.nudge(t, "the person", worker)
	waitUntil(t, 10*time.Second, func() bool {
		rest, err := f.app.workerUnrest(worker.ID)
		return err == nil && rest.unrest == workerRunning
	})
	f.app.threadRequestsWG.Wait()
	if rows := f.agentRows(t, f.caller.ID).Rows; len(rows) != 1 || rows[0].Token != "open-spawn" {
		t.Fatalf("rows %+v; want the spawn's row alone", rows)
	}
}

// The Stop of a nudged worker's row brings it to rest and tells its
// spawner, as the Stop of a worker that outlived its spawn does.
func TestAStopOfANudgedWorkersRowTellsItsSpawner(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerStart()
	installMockClaudeTurns(t, f.app, [][]string{heldTurnLines("stopped", "On it.")})
	worker := f.restedWorker(t, "stop-spawn", store.PlacementOwned)
	f.nudge(t, "the person", worker)
	var row ThreadAgentRow
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		open := f.openRows(t, f.caller.ID)
		if len(open) == 1 {
			row = open[0]
		}
		return len(open) == 1 && open[0].Worker == workerRunning
	})
	stop, err := f.app.StopThreadAgentRow(t.Context(), f.caller.ID, row.Token)
	if err != nil || stop.Outcome != threadAgentStopped {
		t.Fatalf("Stop = %+v, %v; want the nudged worker stopped", stop, err)
	}
	if rest, err := f.app.workerUnrest(worker.ID); err != nil || rest.unrest != "" {
		t.Fatalf("the worker reads %+v, %v; want it at rest", rest, err)
	}
	if got := len(workerStoppedTexts(t, f.app, f.caller.ID, row.Token)); got != 1 {
		t.Fatalf("the spawner was told %d times, want once", got)
	}
}

// A rest read while a nudge's message is on its way, before the worker
// has it, ends no run: the nudge's run counts the work its message starts.
func TestANudgesRunIsNotEndedBeforeItsMessageLands(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerStart()
	installMockClaudeTurns(t, f.app, [][]string{heldTurnLines("landing", "On it.")})
	worker := f.restedWorker(t, "landing-spawn", store.PlacementOwned)
	ended := make(chan error, 1)
	f.app.sendStageFn = func(threadID, stage string) {
		if threadID == worker.ID && stage == "placed" {
			ended <- f.app.endRunsAtRest(worker.ID)
		}
	}
	f.nudge(t, "the person", worker)
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
	f.app.threadRequestsWG.Wait()
	open := f.openRows(t, f.caller.ID)
	if len(open) != 1 || open[0].Kind != threadAgentKindNudge {
		t.Fatalf("open rows %+v; want the nudge's run open once its message landed", open)
	}
	if row := f.request(t, open[0].Token); !row.RunOpen() {
		t.Fatalf("the nudge %+v; want its run open", row)
	}
}

// A nudged turn that ends before its message's send returns ends its run
// once the send is over: the rest pass at the turn's end found the thread
// held for the message and skipped it, and nothing else would read it.
func TestANudgeWhoseTurnEndsBeforeItsSendReturnsEnds(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.restedWorker(t, "quick-spawn", store.PlacementOwned)
	f.app.sendMessageFn = func(threadID, _ string, _ []string) error {
		workerTurn(t, f.app, threadID, "quick", 1, 30)
		f.app.threadRequestsWG.Wait()
		return nil
	}
	f.nudge(t, "the person", worker)
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		open, err := f.app.store.CallerRunOpen(f.caller.ID, "", worker.ID)
		return err == nil && !open
	})
	if open := f.openRows(t, f.caller.ID); len(open) != 0 {
		t.Fatalf("open rows %+v after the nudged turn ended", open)
	}
}

// A nudge whose message is refused starts no work and records no run.
func TestARefusedNudgeRecordsNoRun(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.restedWorker(t, "refused-spawn", store.PlacementOwned)
	f.app.sendMessageFn = func(string, string, []string) error { return errors.New("refused") }
	if _, err := f.app.SendMessageWithOptions(t.Context(), worker.ID, "also check the docs", pageSendOptions(uuid.NewString())); err == nil {
		t.Fatal("the refused message was sent")
	}
	f.app.threadRequestsWG.Wait()
	if open := f.openRows(t, f.caller.ID); len(open) != 0 {
		t.Fatalf("open rows %+v; want none for a refused nudge", open)
	}
	if tokens := f.pastTokens(t, f.caller.ID); len(tokens) != 1 || tokens[0] != "refused-spawn" {
		t.Fatalf("Past %q; want the spawn's run alone", tokens)
	}
}

// Show in Sidebar of a nudged owned worker ends the nudge's run then: the
// worker is the person's from there, no row of its spawner's, and holds the
// spawner no longer.
func TestShowInSidebarEndsANudgesRun(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerStart()
	installMockClaudeTurns(t, f.app, [][]string{heldTurnLines("shown", "On it.")})
	worker := f.restedWorker(t, "shown-spawn", store.PlacementOwned)
	f.nudge(t, "the person", worker)
	var nudge string
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		open := f.openRows(t, f.caller.ID)
		if len(open) == 1 {
			nudge = open[0].Token
		}
		return len(open) == 1 && open[0].Worker == workerRunning
	})
	if _, err := f.app.SetThreadPlacement([]string{worker.ID}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	f.app.threadRequestsWG.Wait()
	if row := f.request(t, nudge); !row.RunEnded() {
		t.Fatalf("the nudge %+v; want its run ended when the worker got a sidebar row", row)
	}
	if open := f.openRows(t, f.caller.ID); len(open) != 0 {
		t.Fatalf("open rows %+v; want none once the worker has a sidebar row", open)
	}
	if rest, err := f.app.threadRest(f.caller.ID); err != nil || rest.unrest != "" {
		t.Fatalf("the spawner reads %+v, %v; want it at rest", rest, err)
	}
	if rest, err := f.app.workerUnrest(worker.ID); err != nil || rest.unrest != workerRunning {
		t.Fatalf("the worker reads %+v, %v; the test needs it still running", rest, err)
	}
	// Taken back out of the sidebar, a later message is a nudge again.
	if _, err := f.app.SetThreadPlacement([]string{worker.ID}, store.PlacementOwned); err != nil {
		t.Fatal(err)
	}
	if open := f.openRows(t, f.caller.ID); len(open) != 0 {
		t.Fatalf("open rows %+v; Remove from Sidebar starts no run of its own", open)
	}
}
