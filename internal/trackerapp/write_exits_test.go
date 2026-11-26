package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// resourceIDs are the resources of v's Stale, one string each, sorted,
// with a repeat kept so a test sees it.
func resourceIDs(v WriteView) []string {
	var out []string
	for _, st := range v.Stale {
		for _, r := range st.Resources {
			parts := slices.DeleteFunc([]string{r.Kind, r.Board, r.Issue, r.Author, r.Day}, func(s string) bool { return s == "" })
			out = append(out, strings.Join(parts, "/"))
		}
	}
	slices.Sort(out)
	return out
}

// exitAt is what happens to a refresh read: the nth call (from 0) of a
// provider method.
type exitAt struct {
	call string
	n    int
}

// Every way a refresh can stop or lose a board, at every read: the
// applied outcome stands, nothing panics, every resource the write named
// is accounted for once (refreshed, not shown, or stale), the reads after
// a failed one or a retired board still run, a failed read is the
// connection's problem, and what the refresh stored before it stopped is
// published.
//
// The write names board 1's structure, issue 10002's row on board 1,
// issue 10001's card, which owes a rows read through each of boards 1
// and 2, the details of 10001 (an open ticket) and 10002 (cached), and a
// day of worklogs. Its reads, in order: Structure, BoardRows of board 1,
// BoardRows of board 2, Details, Worklogs. The card is stale when either
// board's read fails or is never made, after the other succeeded. 10002's
// body, which no ticket has open, is dropped just before the details read
// and is stale only when the refresh stops before that.
func TestEveryRefreshExitAccountsForEveryResource(t *testing.T) {
	t.Parallel()
	all := []string{"card/10001", "detail/10001", "detail/10002", "row/1/10002", "structure/1", "worklogs/acc-user1/2026-11-02"}
	allBut := func(id string) []string {
		return slices.DeleteFunc(slices.Clone(all), func(s string) bool { return s == id })
	}
	reads := []string{"Structure", "BoardRows", "BoardRows", "Details", "Worklogs"}
	type action int
	const (
		fail action = iota
		cancelDuring
		cancelBefore
		retire
		end
	)
	cases := []struct {
		name  string
		at    exitAt
		do    action
		stale []string
		// calls are the reads made; board1 reports board 1's rows stored.
		calls  []string
		board1 bool
		ended  bool
	}{
		{"structure fails", exitAt{"Structure", 0}, fail, []string{"structure/1"}, reads, true, false},
		{"board 1 rows fail", exitAt{"BoardRows", 0}, fail, []string{"card/10001", "row/1/10002"}, reads, false, false},
		{"board 2 rows fail", exitAt{"BoardRows", 1}, fail, []string{"card/10001"}, reads, true, false},
		{"details fail", exitAt{"Details", 0}, fail, []string{"detail/10001"}, reads, true, false},
		{"worklogs fail", exitAt{"Worklogs", 0}, fail, []string{"worklogs/acc-user1/2026-11-02"}, reads, true, false},
		{"cancelled during structure", exitAt{"Structure", 0}, cancelDuring, all, reads[:1], false, false},
		{"cancelled during board 1 rows", exitAt{"BoardRows", 0}, cancelDuring, allBut("structure/1"), reads[:2], false, false},
		{"cancelled during board 2 rows", exitAt{"BoardRows", 1}, cancelDuring, []string{"card/10001", "detail/10001", "detail/10002", "worklogs/acc-user1/2026-11-02"}, reads[:3], true, false},
		{"cancelled during details", exitAt{"Details", 0}, cancelDuring, []string{"detail/10001", "worklogs/acc-user1/2026-11-02"}, reads[:4], true, false},
		{"cancelled during worklogs", exitAt{"Worklogs", 0}, cancelDuring, []string{"worklogs/acc-user1/2026-11-02"}, reads, true, false},
		{"cancelled before board 1 rows", exitAt{"Structure", 0}, cancelBefore, allBut("structure/1"), reads[:2], false, false},
		{"cancelled before board 2 rows", exitAt{"BoardRows", 0}, cancelBefore, []string{"card/10001", "detail/10001", "detail/10002", "worklogs/acc-user1/2026-11-02"}, reads[:3], true, false},
		{"cancelled before details", exitAt{"BoardRows", 1}, cancelBefore, []string{"detail/10001", "worklogs/acc-user1/2026-11-02"}, reads[:4], true, false},
		{"cancelled before worklogs", exitAt{"Details", 0}, cancelBefore, []string{"worklogs/acc-user1/2026-11-02"}, reads, true, false},
		{"board 1 retired during its structure", exitAt{"Structure", 0}, retire, nil, []string{"Structure", "BoardRows", "Details", "Worklogs"}, false, false},
		{"board 1 retired during its rows", exitAt{"BoardRows", 0}, retire, nil, reads, false, false},
		{"board 2 retired during its rows", exitAt{"BoardRows", 1}, retire, nil, reads, true, false},
		{"turned off during structure", exitAt{"Structure", 0}, end, nil, reads[:1], false, true},
		{"turned off during board 1 rows", exitAt{"BoardRows", 0}, end, nil, reads[:2], false, true},
		{"turned off during details", exitAt{"Details", 0}, end, nil, reads[:4], false, true},
		{"turned off during worklogs", exitAt{"Worklogs", 0}, end, nil, reads, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newWriteFixture(t)
			f.subscribe(t, testBoard)
			f.events.boardEvents(t, 1)
			f.openDetail(t, "10002")
			must[TicketSubscription](t)(f.svc.SubscribeTicket(f.conn.ID, "10001", true, untied))
			f.events.issueEvents(t, 1)
			f.settledTickets(t)
			u, d := tracker.UserID("acc-user1"), day(t, "2026-11-02")
			f.answer(tracker.Result{Parts: []tracker.Part{appliedPart("10001", tracker.StepEdit, tracker.CardOf("10001"), tracker.DetailOf("10001"),
				tracker.RowOf(testBoard, "10002"), tracker.StructureOf(testBoard), tracker.DetailOf("10002"), tracker.WorklogsOf(u, d))}})
			f.state.set(testBoard, writeRow("10001", 1, "z"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var mu sync.Mutex
			seen := map[string]int{}
			f.p.set(func(p *scripted) {
				p.block = func(callCtx context.Context, name string) error {
					if err := callCtx.Err(); err != nil {
						// A provider fails a call whose context ended.
						return err
					}
					mu.Lock()
					n := seen[name]
					seen[name]++
					mu.Unlock()
					if (exitAt{name, n}) != c.at {
						return nil
					}
					switch c.do {
					case fail:
						return &trackerapi.TransientError{Err: errors.New("reset")}
					case cancelDuring:
						cancel()
						return context.Canceled
					case cancelBefore:
						cancel()
					case retire:
						f.listBoards(f.conn.ID, slices.DeleteFunc(slices.Clone(testBoards), func(b tracker.BoardID) bool {
							return b == tracker.BoardID(fmt.Sprint(n+1))
						})...)
					case end:
						must[Connection](t)(f.svc.SetEnabled(f.conn.ID, false))
					}
					return nil
				}
			})
			boards, _, _ := f.events.snapshot()
			nb := len(boards)
			before := len(f.p.callsInOrder())
			v, err := f.svc.Edit(ctx, f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"New"}}}})
			if err != nil || !slices.Equal(states(v), []string{"10001 edit applied"}) || v.Ended != c.ended {
				t.Fatalf("%+v, %v", v, err)
			}
			if got := f.callsSince(before)[1:]; !slices.Equal(got, c.calls) {
				t.Errorf("reads %v, want %v", got, c.calls)
			}
			if got := resourceIDs(v); !slices.Equal(got, c.stale) {
				t.Errorf("stale %v, want %v", got, c.stale)
			}
			for _, st := range v.Stale {
				if st.Failure.Kind != string(FailureTransient) {
					t.Errorf("stale %+v", st)
				}
			}
			// A failed read settles the problem whatever read follows it; a
			// cancelled write or a retired board settles nothing.
			if want := map[bool]FailureKind{true: FailureTransient}[c.do == fail]; f.problem() != want {
				t.Errorf("problem %q, want %q", f.problem(), want)
			}
			if c.board1 {
				f.events.waitFor(t, "board 1's event of the stored row", func(b []BoardEvent, _ []IssueEvent, _ []error) bool {
					return slices.ContainsFunc(b[nb:], func(e BoardEvent) bool { return slices.Contains(e.Changed, tracker.IssueID("10001")) })
				})
			}
		})
	}
}

// An edit that takes an issue the mirror keeps through a thread link off
// every board: both rows reads answer it missing and remove its rows, and
// its card is read by itself in the same refresh, so the mirror holds the
// edit and nothing is stale.
func TestAnEditThatTakesAKeptIssueOffEveryBoard(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.linkThread("thread-1", "10001")
	f.answer(appliedEdit)
	changed := writeRow("10001", 1, "z").Card
	changed.Title = "Off every board"
	f.p.set(func(p *scripted) {
		p.boardRows = func(_ tracker.BoardID, ids []tracker.IssueID) (tracker.Rows, error) {
			return tracker.Rows{Missing: slices.Clone(ids)}, nil
		}
		p.cards = func(asked []tracker.IssueID) (tracker.Cards, error) {
			if !slices.Equal(asked, ids("10001")) {
				return tracker.Cards{}, fmt.Errorf("test: cards read of %v", asked)
			}
			return tracker.Cards{Cards: []tracker.Card{changed}}, nil
		}
	})
	before := len(f.p.callsInOrder())
	v, err := f.svc.Edit(t.Context(), f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{changed.Title}}}})
	if err != nil || len(v.Stale) > 0 {
		t.Fatalf("%+v, %v", v, err)
	}
	if got := f.callsSince(before); !slices.Equal(got, []string{"Edit", "BoardRows", "BoardRows", "Cards"}) {
		t.Fatalf("calls %v", got)
	}
	if issue := must[IssueView](t)(f.svc.Issue(f.conn.ID, "10001")); issue.Card.Title != changed.Title {
		t.Fatalf("the kept card is %q", issue.Card.Title)
	}
	if got := f.rowIDs(testBoard); slices.Contains(got, "10001") {
		t.Fatalf("board 1 still holds 10001: %v", got)
	}
}

// A rank on board 1 names 10001's row there and its card, which board 2
// shows too: board 1's read succeeding does not settle the card, so
// board 2's failed read leaves it stale, board 2's order as it was, and
// the failure the connection's problem.
func TestARankReportsAFailedSiblingBoard(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.answer(tracker.Result{Parts: []tracker.Part{rankPart(testBoard, "10001", tracker.Applied())}})
	f.state.set(testBoard, writeRow("10001", 1, "z"))
	f.p.set(func(p *scripted) {
		p.boardRows = func(b tracker.BoardID, asked []tracker.IssueID) (tracker.Rows, error) {
			if b == "2" {
				return tracker.Rows{}, &trackerapi.TransientError{Err: errors.New("reset")}
			}
			return f.state.boardRows(b, asked)
		}
	})
	v, err := f.svc.Rank(t.Context(), f.conn.ID, RankRequest{Board: "1", Issues: []string{"10001"}, Prev: "10003"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resourceIDs(v); !slices.Equal(got, []string{"card/10001"}) || v.Stale[0].Failure.Kind != string(FailureTransient) {
		t.Fatalf("stale %+v", v.Stale)
	}
	if f.orderOf(t, testBoard, "10001") != "z" || f.orderOf(t, "2", "10001") != "m" || f.problem() != FailureTransient {
		t.Fatalf("orders %q %q, problem %q", f.orderOf(t, testBoard, "10001"), f.orderOf(t, "2", "10001"), f.problem())
	}
}

// A resource two failed reads owed is reported once, with the first
// failure, and the last failure is the connection's problem.
func TestAResourceTwoFailedReadsOwedIsReportedOnce(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.answer(appliedEdit)
	f.p.set(func(p *scripted) {
		p.boardRows = func(b tracker.BoardID, _ []tracker.IssueID) (tracker.Rows, error) {
			if b == "2" {
				return tracker.Rows{}, &trackerapi.RateLimitedError{Tracker: "Jira", Until: time.UnixMilli(1_800_000_060_000)}
			}
			return tracker.Rows{}, &trackerapi.TransientError{Err: errors.New("reset")}
		}
	})
	v, err := f.svc.Edit(t.Context(), f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"New"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := resourceIDs(v); !slices.Equal(got, []string{"card/10001"}) || v.Stale[0].Failure.Kind != string(FailureTransient) {
		t.Fatalf("stale %+v", v.Stale)
	}
	if f.problem() != FailureRateLimited {
		t.Fatalf("problem %q", f.problem())
	}
}
