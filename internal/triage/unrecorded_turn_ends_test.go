package triage

import (
	"database/sql"
	"sync"
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
// writes.
type endsFixture struct {
	router    *Router
	st        *store.Store
	emissions *emissionLog
	raw       *sql.DB
	base      time.Time
}

func newEndsFixture(t *testing.T) *endsFixture {
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
	t.Cleanup(router.DrainWireItemRefresh)
	t.Cleanup(router.WaitForPendingSettles)
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

// endFrames is every provider:turn_end_unrecorded frame of t1, in order.
func (f *endsFixture) endFrames() []bool {
	var out []bool
	for _, e := range f.emissions.snapshot() {
		if e.eventName != eventchan.ProviderTurnEndUnrecorded.String() {
			continue
		}
		if event := e.data.(TurnEndUnrecordedEvent); event.ThreadID == "t1" {
			out = append(out, event.Unrecorded)
		}
	}
	return out
}

// activityOf is t1's LiveActivitySnapshot entry.
func (f *endsFixture) activityOf() (ThreadLiveActivity, bool) {
	for _, entry := range f.router.LiveActivitySnapshot() {
		if entry.ThreadID == "t1" {
			return entry, true
		}
	}
	return ThreadLiveActivity{}, false
}

func sameFrames(got, want []bool) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A turn whose end the store refused has ended: both snapshots and one
// frame say so, with the turn's end time, and nothing writes the end
// again once the store takes writes: the row stays open.
func TestATurnWhoseEndTheStoreRefusedHasEnded(t *testing.T) {
	f := newEndsFixture(t)
	f.failTurn(t, 0, 0, time.Second)
	snapshot := f.router.LiveStateSnapshotForThread("t1")
	if !snapshot.TurnEndUnrecorded || snapshot.ActiveTurn != nil {
		t.Fatalf("snapshot after the refused end: unrecorded %v, round %+v; want unrecorded and no round", snapshot.TurnEndUnrecorded, snapshot.ActiveTurn)
	}
	if entry, listed := f.activityOf(); !listed || !entry.TurnEndUnrecorded || entry.ActiveTurn != nil {
		t.Fatalf("live activity of t1 = %+v (listed %v); want it listed as unrecorded with no round", entry, listed)
	}
	endedAt, unrecorded := f.router.UnrecordedTurnEnd("t1", "t1:0")
	if !unrecorded || endedAt != f.base.Add(time.Second).UnixMilli() {
		t.Fatalf("UnrecordedTurnEnd = %d, %v; want the completion's time", endedAt, unrecorded)
	}
	if _, other := f.router.UnrecordedTurnEnd("t2", "t1:0"); other {
		t.Fatal("the fact is read for a thread it is not of")
	}
	if frames := f.endFrames(); !sameFrames(frames, []bool{true}) {
		t.Fatalf("turn_end_unrecorded frames = %v, want [true]", frames)
	}

	f.exec(t, allowTurnEndsSQL)
	f.router.WaitForPendingSettles()
	if turn := f.turn(t, "t1:0"); turn.CompletedAt != nil {
		t.Fatalf("turn row %+v; want it left open for the boot sweep", turn)
	}
	if !f.router.LiveStateSnapshotForThread("t1").TurnEndUnrecorded {
		t.Fatal("the fact went without a turn start")
	}
}

// A turn whose row is gone when its end is written leaves no open row, so
// there is nothing to hold: the thread is not read as an unrecorded end.
func TestATurnWithNoRowLeavesNoUnrecordedEnd(t *testing.T) {
	f := newEndsFixture(t)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: 0}, 0)
	f.exec(t, `DELETE FROM turns WHERE turn_id = 't1:0'`)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()}, time.Second)
	if f.router.LiveStateSnapshotForThread("t1").TurnEndUnrecorded {
		t.Fatal("a turn with no row is held as an unrecorded end")
	}
	if frames := f.endFrames(); len(frames) != 0 {
		t.Fatalf("turn_end_unrecorded frames = %v, want none", frames)
	}
}

// A turn start forgets the thread's facts, in both shapes it takes: a new
// row, which is the thread's newest now, and a start that runs on the open
// row it adopts. The old row stays open, as on a failed write before.
func TestATurnStartForgetsTheThreadsUnrecordedEnds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		index   int
		running string
	}{
		{name: "next turn", index: 1, running: "t1:1"},
		{name: "same row adopted", index: 0, running: "t1:0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEndsFixture(t)
			f.failTurn(t, 0, 0, time.Second)
			f.exec(t, allowTurnEndsSQL)
			f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: tc.index}, 2*time.Second)
			snapshot := f.router.LiveStateSnapshotForThread("t1")
			if snapshot.TurnEndUnrecorded || snapshot.ActiveTurn == nil {
				t.Fatalf("snapshot after the start: unrecorded %v, round %+v; want a running turn only", snapshot.TurnEndUnrecorded, snapshot.ActiveTurn)
			}
			if _, unrecorded := f.router.UnrecordedTurnEnd("t1", "t1:0"); unrecorded {
				t.Fatal("turn 0 is still held as ended after a turn started")
			}
			if frames := f.endFrames(); !sameFrames(frames, []bool{true, false}) {
				t.Fatalf("turn_end_unrecorded frames = %v, want [true false]", frames)
			}
			if turn := f.turn(t, "t1:0"); turn.CompletedAt != nil {
				t.Fatalf("turn 0 %+v; want it still open", turn)
			}
			f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()}, 3*time.Second)
			if turn := f.turn(t, tc.running); turn.CompletedAt == nil {
				t.Fatalf("turn %s once it ended %+v; want settled", tc.running, turn)
			}
			if entry, listed := f.activityOf(); listed {
				t.Fatalf("live activity of t1 once its turn settled = %+v; want none", entry)
			}
		})
	}
}

// A soft-closed turn the model continues runs again: the soft close's
// end the store refused is no longer the turn's end.
func TestAContinuedSoftCloseForgetsItsEnd(t *testing.T) {
	f := newEndsFixture(t)
	f.exec(t, refuseTurnEndsSQL)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart}, 0)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}}, time.Second)
	if _, unrecorded := f.router.UnrecordedTurnEnd("t1", "t1:0"); !unrecorded {
		t.Fatal("the refused soft close left no fact")
	}
	evt := parentContentBlockStart("t1", "text")
	evt.Timestamp = f.base.Add(2 * time.Second)
	if err := f.router.Handle(evt); err != nil {
		t.Fatalf("continuation: %v", err)
	}
	snapshot := f.router.LiveStateSnapshotForThread("t1")
	if snapshot.TurnEndUnrecorded || snapshot.ActiveTurn == nil {
		t.Fatalf("snapshot after the continuation: unrecorded %v, round %+v; want the turn running", snapshot.TurnEndUnrecorded, snapshot.ActiveTurn)
	}
	if frames := f.endFrames(); !sameFrames(frames, []bool{true, false}) {
		t.Fatalf("turn_end_unrecorded frames = %v, want [true false]", frames)
	}
}

// A boot sweep that fails leaves the previous instance's rows open: they
// are held as ended, for a thread with no router state at all, which the
// live activity lists for that alone.
func TestAFailedBootSweepHoldsTheCrashedTurnsAsEnded(t *testing.T) {
	f := newEndsFixture(t)
	if err := f.st.InsertTurn(store.Turn{TurnID: "t1:0", ThreadID: "t1", TurnIndex: 0, StartedAt: f.base.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	f.exec(t, refuseTurnEndsSQL)
	if _, err := f.router.RecoverCrashedTurns(); err == nil {
		t.Fatal("the sweep reported no failure")
	}
	if !f.router.LiveStateSnapshotForThread("t1").TurnEndUnrecorded {
		t.Fatal("the crashed turn the sweep left open is not held as ended")
	}
	if entry, listed := f.activityOf(); !listed || !entry.TurnEndUnrecorded {
		t.Fatalf("live activity of t1 = %+v (listed %v); want it listed as unrecorded", entry, listed)
	}
	if endedAt, unrecorded := f.router.UnrecordedTurnEnd("t1", "t1:0"); !unrecorded || endedAt != 0 {
		t.Fatalf("UnrecordedTurnEnd = %d, %v; want held with no end time", endedAt, unrecorded)
	}
	if frames := f.endFrames(); !sameFrames(frames, []bool{true}) {
		t.Fatalf("turn_end_unrecorded frames = %v, want [true]", frames)
	}
}

// A deleted thread's facts go with its rows.
func TestADeletedThreadForgetsItsUnrecordedEnds(t *testing.T) {
	f := newEndsFixture(t)
	f.failTurn(t, 0, 0, time.Second)
	f.router.ForgetUnrecordedTurnEnds("t1")
	if _, listed := f.activityOf(); listed {
		t.Fatal("the deleted thread is still listed")
	}
	if frames := f.endFrames(); !sameFrames(frames, []bool{true, false}) {
		t.Fatalf("turn_end_unrecorded frames = %v, want [true false]", frames)
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
// the thread has failed all the same: its frame and snapshots carry it,
// and the row is shown unsaved.
func TestARefusedEndWhoseErrorRowIsRefusedStillFailsTheThread(t *testing.T) {
	f := newEndsFixture(t)
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
	if frames := f.endFrames(); !sameFrames(frames, []bool{true}) {
		t.Fatalf("turn_end_unrecorded frames = %v, want [true]", frames)
	}
	if entry, listed := f.activityOf(); !listed || !entry.TurnEndUnrecorded {
		t.Fatalf("live activity of t1 = %+v (listed %v); want it listed as unrecorded", entry, listed)
	}
}

// The round ends before provider:turn_completed goes out, so whatever the
// frame starts reads it ended; until then a snapshot carries it.
func TestATurnsRoundEndsBeforeItsCompletionIsAnnounced(t *testing.T) {
	f := newEndsFixture(t)
	var roundAtEmit *ActiveTurnSnapshot
	read := false
	f.router.emit = func(name eventchan.Channel, data any) {
		if name == eventchan.ProviderTurnCompleted && !read {
			read = true
			roundAtEmit = f.router.LiveStateSnapshotForThread("t1").ActiveTurn
		}
		f.emissions.add(emitted{name.String(), data})
	}
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart}, 0)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()}, time.Second)
	if !read {
		t.Fatal("no provider:turn_completed")
	}
	if roundAtEmit != nil {
		t.Fatalf("round when provider:turn_completed went out = %+v; want it ended", roundAtEmit)
	}
}
