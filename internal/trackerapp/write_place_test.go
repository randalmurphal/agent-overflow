package trackerapp

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// createdResult is a create's answer naming issue.
func createdResult(issue string) tracker.Result {
	id := tracker.IssueID(issue)
	return tracker.Result{Parts: []tracker.Part{appliedPart(issue, tracker.StepCreate, tracker.CardOf(id), tracker.DetailOf(id))}}
}

// placements records the boards of each NewRows call and answers through
// answer.
func placements(f *writeFixture, answer func(tracker.IssueID, []tracker.BoardID) (tracker.Placement, error)) func() [][]tracker.BoardID {
	var mu sync.Mutex
	var asked [][]tracker.BoardID
	f.p.set(func(p *scripted) {
		p.newRows = func(issue tracker.IssueID, boards []tracker.BoardID) (tracker.Placement, error) {
			mu.Lock()
			asked = append(asked, slices.Clone(boards))
			mu.Unlock()
			return answer(issue, boards)
		}
	})
	return func() [][]tracker.BoardID {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// A create places the new issue on its board and on every other
// subscribed board of the connection in one read, stores its row on each
// that holds it with no rows read, tells each board's subscribers, and
// answers with nothing stale.
func TestCreatePlacesTheNewIssueOnEveryBoardHoldingIt(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.subscribe(t, "2")
	idleBoards(t, f.svc, f.p)
	f.state.set(testBoard, writeRow("10004", 1, "d"))
	f.state.set("2", writeRow("10004", 1, "n"))
	asked := placements(f, f.state.placement)
	reads := f.state.recordRows(f.p)
	f.answer(createdResult("10004"))
	before := len(f.p.callsInOrder())
	events, _, _ := f.events.snapshot()
	v, err := f.svc.Create(t.Context(), f.conn.ID, CreateRequest{Board: "1", Project: "10000", Title: "New task"})
	if err != nil || v.Created != "10004" || len(v.Stale) > 0 || v.NotOnBoard != "" {
		t.Fatalf("view %+v, %v", v, err)
	}
	if got := f.callsSince(before); !slices.Equal(got, []string{"Create", "NewRows"}) || len(reads()) != 0 {
		t.Fatalf("calls %v", got)
	}
	if got := asked(); len(got) != 1 || !slices.Equal(got[0], []tracker.BoardID{testBoard, "2"}) {
		t.Fatalf("placed on %v", got)
	}
	if f.orderOf(t, testBoard, "10004") != "d" || f.orderOf(t, "2", "10004") != "n" {
		t.Fatal("the new issue's rows were not stored")
	}
	f.events.waitFor(t, "board 2 told of the new issue", func(b []BoardEvent, _ []IssueEvent, _ []error) bool {
		return slices.ContainsFunc(b[len(events):], func(e BoardEvent) bool {
			return e.Scope.Board == "2" && slices.Contains(e.Changed, "10004")
		})
	})
}

// A create the tracker places on no board of its own stores nothing,
// reads no rows, and answers which board does not hold it.
func TestCreateOffItsBoardAnswersNotOnBoard(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	placements(f, f.state.placement)
	reads := f.state.recordRows(f.p)
	f.answer(createdResult("10004"))
	v, err := f.svc.Create(t.Context(), f.conn.ID, CreateRequest{Board: "1", Project: "10000", Title: "An epic"})
	if err != nil || v.Created != "10004" || len(v.Stale) > 0 || v.NotOnBoard != "1" {
		t.Fatalf("view %+v, %v", v, err)
	}
	if len(reads()) != 0 || f.orderOf(t, testBoard, "10004") != "" {
		t.Fatal("a create off its board read or stored a row")
	}
}

// With no answer for the create's board, nothing is guessed: the row is
// owed the board's rows read, as any write's row is, whether the
// placement failed or did not judge that board. A board read that misses
// the new issue stores nothing and leaves nothing stale; one that finds
// it stores it.
func TestCreateWithNoPlacementReadsItsRow(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]func(tracker.IssueID, []tracker.BoardID) (tracker.Placement, error){
		"failed": func(tracker.IssueID, []tracker.BoardID) (tracker.Placement, error) {
			return tracker.Placement{}, errors.New("test: the tracker limited the request")
		},
		"not judged": func(_ tracker.IssueID, boards []tracker.BoardID) (tracker.Placement, error) {
			return tracker.Placement{Unknown: []tracker.UnjudgedBoard{{Board: boards[0], Err: errors.New("test: the filter cannot be read")}}}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newWriteFixture(t)
			placements(f, answer)
			reads := f.state.recordRows(f.p)
			f.answer(createdResult("10004"))
			v, err := f.svc.Create(t.Context(), f.conn.ID, CreateRequest{Board: "1", Project: "10000", Title: "New task"})
			if err != nil || len(v.Stale) > 0 || v.NotOnBoard != "" {
				t.Fatalf("view %+v, %v", v, err)
			}
			if got := reads(); !slices.Equal(got, []string{"1:[10004]"}) || f.orderOf(t, testBoard, "10004") != "" {
				t.Fatalf("reads %v; a row the board read missed was stored", got)
			}
			f.state.set(testBoard, writeRow("10005", 1, "e"))
			f.answer(createdResult("10005"))
			if v, err := f.svc.Create(t.Context(), f.conn.ID, CreateRequest{Board: "1", Project: "10000", Title: "New task"}); err != nil || len(v.Stale) > 0 {
				t.Fatalf("view %+v, %v", v, err)
			}
			if got := reads(); !slices.Equal(got, []string{"1:[10005]"}) || f.orderOf(t, testBoard, "10005") != "e" {
				t.Fatalf("reads %v; the row the board read found was not stored", got)
			}
		})
	}
}

// A placed row the store does not take, because a listing that began
// after the placement's read and missed the new issue (the index not
// caught up) recorded a removal first, is not refreshed: it is owed the
// board's rows read, which stores it.
func TestCreateReadsItsRowWhenTheStoreRefusedThePlacement(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	var clock atomic.Int64
	clock.Store(1_800_000_000_000)
	f.svc.now = func() time.Time { return time.UnixMilli(clock.Add(1)) }
	f.state.set(testBoard, writeRow("10004", 1, "d"))
	arrived, release := make(chan struct{}), make(chan struct{})
	placements(f, func(issue tracker.IssueID, boards []tracker.BoardID) (tracker.Placement, error) {
		close(arrived)
		<-release
		return f.state.placement(issue, boards)
	})
	f.p.set(func(p *scripted) {
		p.boardIssues = func(b tracker.BoardID, w time.Time) (tracker.Listing, error) {
			l, err := f.state.listing(b, w)
			l.IDs = slices.DeleteFunc(l.IDs, func(id tracker.IssueID) bool { return id == "10004" })
			return l, err
		}
	})
	reads := f.state.recordRows(f.p)
	f.answer(createdResult("10004"))
	type answer struct {
		v   WriteView
		err error
	}
	done := make(chan answer, 1)
	go func() {
		v, err := f.svc.Create(t.Context(), f.conn.ID, CreateRequest{Board: "1", Project: "10000", Title: "New task"})
		done <- answer{v, err}
	}()
	<-arrived
	must[Outcome](t)(f.svc.tickReconcile(t.Context(), f.scope(), Background))
	close(release)
	a := <-done
	if a.err != nil || len(a.v.Stale) > 0 {
		t.Fatalf("view %+v, %v", a.v, a.err)
	}
	if got := reads(); !slices.Equal(got, []string{"1:[10004]"}) || f.orderOf(t, testBoard, "10004") != "d" {
		t.Fatalf("reads %v; order %q", got, f.orderOf(t, testBoard, "10004"))
	}
}

// hookedStore runs a test's hooks in a write's refresh: before its
// targets read, and before each rows write, which fails when the hook
// does.
type hookedStore struct {
	*store.Store
	targets func()
	rows    func(page store.MirrorRowPage) error
}

func (s *hookedStore) ReadMirrorRefreshTargets(root int64, cards, details, boards []string) (store.MirrorRefreshTargets, error) {
	if s.targets != nil {
		s.targets()
	}
	return s.Store.ReadMirrorRefreshTargets(root, cards, details, boards)
}

func (s *hookedStore) WriteMirrorRows(root int64, page store.MirrorRowPage) (store.MirrorChanges, error) {
	if s.rows != nil {
		if err := s.rows(page); err != nil {
			return store.MirrorChanges{}, err
		}
	}
	return s.Store.WriteMirrorRows(root, page)
}

// A create from a board the mirror does not show, never read or retired
// before the refresh read its targets, still places the new issue on
// every shown board that holds it, in one read that does not ask about
// the create's board; its row there is not shown. With no shown board to
// ask, nothing is asked.
func TestCreatePlacesOnShownBoardsWhenItsOwnIsNot(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		board     tracker.BoardID
		subscribe bool
		unlist    bool
		asked     [][]tracker.BoardID
	}{
		"its board never read":                    {board: "7", subscribe: true, asked: [][]tracker.BoardID{{"2"}}},
		"its board retired before the targets":    {board: testBoard, subscribe: true, unlist: true, asked: [][]tracker.BoardID{{"2"}}},
		"no shown board":                          {board: "7"},
		"its board retired and no other to place": {board: testBoard, unlist: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newWriteFixture(t)
			if tc.subscribe {
				f.subscribe(t, "2")
				idleBoards(t, f.svc, f.p)
			}
			f.state.set("2", writeRow("10004", 1, "n"))
			f.state.set(tc.board, writeRow("10004", 1, "d"))
			hs := &hookedStore{Store: f.st}
			if tc.unlist {
				hs.targets = func() { listBoards(t, f.st, f.conn.ID, "2", "7", "9") }
			}
			f.svc.store = hs
			asked := placements(f, f.state.placement)
			reads := f.state.recordRows(f.p)
			f.answer(createdResult("10004"))
			v, err := f.svc.Create(t.Context(), f.conn.ID, CreateRequest{Board: string(tc.board), Project: "10000", Title: "New task"})
			if err != nil || v.Created != "10004" || len(v.Stale) > 0 || v.NotOnBoard != "" {
				t.Fatalf("view %+v, %v", v, err)
			}
			if got := asked(); fmt.Sprint(got) != fmt.Sprint(tc.asked) || f.p.count("NewRows") != len(tc.asked) {
				t.Fatalf("placed on %v in %d reads, want %v", got, f.p.count("NewRows"), tc.asked)
			}
			if got := reads(); len(got) != 0 {
				t.Fatalf("rows read %v", got)
			}
			want := ""
			if tc.subscribe {
				want = "n"
			}
			if got := f.orderOf(t, "2", "10004"); got != want {
				t.Fatalf("the row on board 2 is %q, want %q", got, want)
			}
		})
	}
}

// A store failure on one board's placed row is that board's alone: the
// next board's row is still stored, and the create's board, whose row the
// store did not take, owes it the board's rows read, which stores it, or
// leaves that row alone stale when the store fails again.
func TestCreatePlacementStoreFailureIsOneBoards(t *testing.T) {
	t.Parallel()
	for name, failures := range map[string]int32{"once": 1, "again": 2} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newWriteFixture(t)
			f.subscribe(t, "2")
			idleBoards(t, f.svc, f.p)
			f.state.set(testBoard, writeRow("10004", 1, "d"))
			f.state.set("2", writeRow("10004", 1, "n"))
			var failed atomic.Int32
			f.svc.store = &hookedStore{Store: f.st, rows: func(page store.MirrorRowPage) error {
				if page.BoardID == string(testBoard) && failed.Add(1) <= failures {
					return errStoreDown
				}
				return nil
			}}
			asked := placements(f, f.state.placement)
			reads := f.state.recordRows(f.p)
			f.answer(createdResult("10004"))
			v, err := f.svc.Create(t.Context(), f.conn.ID, CreateRequest{Board: "1", Project: "10000", Title: "New task"})
			if got := asked(); len(got) != 1 || !slices.Equal(got[0], []tracker.BoardID{testBoard, "2"}) || failed.Load() != 2 {
				t.Fatalf("placed on %v; board 1's rows stored %d times, want the placement's and the rows read's", got, failed.Load())
			}
			if got := f.orderOf(t, "2", "10004"); got != "n" {
				t.Fatalf("board 2's row %q: the failure on board 1 dropped it", got)
			}
			wantStale, wantOrder := []string(nil), "d"
			if failures > 1 {
				wantStale, wantOrder = []string{"row:10004"}, ""
			}
			if err != nil || !slices.Equal(staleIDs(v), wantStale) || v.NotOnBoard != "" {
				t.Fatalf("view %+v, %v", v, err)
			}
			if got := reads(); !slices.Equal(got, []string{"1:[10004]"}) || f.orderOf(t, testBoard, "10004") != wantOrder {
				t.Fatalf("reads %v; board 1's row %q", got, f.orderOf(t, testBoard, "10004"))
			}
		})
	}
}
