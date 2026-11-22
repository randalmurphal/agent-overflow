package triage

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
// writes, with the threads its turn-ended observer was told of.
type endsFixture struct {
	router    *Router
	st        *store.Store
	emissions *emissionLog
	raw       *sql.DB
	base      time.Time
	mu        sync.Mutex
	observed  []string
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
	f := &endsFixture{router: router, st: st, emissions: emissions, raw: raw, base: time.UnixMilli(1_700_000_000_000)}
	// The thread was never read: its turns run at base, before the wall
	// clock that stamps a new thread read.
	f.exec(t, `UPDATE threads SET last_read_at = NULL WHERE id = 't1'`)
	router.SetTurnEndedObserver(func(threadID string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.observed = append(f.observed, threadID)
	})
	return f
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

// ended is TurnEnded of t1, failing the test on a read error.
func (f *endsFixture) ended(t *testing.T) (store.EndedTurn, bool) {
	t.Helper()
	turn, ended, err := f.router.TurnEnded("t1")
	if err != nil {
		t.Fatalf("TurnEnded: %v", err)
	}
	return turn, ended
}

// frames is the names of t1's provider:turn_end_unrecorded and
// provider:turn_started frames, in order.
func (f *endsFixture) frames() []string {
	var out []string
	for _, e := range f.emissions.snapshot() {
		switch event := e.data.(type) {
		case TurnEndUnrecordedEvent:
			if event.ThreadID == "t1" {
				out = append(out, "ended")
			}
		case TurnStartedEvent:
			if event.ThreadID == "t1" {
				out = append(out, "started")
			}
		}
	}
	return out
}

func (f *endsFixture) observedThreads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.observed...)
}

func sameStrings(got, want []string) bool {
	return strings.Join(got, ",") == strings.Join(want, ",")
}

// A turn whose end the store refused has ended once nothing runs it: it
// reads as the next boot's sweep records it, interrupted at the turn's last
// activity and unread, the app and every client are told once, and nothing
// writes the end again before the sweep.
func TestATurnWhoseEndTheStoreRefusedEndsAsTheSweepRecordsIt(t *testing.T) {
	f := newEndsFixture(t)
	f.failTurn(t, 0, 0, time.Second)
	turn, ended := f.ended(t)
	if !ended || turn.TurnID != "t1:0" || !turn.Unread {
		t.Fatalf("TurnEnded = %+v, %v; want turn t1:0 ended and unread", turn, ended)
	}
	if frames := f.frames(); !sameStrings(frames, []string{"started", "ended"}) {
		t.Fatalf("frames = %v, want the start, then one end", frames)
	}
	if observed := f.observedThreads(); !sameStrings(observed, []string{"t1"}) {
		t.Fatalf("turn-ended observer told of %v, want t1 once", observed)
	}
	if snapshot := f.router.LiveStateSnapshotForThread("t1"); snapshot.ActiveTurn != nil {
		t.Fatalf("snapshot round %+v; want none", snapshot.ActiveTurn)
	}

	f.exec(t, allowTurnEndsSQL)
	f.router.WaitForPendingSettles()
	if open := f.turn(t, "t1:0"); open.CompletedAt != nil {
		t.Fatalf("turn row %+v; want it left open for the boot sweep", open)
	}
	if _, err := f.router.RecoverCrashedTurns(); err != nil {
		t.Fatalf("RecoverCrashedTurns: %v", err)
	}
	swept := f.turn(t, "t1:0")
	if swept.CompletedAt == nil || *swept.CompletedAt != turn.EndedAt || swept.StopReason != "interrupted" {
		t.Fatalf("swept row %+v; want interrupted at %d, the end TurnEnded read", swept, turn.EndedAt)
	}
	thread, err := f.st.GetThread("t1")
	if err != nil {
		t.Fatal(err)
	}
	if thread.HasIncompleteTurn != turn.Unread || thread.HasOpenTurn {
		t.Fatalf("swept thread: HasIncompleteTurn %v, HasOpenTurn %v; want %v, false", thread.HasIncompleteTurn, thread.HasOpenTurn, turn.Unread)
	}
}

// A thread read since its turn ended reads the ended turn as read: no
// client is told, the app still is. The turn ends at its last item write,
// the error row that says why its end was refused, so the read is stamped
// past it.
func TestAReadThreadsEndedTurnTellsTheAppAlone(t *testing.T) {
	f := newEndsFixture(t)
	f.exec(t, refuseTurnEndsSQL)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart}, 0)
	f.exec(t, `UPDATE threads SET last_read_at = `+strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)+` WHERE id = 't1'`)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()}, time.Second)
	if turn, ended := f.ended(t); !ended || turn.Unread {
		t.Fatalf("TurnEnded = %+v, %v; want ended and read", turn, ended)
	}
	if frames := f.frames(); !sameStrings(frames, []string{"started"}) {
		t.Fatalf("frames = %v, want the start alone", frames)
	}
	if observed := f.observedThreads(); !sameStrings(observed, []string{"t1"}) {
		t.Fatalf("turn-ended observer told of %v, want t1", observed)
	}
}

// A turn that settled has not ended unrecorded, however a reader read its
// row before: TurnEnded reads the row itself. A running turn has not
// either. A turn with no row leaves nothing open.
func TestOnlyAnOpenRowWithNothingLiveHasEnded(t *testing.T) {
	f := newEndsFixture(t)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart}, 0)
	if turn, ended := f.ended(t); ended {
		t.Fatalf("TurnEnded of a running turn = %+v; want not ended", turn)
	}
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()}, time.Second)
	if turn, ended := f.ended(t); ended {
		t.Fatalf("TurnEnded of a settled turn = %+v; want not ended", turn)
	}

	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: 1}, 2*time.Second)
	f.exec(t, `DELETE FROM turns WHERE turn_id = 't1:1'`)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()}, 3*time.Second)
	if turn, ended := f.ended(t); ended {
		t.Fatalf("TurnEnded with the turn's row gone = %+v; want not ended", turn)
	}
	if frames := f.frames(); !sameStrings(frames, []string{"started", "started"}) {
		t.Fatalf("frames = %v, want the two starts alone", frames)
	}
	if observed := f.observedThreads(); len(observed) != 0 {
		t.Fatalf("turn-ended observer told of %v, want nothing", observed)
	}
}

// A turn that opens while TurnEnded reads the row makes the thread live:
// the read reports no end, though the row it read is open. A turn that
// opened and ended unrecorded in that window is the one that ended.
func TestATurnOpenedWhileTheEndIsReadIsNotTakenForEnded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		inGap func(t *testing.T, f *endsFixture)
		want  string
	}{
		{name: "turn starts", inGap: func(t *testing.T, f *endsFixture) {
			f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: 1}, 2*time.Second)
		}},
		{name: "turn starts and its end is refused", inGap: func(t *testing.T, f *endsFixture) {
			f.failTurn(t, 1, 2*time.Second, 3*time.Second)
		}, want: "t1:1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEndsFixture(t)
			f.failTurn(t, 0, 0, time.Second)
			f.exec(t, allowTurnEndsSQL)
			// The gap's own turn end reads again through this gate: only
			// the outer read opens the gap.
			var fired atomic.Bool
			f.router.SetTurnEndedReadGate(func(string) {
				if fired.CompareAndSwap(false, true) {
					tc.inGap(t, f)
				}
			})
			turn, ended := f.ended(t)
			if ended != (tc.want != "") || turn.TurnID != tc.want {
				t.Fatalf("TurnEnded across a turn opening = %+v, %v; want ended %q", turn, ended, tc.want)
			}
		})
	}
}

// Every way a turn opens on a thread whose turn ended unrecorded sends
// provider:turn_started after that end's frame, which a client clears the
// end on, and the thread then runs.
func TestEveryTurnOpenAfterAnEndedTurnIsAnnouncedAfterIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(t *testing.T, f *endsFixture)
	}{
		{name: "next turn", open: func(t *testing.T, f *endsFixture) {
			f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: 1}, 2*time.Second)
		}},
		{name: "same row adopted", open: func(t *testing.T, f *endsFixture) {
			f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: 0}, 2*time.Second)
		}},
		{name: "queued echo", open: func(t *testing.T, f *endsFixture) {
			f.router.openQueuedEchoTurn("t1", 1, f.base.Add(2*time.Second).UnixMilli(), -1, "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEndsFixture(t)
			f.failTurn(t, 0, 0, time.Second)
			f.exec(t, allowTurnEndsSQL)
			tc.open(t, f)
			if frames := f.frames(); !sameStrings(frames, []string{"started", "ended", "started"}) {
				t.Fatalf("frames = %v, want the end between the two starts", frames)
			}
			if turn, ended := f.ended(t); ended {
				t.Fatalf("TurnEnded while the next turn runs = %+v; want not ended", turn)
			}
		})
	}
	t.Run("soft close continued", func(t *testing.T) {
		f := newEndsFixture(t)
		f.exec(t, refuseTurnEndsSQL)
		f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart}, 0)
		f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}}, time.Second)
		if _, ended := f.ended(t); !ended {
			t.Fatal("the refused soft close did not end the turn")
		}
		evt := parentContentBlockStart("t1", "text")
		evt.Timestamp = f.base.Add(2 * time.Second)
		if err := f.router.Handle(evt); err != nil {
			t.Fatalf("continuation: %v", err)
		}
		if frames := f.frames(); !sameStrings(frames, []string{"started", "ended", "started"}) {
			t.Fatalf("frames = %v, want the end between the two starts", frames)
		}
		if turn, ended := f.ended(t); ended {
			t.Fatalf("TurnEnded while the continued turn runs = %+v; want not ended", turn)
		}
	})
}

// An end's frame is decided and sent under the thread's endedLock, and a
// turn start sends its frame under it too, so a start never reaches a
// client ahead of an end decided before it.
func TestAnEndsFrameAndATurnStartAreOrdered(t *testing.T) {
	f := newEndsFixture(t)
	held := false
	f.router.SetTurnEndedReadGate(func(threadID string) {
		lock := &f.router.identity(threadID).endedLock
		if lock.TryLock() {
			lock.Unlock()
			return
		}
		held = true
	})
	f.failTurn(t, 0, 0, time.Second)
	if !held {
		t.Fatal("the end was read without the thread's endedLock held")
	}

	lock := &f.router.identity("t1").endedLock
	lock.Lock()
	sent := make(chan struct{})
	go func() {
		f.router.emitTurnStarted(ActiveTurnSnapshot{ThreadID: "t1", TurnID: "round"})
		close(sent)
	}()
	select {
	case <-sent:
		lock.Unlock()
		t.Fatal("provider:turn_started went out with the thread's endedLock held")
	case <-time.After(100 * time.Millisecond):
	}
	lock.Unlock()
	<-sent
}

// A boot sweep that fails leaves the previous instance's rows open: with
// nothing in this process running them, they have ended. Nothing dropped a
// live mark, so nobody is told; every reader asks.
func TestAFailedBootSweepLeavesItsTurnsEnded(t *testing.T) {
	f := newEndsFixture(t)
	if err := f.st.InsertTurn(store.Turn{TurnID: "t1:0", ThreadID: "t1", TurnIndex: 0, StartedAt: f.base.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	f.exec(t, refuseTurnEndsSQL)
	if _, err := f.router.RecoverCrashedTurns(); err == nil {
		t.Fatal("the sweep reported no failure")
	}
	if turn, ended := f.ended(t); !ended || turn.TurnID != "t1:0" || turn.EndedAt != f.base.UnixMilli() {
		t.Fatalf("TurnEnded = %+v, %v; want t1:0 ended at its start, its last activity", turn, ended)
	}
	if frames := f.frames(); len(frames) != 0 {
		t.Fatalf("frames = %v, want none", frames)
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
// the turn has ended all the same: its frame goes out, the row is shown
// unsaved, and the thread does not read Failed, as after the sweep.
func TestARefusedEndWhoseErrorRowIsRefusedStillEndsTheTurn(t *testing.T) {
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
	if frames := f.frames(); !sameStrings(frames, []string{"started", "ended"}) {
		t.Fatalf("frames = %v, want the start, then the end", frames)
	}
	thread, err := f.st.GetThread("t1")
	if err != nil {
		t.Fatal(err)
	}
	if thread.HasFailedTurn {
		t.Fatal("the thread reads Failed with no error row saved")
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

// liveMarks are the threadState fields liveLocked reads.
var liveMarks = map[string]bool{
	"turnStarting": true, "turnOpenings": true, "openTurnSet": true, "currentRoundOpen": true, "endingRoundSet": true,
}

// Every clear of a live mark, and every delete of a thread's state, runs
// as a drop of releaseLiveLocked, whose result its caller uses: a drop
// anywhere else would leave a thread whose turn ended unannounced.
func TestEveryLiveMarkDropsThroughRelease(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	drops := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		var stack []ast.Node
		inRelease := func() bool {
			for i := len(stack) - 1; i > 0; i-- {
				if _, ok := stack[i].(*ast.FuncLit); !ok {
					continue
				}
				if call, ok := stack[i-1].(*ast.CallExpr); ok && isSelectorCall(call, "releaseLiveLocked") {
					return true
				}
			}
			return false
		}
		report := func(node ast.Node, what string) {
			if !inRelease() {
				t.Errorf("%s: %s outside a releaseLiveLocked drop", fset.Position(node.Pos()), what)
			}
			drops++
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if node == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, node)
			switch n := node.(type) {
			case *ast.ExprStmt:
				if call, ok := n.X.(*ast.CallExpr); ok && isSelectorCall(call, "releaseLiveLocked") {
					t.Errorf("%s: releaseLiveLocked's result is dropped", fset.Position(n.Pos()))
				}
			case *ast.IncDecStmt:
				if field := markField(n.X); field == "turnOpenings" && n.Tok == token.DEC {
					report(n, "turnOpenings--")
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					field := markField(lhs)
					if field == "" {
						continue
					}
					if n.Tok != token.ASSIGN || len(n.Rhs) != len(n.Lhs) || clears(field, n.Rhs[i]) {
						report(n, "a clear of "+field)
					}
				}
			case *ast.CallExpr:
				if fn, ok := n.Fun.(*ast.Ident); ok && (fn.Name == "delete" || fn.Name == "clear") && len(n.Args) > 0 {
					if sel, ok := n.Args[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "threads" {
						report(n, fn.Name+" of the thread states")
					}
				}
			}
			return true
		})
	}
	if drops < 6 {
		t.Fatalf("found %d drops of live marks; the walk is not reading the router's files", drops)
	}
}

// markField names the live mark expr assigns, or "".
func markField(expr ast.Expr) string {
	if sel, ok := expr.(*ast.SelectorExpr); ok && liveMarks[sel.Sel.Name] {
		return sel.Sel.Name
	}
	return ""
}

// clears reports whether assigning value to the mark field may clear it:
// nil to the turn start's channel, anything but true to a flag, and any
// assignment to the openings count, which only ++ raises.
func clears(field string, value ast.Expr) bool {
	ident, isIdent := value.(*ast.Ident)
	switch field {
	case "turnStarting":
		return isIdent && ident.Name == "nil"
	case "turnOpenings":
		return true
	}
	return !isIdent || ident.Name != "true"
}

func isSelectorCall(call *ast.CallExpr, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name
}
