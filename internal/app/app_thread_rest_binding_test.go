package app

// A workflow run keeps the thread it reports to from rest while it runs and
// while its report is owed; its binding moving reassesses the thread it left
// as well as the one it reports to now; and a held request reads its
// thread's rest itself, whatever the live worker record says.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/engine"
)

// reportTurn is the worker's turn that reads a run's report: it echoes the
// message, streams its answer, as the CLI does, and ends.
func reportTurn(text string) []string {
	return []string{
		mockClaudeInitLine,
		testutil.MockReplayUserMessage(),
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg-report","role":"assistant"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + quoteJSON(text) + `}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"assistant","message":{"id":"msg-report","role":"assistant","content":[{"type":"text","text":` + quoteJSON(text) + `}]}}`,
		mockClaudeResultLine,
	}
}

// reportingRuns starts workflow runs in a project whose one tool check
// holds until gate exists, then passes: running until the gate, then done.
type reportingRuns struct {
	f       *requestFixture
	project store.Project
	gate    string
}

// reportingRuns writes the workflow and its profile in thread's project and
// starts the engine. The check is released when the test ends.
func (f *requestFixture) reportingRuns(t *testing.T, threadID string) *reportingRuns {
	t.Helper()
	thread, err := f.app.store.GetThread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	project := mustReloadProject(t, f.app.store, thread.ProjectID)
	root := t.TempDir()
	dir := filepath.Join(root, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	definition := `id: tool-flow
name: Tool flow
phases:
  - id: check
    driver: tool
    check: hold
    gate:
      routes:
        - when:
            eq:
              ref: check.passed
              value: true
          to: done
        - to: failed
cleanup: manual
`
	if err := os.WriteFile(filepath.Join(dir, "tool-flow.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(t.TempDir(), "run-gate")
	script := writeExecutable(t, "hold.sh", "#!/bin/sh\nwhile [ ! -e "+gate+" ]; do sleep 0.05; done\necho ok\n")
	profile := filepath.Join(root, "projects", project.Slug)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "profile.yaml"), []byte("checks:\n  hold: [\""+script+"\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, f.app, root)
	t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0o600) })
	return &reportingRuns{f: f, project: project, gate: gate}
}

// start starts a run threadID asked for, as run_start does: agent-sourced
// and bound to it.
func (r *reportingRuns) start(t *testing.T, threadID, goal string) store.WorkItem {
	t.Helper()
	item, err := r.f.app.startWorkflowRun(r.project.ID, "tool-flow", "shared", goal, json.RawMessage(`{}`), nil, "", false, workflowSourceAgent, "")
	if err != nil {
		t.Fatal(err)
	}
	scope := transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: threadID, ProjectID: r.project.ID}
	if reason := r.f.app.bindOriginThread(scope, item); reason != "" {
		t.Fatalf("bind the run: %s", reason)
	}
	return item
}

// finish lets the check pass, and waits for the run to be done.
func (r *reportingRuns) finish(t *testing.T, item store.WorkItem) {
	t.Helper()
	release(t, r.gate)
	waitForWorkflowItem(t, r.f.app, item.ID, engine.StateDone, "")
}

// awaitWorkerRow waits until token's row shows its worker as worker with
// activity, "" for both once it rests.
func (f *requestFixture) awaitWorkerRow(t *testing.T, token string, worker workerState, activity string) {
	t.Helper()
	var row ThreadAgentRow
	deadline := time.Now().Add(10 * time.Second)
	for {
		row, _ = agentRowByToken(f.agentRows(t, f.caller.ID).Rows, token)
		if row.Worker == string(worker) && (worker == workerAtRest || row.Activity == activity) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("row %s shows worker %q with %q; want %q with %q", token, row.Worker, row.Activity, worker, activity)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A run bound to worker W keeps W, and its spawn, from rest. Rebound to
// worker X, W rests: its row shows it at rest and its request settles,
// while X, its loop cancelled, is kept by the run. Unbound, X rests.
func TestRebindingARunLetsTheWorkerItLeftRest(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	turns := filepath.Join(t.TempDir(), "turns")
	installMockClaudeTurns(t, f.app, [][]string{gatedTurn(turns, "On it.")})
	w := f.spawnWorker(t, "run the checks")
	x := f.spawnWorker(t, "watch the board")
	f.awaitTurnRunning(t, w.Token)
	f.awaitTurnRunning(t, x.Token)
	item := f.reportingRuns(t, w.ThreadID).start(t, w.ThreadID, "ship it")
	f.armLoop(t, x.ThreadID, "board")
	release(t, turns)
	f.awaitRestHold(t, w)
	f.awaitRestHold(t, x)
	f.awaitWorkerRow(t, w.Token, workerWaiting, "Workflow · ship it")

	if _, err := f.app.WorkflowBindThread(item.ID, x.ThreadID); err != nil {
		t.Fatalf("WorkflowBindThread: %v", err)
	}
	f.awaitWorkerRow(t, w.Token, "", "")
	if row := f.awaitRequestState(t, w.Token, store.ThreadRequestFinished); string(row.Answer) != "On it." {
		t.Fatalf("W settled %+v; want finished with its message", row)
	}

	wakes, err := f.app.store.ListAgentWakes(x.ThreadID)
	if err != nil || len(wakes) != 1 {
		t.Fatalf("X's wakes %+v, %v", wakes, err)
	}
	if err := f.app.CancelThreadWake(x.ThreadID, store.AgentWakeLoop, wakes[0].ID); err != nil {
		t.Fatalf("CancelThreadWake: %v", err)
	}
	f.awaitWorkerRow(t, x.Token, workerWaiting, "Workflow · ship it")
	f.app.threadRequestsWG.Wait()
	f.app.settleRestedReceipts()
	if row := f.request(t, x.Token); row.State != store.ThreadRequestRunning || !f.app.restHeld(x.ThreadID, x.Token) {
		t.Fatalf("X's request %+v while the run reports to it; want it held", row)
	}

	if _, err := f.app.WorkflowUnbindThread(item.ID); err != nil {
		t.Fatalf("WorkflowUnbindThread: %v", err)
	}
	f.awaitWorkerRow(t, x.Token, "", "")
	if row := f.awaitRequestState(t, x.Token, store.ThreadRequestFinished); string(row.Answer) != "On it." {
		t.Fatalf("X settled %+v; want finished with its message", row)
	}
}

// A held request whose thread is at rest settles on the next pass even
// when the live worker record, stale, still says the thread waits: the
// settlement reads the thread's rest itself.
func TestAStaleWorkerRecordDoesNotHoldARequest(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	ack := f.heldOnALoop(t)
	// The loop goes with no frame, and the record is planted as waiting on
	// it, as a missed invalidation leaves it.
	if _, found, err := f.app.store.CancelAgentWake(ack.ThreadID, "board", store.AgentWakeLoop, agentWakeOutcomeCancelled); err != nil || !found {
		t.Fatalf("CancelAgentWake: found=%v err=%v", found, err)
	}
	f.app.threadRequestsWG.Wait()
	stale := workerRest{unrest: workerWaiting, detail: "Loop · board"}
	f.app.liveWorkers.mu.Lock()
	f.app.liveWorkers.threads[ack.ThreadID] = liveWorker{workerRest: stale, link: ack.Token, waits: stale}
	f.app.liveWorkers.mu.Unlock()

	f.app.settleRestedReceipts()
	f.app.threadRequestsWG.Wait()
	if row := f.request(t, ack.Token); row.State != store.ThreadRequestFinished || string(row.Answer) != "Watching the board." {
		t.Fatalf("request %+v with its thread at rest; want finished with its message", row)
	}
}

// A run's report is owed between its rest and the wake that carries it:
// the wake queue blocked, the run done, a rest pass keeps the spawn held.
// Released, the report reaches the worker, and the spawn settles once the
// worker's turn for it ends, with that turn's message.
func TestAWorkflowRunsOwedReportHoldsTheSpawnUntilTheWorkerReadsIt(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	turns := filepath.Join(t.TempDir(), "turns")
	installMockClaudeTurns(t, f.app, [][]string{
		gatedTurn(turns, "Started the run."),
		reportTurn("The run is done."),
	})
	w := f.spawnWorker(t, "run the checks")
	f.awaitTurnRunning(t, w.Token)
	runs := f.reportingRuns(t, w.ThreadID)
	item := runs.start(t, w.ThreadID, "ship it")
	release(t, turns)
	f.awaitRestHold(t, w)

	unblock := f.app.workflowApplication().BlockWakeQueueForTesting()
	t.Cleanup(unblock)
	runs.finish(t, item)
	f.app.threadRequestsWG.Wait()
	f.app.settleRestedReceipts()
	f.app.threadRequestsWG.Wait()
	if row := f.request(t, w.Token); row.State != store.ThreadRequestRunning || !f.app.restHeld(w.ThreadID, w.Token) {
		t.Fatalf("request %+v with the run's report owed; want it held", row)
	}
	f.awaitWorkerRow(t, w.Token, workerWaiting, "Workflow · ship it")

	unblock()
	row := f.awaitRequestState(t, w.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "The run is done." {
		t.Fatalf("settled %+v; want finished with the turn that read the report", row)
	}
	f.awaitWorkerRow(t, w.Token, "", "")
	f.expectNoReportOwed(t, w.ThreadID)
}

// A running run bound below the worker is bound to a thread off the
// worker's tree and to the child below it, two store writes with nothing
// told between them, and rests with its report's delivery held. The hold is
// where the last write left the binding: bound below, the worker's request
// stays held, the thread the run left reads at rest, and the request
// settles once the child has read the report; bound off the tree, the
// request settles at the worker's turn end and that thread reads busy.
func TestARunsHoldFollowsItsLastBinding(t *testing.T) {
	t.Parallel()
	for _, last := range []string{"below", "off the tree"} {
		t.Run(last, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			worker, token, gate, children := f.heldWorker(t, 1)
			child, off := children[0].ID, f.caller.ID
			runs := f.reportingRuns(t, child)
			item := runs.start(t, child, "ship it")
			order, holder, left := []string{off, child}, child, off
			if last != "below" {
				order, holder, left = []string{child, off}, off, child
			}
			for _, thread := range order {
				if _, err := f.app.store.UpdateWorkItemOriginThread(item.ID, thread); err != nil {
					t.Fatal(err)
				}
			}
			unblock := f.app.workflowApplication().BlockWakeQueueForTesting()
			t.Cleanup(unblock)
			runs.finish(t, item)
			f.endWorkerTurn(t, worker, gate)
			if held, err := f.app.store.ReadThreadRestSources(holder); err != nil || !held.Workflow {
				t.Fatalf("the thread the run reports to: %+v, %v; want it held by the run", held, err)
			}
			if rested, err := f.app.store.ReadThreadRestSources(left); err != nil || rested.Workflow {
				t.Fatalf("the thread the run left: %+v, %v; want it at rest", rested, err)
			}
			if holder == off {
				f.expectFinished(t, token)
				return
			}
			f.expectHeld(t, worker, token, "with the report owed to the child below")
			// The child's session, started by the report, reads it.
			installMockClaudeTurns(t, f.app, [][]string{reportTurn("Read the report.")})
			unblock()
			f.expectFinished(t, token)
		})
	}
}

// A run that ends cancelled reports that to the worker like any rest; the
// spawn settles once the worker has read it, and nothing is left owed.
func TestACancelledWorkflowRunLetsTheSpawnSettle(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	turns := filepath.Join(t.TempDir(), "turns")
	installMockClaudeTurns(t, f.app, [][]string{
		gatedTurn(turns, "Started the run."),
		reportTurn("The run was cancelled."),
	})
	w := f.spawnWorker(t, "run the checks")
	f.awaitTurnRunning(t, w.Token)
	item := f.reportingRuns(t, w.ThreadID).start(t, w.ThreadID, "ship it")
	release(t, turns)
	f.awaitRestHold(t, w)

	if err := f.app.WorkflowCancelItem(t.Context(), item.ID); err != nil {
		t.Fatalf("WorkflowCancelItem: %v", err)
	}
	row := f.awaitRequestState(t, w.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "The run was cancelled." {
		t.Fatalf("settled %+v; want finished with the turn that read the report", row)
	}
	f.awaitWorkerRow(t, w.Token, "", "")
	f.app.workflowApplication().WaitWake()
	f.expectNoReportOwed(t, w.ThreadID)
}

// A rest whose wake says what the worker was already told coalesces: the
// report it owed ends with its wake job, and the spawn held on it settles.
func TestACoalescedWorkflowWakeLetsTheSpawnSettle(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	turns, read := filepath.Join(t.TempDir(), "turns"), filepath.Join(t.TempDir(), "read")
	installMockClaudeTurns(t, f.app, [][]string{
		gatedTurn(turns, "Started the run."),
		append(reportTurn("The run is done.")[:7:7], testutil.MockWaitForFile(read), mockClaudeResultLine),
	})
	w := f.spawnWorker(t, "run the checks")
	f.awaitTurnRunning(t, w.Token)
	runs := f.reportingRuns(t, w.ThreadID)
	item := runs.start(t, w.ThreadID, "ship it")
	release(t, turns)
	f.awaitRestHold(t, w)
	runs.finish(t, item)
	waitUntil(t, 20*time.Second, func() bool {
		_, found, err := f.app.store.GetTurnByThreadIndex(w.ThreadID, 1)
		return err == nil && found
	})

	// The run runs and comes to the same rest again while the worker reads
	// the first.
	unblock := f.app.workflowApplication().BlockWakeQueueForTesting()
	t.Cleanup(unblock)
	if err := f.app.store.UpdateWorkItemState(item.ID, string(engine.StateRunning), "", 0); err != nil {
		t.Fatal(err)
	}
	if err := f.app.store.UpdateWorkItemState(item.ID, string(engine.StateDone), "", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	f.app.workflowApplication().HandleEngineEvent(eventchan.WorkflowItemState, engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateDone,
	}, f.app.emit)
	release(t, read)
	waitUntil(t, 20*time.Second, func() bool {
		turn, found, err := f.app.store.GetTurnByThreadIndex(w.ThreadID, 1)
		return err == nil && found && turn.CompletedAt != nil
	})
	f.app.threadRequestsWG.Wait()
	f.app.settleRestedReceipts()
	f.app.threadRequestsWG.Wait()
	if row := f.request(t, w.Token); row.State != store.ThreadRequestRunning {
		t.Fatalf("request %+v with the repeat's report owed; want it held", row)
	}

	unblock()
	row := f.awaitRequestState(t, w.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "The run is done." {
		t.Fatalf("settled %+v; want finished with the turn that read the report", row)
	}
	f.awaitWorkerRow(t, w.Token, "", "")
	f.expectNoReportOwed(t, w.ThreadID)
	if turn, found, err := f.app.store.GetTurnByThreadIndex(w.ThreadID, 2); err != nil || found {
		t.Fatalf("the repeat reached the worker: turn %+v, %v", turn, err)
	}
}

// expectNoReportOwed fails unless no workflow run holds threadID from rest.
func (f *requestFixture) expectNoReportOwed(t *testing.T, threadID string) {
	t.Helper()
	if src, err := f.app.store.ReadThreadRestSources(threadID); err != nil || src.Workflow {
		t.Fatalf("a run still holds %s: %+v, %v", threadID, src, err)
	}
}

// A report a previous process owed died with its wake job: the engine's
// start forgets it, and the thread the run is bound to reads at rest.
func TestTheEnginesStartForgetsTheReportsAPreviousProcessOwed(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	item := store.WorkItem{ID: "owed-before", ProjectID: f.caller.ProjectID, Goal: "ship it", WorkflowID: "tool-flow",
		WorkflowScope: "shared", State: string(engine.StateRunning), Source: "manual", OriginThreadID: f.caller.ID,
		CreatedAt: time.Now().UnixMilli()}
	if err := f.app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	if err := f.app.store.UpdateWorkItemState(item.ID, string(engine.StateDone), "", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if src, err := f.app.store.ReadThreadRestSources(f.caller.ID); err != nil || !src.Workflow {
		t.Fatalf("the report owed before the start: %+v, %v; want it held", src, err)
	}
	f.reportingRuns(t, f.caller.ID)
	f.expectNoReportOwed(t, f.caller.ID)
}
