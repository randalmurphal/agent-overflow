package triage

import (
	"database/sql"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

// refuseTurnEndsSQL makes the store refuse every write of a turn's end, as
// a database that takes no writes does for that one row.
const refuseTurnEndsSQL = `CREATE TRIGGER refuse_turn_settle BEFORE UPDATE OF completed_at ON turns
	WHEN NEW.completed_at IS NOT NULL BEGIN SELECT RAISE(ABORT, 'injected settle failure'); END`

const allowTurnEndsSQL = `DROP TRIGGER refuse_turn_settle`

// endsFixture is a router over a store a second connection can make refuse
// writes, with the retry timer at retry.
type endsFixture struct {
	router    *Router
	st        *store.Store
	emissions *emissionLog
	raw       *sql.DB
	base      time.Time
}

func newEndsFixture(t *testing.T, retry time.Duration) *endsFixture {
	t.Helper()
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
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close the trigger connection: %v", err)
		}
	})
	emissions := &emissionLog{}
	router := NewRouter(st, func(name eventchan.Channel, data any) { emissions.add(emitted{name.String(), data}) })
	holdWireRefresh(router)
	router.delays.endRetry = retry
	t.Cleanup(router.DrainWireItemRefresh)
	t.Cleanup(router.WaitForPendingSettles)
	// Runs first: no pass may write after the test.
	t.Cleanup(router.StopTurnEndRetries)
	createTestThread(t, st, "t1")
	return &endsFixture{router: router, st: st, emissions: emissions, raw: raw, base: time.UnixMilli(1_700_000_000_000)}
}

func (f *endsFixture) exec(t *testing.T, statement string) {
	t.Helper()
	if _, err := f.raw.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

func (f *endsFixture) handle(t *testing.T, evt provider.ProviderEvent, at time.Duration) {
	t.Helper()
	evt.ThreadID = "t1"
	evt.Timestamp = f.base.Add(at)
	if err := f.router.Handle(evt); err != nil {
		t.Fatalf("%s: %v", evt.Kind, err)
	}
}

func (f *endsFixture) turn(t *testing.T, turnID string) store.Turn {
	t.Helper()
	turn, found, err := f.st.GetTurn(turnID)
	if err != nil || !found {
		t.Fatalf("turn %s: found %v, %v", turnID, found, err)
	}
	return turn
}

// failTurn runs turn index to a normal end the store refuses.
func (f *endsFixture) failTurn(t *testing.T, index int, start, end time.Duration) {
	t.Helper()
	f.exec(t, refuseTurnEndsSQL)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: index}, start)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()}, end)
}

// A turn whose end the store refused has ended: the snapshot says so, and
// the retry timer writes the end, with the turn's own outcome and time,
// once the store takes writes again, with no restart. Nothing is armed
// once nothing is left to write.
func TestAnUnrecordedTurnEndIsWrittenOnceTheStoreTakesIt(t *testing.T) {
	f := newEndsFixture(t, time.Millisecond)
	passes := make(chan struct{}, 1)
	f.router.ends.mu.Lock()
	f.router.ends.passed = func() {
		select {
		case passes <- struct{}{}:
		default:
		}
	}
	f.router.ends.mu.Unlock()

	f.failTurn(t, 0, 0, time.Second)
	snapshot := f.router.LiveStateSnapshotForThread("t1")
	if !snapshot.TurnEndUnrecorded || snapshot.ActiveTurn != nil {
		t.Fatalf("snapshot after the refused end: unrecorded %v, round %+v; want unrecorded and no round", snapshot.TurnEndUnrecorded, snapshot.ActiveTurn)
	}
	endedAt, unrecorded := f.router.UnrecordedTurnEnd("t1:0")
	if !unrecorded || endedAt != f.base.Add(time.Second).UnixMilli() {
		t.Fatalf("UnrecordedTurnEnd = %d, %v; want the completion's time", endedAt, unrecorded)
	}
	if turn := f.turn(t, "t1:0"); turn.CompletedAt != nil {
		t.Fatalf("turn row %+v; want it still open while the store refuses", turn)
	}
	if !f.router.ends.armed() {
		t.Fatal("no retry armed for the refused end")
	}
	// A pass that fails keeps the end and the timer.
	<-passes
	if _, unrecorded := f.router.UnrecordedTurnEnd("t1:0"); !unrecorded {
		t.Fatal("a failed pass dropped the end")
	}

	f.exec(t, allowTurnEndsSQL)
	deadline := time.After(10 * time.Second)
	for f.router.LiveStateSnapshotForThread("t1").TurnEndUnrecorded {
		select {
		case <-passes:
		case <-deadline:
			t.Fatal("the end was never written once the store took writes")
		}
	}
	turn := f.turn(t, "t1:0")
	if turn.CompletedAt == nil || *turn.CompletedAt != endedAt || turn.StopReason != "end_turn" {
		t.Fatalf("turn row %+v; want settled end_turn at %d", turn, endedAt)
	}
	f.router.ends.mu.Lock()
	armed, delay := f.router.ends.timer != nil, f.router.ends.delay
	f.router.ends.mu.Unlock()
	if armed || delay != 0 {
		t.Fatalf("after the end was written: armed %v, delay %v; want nothing armed", armed, delay)
	}
}

// A turn whose end the store takes owes nothing and arms nothing.
func TestTurnEndRetriesAreNotArmedWhenNothingIsOwed(t *testing.T) {
	f := newEndsFixture(t, time.Millisecond)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: 0}, 0)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()}, time.Second)
	if f.router.ends.armed() || f.router.LiveStateSnapshotForThread("t1").TurnEndUnrecorded {
		t.Fatal("a recorded end left a retry armed or an end unrecorded")
	}
	f.router.RetryUnrecordedTurnEnds("t1")
	if f.router.ends.armed() {
		t.Fatal("a retry with nothing owed armed the timer")
	}
}

// StopTurnEndRetries waits out a pass that runs and arms no more, so the
// store can close behind it.
func TestStopTurnEndRetriesWaitsOutAPassAndArmsNoMore(t *testing.T) {
	f := newEndsFixture(t, time.Millisecond)
	// The first pass runs on until it sees the stop, and only then ends:
	// a Stop that returns before that returned while it ran.
	entered := make(chan struct{})
	var once sync.Once
	var passEnded atomic.Bool
	f.router.ends.mu.Lock()
	f.router.ends.passed = func() {
		once.Do(func() {
			close(entered)
			for {
				f.router.ends.mu.Lock()
				stopped := f.router.ends.stopped
				f.router.ends.mu.Unlock()
				if stopped {
					break
				}
				runtime.Gosched()
			}
			passEnded.Store(true)
		})
	}
	f.router.ends.mu.Unlock()
	f.failTurn(t, 0, 0, time.Second)
	<-entered

	f.router.StopTurnEndRetries()
	if !passEnded.Load() {
		t.Fatal("StopTurnEndRetries returned while a pass ran")
	}
	if f.router.ends.armed() {
		t.Fatal("a pass is armed after StopTurnEndRetries")
	}
	// An end refused after the stop is kept, and nothing is armed for it.
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: 1}, 2*time.Second)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()}, 3*time.Second)
	if _, unrecorded := f.router.UnrecordedTurnEnd("t1:1"); !unrecorded {
		t.Fatal("the end refused after the stop was not kept")
	}
	if f.router.ends.armed() {
		t.Fatal("an end refused after the stop armed a pass")
	}
}

// A soft-closed turn the model continues runs again: a write of the soft
// close's end in flight lands before the reopen, never after it, and no
// later retry settles the continuation.
func TestARetryNeverSettlesAReopenedTurn(t *testing.T) {
	f := newEndsFixture(t, time.Hour)
	f.exec(t, refuseTurnEndsSQL)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart}, 0)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}}, time.Second)
	if _, unrecorded := f.router.UnrecordedTurnEnd("t1:0"); !unrecorded {
		t.Fatal("the refused soft close left no unrecorded end")
	}
	f.exec(t, allowTurnEndsSQL)

	writing, release := make(chan struct{}), make(chan struct{})
	f.router.SetTurnSettleGate(func(string) {
		close(writing)
		<-release
	})
	retried := make(chan struct{})
	go func() {
		f.router.RetryUnrecordedTurnEnds("t1")
		close(retried)
	}()
	<-writing
	f.router.SetTurnSettleGate(nil)

	waiting := make(chan struct{})
	f.router.ends.mu.Lock()
	f.router.ends.dropWaiting = func() { close(waiting) }
	f.router.ends.mu.Unlock()
	continued := make(chan error, 1)
	go func() {
		evt := parentContentBlockStart("t1", "text")
		evt.Timestamp = f.base.Add(2 * time.Second)
		continued <- f.router.Handle(evt)
	}()
	select {
	case <-waiting:
	case err := <-continued:
		close(release)
		t.Fatalf("the reopen went ahead of the write in flight (%v)", err)
	}
	close(release)
	<-retried
	if err := <-continued; err != nil {
		t.Fatalf("continuation: %v", err)
	}
	if turn := f.turn(t, "t1:0"); turn.CompletedAt != nil {
		t.Fatalf("turn row %+v; want the continuation's turn open", turn)
	}
	if _, unrecorded := f.router.UnrecordedTurnEnd("t1:0"); unrecorded {
		t.Fatal("the reopened turn still owes its soft close's end")
	}
	f.router.RetryUnrecordedTurnEnds("t1")
	if turn := f.turn(t, "t1:0"); turn.CompletedAt != nil {
		t.Fatalf("turn row after a later retry %+v; want still open", turn)
	}
}

// A message queued while the row was open is sent once its end is written,
// once, however the retry's flush and a boundary's race.
func TestARecordedEndSendsTheQueueOnce(t *testing.T) {
	f := newEndsFixture(t, time.Hour)
	rec := &recordingDispatcher{}
	f.router.SetFlushDispatcher(rec.dispatch)
	f.failTurn(t, 0, 0, time.Second)
	f.router.RegisterQueueItem("t1", makeQueueItem("queue:0", "after the open row"))
	f.exec(t, allowTurnEndsSQL)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		f.router.RetryUnrecordedTurnEnds("t1")
	}()
	go func() {
		defer wg.Done()
		f.router.maybeFlushQueueAtBoundary("t1", "")
	}()
	wg.Wait()
	f.router.WaitForPendingSettles()
	sent := 0
	for _, call := range rec.snapshot() {
		sent += len(call.Items)
	}
	if sent != 1 {
		t.Fatalf("queued message sent %d times, want once: %+v", sent, rec.snapshot())
	}
	if f.turn(t, "t1:0").CompletedAt == nil {
		t.Fatal("the end was not written")
	}
}

// The end written before a new turn's row does not send the queue under
// that turn: the turn's own end does.
func TestAnEndWrittenAsATurnStartsLeavesTheQueueToThatTurn(t *testing.T) {
	f := newEndsFixture(t, time.Hour)
	rec := &recordingDispatcher{}
	f.router.SetFlushDispatcher(rec.dispatch)
	f.failTurn(t, 0, 0, time.Second)
	f.router.RegisterQueueItem("t1", makeQueueItem("queue:0", "after the open row"))
	f.exec(t, allowTurnEndsSQL)

	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: 1}, 2*time.Second)
	f.router.WaitForPendingSettles()
	if turn := f.turn(t, "t1:0"); turn.CompletedAt == nil || turn.StopReason != "end_turn" {
		t.Fatalf("turn 0 after turn 1 started: %+v; want settled end_turn", turn)
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("the queue was sent as turn 1 opened: %+v", calls)
	}
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()}, 3*time.Second)
	calls := rec.waitForAtLeastCalls(t, 1)
	if len(calls) != 1 || len(calls[0].Items) != 1 || calls[0].Items[0].ID != "queue:0" {
		t.Fatalf("dispatcher calls = %+v, want queue:0 once at turn 1's end", calls)
	}
}

// A late completion of a turn whose end the store refused folds onto that
// end, written first, so the turn keeps how it ended.
func TestALateCompletionFoldsOntoTheUnrecordedEnd(t *testing.T) {
	f := newEndsFixture(t, time.Hour)
	f.failTurn(t, 0, 0, time.Second)
	f.exec(t, allowTurnEndsSQL)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{Aborted: true}}, 2*time.Second)
	turn := f.turn(t, "t1:0")
	if turn.CompletedAt == nil || turn.StopReason != "end_turn" {
		t.Fatalf("turn row after the late completion %+v; want the first end, end_turn", turn)
	}
	if _, unrecorded := f.router.UnrecordedTurnEnd("t1:0"); unrecorded {
		t.Fatal("the end is still owed after the late completion")
	}
}

// A boot sweep that fails leaves the previous instance's rows open: they
// are owed as crashed turns, read as ended, and settled by the sweep's own
// rule once the store takes writes.
func TestAFailedBootSweepOwesTheCrashedTurns(t *testing.T) {
	f := newEndsFixture(t, time.Hour)
	if err := f.st.InsertTurn(store.Turn{TurnID: "t1:0", ThreadID: "t1", TurnIndex: 0, StartedAt: f.base.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	f.exec(t, refuseTurnEndsSQL)
	if _, err := f.router.RecoverCrashedTurns(); err == nil {
		t.Fatal("the sweep reported no failure")
	}
	if !f.router.LiveStateSnapshotForThread("t1").TurnEndUnrecorded {
		t.Fatal("the crashed turn the sweep left open is not owed")
	}
	if endedAt, unrecorded := f.router.UnrecordedTurnEnd("t1:0"); !unrecorded || endedAt != 0 {
		t.Fatalf("UnrecordedTurnEnd = %d, %v; want owed with no end time", endedAt, unrecorded)
	}
	f.exec(t, allowTurnEndsSQL)
	f.router.RetryUnrecordedTurnEnds("t1")
	turn := f.turn(t, "t1:0")
	if turn.CompletedAt == nil || turn.StopReason != "interrupted" {
		t.Fatalf("turn row %+v; want settled interrupted by the sweep's rule", turn)
	}
	if f.router.LiveStateSnapshotForThread("t1").TurnEndUnrecorded {
		t.Fatal("the crashed turn is still owed once settled")
	}
}

// unsavedRowsSeen records the rows the store refused.
type unsavedRowsSeen struct {
	mu      sync.Mutex
	refused []UnsavedRow
}

func (u *unsavedRowsSeen) RowRefused(row UnsavedRow, _ error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.refused = append(u.refused, row)
}

func (u *unsavedRowsSeen) RowSaved(UnsavedRow) {}

// When the store refuses the turn's end and the error row that says so,
// the thread still fails on every client: the row is shown unsaved, and
// the error notice that marks every sidebar goes out.
func TestARefusedEndWhoseErrorRowIsRefusedStillReachesEveryClient(t *testing.T) {
	f := newEndsFixture(t, time.Hour)
	unsaved := &unsavedRowsSeen{}
	f.router.SetUnsavedRowObserver(unsaved)
	f.exec(t, `CREATE TRIGGER refuse_error_rows BEFORE INSERT ON items
		WHEN NEW.kind = 'error' BEGIN SELECT RAISE(ABORT, 'injected error row failure'); END`)
	f.failTurn(t, 0, 0, time.Second)

	unsaved.mu.Lock()
	refused := append([]UnsavedRow(nil), unsaved.refused...)
	unsaved.mu.Unlock()
	if len(refused) != 1 || refused[0].ThreadID != "t1" || refused[0].Severity != UnsavedSeverityError {
		t.Fatalf("unsaved rows = %+v, want the error row of t1", refused)
	}
	notices := 0
	for _, e := range f.emissions.snapshot() {
		if e.eventName == eventchan.ThreadErrorNotice.String() && e.data.(ThreadErrorNoticeEvent).ThreadID == "t1" {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("error notices = %d, want 1 for the refused error row", notices)
	}
}
