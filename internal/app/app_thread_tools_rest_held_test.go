package app

// A request held for its thread's rest is a phase of the request's own
// lifecycle (app_thread_tools_rest_settle.go): a later turn of the thread
// that errors, or whose session ends mid-turn, settles it as its own turn's
// end would; only rest settles it `finished`. Rest is read from facts
// complete when they are read: at boot no held request settles until the
// sources are recovered, and a thread is not at rest while an answer is on
// its way to it.

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadtools"

	"github.com/google/uuid"
)

// echoedTurn is a turn that takes the message it answers in, echoing it
// as the CLI does, says text and ends.
func echoedTurn(id, text string) []string {
	return []string{
		mockClaudeInitLine,
		testutil.MockReplayUserMessage(),
		`{"type":"assistant","message":{"id":"msg-` + id + `","role":"assistant","content":[{"type":"text","text":` + quoteJSON(text) + `}]}}`,
		mockClaudeResultLine,
	}
}

// heldOnALoop spawns a worker whose first turn arms a loop and ends, so its
// request is held, with the turns after it from later.
func (f *requestFixture) heldOnALoop(t *testing.T, later ...[]string) threadtools.RequestAck {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "first")
	installMockClaudeTurns(t, f.app, append([][]string{gatedTurn(gate, "Watching the board.")}, later...))
	ack := f.spawnWorker(t, "watch the board")
	f.awaitTurnRunning(t, ack.Token)
	f.armLoop(t, ack.ThreadID, "board")
	release(t, gate)
	f.awaitRestHold(t, ack)
	return ack
}

// A held request's thread errors in a later turn: the request settles
// errored with the error, not `finished` at some later rest.
func TestAHeldSpawnSettlesErroredWhenALaterTurnFails(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	ack := f.heldOnALoop(t, []string{mockClaudeInitLine,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["the sandbox denied the write"]}`})
	if _, err := f.app.SendMessageWithOptions(t.Context(), ack.ThreadID, "check it again", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestErrored)
	if !strings.Contains(string(row.Answer), "the sandbox denied the write") || row.AnswerKind != store.ThreadAnswerError {
		t.Fatalf("settled %+v; want errored with the later turn's error", row)
	}
	if f.app.restHeld(ack.ThreadID, ack.Token) {
		t.Fatal("the errored request is still held")
	}
}

// A held request's thread loses its session during a later turn: the
// request settles interrupted, with what the thread wrote, not `finished`
// once the session's end left it at rest.
func TestAHeldSpawnSettlesInterruptedWhenItsSessionEndsInALaterTurn(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	second := filepath.Join(t.TempDir(), "second")
	ack := f.heldOnALoop(t, gatedTurn(second, "Halfway through the check."))
	if _, err := f.app.SendMessageWithOptions(t.Context(), ack.ThreadID, "check it again", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	waitUntil(t, 20*time.Second, func() bool {
		row, found, err := f.app.store.LastTopLevelItem(ack.ThreadID, "assistant_text")
		return err == nil && found && row.Summary == "Halfway through the check."
	})
	if err := archiveThread(f.app, ack.ThreadID); err != nil {
		t.Fatal(err)
	}
	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestInterrupted)
	if row.Cause != threadtools.CauseSessionEndedWorking || string(row.Answer) != "Halfway through the check." {
		t.Fatalf("settled %s %q %q; want interrupted while still working with what it wrote", row.State, row.Cause, row.Answer)
	}
}

// A worker whose own helper answered it is not at rest until that answer
// reaches a turn of its: read as the wake is about to be queued, it waits
// on it, and its request settles with what it said after reading it.
func TestAWorkerIsNotAtRestWhileItsHelpersAnswerIsOnItsWay(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	// The provider's echo confirms a queued answer.
	f.app.configureTriageQueueCallbacks()
	f.holdCallerSession()
	workerGate := filepath.Join(t.TempDir(), "worker")
	installMockClaudeTurns(t, f.app, [][]string{
		gatedTurn(workerGate, "Handed it to a helper."),
		echoedTurn("read", "The helper is done; all clear."),
	})
	ack := f.spawnWorker(t, "run the board")
	f.awaitTurnRunning(t, ack.Token)
	helperGate := filepath.Join(t.TempDir(), "helper")
	installMockClaudeTurns(t, f.app, [][]string{gatedTurn(helperGate, "Helper done.")})
	helper, err := f.adapter().Spawn(t.Context(), f.targetIdentity(t, ack.ThreadID), threadtools.SpawnCall{Prompt: "help", Notify: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	f.awaitTurnRunning(t, helper.Token)

	var mu sync.Mutex
	var atCrossing *workerRest
	f.app.threadWakeRenderedForTest = func(token string) {
		if token != helper.Token {
			return
		}
		rest, err := f.app.threadRest(ack.ThreadID)
		if err != nil {
			t.Errorf("threadRest: %v", err)
		}
		mu.Lock()
		atCrossing = &rest
		mu.Unlock()
	}
	release(t, workerGate)
	f.awaitRestHold(t, ack)
	release(t, helperGate)

	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "The helper is done; all clear." {
		t.Fatalf("settled %+v; want what the worker said once it read its helper's answer", row)
	}
	mu.Lock()
	defer mu.Unlock()
	if atCrossing == nil || atCrossing.unrest != workerQueued {
		t.Fatalf("the worker's rest with its helper's answer about to be queued: %+v; want it queued", atCrossing)
	}
}

// No held request settles at rest before the boot recovered what its thread
// waits on: here a helper whose request replied and whose loop keeps it
// working, which only the boot's recovery records. Once recovered the
// worker waits on it, and settles when the helper's loop is cancelled.
func TestNoHeldSpawnSettlesBeforeTheBootRecoveredItsWorkers(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.app.configureTriageQueueCallbacks()
	f.holdCallerSession()
	workerGate := filepath.Join(t.TempDir(), "worker")
	installMockClaudeTurns(t, f.app, [][]string{
		gatedTurn(workerGate, "Started a helper."),
		echoedTurn("noted", "Noted the helper's reply."),
	})
	ack := f.spawnWorker(t, "run the board")
	f.awaitTurnRunning(t, ack.Token)
	helperGate := filepath.Join(t.TempDir(), "helper")
	installMockClaudeTurns(t, f.app, [][]string{gatedTurn(helperGate, "Helper looping.")})
	helper, err := f.adapter().Spawn(t.Context(), f.targetIdentity(t, ack.ThreadID), threadtools.SpawnCall{Prompt: "help", Notify: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	f.awaitTurnRunning(t, helper.Token)
	f.armLoop(t, helper.ThreadID, "helper")
	if _, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, helper.ThreadID), threadtools.ReplyCall{Token: helper.Token, Text: "Looping for you."}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	release(t, helperGate)
	release(t, workerGate)
	f.awaitRestHold(t, ack)
	// The worker read its helper's reply in its second turn and waits on it.
	waitUntil(t, 20*time.Second, func() bool {
		row, found, err := f.app.store.LastTopLevelItem(ack.ThreadID, "assistant_text")
		due, dueErr := f.app.store.ThreadAnswerDue(ack.ThreadID)
		return err == nil && found && row.Summary == "Noted the helper's reply." && dueErr == nil && !due &&
			f.app.workerHeldUnrest(ack.ThreadID) == ""
	})
	f.app.threadRequestsWG.Wait()
	if row := f.request(t, ack.Token); row.State != store.ThreadRequestRunning {
		t.Fatalf("request %+v; want it running while its helper loops", row)
	}

	after := f.restart(t)
	after.watchAgentRows()
	after.app.sweepThreadRequestsAtBoot()
	if !after.app.restHeld(ack.ThreadID, ack.Token) {
		t.Fatal("the boot did not hold the request again")
	}
	// A pass before the recovery, as any event of the boot may ask for.
	after.app.settleRestedReceipts()
	if row := after.request(t, ack.Token); row.State != store.ThreadRequestRunning {
		t.Fatalf("request after a pass before the recovery %+v; want it running", row)
	}
	after.app.recoverRestAtBoot()
	after.app.threadRequestsWG.Wait()
	if row := after.request(t, ack.Token); row.State != store.ThreadRequestRunning {
		t.Fatalf("request after the recovery %+v; want it running while its helper loops", row)
	}
	if !after.app.liveWorker(helper.ThreadID) {
		t.Fatal("the recovery did not record the looping helper")
	}
	wakes, err := after.app.store.ListAgentWakes(helper.ThreadID)
	if err != nil || len(wakes) != 1 {
		t.Fatalf("wakes %+v, %v", wakes, err)
	}
	if err := after.app.CancelThreadWake(helper.ThreadID, store.AgentWakeLoop, wakes[0].ID); err != nil {
		t.Fatalf("CancelThreadWake: %v", err)
	}
	row := after.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "Noted the helper's reply." {
		t.Fatalf("settled %+v; want the worker's last message", row)
	}
}

// A held spawn whose worker, given a sidebar row, ended its turn asking an
// async question does not settle at the boot: the boot's passes wait for
// its recovery, and the recovery reads the stored question, which after the
// restart is the person's and has no route. Once the question is dismissed
// the worker is at rest and the spawn settles finished. An owned worker's
// such question holds nothing (TestAQuestionFromAnEarlierRunHoldsNothingForAnOwnedWorker).
func TestNoHeldSpawnSettlesBeforeTheBootRestoredItsWorkersQuestion(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.app.configureTriageQueueCallbacks()
	f.holdCallerSession()
	workerGate := filepath.Join(t.TempDir(), "worker")
	// The question is recorded while the turn waits, before the message it
	// ends with, so the message is the thread's last.
	installMockClaudeTurns(t, f.app, [][]string{{
		mockClaudeInitLine,
		testutil.MockWaitForFile(workerGate),
		`{"type":"assistant","message":{"id":"msg-asked","role":"assistant","content":[{"type":"text","text":"Asked which board."}]}}`,
		mockClaudeResultLine,
	}})
	ack := f.spawnWorker(t, "run the board")
	f.awaitTurnRunning(t, ack.Token)
	if _, err := f.app.store.SetThreadPlacement([]string{ack.ThreadID}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.store.RecordAsyncQuestions(store.Item{ID: "question:ask", ThreadID: ack.ThreadID, Kind: "assistant_text", Role: "assistant",
		Status: "completed", Meta: `{"delivery":"async","questions":[{"title":"Which board?"}]}`}); err != nil {
		t.Fatal(err)
	}
	f.app.noteWorkerWaiting(ack.ThreadID)
	release(t, workerGate)
	f.awaitRestHold(t, ack)

	after := f.restart(t)
	after.watchAgentRows()
	after.app.sweepThreadRequestsAtBoot()
	if !after.app.restHeld(ack.ThreadID, ack.Token) {
		t.Fatal("the boot did not hold the request again")
	}
	after.app.settleRestedReceipts()
	if row := after.request(t, ack.Token); row.State != store.ThreadRequestRunning {
		t.Fatalf("request after a pass before the recovery %+v; want it running", row)
	}
	// No route says the worker asks: only the stored question does.
	after.app.recoverRestAtBoot()
	after.app.threadRequestsWG.Wait()
	if row := after.request(t, ack.Token); row.State != store.ThreadRequestRunning {
		t.Fatalf("request after the recovery %+v; want it running while its worker asks", row)
	}
	if rest, err := after.app.threadRest(ack.ThreadID); err != nil || rest.unrest != workerBlocked {
		t.Fatalf("the worker's rest %+v, %v; want it blocked on its question", rest, err)
	}
	if _, err := after.app.store.DismissUnansweredAsyncQuestions(ack.ThreadID); err != nil {
		t.Fatal(err)
	}
	after.app.noteWorkerWaiting(ack.ThreadID)
	row := after.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "Asked which board." {
		t.Fatalf("settled %+v; want the worker's last message", row)
	}
}
