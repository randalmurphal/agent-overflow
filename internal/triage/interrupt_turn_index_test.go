package triage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

// heldTurnStart is a turn start parked between its turns-row write and
// its open turn (SetTurnRowWrittenGate).
type heldTurnStart struct {
	release func()
	// handled receives the start's Handle error, or errAbortedStart when
	// the start was aborted at the gate.
	handled chan error
}

var errAbortedStart = errors.New("turn start aborted at the gate")

// holdTurnStart runs evt (an EventTurnStart) on its own goroutine and
// returns once the start is parked after writing the turns row. With
// abort set, the released start panics out of the gate, unwinding
// before setOpenTurn.
func holdTurnStart(t *testing.T, router *Router, evt provider.ProviderEvent, abort bool) heldTurnStart {
	t.Helper()
	inGap := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	router.SetTurnRowWrittenGate(func(id string) {
		if id != evt.ThreadID {
			return
		}
		once.Do(func() {
			close(inGap)
			<-release
			if abort {
				panic(errAbortedStart)
			}
		})
	})
	t.Cleanup(func() { router.SetTurnRowWrittenGate(nil) })
	handled := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered != errAbortedStart {
					panic(recovered)
				}
				handled <- errAbortedStart
			}
		}()
		handled <- router.Handle(evt)
	}()
	select {
	case <-inGap:
	case <-time.After(10 * time.Second):
		t.Fatal("turn start never reached the row-written gate")
	}
	var releaseOnce sync.Once
	held := heldTurnStart{release: func() { releaseOnce.Do(func() { close(release) }) }, handled: handled}
	t.Cleanup(held.release)
	return held
}

func turnStartEvent(threadID string, turnIndex int) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: threadID, TurnIndex: turnIndex, Timestamp: time.Now(),
	}
}

// runThroughHeldStart runs interrupt on its own goroutine, releases the
// held start only once interrupt is waiting on it in InterruptTurnIndex
// (SetInterruptWaitHook), and returns interrupt's result and the start's
// Handle outcome. An interrupt that finishes without waiting sampled the
// start's gap and fails the test.
func runThroughHeldStart[T any](t *testing.T, router *Router, threadID string, held heldTurnStart, interrupt func() T) (T, error) {
	t.Helper()
	waiting := make(chan struct{})
	var once sync.Once
	router.SetInterruptWaitHook(func(id string) {
		if id == threadID {
			once.Do(func() { close(waiting) })
		}
	})
	t.Cleanup(func() { router.SetInterruptWaitHook(nil) })
	done := make(chan T, 1)
	go func() { done <- interrupt() }()
	select {
	case <-waiting:
	case res := <-done:
		t.Fatalf("interrupt finished without waiting on the held turn start: %+v", res)
	case <-time.After(10 * time.Second):
		t.Fatal("interrupt never waited on the held turn start")
	}
	held.release()
	var startErr error
	select {
	case startErr = <-held.handled:
	case <-time.After(10 * time.Second):
		t.Fatal("released turn start never returned")
	}
	select {
	case res := <-done:
		return res, startErr
	case <-time.After(10 * time.Second):
		t.Fatal("interrupt still waiting after the turn start returned")
	}
	panic("unreachable")
}

type interruptResult struct {
	errID string
	err   error
}

// interruptThroughHeldStart runs the production interrupt sequence
// through the held start (runThroughHeldStart).
func interruptThroughHeldStart(t *testing.T, router *Router, threadID string, held heldTurnStart) interruptResult {
	t.Helper()
	res, startErr := runThroughHeldStart(t, router, threadID, held, func() interruptResult {
		errID, err := markUserInterruptForTest(router, threadID)
		return interruptResult{errID: errID, err: err}
	})
	if startErr != nil {
		t.Fatalf("turn start: %v", startErr)
	}
	return res
}

// requireStoppedByUserRow asserts errID names a persisted "Stopped by
// user" row on turnIndex.
func requireStoppedByUserRow(t *testing.T, st *store.Store, threadID, errID string, turnIndex int) {
	t.Helper()
	if errID == "" {
		t.Fatal("MarkUserInterrupt wrote no error row")
	}
	row, ok, err := st.GetThreadItem(threadID, errID)
	if err != nil || !ok {
		t.Fatalf("missing error row %s: found=%v err=%v", errID, ok, err)
	}
	if row.Summary != "Stopped by user" || row.TurnIndex != turnIndex {
		t.Fatalf("error row = summary %q turn %d, want 'Stopped by user' on turn %d",
			row.Summary, row.TurnIndex, turnIndex)
	}
}

// requireMarkerSurvivesLaterError routes a later error in the open turn
// and asserts it lands as its own row: a marker written before the open
// turn was set would share its id with this error and be overwritten.
func requireMarkerSurvivesLaterError(t *testing.T, router *Router, st *store.Store, threadID, errID string, turnIndex int) {
	t.Helper()
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: threadID, Content: "later error", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("later error: %v", err)
	}
	requireStoppedByUserRow(t, st, threadID, errID, turnIndex)
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind == ItemKindError && item.Summary == "later error" {
			return
		}
	}
	t.Fatal("later error row missing")
}

// requireInterruptWritesNothing asserts the resolver reports no turn and
// the interrupt sequence adds no row.
func requireInterruptWritesNothing(t *testing.T, router *Router, st *store.Store, threadID string) {
	t.Helper()
	before, err := st.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if got := router.InterruptTurnIndex(threadID); got != -1 {
		t.Fatalf("InterruptTurnIndex = %d, want -1", got)
	}
	if errID, err := markUserInterruptForTest(router, threadID); err != nil || errID != "" {
		t.Fatalf("MarkUserInterrupt = %q, %v; want no row", errID, err)
	}
	after, err := st.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("items = %d after interrupt, want %d", len(after), len(before))
	}
}

// An interrupt sampled after handleTurnStart wrote the active turns row
// (what workflow takeover's GetActiveTurn gate sees) but before the open
// turn is set waits for the start and records the stop on that turn.
func TestInterruptTurnIndex_WaitsOutTurnStartGap(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	held := holdTurnStart(t, router, turnStartEvent("t1", 1), false)
	if active, ok, err := st.GetActiveTurn("t1"); err != nil || !ok || active.TurnIndex != 1 {
		t.Fatalf("precondition: GetActiveTurn = %+v, %v, %v; want active turn 1", active, ok, err)
	}
	if got := router.OpenTurnIndex("t1"); got != -1 {
		t.Fatalf("precondition: OpenTurnIndex = %d, want -1", got)
	}

	res := interruptThroughHeldStart(t, router, "t1", held)
	if res.err != nil {
		t.Fatalf("MarkUserInterrupt: %v", res.err)
	}
	requireStoppedByUserRow(t, st, "t1", res.errID, 1)
	requireMarkerSurvivesLaterError(t, router, st, "t1", res.errID, 1)
}

// A live revert (Codex thread/revert) keeps the session and its router
// state, deletes the cut turn's row, and the next turn reuses the index.
// That turn's start gap is cut like any other.
func TestInterruptTurnIndex_LiveRevertReusedIndexIsCut(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
	if _, _, err := st.DeleteConversationFromTurn("t1", 1); err != nil {
		t.Fatal(err)
	}

	held := holdTurnStart(t, router, turnStartEvent("t1", 1), false)
	res := interruptThroughHeldStart(t, router, "t1", held)
	if res.err != nil {
		t.Fatalf("MarkUserInterrupt: %v", res.err)
	}
	requireStoppedByUserRow(t, st, "t1", res.errID, 1)
}

// A queued echo can open turn N before the provider's start for N
// arrives. The turn is already open while that start is held, but the
// start's setOpenTurn will still reset the turn's error sequence, so
// the interrupt waits for it too and its marker survives the turn's
// next error.
func TestInterruptTurnIndex_WaitsOutStartOfEchoOpenedTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.openQueuedEchoTurn("t1", 1, time.Now().UnixMilli(), -1)
	if got := router.OpenTurnIndex("t1"); got != 1 {
		t.Fatalf("precondition: OpenTurnIndex = %d, want the echo-opened turn 1", got)
	}

	evt := turnStartEvent("t1", 1)
	evt.TurnID = "codex-turn-1"
	held := holdTurnStart(t, router, evt, false)
	res := interruptThroughHeldStart(t, router, "t1", held)
	if res.err != nil {
		t.Fatalf("MarkUserInterrupt: %v", res.err)
	}
	requireStoppedByUserRow(t, st, "t1", res.errID, 1)
	requireMarkerSurvivesLaterError(t, router, st, "t1", res.errID, 1)
}

// A turn start that unwinds before setting the open turn still releases
// a waiting interrupt, which then finds no turn, and leaves no start
// marker behind.
func TestInterruptTurnIndex_AbortedStartReleasesWaiter(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	held := holdTurnStart(t, router, turnStartEvent("t1", 1), true)
	got, startErr := runThroughHeldStart(t, router, "t1", held, func() int {
		return router.InterruptTurnIndex("t1")
	})
	if startErr != errAbortedStart {
		t.Fatalf("turn start = %v, want the aborted start", startErr)
	}
	if got != -1 {
		t.Fatalf("InterruptTurnIndex after the aborted start = %d, want -1", got)
	}
	router.mu.Lock()
	marker := router.threadStateIfPresent("t1").turnStarting
	router.mu.Unlock()
	if marker != nil {
		t.Fatal("aborted start left its start marker set")
	}
}

// A turn that completed leaves its router state with no open turn and
// no start in progress: the interrupt cuts nothing and completed
// history is not relabelled "Stopped by user" (CT12-1).
func TestInterruptTurnIndex_CompletedTurnIsNotCut(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
	requireInterruptWritesNothing(t, router, st, "t1")
}

// handleFatalTurnError routes a fatal turn error whose wire completion
// is still expected: the router closes the open turn and the row stays
// active until that completion.
func handleFatalTurnError(t *testing.T, router *Router, threadID string) {
	t.Helper()
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventError, ThreadID: threadID, Content: "usage is exhausted",
		Meta: json.RawMessage(`{"fatal":true,"expect_turn_complete":true,"codexErrorInfo":"usageLimitExceeded"}`),
		Failure: &provider.FailureMeta{
			Class: provider.FailureFatal, Boundary: provider.FailureBoundaryTurn,
			Reason: provider.FailureReasonUsageLimit, Code: "usageLimitExceeded",
		},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("fatal error: %v", err)
	}
}

// A fatal error closes the turn while its row stays active; an interrupt
// then (workflow cleanup) is not a user stop.
func TestInterruptTurnIndex_FatallyClosedTurnIsNotCut(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	handleFatalTurnError(t, router, "t1")
	if active, ok, err := st.GetActiveTurn("t1"); err != nil || !ok || active.TurnIndex != 1 {
		t.Fatalf("precondition: GetActiveTurn = %+v, %v, %v; want active turn 1", active, ok, err)
	}
	requireInterruptWritesNothing(t, router, st, "t1")
}

// When cleanup's settlement of a fatally closed turn fails, the row
// stays active after CleanupThread dropped the router state. A later
// session's interrupt must not read that dead row as the turn it cut.
func TestInterruptTurnIndex_DeadActiveRowAfterFailedSettleIsNotCut(t *testing.T) {
	path := storetest.ClonePath(t)
	st, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	router := NewRouter(st, func(eventchan.Channel, any) {})
	t.Cleanup(router.DrainWireItemRefresh)
	createTestThread(t, st, "t1")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	handleFatalTurnError(t, router, "t1")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := raw.Exec(`CREATE TRIGGER fail_settle BEFORE UPDATE ON turns BEGIN SELECT RAISE(ABORT, 'injected settle failure'); END`)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if execErr != nil {
		t.Fatal(execErr)
	}
	router.CleanupThread("t1")
	router.MarkThreadActive("t1")
	if active, ok, err := st.GetActiveTurn("t1"); err != nil || !ok || active.TurnIndex != 1 {
		t.Fatalf("precondition: GetActiveTurn = %+v, %v, %v; want the unsettled turn 1", active, ok, err)
	}

	requireInterruptWritesNothing(t, router, st, "t1")
	// Positive control: the store still takes the marker when given the
	// turn, so the -1 sample alone kept it out.
	tok := router.MarkFlushSendsInterrupted("t1", 1)
	errID, err := router.MarkUserInterrupt("t1", 1, tok)
	if err != nil {
		t.Fatalf("MarkUserInterrupt control: %v", err)
	}
	requireStoppedByUserRow(t, st, "t1", errID, 1)
}

// The router's open turn is the turn being cut, whatever the store's
// latest row says.
func TestInterruptTurnIndex_OpenTurnWinsOverRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := st.InsertTurn(store.Turn{TurnID: "t1:2", ThreadID: "t1", TurnIndex: 2, StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if got := router.InterruptTurnIndex("t1"); got != 1 {
		t.Fatalf("InterruptTurnIndex = %d, want open turn 1", got)
	}
	errID, err := markUserInterruptForTest(router, "t1")
	if err != nil {
		t.Fatalf("MarkUserInterrupt: %v", err)
	}
	requireStoppedByUserRow(t, st, "t1", errID, 1)
}
