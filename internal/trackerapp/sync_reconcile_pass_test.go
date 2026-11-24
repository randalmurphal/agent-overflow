package trackerapp

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// passRule is the rule a passBoard lists by.
var passRule = tracker.Eligibility{Window: time.UnixMilli(1_790_000_000_000), Sprints: []tracker.SprintID{"5"}}

// passBoard scripts a board whose listing names n issues from 20000 up,
// all of them complete, by passRule, a delta that delivers none of them, and rows by
// ID that answer only the issues available says; it records each issue
// a rows read asks for.
type passBoard struct {
	mu        sync.Mutex
	listed    []tracker.IssueID
	available func(i int) bool
	asked     map[tracker.IssueID]int
	// fail, when set, fails the rows read it returns an error for.
	fail func(read int) error
	read int
}

func newPassBoard(m *mirrorFixture, n int, available func(i int) bool) *passBoard {
	b := &passBoard{available: available, asked: map[tracker.IssueID]int{}}
	for i := range n {
		b.listed = append(b.listed, tracker.IssueID(fmt.Sprint(20000+i)))
	}
	m.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{Next: cursorAfter("none")}, nil
		}
		p.boardIssues = func(tracker.BoardID, time.Time) (tracker.Listing, error) {
			return tracker.Listing{IDs: slices.Clone(b.listed), Complete: true, Rule: passRule}, nil
		}
		p.boardRows = b.rows
		p.history = func(tracker.HistoryQuery) (tracker.History, error) { return tracker.History{}, nil }
	})
	return b
}

func (b *passBoard) rows(_ tracker.BoardID, asked []tracker.IssueID) (tracker.Rows, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.read++
	for _, id := range asked {
		b.asked[id]++
	}
	if b.fail != nil {
		if err := b.fail(b.read); err != nil {
			return tracker.Rows{}, err
		}
	}
	var out tracker.Rows
	for _, id := range asked {
		i := slices.Index(b.listed, id)
		if i >= 0 && b.available(i) {
			out.Rows = append(out.Rows, row(string(id), 1_799_000_000_000, string(id)))
		} else {
			out.Missing = append(out.Missing, id)
		}
	}
	return out, nil
}

// askedOnce reports whether every listed issue was asked for exactly once.
func (b *passBoard) askedOnce() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range b.listed {
		if b.asked[id] != 1 {
			return false
		}
	}
	return len(b.asked) == len(b.listed)
}

// pumpLoads subscribes to m's board and waits until the pump stored
// issue's row, failing at a second listing of the board, and returns the
// provider calls made until then. With idle set it then runs at once
// before, then a refresh: a pump that ticks again on its own after
// loading the issue shows as a background call after the calls returned,
// since the refresh's calls are interactive.
func pumpLoads(t *testing.T, m *mirrorFixture, issue tracker.IssueID, idle bool, before func()) []string {
	t.Helper()
	listedAgain := make(chan struct{})
	m.p.set(func(p *scripted) {
		list, lists := p.boardIssues, 0
		p.boardIssues = func(b tracker.BoardID, w time.Time) (tracker.Listing, error) {
			if lists++; lists == 2 {
				close(listedAgain)
			}
			return list(b, w)
		}
	})
	sub := m.subscribe(t, testBoard)
	// A bound no healthy run reaches, under -race and a loaded machine too.
	deadline := time.After(2 * time.Minute)
	for {
		// Checked at each event: a tick that stores a row ends with one,
		// which names no issue past 100 (Truncated).
		m.events.mu.Lock()
		changed := m.events.changed
		m.events.mu.Unlock()
		if rows, _, err := m.st.ListMirrorIssueRows(m.root(), string(issue), ""); err != nil {
			t.Fatal(err)
		} else if len(rows) > 0 {
			break
		}
		select {
		case <-changed:
		case <-listedAgain:
			t.Fatalf("the pump listed the board again before it loaded %s: %v", issue, describeCounts(countsOf(m.p.callsInOrder())))
		case <-deadline:
			t.Fatalf("the pump did not load %s: %v", issue, describeCounts(countsOf(m.p.callsInOrder())))
		}
	}
	loaded := m.p.callsInOrder()
	before()
	if !idle {
		return loaded
	}
	if err := m.svc.RefreshSubscription(t.Context(), sub.ID); err != nil {
		t.Fatal(err)
	}
	calls, interactive := m.p.callsInOrder(), m.p.interactiveCalls()
	for i := len(loaded); i < len(calls); i++ {
		if !interactive[i] {
			t.Fatalf("after loading %s the pump ticked again on its own: %v", issue, calls[len(loaded):])
		}
	}
	return loaded
}

// The reviewer's board: 1,001 listed issues, the first 1,000 of which the
// rows read answers missing. The pump lists the board once, reads each
// candidate once (11 rows reads), loads the last, and then waits its
// interval: a candidate the rows read missed waits for the next pass.
func TestAReconcilePassReadsEachCandidateOnce(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureOn(t, newFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour}))
	n := reconcileBatches*readBatch + 1
	b := newPassBoard(m, n, func(i int) bool { return i == n-1 })
	last := b.listed[n-1]
	once := false
	calls := countsOf(pumpLoads(t, m, last, true, func() { once = b.askedOnce() }))
	if calls["BoardIssues"] != 1 || calls["BoardRows"] != 11 || calls["ChangedRows"] != 2 || !once {
		t.Fatalf("the pump's ticks called %s; want one listing, 11 rows reads, two ticks, each candidate asked once", describeCounts(calls))
	}
	if rows, _, err := m.st.ListMirrorIssueRows(m.root(), string(last), ""); err != nil || len(rows) != 1 {
		t.Fatalf("the last issue's rows %+v, %v", rows, err)
	}
}

// A large import the rows read answers whole is read row by row once: one
// listing, then the pass's batches over three ticks. (The history of the
// rows it loads keeps the pump ticking after it.)
func TestAReconcilePassOfALargeImportReadsEachRowOnce(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureOn(t, newFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour}))
	n := 2*reconcileBatches*readBatch + readBatch
	b := newPassBoard(m, n, func(int) bool { return true })
	once := false
	calls := countsOf(pumpLoads(t, m, b.listed[n-1], false, func() { once = b.askedOnce() }))
	if calls["BoardIssues"] != 1 || calls["BoardRows"] != (n+readBatch-1)/readBatch || !once {
		t.Fatalf("the import's ticks called %s; want one listing, %d rows reads, each row once", describeCounts(calls), (n+readBatch-1)/readBatch)
	}
}

// passTick runs a tick of m's board as a pump runs it, its reconcile
// going on with holder's pass.
func passTick(t *testing.T, m *mirrorFixture, holder *boardPump, due BoardDue) Outcome {
	t.Helper()
	out, _, err := m.svc.syncBoard(t.Context(), holder, Background, func(*run) BoardDue { return due })
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A rows read that fails keeps its batch and the rest in the pass: the
// retry goes on from it without listing the board again. A pass of an
// earlier board lifetime (here a root the store detached) is not gone on
// with: the next reconcile lists again.
func TestAReconcilePassResumesAtAFailedBatchAndEndsWithItsRoot(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	n := reconcileBatches*readBatch + readBatch*5
	b := newPassBoard(m, n, func(int) bool { return true })
	b.fail = failAt(3, &trackerapi.TransientError{Err: errors.New("reset")})
	holder := &boardPump{scope: m.scope()}
	due := BoardDue{Structure: true, Reconcile: true}
	if out := passTick(t, m, holder, due); out.Failure == nil || out.Failure.Kind != FailureTransient {
		t.Fatalf("the failing tick %+v", out)
	}
	if out := passTick(t, m, holder, BoardDue{Reconcile: true}); out.Failure != nil || !out.More() || len(out.Changes().Rows) != reconcileBatches*readBatch {
		t.Fatalf("the retry %+v", out)
	}
	if out := passTick(t, m, holder, BoardDue{Reconcile: true}); out.Failure != nil || out.More() || len(out.Changes().Rows) != readBatch*3 {
		t.Fatalf("the pass's last tick %+v", out)
	}
	// The failed batch, the third, was asked twice; every other issue once.
	b.mu.Lock()
	twice := true
	for _, id := range b.listed[2*readBatch : 3*readBatch] {
		twice = twice && b.asked[id] == 2
		b.asked[id] = 1
	}
	b.mu.Unlock()
	if m.p.count("BoardIssues") != 1 || !twice || !b.askedOnce() {
		t.Fatalf("%d listings; the failed batch asked twice %v; each other issue once %v", m.p.count("BoardIssues"), twice, b.askedOnce())
	}
	if m.sync(testBoard).ReconciledAt == 0 {
		t.Fatal("the finished pass recorded no reconcile")
	}
	// A pass left part way under a root the store then detached: the tick
	// takes the new root and lists it.
	b.listed = append(b.listed, b.listed...)
	for i := range n {
		b.listed[n+i] = tracker.IssueID(fmt.Sprint(30000 + i))
	}
	if out := passTick(t, m, holder, BoardDue{Reconcile: true}); !out.More() || m.p.count("BoardIssues") != 2 {
		t.Fatalf("a new pass %+v after %d listings", out, m.p.count("BoardIssues"))
	}
	if _, err := m.st.DetachMirror(m.conn.ID); err != nil {
		t.Fatal(err)
	}
	if out := passTick(t, m, holder, due); out.Ended || m.p.count("BoardIssues") != 3 {
		t.Fatalf("the tick on the new root %+v after %d listings", out, m.p.count("BoardIssues"))
	}
}
