package trackerapp

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"agent-overflow/internal/tracker"
)

// errBatchFailed is a read of the second batch failing.
var errBatchFailed = errors.New("test: the tracker failed the second batch")

// staleIDs are the resources of v's stale reads, as kind:issue, in order.
func staleIDs(v WriteView) []string {
	var out []string
	for _, st := range v.Stale {
		for _, r := range st.Resources {
			out = append(out, r.Kind+":"+r.Issue)
		}
	}
	return out
}

// manyOnBoard puts n issues from 20001 on the fixture's board 1, synced,
// and returns their IDs.
func manyOnBoard(t *testing.T, f *writeFixture, n int) []string {
	t.Helper()
	var issues []string
	for i := range n {
		id := fmt.Sprint(20001 + i)
		issues = append(issues, id)
		f.state.set(testBoard, writeRow(id, 0, fmt.Sprintf("b%03d", i)))
	}
	tickOK(t, f.svc, f.scope(), BoardDue{Delta: true})
	return issues
}

// A rows read made in batches reports by batch: the issues of a batch
// read and stored are refreshed, and only those of the batch that failed
// are stale, their row and their card.
func TestARowsReadFailingInItsSecondBatchLeavesThatBatchStale(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	issues := manyOnBoard(t, f, readBatch+1)
	var batches int
	f.p.set(func(p *scripted) {
		p.boardRows = func(b tracker.BoardID, asked []tracker.IssueID) (tracker.Rows, error) {
			if batches++; batches == 2 {
				return tracker.Rows{}, errBatchFailed
			}
			return f.state.boardRows(b, asked)
		}
	})
	var res tracker.Result
	for _, id := range issues {
		res.Parts = append(res.Parts, rankPart(testBoard, id, tracker.Applied()))
	}
	f.answer(res)
	v, err := f.svc.Rank(t.Context(), f.conn.ID, RankRequest{Board: "1", Issues: issues, Prev: "10003"})
	if err != nil {
		t.Fatal(err)
	}
	last := issues[len(issues)-1]
	if got := staleIDs(v); !slices.Equal(got, []string{"row:" + last, "card:" + last}) || batches != 2 {
		t.Fatalf("%d batches; stale %d resources: %v", batches, len(got), got)
	}
}

// A cards read made in batches reports by batch too: the cards of a batch
// read and stored are refreshed, and only those of the batch that failed
// are stale.
func TestACardsReadFailingInItsSecondBatchLeavesThatBatchStale(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	issues := manyOnBoard(t, f, readBatch+1)
	var batches int
	f.p.set(func(p *scripted) {
		// The board no longer holds them, so each card is read by itself.
		p.boardRows = func(_ tracker.BoardID, asked []tracker.IssueID) (tracker.Rows, error) {
			return tracker.Rows{Missing: asked}, nil
		}
		p.cards = func(asked []tracker.IssueID) (tracker.Cards, error) {
			if batches++; batches == 2 {
				return tracker.Cards{}, errBatchFailed
			}
			return f.state.cardsRead(asked)
		}
	})
	var res tracker.Result
	for _, id := range issues {
		res.Parts = append(res.Parts, appliedPart(id, tracker.StepEdit, tracker.CardOf(tracker.IssueID(id)), tracker.DetailOf(tracker.IssueID(id))))
	}
	f.answer(res)
	v, err := f.svc.Edit(t.Context(), f.conn.ID, EditRequest{Issue: issues[0], Fields: []FieldEditInput{{Field: "title", Typed: []string{"x"}}}})
	if err != nil {
		t.Fatal(err)
	}
	last := issues[len(issues)-1]
	if got := staleIDs(v); !slices.Equal(got, []string{"card:" + last}) || batches != 2 {
		t.Fatalf("%d batches; stale %d resources: %v", batches, len(got), got)
	}
}
