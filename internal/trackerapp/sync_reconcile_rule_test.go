package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// A reconcile pass whose rows read the tracker refuses for the listing's
// rule (tracker.ErrRuleRefused: the tracker refused the query that carried
// it) ends, and the refusal is the reconcile's failure, held in the board's
// sync state: the board is listed again a reconcile interval after the
// refused call returned, or at a person's refresh, and nothing lists twice
// for it. Every other failure keeps the exact pass. These run the
// reconcile against a scripted provider, whose BoardIssues calls are the
// listings counted here (the provider level): the handling is the same
// whichever provider refuses.

// failAt is a passBoard.fail that fails the rows reads numbered reads
// with err.
func failAt(read int, err error, more ...int) func(int) error {
	return func(n int) error {
		if n == read || slices.Contains(more, n) {
			return err
		}
		return nil
	}
}

// ruleRefused is a provider's typed refusal of a rule, wrapping its 400.
func ruleRefused() error {
	return fmt.Errorf("%w: %w", tracker.ErrRuleRefused, &trackerapi.StatusError{Status: http.StatusBadRequest})
}

// rowsHeld reports whether m's mirror holds a row of each of issues.
func rowsHeld(t *testing.T, m *mirrorFixture, issues []tracker.IssueID) bool {
	t.Helper()
	for _, id := range issues {
		rows, _, err := m.st.ListMirrorIssueRows(m.root(), string(id), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			return false
		}
	}
	return true
}

// dueTick runs a tick of m's board as its pump runs it, the units the
// pump's own due rule (boardDue) picks.
func dueTick(t *testing.T, m *mirrorFixture, holder *boardPump) Outcome {
	t.Helper()
	out, _, err := m.svc.syncBoard(t.Context(), holder, Background, m.svc.boardDue)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// listings are the BoardIssues calls m's provider took: the listings, at
// the provider level.
func listings(m *mirrorFixture) int { return m.p.count("BoardIssues") }

// refusal is the board's stored reconcile failure.
func refusal(m *mirrorFixture) store.MirrorReconcileFailure {
	return m.sync(testBoard).ReconcileFailure
}

// refusedPump is an idle pump of m's board whose first tick a refusal that
// persists failed: one listing, the refusal stored and shown.
func refusedPump(t *testing.T, m *mirrorFixture) (*passBoard, *boardPump) {
	t.Helper()
	b := newPassBoard(m, 2*readBatch, func(int) bool { return true })
	b.fail = func(int) error { return ruleRefused() }
	p := idlePump(m)
	if res := m.svc.boardTick(p, false, Background); !res.failed || res.failure == nil || listings(m) != 1 {
		t.Fatalf("the first tick %+v after %d listings; want the refusal after 1", res, listings(m))
	}
	if rf := refusal(m); !rf.Refused || p.pass.openIn(m.sync(testBoard).Lifetime) || shown(p).At != rf.Wall {
		t.Fatalf("after the refusal: stored %+v, the pass %+v, shown %+v; want it stored, the pass ended, shown at its time", rf, p.pass, shown(p))
	}
	return b, p
}

// shown is the failure p's subscribers are shown: the recorded sync
// state's, zero for none.
func shown(p *boardPump) FailureView {
	if f := p.sync.Failure; f != nil {
		return *f
	}
	return FailureView{}
}

// tickAt runs p's scheduled tick and reports the listings it made.
func tickAt(t *testing.T, m *mirrorFixture, p *boardPump) (tickResult, int) {
	t.Helper()
	before := listings(m)
	res := m.svc.boardTick(p, false, Background)
	return res, listings(m) - before
}

// A refusal from a state with none ends the pass after one listing and is
// the reconcile's failure; a refusal that persists then costs one listing
// a reconcile interval, counted over three intervals: a tick a second
// before each deadline lists nothing and does not fail, the board still
// showing the refusal, and the tick at it lists once and restarts the wait.
func TestARefusalThatPersistsListsOnceAnInterval(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	_, p := refusedPump(t, m)
	for i := range 3 {
		at := refusal(m).Wall
		m.pass(m.svc.cadence.Reconcile - time.Second)
		if res, n := tickAt(t, m, p); res.failed || n != 0 || shown(p).At != at {
			t.Fatalf("interval %d, a second early: %+v, %d listings, shown %+v; want no failure, none, the refusal at %d", i, res, n, shown(p), at)
		}
		m.pass(time.Second)
		if res, n := tickAt(t, m, p); !res.failed || n != 1 || refusal(m).Wall <= at || !refusal(m).Refused {
			t.Fatalf("interval %d, at the deadline: %+v, %d listings, stored %+v; want the refusal again after 1, later than %d", i, res, n, refusal(m), at)
		}
	}
	if listings(m) != 4 {
		t.Fatalf("%d listings over three intervals; want 4", listings(m))
	}
}

// A refusal in the third batch, after two committed, keeps those rows and
// ends the pass with one listing.
func TestARefusalInALaterBatchKeepsTheCommittedRows(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	b := newPassBoard(m, 4*readBatch, func(int) bool { return true })
	b.fail = failAt(3, ruleRefused())
	holder := &boardPump{scope: m.scope()}
	if out := passTick(t, m, holder, BoardDue{Structure: true, Reconcile: true}); out.Failure == nil || listings(m) != 1 {
		t.Fatalf("the refused tick %+v after %d listings; want the refusal after 1", out, listings(m))
	}
	if !rowsHeld(t, m, b.listed[:2*readBatch]) || rowsHeld(t, m, b.listed[2*readBatch:]) || holder.pass.openIn(m.sync(testBoard).Lifetime) || !refusal(m).Refused {
		t.Fatalf("the pass %+v, stored %+v; want the two committed batches held, the pass ended and the refusal stored", holder.pass, refusal(m))
	}
}

// The wait runs from when the refused call returned, not from the read it
// began: a rows read that takes a minute before the tracker refuses it
// waits a full interval after the refusal.
func TestARefusalWaitsFromWhenTheRefusedCallReturned(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	b := newPassBoard(m, 2*readBatch, func(int) bool { return true })
	b.fail = func(int) error {
		m.pass(time.Minute)
		return ruleRefused()
	}
	p := idlePump(m)
	if res := m.svc.boardTick(p, false, Background); !res.failed || !refusal(m).Refused {
		t.Fatalf("the refused tick %+v, stored %+v", res, refusal(m))
	}
	m.pass(m.svc.cadence.Reconcile - time.Minute)
	if _, n := tickAt(t, m, p); n != 0 {
		t.Fatalf("an interval after the read began, a minute before the refusal's: %d listings; want none", n)
	}
	m.pass(time.Minute)
	if _, n := tickAt(t, m, p); n != 1 {
		t.Fatalf("an interval after the refusal: %d listings; want 1", n)
	}
}

// A pump made again inside the wait, by a reopen or a restart, lists
// nothing and shows the refusal at its own time, whether the last pass
// that completed is recent or none ever did; the restarted service lists
// at the deadline.
func TestAReopenOrARestartInsideTheWaitListsNothing(t *testing.T) {
	t.Parallel()
	for _, recent := range []bool{false, true} {
		t.Run(fmt.Sprint("a recent pass ", recent), func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			b := newPassBoard(m, 2*readBatch, func(int) bool { return true })
			p := idlePump(m)
			if recent {
				if res := m.svc.boardTick(p, false, Background); res.failed || m.sync(testBoard).ReconciledAt == 0 {
					t.Fatalf("the first pass %+v", res)
				}
				b.mu.Lock()
				b.listed = append(b.listed, "30000")
				b.mu.Unlock()
			}
			b.fail = func(int) error { return ruleRefused() }
			if res := m.svc.boardTick(p, true, Interactive); !res.failed || !refusal(m).Refused {
				t.Fatalf("the refused refresh %+v, stored %+v", res, refusal(m))
			}
			at, before := refusal(m).Wall, listings(m)
			m.pass(time.Second)
			reopened := idlePump(m)
			if res, n := tickAt(t, m, reopened); res.failed || n != 0 || shown(reopened).At != at {
				t.Fatalf("a reopen: %+v, %d listings, shown %+v; want none, the refusal at %d", res, n, shown(reopened), at)
			}
			// The board's view, which a pane opens with, shows it too.
			if v := must[BoardView](t)(m.svc.Board(m.scope(), StatusGroups{}, nil)); v.Sync.Failure == nil || v.Sync.Failure.At != at {
				t.Fatalf("the board's view shows %+v; want the refusal at %d", v.Sync.Failure, at)
			}
			m.restart(t)
			restarted := idlePump(m)
			if res, n := tickAt(t, m, restarted); res.failed || n != 0 || shown(restarted).At != at {
				t.Fatalf("a restart: %+v, %d listings, shown %+v; want none, the refusal at %d", res, n, shown(restarted), at)
			}
			m.pass(m.svc.cadence.Reconcile - time.Second)
			if _, n := tickAt(t, m, restarted); n != 1 || listings(m) != before+1 {
				t.Fatalf("the restarted service at the deadline: %d listings; want 1", n)
			}
		})
	}
}

// A delta that reports More at the deadline defers the reconcile, which
// lists nothing while the walk goes on, the refusal still shown; the tick
// the walk ends lists once, which either completes and clears the
// refusal or is refused and restarts the wait.
func TestADeltaWalkAtTheDeadlineDefersTheListing(t *testing.T) {
	t.Parallel()
	for _, refusedAgain := range []bool{false, true} {
		t.Run(fmt.Sprint("refused again ", refusedAgain), func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			b, p := refusedPump(t, m)
			at := refusal(m).Wall
			walk := 0
			m.p.set(func(sp *scripted) {
				sp.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
					walk++
					return tracker.RowPage{Next: cursorAfter(fmt.Sprint("w", walk)), More: walk < deltaPages+1}, nil
				}
			})
			m.pass(m.svc.cadence.Reconcile)
			if res, n := tickAt(t, m, p); res.failed || !res.more || n != 0 || shown(p).At != at {
				t.Fatalf("the deadline's tick, the walk going on: %+v, %d listings, shown %+v; want More, none, the refusal at %d", res, n, shown(p), at)
			}
			if !refusedAgain {
				b.mu.Lock()
				b.fail = nil
				b.mu.Unlock()
			}
			res, n := tickAt(t, m, p)
			switch {
			case n != 1:
				t.Fatalf("the tick the walk ended: %+v, %d listings; want 1", res, n)
			case refusedAgain && (!res.failed || refusal(m).Wall <= at):
				t.Fatalf("refused again: %+v, stored %+v; want a new refusal", res, refusal(m))
			case !refusedAgain && (res.failed || refusal(m) != store.MirrorReconcileFailure{} || p.sync.Failure != nil):
				t.Fatalf("completed: %+v, stored %+v, shown %+v; want the refusal cleared", res, refusal(m), p.sync.Failure)
			}
		})
	}
}

// A delta failure inside the wait is shown, the newer failure; once the
// delta recovers the refusal shows again at its own time, and the wait is
// unchanged.
func TestADeltaFailureInsideTheWaitAndItsRecovery(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	_, p := refusedPump(t, m)
	at := refusal(m).Wall
	delta := m.p.changedRows
	m.p.set(func(sp *scripted) {
		sp.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{}, &trackerapi.TransientError{Err: errors.New("reset")}
		}
	})
	m.pass(time.Second)
	if res, n := tickAt(t, m, p); !res.failed || n != 0 || shown(p).Kind != string(FailureTransient) {
		t.Fatalf("the delta's failure: %+v, %d listings, shown %+v; want the delta's failure shown", res, n, shown(p))
	}
	m.p.set(func(sp *scripted) { sp.changedRows = delta })
	m.pass(time.Second)
	if res, n := tickAt(t, m, p); res.failed || n != 0 || shown(p).At != at || !refusal(m).Refused {
		t.Fatalf("the delta's recovery: %+v, %d listings, shown %+v; want the refusal shown again at %d", res, n, shown(p), at)
	}
	m.pass(m.svc.cadence.Reconcile - 3*time.Second)
	if _, n := tickAt(t, m, p); n != 0 {
		t.Fatalf("a second before the deadline: %d listings; want none", n)
	}
	m.pass(time.Second)
	if _, n := tickAt(t, m, p); n != 1 {
		t.Fatalf("at the deadline: %d listings; want 1", n)
	}
}

// A person's refresh inside the wait lists once whatever the wait: a
// refusal again restarts the wait from it, a pass that completes clears
// the refusal and the board reconciles on its interval again.
func TestARefreshInsideTheWaitListsOnce(t *testing.T) {
	t.Parallel()
	for _, recovers := range []bool{false, true} {
		t.Run(fmt.Sprint("recovers ", recovers), func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			b, p := refusedPump(t, m)
			at := refusal(m).Wall
			if recovers {
				b.mu.Lock()
				b.fail = nil
				b.mu.Unlock()
			}
			m.pass(m.svc.cadence.Reconcile / 2)
			before := listings(m)
			res := m.svc.boardTick(p, true, Interactive)
			if listings(m) != before+1 {
				t.Fatalf("the refresh made %d listings; want 1", listings(m)-before)
			}
			if recovers {
				if res.failed || refusal(m) != (store.MirrorReconcileFailure{}) || p.sync.Failure != nil {
					t.Fatalf("the refresh %+v, stored %+v, shown %+v; want the refusal cleared", res, refusal(m), p.sync.Failure)
				}
			} else if !res.failed || refusal(m).Wall <= at || shown(p).At != refusal(m).Wall {
				t.Fatalf("the refresh %+v, stored %+v, shown %+v; want the new refusal shown at its time", res, refusal(m), shown(p))
			}
			m.pass(m.svc.cadence.Reconcile - time.Second)
			if _, n := tickAt(t, m, p); n != 0 {
				t.Fatalf("a second before an interval from the refresh: %d listings; want none", n)
			}
			m.pass(time.Second)
			if _, n := tickAt(t, m, p); n != 1 {
				t.Fatalf("an interval after the refresh: %d listings; want 1", n)
			}
		})
	}
}

// A refresh inside the wait whose listing is rate limited shows that
// failure in the refusal's place with its RetryAt; the stored failure is
// no refusal, so the retry lists once, and a 400 then restarts the wait:
// 400, 429, 400. A 429 at a candidate read keeps the pass.
func TestARateLimitedRefreshInsideTheWait(t *testing.T) {
	t.Parallel()
	t.Run("at the listing", func(t *testing.T) {
		t.Parallel()
		m := newMirrorFixture(t)
		_, p := refusedPump(t, m)
		list := m.p.boardIssues
		until := time.Now().Add(2 * time.Minute)
		m.p.set(func(sp *scripted) {
			sp.boardIssues = func(tracker.BoardID, time.Time) (tracker.Listing, error) {
				return tracker.Listing{}, &trackerapi.RateLimitedError{Until: until}
			}
		})
		m.pass(time.Second)
		res := m.svc.boardTick(p, true, Interactive)
		if !res.failed || !res.retryAt.Equal(until) || shown(p).Kind != string(FailureRateLimited) || refusal(m).Refused {
			t.Fatalf("the rate-limited refresh %+v, shown %+v, stored %+v; want the 429 shown, its RetryAt, no refusal", res, shown(p), refusal(m))
		}
		m.p.set(func(sp *scripted) { sp.boardIssues = list })
		m.pass(2 * time.Minute)
		if res, n := tickAt(t, m, p); !res.failed || n != 1 || !refusal(m).Refused {
			t.Fatalf("the retry: %+v, %d listings, stored %+v; want 1 and the refusal again", res, n, refusal(m))
		}
		m.pass(time.Second)
		if _, n := tickAt(t, m, p); n != 0 {
			t.Fatalf("after the second 400: %d listings; want the wait", n)
		}
	})
	t.Run("at a candidate read", func(t *testing.T) {
		t.Parallel()
		m := newMirrorFixture(t)
		b, p := refusedPump(t, m)
		b.mu.Lock()
		b.fail = func(int) error { return &trackerapi.RateLimitedError{Until: time.Now().Add(time.Minute)} }
		b.mu.Unlock()
		m.pass(time.Second)
		if res := m.svc.boardTick(p, true, Interactive); !res.failed || len(p.pass.left) != len(b.listed) || shown(p).Kind != string(FailureRateLimited) {
			t.Fatalf("the refresh %+v, the pass %d candidates, shown %+v; want the 429 and the pass kept", res, len(p.pass.left), shown(p))
		}
	})
}

// A refresh that goes on with an open pass lists nothing; a refusal then
// ends the pass and starts the wait.
func TestARefreshGoingOnWithAnOpenPassThatIsRefused(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	b := newPassBoard(m, (reconcileBatches+1)*readBatch, func(int) bool { return true })
	p := idlePump(m)
	if res := m.svc.boardTick(p, true, Interactive); res.failed || !res.more || listings(m) != 1 {
		t.Fatalf("the first refresh %+v after %d listings; want More after 1", res, listings(m))
	}
	b.mu.Lock()
	b.fail = func(int) error { return ruleRefused() }
	b.mu.Unlock()
	if res := m.svc.boardTick(p, true, Interactive); !res.failed || listings(m) != 1 || !refusal(m).Refused || p.pass.openIn(m.sync(testBoard).Lifetime) {
		t.Fatalf("the refresh going on %+v after %d listings, stored %+v; want the refusal after 1 and the pass ended", res, listings(m), refusal(m))
	}
	m.pass(time.Second)
	if _, n := tickAt(t, m, p); n != 0 {
		t.Fatalf("the next tick: %d listings; want the wait", n)
	}
}

// The pump's interactive ticks that are no refresh, its opening tick and
// the tick a write asks for now (Service.Create), follow its due rule:
// inside the wait they list nothing.
func TestAnInteractiveTickInsideTheWaitListsNothing(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	_, p := refusedPump(t, m)
	m.pass(time.Second)
	if res := m.svc.boardTick(idlePump(m), false, Interactive); res.failed || listings(m) != 1 {
		t.Fatalf("an opening tick %+v after %d listings; want none more", res, listings(m))
	}
	if res := m.svc.boardTick(p, false, Interactive); res.failed || listings(m) != 1 {
		t.Fatalf("a tick due now %+v after %d listings; want none more", res, listings(m))
	}
}

// A wall clock that steps back inside the wait ends it, as every interval
// whose start is unknown: one listing, never two, then the wait again.
func TestAClockSteppedBackInsideTheWaitListsOnce(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	_, p := refusedPump(t, m)
	m.pass(-time.Hour)
	if res, n := tickAt(t, m, p); !res.failed || n != 1 {
		t.Fatalf("the tick after the step: %+v, %d listings; want the refusal after 1", res, n)
	}
	m.pass(time.Second)
	if _, n := tickAt(t, m, p); n != 0 {
		t.Fatalf("the next tick: %d listings; want the wait", n)
	}
}

// The view shows the newer of the board's two failures by order time,
// the reconcile's on a tie, each at its wall time; when one clears the
// other shows again with its own time.
func TestSyncViewShowsTheNewerFailure(t *testing.T) {
	t.Parallel()
	other := store.MirrorSyncFailure{At: 20, Wall: 2000, Kind: string(FailureTransient), Message: "delta"}
	rec := store.MirrorReconcileFailure{MirrorSyncFailure: store.MirrorSyncFailure{At: 10, Wall: 1000, Kind: string(FailureTracker), Message: "refused"}, Refused: true}
	at := func(f store.MirrorSyncFailure, order int64) store.MirrorSyncFailure { f.At = order; return f }
	for name, tc := range map[string]struct {
		o    store.MirrorSyncOutcome
		want *FailureView
	}{
		"none":                    {store.MirrorSyncOutcome{}, nil},
		"the other's newer":       {store.MirrorSyncOutcome{Failure: other, ReconcileFailure: rec}, &FailureView{Kind: other.Kind, Message: "delta", At: 2000}},
		"the reconcile's newer":   {store.MirrorSyncOutcome{Failure: at(other, 5), ReconcileFailure: rec}, &FailureView{Kind: rec.Kind, Message: "refused", At: 1000}},
		"a tie":                   {store.MirrorSyncOutcome{Failure: at(other, 10), ReconcileFailure: rec}, &FailureView{Kind: rec.Kind, Message: "refused", At: 1000}},
		"the other cleared":       {store.MirrorSyncOutcome{ReconcileFailure: rec}, &FailureView{Kind: rec.Kind, Message: "refused", At: 1000}},
		"the reconcile's cleared": {store.MirrorSyncOutcome{Failure: other}, &FailureView{Kind: other.Kind, Message: "delta", At: 2000}},
	} {
		if got := syncViewOf(tc.o).Failure; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: shown %+v; want %+v", name, got, tc.want)
		}
	}
}

// A tick inside the wait runs its other units as due and does not fail:
// no failure in its outcome, the connection's problem settled from the
// calls it made, no failure time written, the stored refusal's time
// unchanged. A delta that delivers a row makes history due, which the
// tick after runs; an idle tick inside the wait then costs what an idle
// tick outside it does (TestIdlePumpTickCosts): one delta request, the
// sync state and the record.
func TestATickInsideTheWaitDoesNotFail(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	_, p := refusedPump(t, m)
	at := refusal(m).Wall
	if m.problem() != FailureTracker {
		t.Fatalf("after the refusal the connection's problem is %q; want the refusal's", m.problem())
	}
	delivered := false
	m.p.set(func(sp *scripted) {
		sp.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			if delivered {
				return tracker.RowPage{Next: cursorAfter("10001")}, nil
			}
			delivered = true
			return tracker.RowPage{Rows: []tracker.Row{row("10001", 1_799_000_000_000, "a")}, Next: cursorAfter("10001")}, nil
		}
	})
	inside := func(what string) {
		t.Helper()
		m.pass(time.Second)
		res, n := tickAt(t, m, p)
		st := m.sync(testBoard)
		if res.failed || res.failure != nil || n != 0 || m.problem() != "" || st.Failure != (store.MirrorSyncFailure{}) || st.ReconcileFailure.Wall != at || !st.ReconcileFailure.Refused {
			t.Fatalf("%s: %+v, %d listings, problem %q, stored %+v; want no failure, none, no problem, no failure time, the refusal at %d",
				what, res, n, m.problem(), st.MirrorSyncOutcome, at)
		}
	}
	inside("the tick whose delta delivers a row")
	if !m.sync(testBoard).HistoryDue {
		t.Fatal("the delivered row made no history due")
	}
	history := m.p.count("History")
	inside("the tick with history due")
	if m.p.count("History") != history+1 {
		t.Fatalf("history ran %d times inside the wait; want once", m.p.count("History")-history)
	}
	cs := newCountingStore(m.st)
	m.svc.store = cs
	before := len(m.p.callsInOrder())
	inside("the idle tick")
	provider, stored := countsOf(m.p.callsInOrder()[before:]), cs.take()
	t.Logf("idle board tick inside the wait: provider %s store %s", describeCounts(provider), describeCounts(stored))
	if describeCounts(provider) != "ChangedRows:1" || describeCounts(stored) != "GetMirrorSync:1 RecordMirrorSyncAttempt:1" {
		t.Errorf("an idle board tick inside the wait cost provider %s, store %s", describeCounts(provider), describeCounts(stored))
	}
}

// A pass that completes followed by a unit that fails records both: the
// reconcile's time, clearing the refusal, and the later unit's failure,
// which is shown.
func TestACompletedReconcileThenAHistoryFailure(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	b, p := refusedPump(t, m)
	b.mu.Lock()
	b.fail = nil
	b.mu.Unlock()
	m.p.set(func(sp *scripted) {
		sp.history = func(tracker.HistoryQuery) (tracker.History, error) {
			return tracker.History{}, &trackerapi.StatusError{Tracker: "jira", Status: http.StatusForbidden}
		}
	})
	m.pass(m.svc.cadence.Reconcile)
	out, recorded, err := m.svc.syncBoard(t.Context(), p, Background, func(*run) BoardDue { return BoardDue{Delta: true, Reconcile: true, History: true} })
	if err != nil || recorded == nil {
		t.Fatalf("the tick: %v, recorded %v", err, recorded)
	}
	st := m.sync(testBoard)
	if out.Failure == nil || out.Failure.Kind != FailureForbidden || st.ReconciledAt == 0 || st.ReconcileFailure != (store.MirrorReconcileFailure{}) ||
		st.Failure.Kind != string(FailureForbidden) || recorded.Failure == nil || recorded.Failure.Kind != string(FailureForbidden) {
		t.Fatalf("the tick %+v, stored %+v, recorded %+v; want the reconcile recorded, the refusal cleared, history's failure shown", out, st.MirrorSyncOutcome, recorded)
	}
}

// A tick stopped after its reconcile completed still records it: the
// stored refusal clears though the later unit was stopped.
func TestAStoppedTickRecordsTheUnitsThatFinished(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	b, p := refusedPump(t, m)
	b.mu.Lock()
	b.fail = nil
	b.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.p.set(func(sp *scripted) {
		sp.history = func(tracker.HistoryQuery) (tracker.History, error) {
			cancel()
			return tracker.History{}, context.Canceled
		}
	})
	m.pass(m.svc.cadence.Reconcile)
	if _, _, err := m.svc.syncBoard(ctx, p, Background, func(*run) BoardDue { return BoardDue{Delta: true, Reconcile: true, History: true} }); !errors.Is(err, context.Canceled) {
		t.Fatalf("the stopped tick: %v; want its stop", err)
	}
	if st := m.sync(testBoard); st.ReconciledAt == 0 || st.ReconcileFailure != (store.MirrorReconcileFailure{}) || st.Failure.Kind != "" {
		t.Fatalf("after the stop the store holds %+v; want the reconcile recorded, the refusal cleared, no failure", st.MirrorSyncOutcome)
	}
}

// A board whose listings are incomplete (the tracker cut them) still
// completes a pass when every candidate was attempted: a reconcile
// failure followed by such a pass clears it, and the next listing is a
// reconcile interval later, no tight loop.
func TestAnIncompleteListingsPassClearsAReconcileFailure(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	b := newPassBoard(m, readBatch, func(int) bool { return true })
	m.p.set(func(sp *scripted) {
		sp.boardIssues = func(tracker.BoardID, time.Time) (tracker.Listing, error) {
			b.mu.Lock()
			defer b.mu.Unlock()
			return tracker.Listing{IDs: slices.Clone(b.listed), Complete: false, Rule: passRule}, nil
		}
	})
	b.fail = failAt(1, &trackerapi.TransientError{Err: errors.New("reset")})
	p := idlePump(m)
	if res := m.svc.boardTick(p, false, Background); !res.failed || refusal(m).Kind != string(FailureTransient) {
		t.Fatalf("the failing tick %+v, stored %+v", res, refusal(m))
	}
	m.pass(time.Second)
	if res, n := tickAt(t, m, p); res.failed || res.more || n != 0 || refusal(m) != (store.MirrorReconcileFailure{}) {
		t.Fatalf("the retry: %+v, %d listings, stored %+v; want the pass done and the failure cleared, listing nothing", res, n, refusal(m))
	}
	m.pass(time.Second)
	if _, n := tickAt(t, m, p); n != 0 {
		t.Fatalf("the tick after: %d listings; want none until the interval", n)
	}
	m.pass(m.svc.cadence.Reconcile)
	if _, n := tickAt(t, m, p); n != 1 {
		t.Fatalf("an interval later: %d listings; want 1", n)
	}
}

// Every failure of a rows read that is not a refusal of the rule keeps the
// exact pass: the failed batch and the rest, and the tick after goes on
// with them without listing the board again.
func TestAFailedRowsReadKeepsTheExactPass(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"canceled":     context.Canceled,
		"network":      &trackerapi.TransientError{Err: errors.New("reset")},
		"rate limited": &trackerapi.RateLimitedError{Until: time.Now().Add(time.Minute)},
		"forbidden":    &trackerapi.StatusError{Status: http.StatusForbidden},
		"server":       &trackerapi.StatusError{Status: http.StatusBadGateway},
		"bad request":  &trackerapi.StatusError{Status: http.StatusBadRequest},
		"oversized":    &trackerapi.BodyTooLargeError{Limit: 1},
		"malformed":    errors.New("decode the rows: unexpected end of JSON input"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			b := newPassBoard(m, 3*readBatch, func(int) bool { return true })
			b.fail = failAt(2, err)
			holder := &boardPump{scope: m.scope()}
			if out := passTick(t, m, holder, BoardDue{Structure: true, Reconcile: true}); out.Failure == nil {
				t.Fatalf("the failing tick %+v", out)
			}
			if want := b.listed[readBatch:]; !slices.Equal(holder.pass.left, want) || !reflect.DeepEqual(holder.pass.rule, passRule) {
				t.Fatalf("the pass after the failure holds %d candidates and rule %+v; want the failed batch and the rest, %d, and the listing's",
					len(holder.pass.left), holder.pass.rule, len(want))
			}
			if out := dueTick(t, m, holder); out.Failure != nil || listings(m) != 1 || !rowsHeld(t, m, b.listed) {
				t.Fatalf("the retry %+v after %d listings", out, listings(m))
			}
		})
	}
}

// A pass whose board lifetime is replaced while the pass is open is not
// gone on with, and a refusal does not wait in the new lifetime, whose sync
// state starts clean: after a refusal, or another failure that kept
// candidates, the next tick lists the new lifetime at once.
func TestAPassOfAReplacedBoardLifetimeListsTheNewOne(t *testing.T) {
	t.Parallel()
	for name, fail := range map[string]error{
		"refused": ruleRefused(),
		"network": &trackerapi.TransientError{Err: errors.New("reset")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			b := newPassBoard(m, 3*readBatch, func(int) bool { return true })
			b.fail = failAt(2, fail)
			holder := &boardPump{scope: m.scope()}
			if out := passTick(t, m, holder, BoardDue{Structure: true, Reconcile: true}); out.Failure == nil {
				t.Fatalf("the failing tick %+v", out)
			}
			old := m.sync(testBoard).Lifetime
			m.listBoards(m.conn.ID, testBoards[1:]...)
			if err := m.svc.FinishMirrorCleanup(); err != nil {
				t.Fatal(err)
			}
			m.listBoards(m.conn.ID, testBoards...)
			before := listings(m)
			if out := dueTick(t, m, holder); out.Failure != nil || listings(m) != before+1 || refusal(m) != (store.MirrorReconcileFailure{}) {
				t.Fatalf("the tick of the new lifetime %+v after %d listings (%d before), stored %+v", out, listings(m), before, refusal(m))
			}
			if now := m.sync(testBoard).Lifetime; now == old || !rowsHeld(t, m, b.listed) {
				t.Fatalf("the board's lifetime %d (was %d); every row held %v", now, old, rowsHeld(t, m, b.listed))
			}
		})
	}
}
