package app

// A spawn completes when its worker is at rest, not when its turn first
// ends (docs/specs/agent-thread-tools.md, "What settles a request"): the
// request stays running while the thread waits on what it started, the
// spawner reads what that is, nothing wakes it, and the request settles
// `finished` with the thread's last message once nothing is left. A send
// to a thread the caller spawned waits the same way; a send to any other
// thread settles on its turn's end.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"

	"github.com/google/uuid"
)

const restShellCommand = "tail -f deploy.log"

// restShellLaunch is a turn's wire lines up to a background Bash it leaves
// running, then text, without the turn's result.
func restShellLaunch(tool, task, text string) []string {
	return []string{
		mockClaudeInitLine,
		fmt.Sprintf(`{"type":"assistant","message":{"id":"msg-%s","role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":%q,"run_in_background":true}}]}}`, tool, tool, restShellCommand),
		fmt.Sprintf(`{"type":"user","tool_use_result":{"stdout":"","stderr":"","interrupted":false,"backgroundTaskId":%q},"message":{"role":"user","content":[{"tool_use_id":%q,"type":"tool_result","content":"Command running in background with ID: %s. Output: /tmp/%s.out","is_error":false}]}}`, task, tool, task, task),
		fmt.Sprintf(`{"type":"system","subtype":"task_started","task_id":%q,"tool_use_id":%q}`, task, tool),
		`{"type":"assistant","message":{"id":"msg-text-` + tool + `","role":"assistant","content":[{"type":"text","text":` + quoteJSON(text) + `}]}}`,
	}
}

// restShellDone is the background Bash ending, as the CLI reports it.
func restShellDone(tool, task string) []string {
	return []string{
		fmt.Sprintf(`{"type":"system","subtype":"task_updated","task_id":%q,"tool_use_id":%q,"patch":{"status":"completed"}}`, task, tool),
		fmt.Sprintf(`{"type":"system","subtype":"task_notification","task_id":%q,"tool_use_id":%q,"status":"completed","summary":"done"}`, task, tool),
	}
}

const mockClaudeResultLine = `{"type":"result","subtype":"success","is_error":false}`

// spawnWorker spawns a worker with nothing to wait on and notify set, so
// its answer is owed to the caller as a message.
func (f *requestFixture) spawnWorker(t *testing.T, prompt string) threadtools.RequestAck {
	t.Helper()
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: prompt, Notify: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	return ack
}

// awaitRestHold waits until the request waits for its thread's rest, its
// turn over, and asserts the spawner is told nothing of it meanwhile.
func (f *requestFixture) awaitRestHold(t *testing.T, ack threadtools.RequestAck) {
	t.Helper()
	waitUntil(t, 20*time.Second, func() bool { return f.app.restHeld(ack.ThreadID, ack.Token) })
	f.app.threadRequestsWG.Wait()
	if row := f.request(t, ack.Token); row.State != store.ThreadRequestRunning {
		t.Fatalf("request %+v; want it running while its worker is not at rest", row)
	}
	for _, row := range durableQueueRows(t, f.app, f.caller.ID) {
		if strings.Contains(row.Message, ack.Token) {
			t.Fatalf("the spawner was woken while its worker works: %+v", row)
		}
	}
}

// statusOf reads one request as thread_status reports it, waiting at most
// wait seconds.
func (f *requestFixture) statusOf(t *testing.T, token string, wait int) threadtools.StatusReport {
	t.Helper()
	report, err := f.adapter().RequestStates(t.Context(), f.callerIdentity(), threadtools.StatusCall{Tokens: []string{token}, WaitSeconds: wait})
	if err != nil {
		t.Fatalf("RequestStates: %v", err)
	}
	return report
}

// The worker's turn ends with its background shell running: the request
// stays running, a wait on it times out saying the shell is what it waits
// on, thread_status with its id says it is not at rest, and nothing wakes
// the spawner. The shell ends: the request settles `finished` with the
// worker's last message, and the spawner is woken once.
func TestASpawnSettlesWhenItsWorkerRests(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	gate := filepath.Join(t.TempDir(), "shell-done")
	turn := append(restShellLaunch("bg-1", "task-1", "Tailing the deploy log."), mockClaudeResultLine, testutil.MockWaitForFile(gate))
	installMockClaudeTurns(t, f.app, [][]string{append(turn, restShellDone("bg-1", "task-1")...)})

	ack := f.spawnWorker(t, "watch the deploy")
	f.awaitRestHold(t, ack)
	// Held, it stays in the turn-end gate: the worker's later turn ends
	// and session ends still read it.
	if tokens, _ := f.app.runningReceiptTokens(ack.ThreadID); !slices.Contains(tokens, ack.Token) {
		t.Fatalf("the held request left the turn-end gate: %v", tokens)
	}
	report := f.statusOf(t, ack.Token, 1)
	if !report.TimedOut || len(report.Requests) != 1 || report.Requests[0].State != threadtools.RequestRunning ||
		!strings.Contains(report.Requests[0].WaitingOn, "deploy.log") {
		t.Fatalf("status while the shell runs %+v; want a timed out wait on a running request waiting on the shell", report)
	}
	watch, err := f.adapter().RequestStates(t.Context(), f.callerIdentity(), threadtools.StatusCall{ThreadIDs: []string{ack.ThreadID}})
	if err != nil || len(watch.Threads) != 1 || watch.Threads[0].Resting || !strings.Contains(watch.Threads[0].WaitingOn, "deploy.log") {
		t.Fatalf("thread status while the shell runs %+v, %v; want it not at rest, waiting on the shell", watch, err)
	}

	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "Tailing the deploy log." || row.AnswerKind != store.ThreadAnswerFinal || row.Revision != 1 {
		t.Fatalf("settled %+v; want finished once with the worker's last message", row)
	}
	waitUntil(t, 10*time.Second, func() bool { return len(durableQueueRows(t, f.app, f.caller.ID)) == 1 })
	if f.app.restHeld(ack.ThreadID, ack.Token) {
		t.Fatal("the settled request is still held")
	}
	watch, err = f.adapter().RequestStates(t.Context(), f.callerIdentity(), threadtools.StatusCall{ThreadIDs: []string{ack.ThreadID}})
	if err != nil || len(watch.Threads) != 1 || !watch.Threads[0].Resting || watch.Threads[0].WaitingOn != "" {
		t.Fatalf("thread status at rest %+v, %v", watch, err)
	}
}

// A person's message to a held worker opens a turn: from the send's write
// to the turn's published round the worker is not at rest. The real start
// is held after it claimed the send, with the shell the worker waited on
// already ended: thread_status does not read it resting, a rest pass does
// not settle the spawn, and the spawn settles once with that turn's message,
// waking the spawner once.
func TestAHeldSpawnsWorkerIsNotAtRestWhileATurnStarts(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	later := append(restShellDone("bg-6", "task-6"), mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-started","role":"assistant","content":[{"type":"text","text":"Answered."}]}}`,
		mockClaudeResultLine)
	installMockClaudeTurns(t, f.app, [][]string{
		append(restShellLaunch("bg-6", "task-6", "Watching."), mockClaudeResultLine),
		later,
	})
	ack := f.spawnWorker(t, "watch the deploy")
	f.awaitRestHold(t, ack)

	inStart, proceed := make(chan struct{}), make(chan struct{})
	var once, released sync.Once
	letGo := func() { released.Do(func() { close(proceed) }) }
	f.app.triage.SetTurnRowWrittenGate(func(id string) {
		if id == ack.ThreadID {
			once.Do(func() { close(inStart); <-proceed })
		}
	})
	t.Cleanup(func() { letGo(); f.app.triage.SetTurnRowWrittenGate(nil) })
	if _, err := f.app.SendMessageWithOptions(t.Context(), ack.ThreadID, "how is it going?", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	select {
	case <-inStart:
	case <-time.After(20 * time.Second):
		t.Fatal("the worker's turn start never reached the gate")
	}
	if running, err := f.app.runningBackgroundTaskCount(ack.ThreadID); err != nil || running != 0 {
		t.Fatalf("background work %d, %v; want the shell ended before the start", running, err)
	}
	state, err := f.app.threadTools().threadStateNow(t.Context(), ack.ThreadID)
	if err != nil || state.Resting {
		t.Fatalf("thread_status inside the start %+v, %v; want it not resting", state, err)
	}
	read, err := f.app.heldThreadAtRest(ack.ThreadID)
	read.done()
	if err != nil || read.rests(ack.ThreadID) {
		t.Fatalf("a held request reads the worker at rest inside the start: %v, %v", read.rests(ack.ThreadID), err)
	}
	f.app.settleRestedReceipts()
	f.app.threadRequestsWG.Wait()
	if row := f.request(t, ack.Token); row.State != store.ThreadRequestRunning {
		t.Fatalf("request %+v settled while its worker's turn starts", row)
	}
	letGo()
	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "Answered." || row.Revision != 1 {
		t.Fatalf("settled %+v; want finished once with the new turn's message", row)
	}
	waitUntil(t, 10*time.Second, func() bool { return len(durableQueueRows(t, f.app, f.caller.ID)) == 1 })
}

// gatedTurn is a turn that says text and holds its result until gate
// exists, so the test can arm what the worker waits on mid-turn.
func gatedTurn(gate, text string) []string {
	return []string{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-gated","role":"assistant","content":[{"type":"text","text":` + quoteJSON(text) + `}]}}`,
		testutil.MockWaitForFile(gate),
		mockClaudeResultLine,
	}
}

func release(t *testing.T, gate string) {
	t.Helper()
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// awaitTurnRunning waits until the spawn's message is in a running turn of
// its thread.
func (f *requestFixture) awaitTurnRunning(t *testing.T, token string) store.ThreadRequestReceipt {
	t.Helper()
	var receipt store.ThreadRequestReceipt
	waitUntil(t, 20*time.Second, func() bool {
		r, found, err := f.app.store.GetThreadRequestReceipt(token)
		receipt = r
		return err == nil && found && r.State == store.ThreadReceiptRunning && r.TurnID != ""
	})
	return receipt
}

// A worker whose own sub-thread is still open is not at rest: its spawn
// waits for that one to settle.
func TestAHeldSpawnWaitsOnItsWorkersOwnSubThread(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	gate := filepath.Join(t.TempDir(), "turn")
	installMockClaudeTurns(t, f.app, [][]string{gatedTurn(gate, "Handed it on.")})
	ack := f.spawnWorker(t, "run the board")
	f.awaitTurnRunning(t, ack.Token)
	grand, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{Token: "grand-spawn", CallerThreadID: ack.ThreadID,
		Kind: store.ThreadRequestSpawn, TargetThreadID: grand.ID, State: store.ThreadRequestRunning, CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	release(t, gate)
	f.awaitRestHold(t, ack)
	if got := f.statusOf(t, ack.Token, 0).Requests[0].WaitingOn; got != "threads it started" {
		t.Fatalf("waiting on %q; want its own threads", got)
	}
	if _, err := f.app.store.SettleThreadRequest("grand-spawn", store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal,
	}); err != nil {
		t.Fatal(err)
	}
	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "Handed it on." {
		t.Fatalf("settled %+v; want the worker's last message", row)
	}
}

// armLoop arms a loop in the worker's name.
func (f *requestFixture) armLoop(t *testing.T, worker, label string) {
	t.Helper()
	if _, err := f.adapter().Loop(t.Context(), threadtools.Caller{ThreadID: worker}, threadtools.LoopCall{Label: label, Note: "check", EverySeconds: 3600}); err != nil {
		t.Fatalf("Loop: %v", err)
	}
}

// A restart between the turn's end and the rest keeps the request held,
// open while the worker's loop is armed, and settles it once that is
// cancelled. A worker that waited only on a shell the restart ended is at
// rest after the boot, and its request settles then.
func TestAHeldSpawnAcrossARestart(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	gate := filepath.Join(t.TempDir(), "turn")
	installMockClaudeTurns(t, f.app, [][]string{gatedTurn(gate, "Looping on the board.")})
	looped := f.spawnWorker(t, "keep an eye on the board")
	f.awaitTurnRunning(t, looped.Token)
	f.armLoop(t, looped.ThreadID, "board")
	release(t, gate)
	f.awaitRestHold(t, looped)

	after := f.restart(t)
	after.watchAgentRows()
	after.app.sweepThreadRequestsAtBoot()
	if !after.app.restHeld(looped.ThreadID, looped.Token) {
		t.Fatal("the boot did not hold the request again")
	}
	after.app.recoverRestAtBoot()
	after.app.threadRequestsWG.Wait()
	if row := after.request(t, looped.Token); row.State != store.ThreadRequestRunning {
		t.Fatalf("request after the boot %+v; want it running while the loop is armed", row)
	}
	if got := after.statusOf(t, looped.Token, 0).Requests[0].WaitingOn; got != "Loop · board" {
		t.Fatalf("waiting on %q; want the loop", got)
	}
	wakes, err := after.app.store.ListAgentWakes(looped.ThreadID)
	if err != nil || len(wakes) != 1 {
		t.Fatalf("wakes %+v, %v", wakes, err)
	}
	if err := after.app.CancelThreadWake(looped.ThreadID, store.AgentWakeLoop, wakes[0].ID); err != nil {
		t.Fatalf("CancelThreadWake: %v", err)
	}
	row := after.awaitRequestState(t, looped.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "Looping on the board." {
		t.Fatalf("settled %+v; want the worker's last message", row)
	}
}

// A worker that waited on its shell alone is at rest once the boot settled
// the shell the restart ended, and the boot's first pass settles it.
func TestAHeldSpawnWhoseShellTheRestartEndedSettlesAtBoot(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	installMockClaudeTurns(t, f.app, [][]string{append(restShellLaunch("bg-3", "task-3", "Tailing it."), mockClaudeResultLine)})
	ack := f.spawnWorker(t, "watch the deploy")
	f.awaitRestHold(t, ack)

	after := f.restart(t)
	after.watchAgentRows()
	after.app.sweepThreadRequestsAtBoot()
	if _, err := after.app.triage.RecoverOrphanedBackgroundTasks(); err != nil {
		t.Fatal(err)
	}
	after.app.recoverRestAtBoot()
	row := after.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "Tailing it." {
		t.Fatalf("request after the boot %+v; want it finished with the worker's last message", row)
	}
}

// The spawner stopping its worker (thread_cancel by thread id) settles the
// held request cancelled and stops the loop that held it. An explicit
// thread_reply settles another at once.
func TestAHeldSpawnEndsByAStopOrAReply(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	gate := filepath.Join(t.TempDir(), "turn")
	installMockClaudeTurns(t, f.app, [][]string{gatedTurn(gate, "Looping.")})
	stopped := f.spawnWorker(t, "loop on it")
	f.awaitTurnRunning(t, stopped.Token)
	f.armLoop(t, stopped.ThreadID, "stopped")
	release(t, gate)
	f.awaitRestHold(t, stopped)
	report, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{ThreadID: stopped.ThreadID})
	if err != nil || report.Effect != threadtools.EffectThreadStopped {
		t.Fatalf("Cancel %+v, %v; want the thread stopped", report, err)
	}
	if row := f.awaitRequestState(t, stopped.Token, store.ThreadRequestCancelled); row.State != store.ThreadRequestCancelled {
		t.Fatalf("request after the stop %+v", row)
	}
	if wakes, err := f.app.store.ListAgentWakes(stopped.ThreadID); err != nil || len(wakes) != 0 {
		t.Fatalf("wakes after the stop %+v, %v; want the loop gone", wakes, err)
	}

	gate2 := filepath.Join(t.TempDir(), "turn2")
	installMockClaudeTurns(t, f.app, [][]string{gatedTurn(gate2, "Looping too.")})
	replied := f.spawnWorker(t, "loop on it too")
	f.awaitTurnRunning(t, replied.Token)
	f.armLoop(t, replied.ThreadID, "replied")
	release(t, gate2)
	f.awaitRestHold(t, replied)
	if _, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, replied.ThreadID), threadtools.ReplyCall{Token: replied.Token, Text: "Looping, and here is the answer."}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	row := f.awaitRequestState(t, replied.Token, store.ThreadRequestReplied)
	if string(row.Answer) != "Looping, and here is the answer." {
		t.Fatalf("replied %+v", row)
	}
	waitUntil(t, 5*time.Second, func() bool { return !f.app.restHeld(replied.ThreadID, replied.Token) })
}

// A stop cancels a worker's loop before it settles the requests on the
// worker. The pass the loop's cancel asks for meanwhile leaves the held
// request to the stop; one the stop left open reads the rest once the stop
// lets go of the thread.
func TestAHeldSpawnIsTheStopsWhileItBringsTheWorkerToRest(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	gate := filepath.Join(t.TempDir(), "turn")
	installMockClaudeTurns(t, f.app, [][]string{gatedTurn(gate, "Looping.")})
	ack := f.spawnWorker(t, "loop on it")
	f.awaitTurnRunning(t, ack.Token)
	f.armLoop(t, ack.ThreadID, "stopping")
	release(t, gate)
	f.awaitRestHold(t, ack)

	letGo := f.app.holdRest(ack.ThreadID)
	if err := f.app.cancelAgentWakesForThread(ack.ThreadID, agentWakeOutcomeCancelled); err != nil {
		t.Fatal(err)
	}
	f.app.settleRestedReceipts()
	f.app.threadRequestsWG.Wait()
	if row := f.request(t, ack.Token); row.State != store.ThreadRequestRunning || !f.app.restHeld(ack.ThreadID, ack.Token) {
		t.Fatalf("request %+v settled at rest while a stop brings its worker to rest", row)
	}
	letGo()
	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "Looping." {
		t.Fatalf("settled %+v; want the worker's last message", row)
	}
}

// A send to the thread the caller spawned waits for its rest as the spawn
// did; a send to a thread it did not spawn settles when its turn ends,
// whatever it left running.
func TestASendWaitsForRestOnlyOnTheCallersOwnWorker(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	installMockClaudeTurns(t, f.app, [][]string{
		{mockClaudeInitLine, `{"type":"assistant","message":{"id":"msg-ready","role":"assistant","content":[{"type":"text","text":"Ready."}]}}`, mockClaudeResultLine},
		append(restShellLaunch("bg-4", "task-4", "Tailing on request."), mockClaudeResultLine),
	})
	spawned := f.spawnWorker(t, "get ready")
	if row := f.awaitRequestState(t, spawned.Token, store.ThreadRequestFinished); string(row.Answer) != "Ready." {
		t.Fatalf("spawn settled %+v", row)
	}
	own, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: spawned.ThreadID, Message: "tail the deploy", Notify: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	own.ThreadID = spawned.ThreadID
	f.awaitRestHold(t, own)
	// Its row reads as its worker waits, on the shell.
	waitUntil(t, 10*time.Second, func() bool {
		row, _ := agentRowByToken(f.agentRows(t, f.caller.ID).Rows, own.Token)
		return row.Worker == workerWaiting && strings.Contains(row.Activity, "deploy.log")
	})

	installMockClaudeTurns(t, f.app, [][]string{append(restShellLaunch("bg-5", "task-5", "Tailing for you."), mockClaudeResultLine)})
	other, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: other.ID, Message: "tail the deploy", Notify: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	row := f.awaitRequestState(t, sent.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "Tailing for you." {
		t.Fatalf("send settled %+v; want its turn's message", row)
	}
	if running, err := f.app.runningBackgroundTaskCount(other.ID); err != nil || running != 1 {
		t.Fatalf("background work %d, %v; the send must settle with the shell still running", running, err)
	}
	if f.request(t, own.Token).State != store.ThreadRequestRunning {
		t.Fatal("the send to the caller's own worker settled while its shell runs")
	}
}
