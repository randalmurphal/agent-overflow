package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
)

func insertTurns(t *testing.T, app *App, threadID string, turns ...store.Turn) {
	t.Helper()
	for _, turn := range turns {
		turn.ThreadID = threadID
		if err := app.store.InsertTurn(turn); err != nil {
			t.Fatal(err)
		}
	}
}

// bindRunningReceipt binds a running request to the thread's turn turnIndex,
// as a dispatch that started the turn for it does.
func bindRunningReceipt(t *testing.T, app *App, threadID, token string, turnIndex int) {
	t.Helper()
	if _, _, err := app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: "d", Kind: "send", TargetThreadID: threadID, State: store.ThreadReceiptAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := app.store.BindThreadReceiptTurn(token, "u:"+token, threadRequestTurnKey(threadID, turnIndex), true); err != nil || !ok {
		t.Fatalf("mark %s running: %v %v", token, ok, err)
	}
	app.noteReceiptRunning(threadID, token, testSessionToken(app, threadID))
}

// keptTurnStops is how many stops the thread keeps.
func keptTurnStops(app *App, threadID string) int {
	app.threadRequests.mu.Lock()
	defer app.threadRequests.mu.Unlock()
	return len(app.threadRequests.stops[threadID])
}

// settlesAsPersonsStop reports whether turn, ended interrupted, settles its
// requests as the user's own Stop.
func settlesAsPersonsStop(t *testing.T, app *App, threadID string, turn store.Turn) bool {
	t.Helper()
	turn.ThreadID, turn.StopReason = threadID, "interrupted"
	settlement, err := app.turnSettlement(threadID, turn)
	if err != nil {
		t.Fatal(err)
	}
	return settlement.State == store.ThreadReceiptInterrupted && settlement.AnswerKind == store.ThreadAnswerFinal
}

// A second Stop before the first stopped turn settles keeps the first
// turn's: each turn's requests read the user's Stop.
func TestEveryStoppedTurnKeepsItsStop(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "stops-two-turns", "claude", t.TempDir())
	bindRunningReceipt(t, app, thread.ID, "tok-0", 0)
	bindRunningReceipt(t, app, thread.ID, "tok-1", 1)
	app.recordTurnStop(thread.ID, stoppedTurn{index: 0, startedAt: 10}, stopByUser(nil))
	app.recordTurnStop(thread.ID, stoppedTurn{index: 1, startedAt: 20}, stopByUser(nil))
	for index, startedAt := range map[int]int64{0: 10, 1: 20} {
		if !settlesAsPersonsStop(t, app, thread.ID, store.Turn{TurnIndex: index, StartedAt: startedAt}) {
			t.Errorf("turn %d, stopped by the person, does not settle as their Stop", index)
		}
	}
}

// Two threads stopped at once keep their own stops.
func TestTwoThreadsKeepTheirOwnStops(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	a := createAppTestThread(t, app, "stops-a", "claude", t.TempDir())
	b := siblingThread(t, app, a, "stops-b")
	for _, id := range []string{a.ID, b.ID} {
		bindRunningReceipt(t, app, id, "tok-"+id, 0)
		app.recordTurnStop(id, stoppedTurn{index: 0, startedAt: 10}, stopByUser(nil))
	}
	for _, id := range []string{a.ID, b.ID} {
		if !app.stoppedByPerson(id, store.Turn{ThreadID: id, TurnIndex: 0, StartedAt: 10}) {
			t.Fatalf("thread %s lost its stop", id)
		}
	}
}

// A stop goes once its turn's end has settled, and only that turn's. Turn
// 0's pass runs while turn 1 is still running; before that pass retires
// what it read, the person stops turn 1 and it ends. Turn 1's stop stays
// for turn 1's own pass, whether turn 0's pass read turn 1's request while
// turn 1 ran or the request reached turn 1 after that pass read which
// requests run.
func TestASettledTurnTakesOnlyItsOwnStop(t *testing.T) {
	t.Parallel()
	for name, boundDuringPass := range map[string]bool{"read while turn 1 ran": false, "bound during the pass": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app := newTestApp(t)
			thread := createAppTestThread(t, app, "stops-settle", "claude", t.TempDir())
			insertTurns(t, app, thread.ID, store.Turn{TurnID: "t0", TurnIndex: 0, StartedAt: 10}, store.Turn{TurnID: "t1", TurnIndex: 1, StartedAt: 20})
			bindRunningReceipt(t, app, thread.ID, "tok-0", 0)
			if !boundDuringPass {
				bindRunningReceipt(t, app, thread.ID, "tok-1", 1)
			}
			appendTurnRow(t, app.store, store.Item{ID: "a:1", ThreadID: thread.ID, TurnIndex: 1, Kind: "assistant_text", Summary: "halfway there"})
			app.recordTurnStop(thread.ID, stoppedTurn{index: 0, startedAt: 10}, stopByUser(nil))
			if err := app.store.UpdateTurnCompleted("t0", 15, "interrupted", "", "", ""); err != nil {
				t.Fatal(err)
			}
			once := false
			app.turnEndSettledForTest = func(string) {
				if once {
					return
				}
				once = true
				if boundDuringPass {
					bindRunningReceipt(t, app, thread.ID, "tok-1", 1)
				}
				app.recordTurnStop(thread.ID, stoppedTurn{index: 1, startedAt: 20}, stopByUser(nil))
				if err := app.store.UpdateTurnCompleted("t1", 25, "interrupted", "", "", ""); err != nil {
					t.Error(err)
				}
			}
			app.settleReceiptsForEndedTurn(thread.ID)

			if receipt, _, _ := app.store.GetThreadRequestReceipt("tok-0"); receipt.State != store.ThreadReceiptInterrupted || receipt.AnswerKind != store.ThreadAnswerFinal {
				t.Fatalf("turn 0's request settled %q/%q, want the person's Stop", receipt.State, receipt.AnswerKind)
			}
			if app.stoppedByPerson(thread.ID, store.Turn{TurnIndex: 0, StartedAt: 10}) {
				t.Error("turn 0's stop outlived its settled end")
			}
			if !app.stoppedByPerson(thread.ID, store.Turn{TurnIndex: 1, StartedAt: 20}) {
				t.Error("turn 0's pass took turn 1's stop")
			}

			// Turn 1's own pass settles its request as the person's Stop.
			app.settleReceiptsForEndedTurn(thread.ID)
			if receipt, _, _ := app.store.GetThreadRequestReceipt("tok-1"); receipt.State != store.ThreadReceiptInterrupted ||
				receipt.AnswerKind != store.ThreadAnswerFinal || string(receipt.Answer) != "halfway there" {
				t.Fatalf("turn 1's request settled %q/%q %q, want the person's Stop with what it wrote", receipt.State, receipt.AnswerKind, receipt.Answer)
			}
		})
	}
}

// A revert gives a new turn the stopped turn's index, and the new turn is
// stopped too, while the old turn's pass runs. The pass retires only the
// stop it read.
func TestAPassRetiresOnlyTheStopsItRead(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "stops-replaced", "claude", t.TempDir())
	insertTurns(t, app, thread.ID, store.Turn{TurnID: "t0", TurnIndex: 0, StartedAt: 10})
	bindRunningReceipt(t, app, thread.ID, "tok-0", 0)
	bindRunningReceipt(t, app, thread.ID, "tok-later", 5)
	app.recordTurnStop(thread.ID, stoppedTurn{index: 0, startedAt: 10}, stopByUser(nil))
	if err := app.store.UpdateTurnCompleted("t0", 15, "interrupted", "", "", ""); err != nil {
		t.Fatal(err)
	}
	app.turnEndSettledForTest = func(string) {
		app.recordTurnStop(thread.ID, stoppedTurn{index: 0, startedAt: 99}, stopByThread("Lead", "lead-id", ""))
	}
	app.settleReceiptsForEndedTurn(thread.ID)
	if got, want := app.turnStopCause(thread.ID, 0), stopByThread("Lead", "lead-id", "").cause; got != want {
		t.Fatalf("cause at index 0 = %q, want the stop recorded during the pass (%q)", got, want)
	}
}

// A request whose message the provider holds, with no turn named for it
// yet, may still be bound to a stopped turn that has ended. The stop stays
// until it is, and the late binding settles as the person's Stop.
func TestAStopWaitsForADeliveryStillToNameItsTurn(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	threadID := f.caller.ID
	insertTurns(t, f.app, threadID, store.Turn{TurnID: "t0", TurnIndex: 0, StartedAt: 10})
	appendTurnRow(t, f.app.store, store.Item{ID: "a:0", ThreadID: threadID, TurnIndex: 0, Kind: "assistant_text", Summary: "halfway there"})
	if _, _, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: "tok", OwnerDeviceID: "d", Kind: "send", TargetThreadID: threadID, State: store.ThreadReceiptAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	f.adapter().markRequestDispatched("tok", threadID, testSessionToken(f.app, threadID), "u:tok")
	f.app.recordTurnStop(threadID, stoppedTurn{index: 0, startedAt: 10}, stopByUser(nil))
	if err := f.app.store.UpdateTurnCompleted("t0", 15, "interrupted", "", "", ""); err != nil {
		t.Fatal(err)
	}
	f.app.settleReceiptsForEndedTurn(threadID)
	if !f.app.stoppedByPerson(threadID, store.Turn{TurnIndex: 0, StartedAt: 10}) {
		t.Fatal("the stop went while a delivery could still name its turn")
	}
	f.app.bindRequestDelivery("tok", threadID, testSessionToken(f.app, threadID), "u:tok", 0, true)
	f.app.threadRequestsWG.Wait()
	if receipt := f.receipt(t, "tok"); receipt.State != store.ThreadReceiptInterrupted ||
		receipt.AnswerKind != store.ThreadAnswerFinal || string(receipt.Answer) != "halfway there" {
		t.Fatalf("receipt = %q/%q %q, want the person's Stop with what the turn wrote", receipt.State, receipt.AnswerKind, receipt.Answer)
	}
}

// A stopped turn whose row reads complete while the router still runs it (a
// soft-closed turn the model continued) has not ended: a request bound to
// it keeps the stop for that turn's real end.
func TestAStopWaitsForARequestsTurnStillRunning(t *testing.T) {
	t.Parallel()
	q := newQueuedRequests(t)
	app := q.f.app
	turn, found, err := app.store.GetTurnByThreadIndex(q.target.ID, 0)
	if err != nil || !found {
		t.Fatalf("fixture: turn 0 = %v %v", found, err)
	}
	bindRunningReceipt(t, app, q.target.ID, "tok", 0)
	app.recordTurnStop(q.target.ID, stoppedTurn{index: 0, startedAt: turn.StartedAt}, stopByUser(nil))
	if err := app.store.UpdateTurnCompleted(turn.TurnID, turn.StartedAt+5, "interrupted", "", "", ""); err != nil {
		t.Fatal(err)
	}
	app.settleReceiptsForEndedTurn(q.target.ID)
	if !app.stoppedByPerson(q.target.ID, turn) {
		t.Fatal("the stop went while the request's turn still runs")
	}
}

// A session end whose settlement of a stopped request fails keeps the
// stop, so the settlement that succeeds later still reads the person's.
func TestAFailedSessionEndSettlementKeepsTheStop(t *testing.T) {
	t.Parallel()
	q := newQueuedRequests(t)
	f := q.f
	r := q.queue(t, 1, 1)
	r.bind(t)
	q.endTurn(t)
	q.liveTarget(t)
	q.startPickupTurn(t)
	r.consumed(t)
	q.say(t, 1, "halfway there")
	if err := f.app.InterruptTurn(q.target.ID, nil); err != nil {
		t.Fatalf("InterruptTurn: %v", err)
	}
	q.handle(t, "stopped turn end", provider.ProviderEvent{Kind: provider.EventTurnComplete,
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "interrupted"}})

	execOnStore(t, f.dbPath, `CREATE TRIGGER refuse_settle BEFORE UPDATE OF state ON thread_request_receipts
		WHEN NEW.state = 'interrupted' BEGIN SELECT RAISE(ABORT, 'injected settlement failure'); END`)
	f.app.dispatchTurnObservers(q.target.ID, provider.ProviderEvent{
		Kind: provider.EventSessionStatus, Content: "disconnected", ThreadID: q.target.ID, SessionToken: "stop-target",
	})
	f.app.threadRequestsWG.Wait()
	if receipt := f.receipt(t, r.token); receipt.State != store.ThreadReceiptRunning {
		t.Fatalf("fixture: receipt %q, want the refused settlement left it running", receipt.State)
	}
	execOnStore(t, f.dbPath, `DROP TRIGGER refuse_settle`)

	f.app.settleReceiptsForEndedTurn(q.target.ID)
	r.wantStoppedWith(t, "halfway there")
}

// A person stopping their own turns while an agent's request waits queued on
// the thread (its receipt accepted, in the running gate) keeps no stop past
// each turn's end, however many turns they stop.
func TestOwnTurnStopsBehindAQueuedRequestAreNotKept(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "stops-queued", "claude", t.TempDir())
	if _, _, err := app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: "tok-q", OwnerDeviceID: "d", Kind: "send", TargetThreadID: thread.ID, State: store.ThreadReceiptAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	app.noteReceiptRunning(thread.ID, "tok-q", testSessionToken(app, thread.ID))
	for i := range 50 {
		id := fmt.Sprintf("t%d", i)
		insertTurns(t, app, thread.ID, store.Turn{TurnID: id, TurnIndex: i, StartedAt: int64(100*i + 1)})
		app.recordTurnStop(thread.ID, stoppedTurn{index: i, startedAt: int64(100*i + 1)}, stopByUser(nil))
		if err := app.store.UpdateTurnCompleted(id, int64(100*i+50), "interrupted", "", "", ""); err != nil {
			t.Fatal(err)
		}
		app.settleReceiptsForEndedTurn(thread.ID)
	}
	if kept := keptTurnStops(app, thread.ID); kept != 0 {
		t.Fatalf("%d stops kept for turns that have ended", kept)
	}
	if _, ok := app.runningReceiptTokens(thread.ID); !ok {
		t.Fatal("the queued request left the gate")
	}
}

// A revert gives a later turn the stopped turn's index. The stop names the
// turn it cut by its start, so the later turn does not read as stopped: a
// stop recorded on an open turn carries the start, and one recorded on a
// turn still to open takes the start of the first row read at its index.
func TestALaterTurnAtAStoppedTurnsIndexIsNotTheStoppedTurn(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "stops-reused", "claude", t.TempDir())
	bindRunningReceipt(t, app, thread.ID, "tok", 0)
	app.recordTurnStop(thread.ID, stoppedTurn{index: 0, startedAt: 10}, stopByUser(nil))
	app.recordTurnStop(thread.ID, stoppedTurn{index: 1}, stopByUser(nil))
	app.recordTurnStop(thread.ID, stoppedTurn{index: 2}, stopByUser(nil))

	if settlesAsPersonsStop(t, app, thread.ID, store.Turn{TurnIndex: 0, StartedAt: 99}) {
		t.Error("a later turn at the open stopped turn's index read as stopped")
	}
	if !settlesAsPersonsStop(t, app, thread.ID, store.Turn{TurnIndex: 1, StartedAt: 20}) {
		t.Fatal("the turn the waiting send opened did not read as stopped")
	}
	if settlesAsPersonsStop(t, app, thread.ID, store.Turn{TurnIndex: 1, StartedAt: 30}) {
		t.Error("a later turn at the waiting send's index read as stopped")
	}

	// The end pass drops a stop whose index a later turn took.
	insertTurns(t, app, thread.ID, store.Turn{TurnID: "t0-later", TurnIndex: 0, StartedAt: 99})
	app.settleReceiptsForEndedTurn(thread.ID)
	if app.turnStopCause(thread.ID, 0) != turnStoppedUnknown {
		t.Error("the stop of a turn a later one replaced was kept")
	}
	if app.turnStopCause(thread.ID, 2) == turnStoppedUnknown {
		t.Error("the stop of a turn with no row yet was dropped")
	}
}

// A Stop of the open turn names it by its row's start at once, so a revert
// that replaces the turn before its end is settled does not leave the stop
// to the next turn at its index. A turn still to open has no row to read.
func TestAStopOfTheOpenTurnNamesItsStart(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "stops-open", "claude", t.TempDir())
	insertTurns(t, app, thread.ID, store.Turn{TurnID: "t0", TurnIndex: 0, StartedAt: 10})
	bindRunningReceipt(t, app, thread.ID, "tok", 0)
	if got := app.stoppedTurnOf(thread.ID, "s", 0, 0); got != (stoppedTurn{index: 0, startedAt: 10, session: "s"}) {
		t.Errorf("stop of the open turn = %+v, want it named by its start", got)
	}
	if got := app.stoppedTurnOf(thread.ID, "s", 1, -1); got != (stoppedTurn{index: 1, session: "s"}) {
		t.Errorf("stop of a waiting send's turn = %+v, want its index alone", got)
	}
}

// A Stop of a send still waiting for its turn names the session it went to.
// That session's end settles what it can and drops the stop, so the turn
// the message opens in a later session is not read as stopped.
func TestASessionEndDropsTheStopsSentToIt(t *testing.T) {
	t.Parallel()
	q := newQueuedRequests(t)
	f := q.f
	f.mockClaudeHoldingTheTurn(t, "mid-thought")
	f.holdCallerTurn(t)
	r := q.queue(t, 1, 1)
	r.bind(t)
	q.endTurn(t)
	q.liveTarget(t)
	if err := f.app.InterruptTurn(q.target.ID, nil); err != nil {
		t.Fatalf("InterruptTurn: %v", err)
	}
	if f.app.turnStopCause(q.target.ID, 1) == turnStoppedUnknown {
		t.Fatal("fixture: the Stop of the waiting send recorded nothing")
	}
	f.app.dispatchTurnObservers(q.target.ID, provider.ProviderEvent{
		Kind: provider.EventSessionStatus, Content: "disconnected", ThreadID: q.target.ID, SessionToken: "another-session",
	})
	f.app.threadRequestsWG.Wait()
	if f.app.turnStopCause(q.target.ID, 1) == turnStoppedUnknown {
		t.Fatal("the end of a session the stop was not sent to dropped it")
	}
	f.app.dispatchTurnObservers(q.target.ID, provider.ProviderEvent{
		Kind: provider.EventSessionStatus, Content: "disconnected", ThreadID: q.target.ID, SessionToken: "stop-target",
	})
	f.app.threadRequestsWG.Wait()
	if f.app.holdsTurnStop(q.target.ID) {
		t.Fatal("the stop outlived the session it was sent to")
	}
}

// A Stop whose interrupt fails records nothing: the turn runs on.
func TestAFailedInterruptKeepsNoStop(t *testing.T) {
	t.Parallel()
	q := newQueuedRequests(t)
	r := q.queue(t, 1, 1)
	r.bind(t)
	q.endTurn(t)
	q.liveTarget(t)
	q.startPickupTurn(t)
	sess, _ := q.f.app.sessionManager().get(q.target.ID)
	_ = sess.Claude.Close()
	if err := q.f.app.InterruptTurn(q.target.ID, nil); err == nil {
		t.Fatal("fixture: the interrupt of a closed session did not fail")
	}
	if q.f.app.holdsTurnStop(q.target.ID) {
		t.Fatal("a Stop whose interrupt failed was kept")
	}
	q.endStoppedTurn(t)
	if row := q.f.request(t, r.token); row.AnswerKind == store.ThreadAnswerFinal && row.State == store.ThreadRequestInterrupted {
		t.Fatalf("request = %s/%s, read as the person's Stop after its interrupt failed", row.State, row.AnswerKind)
	}
}

// A request's turn a person's Stop cut settles `interrupted` with what the
// turn wrote by then as its final answer, however the request's message
// reached that turn: echoed before the Stop, taken in with no echo and bound
// only at the turn's end, or still waiting for its turn to open.

// liveTarget gives the queued requests' target a session whose CLI acks
// every interrupt, so a Stop goes through the app's own interrupt.
func (q *queuedRequests) liveTarget(t *testing.T) {
	t.Helper()
	sess, err := claude.NewSession(context.Background(), q.target.ID,
		claude.Config{Reaper: procutiltest.Reaper(), Retention: claude.UserHomeRetention(0), Binary: writeClaudeInterruptRecorderBinary(t, filepath.Join(t.TempDir(), "interrupts.log")), WorkDir: t.TempDir()},
		func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	q.f.app.sessionManager().put(q.target.ID, session{Provider: string(provider.Claude), Token: q.session, Claude: sess})
}

// endStoppedTurn is the CLI ending the turn the Stop cut, and the turn
// observer settling what it answered.
func (q *queuedRequests) endStoppedTurn(t *testing.T) {
	t.Helper()
	q.handle(t, "stopped turn end", provider.ProviderEvent{Kind: provider.EventTurnComplete,
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "interrupted"}})
	q.f.app.settleReceiptsForEndedTurn(q.target.ID)
}

func (r *queuedRequest) wantStoppedWith(t *testing.T, text string) {
	t.Helper()
	row := r.q.f.request(t, r.token)
	if row.State != store.ThreadRequestInterrupted || row.AnswerKind != store.ThreadAnswerFinal || !strings.Contains(string(row.Answer), text) {
		t.Fatalf("request = %s/%s %q, want interrupted with %q as its final answer", row.State, row.AnswerKind, row.Answer, text)
	}
}

func TestAPersonsStopKeepsWhatTheRequestsTurnWrote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// reach runs the request's message into turn 1 before the Stop.
		reach func(t *testing.T, q *queuedRequests, r *queuedRequest)
		// open runs after the Stop, before the turn's end.
		open func(t *testing.T, q *queuedRequests, r *queuedRequest)
	}{
		{"echoed before the Stop", func(t *testing.T, q *queuedRequests, r *queuedRequest) {
			q.startPickupTurn(t)
			r.consumed(t)
			q.say(t, 1, "halfway there")
		}, nil},
		{"taken in without an echo", func(t *testing.T, q *queuedRequests, r *queuedRequest) {
			q.startPickupTurn(t)
			q.say(t, 1, "halfway there")
			if receipt := q.f.receipt(t, r.token); receipt.TurnID != "" {
				t.Fatalf("fixture: receipt bound to %q before the turn's end", receipt.TurnID)
			}
		}, nil},
		{"waiting for its turn to open", nil, func(t *testing.T, q *queuedRequests, r *queuedRequest) {
			q.startPickupTurn(t)
			q.say(t, 1, "halfway there")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := newQueuedRequests(t)
			f := q.f
			f.mockClaudeHoldingTheTurn(t, "mid-thought")
			f.holdCallerTurn(t)
			q.notify = true
			r := q.queue(t, 1, 1)
			r.bind(t)
			q.endTurn(t)
			q.liveTarget(t)
			if tc.reach != nil {
				tc.reach(t, q, r)
			}
			if err := f.app.InterruptTurn(q.target.ID, nil); err != nil {
				t.Fatalf("InterruptTurn: %v", err)
			}
			if tc.open != nil {
				tc.open(t, q, r)
			}
			q.endStoppedTurn(t)
			r.wantStoppedWith(t, "halfway there")
		})
	}
}
